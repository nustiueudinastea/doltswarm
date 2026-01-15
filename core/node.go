package core

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

type IdentityResolver interface {
	PublicKeyForPeerID(ctx context.Context, peerID string) ([]byte, error)
}

type NodeConfig struct {
	Dir       string
	Repo      RepoID
	Signer    Signer
	Transport Transport
	Log       *logrus.Entry

	// DB is optional. When set, OpenNode will reuse this already-open DB instead of opening a new one.
	// The Node will not close it on Close().
	DB *DB

	MaxClockSkew   time.Duration
	RepairInterval time.Duration
	// SyncDebounce coalesces bursts of gossip events into a single sync pass, reducing fetch/replay churn.
	// Zero uses a conservative default.
	SyncDebounce time.Duration
	// PullFirstTimeout bounds the best-effort pre-commit sync pass run by Node.Commit/ExecAndCommit.
	// Zero uses a conservative default.
	PullFirstTimeout time.Duration
	// PullFirstPasses controls how many sync passes are attempted before committing.
	// Multiple passes improve the chance of contacting an up-to-date provider when providers are picked opportunistically.
	// Zero uses a conservative default.
	PullFirstPasses int

	// HintProviderTTL controls how long the node remembers "which peer mentioned a given HLC".
	// This is used to prefer a provider likely to have the objects for a hinted commit.
	// Zero uses a conservative default.
	HintProviderTTL time.Duration
	// MaxHintProviders bounds the hint map to prevent unbounded growth in long-running nodes.
	// Zero uses a conservative default.
	MaxHintProviders int

	Identity IdentityResolver

	// Watermark configuration for finalization
	// MinPeersForWatermark is the minimum number of active peers required to compute a watermark.
	// Zero uses the default (2).
	MinPeersForWatermark int
	// WatermarkSlack is the buffer subtracted from min(heads) for conservative finalization.
	// Zero uses the default (30s).
	WatermarkSlack time.Duration
	// PeerActivityTTL is how long before a peer is considered inactive.
	// Zero uses the default (5m).
	PeerActivityTTL time.Duration
	// EnableResubmission controls whether offline commits are automatically resubmitted.
	// Defaults to true when not explicitly set.
	EnableResubmission *bool
}

type hintProviderEntry struct {
	from      string
	expiresAt time.Time
}

type Node struct {
	cfg NodeConfig
	db  *DB
	idx CommitIndex

	ownsDB bool

	mu sync.Mutex

	providerMu    sync.Mutex
	hintProviders map[HLCTimestamp]hintProviderEntry
	peerHeads     map[string]HLCTimestamp
	lastProvider  string

	// Peer activity tracking for watermark computation
	peerActivityMu sync.RWMutex
	peerActivity   map[string]PeerActivity
}

func OpenNode(cfg NodeConfig) (*Node, error) {
	if cfg.Repo.RepoName == "" {
		return nil, fmt.Errorf("Repo is required")
	}
	if cfg.Signer == nil {
		return nil, fmt.Errorf("Signer is required")
	}
	if cfg.Transport == nil {
		return nil, fmt.Errorf("Transport is required")
	}
	if cfg.Transport.Providers() == nil {
		return nil, fmt.Errorf("Transport.Providers is required")
	}
	if cfg.Log == nil {
		cfg.Log = logrus.NewEntry(logrus.StandardLogger())
	}
	if cfg.MaxClockSkew == 0 {
		cfg.MaxClockSkew = MaxClockSkew
	}
	if cfg.RepairInterval == 0 {
		cfg.RepairInterval = 30 * time.Second
	}
	if cfg.SyncDebounce == 0 {
		cfg.SyncDebounce = 250 * time.Millisecond
	}
	if cfg.PullFirstTimeout == 0 {
		cfg.PullFirstTimeout = 10 * time.Second
	}
	if cfg.PullFirstPasses == 0 {
		cfg.PullFirstPasses = 2
	}
	if cfg.HintProviderTTL == 0 {
		cfg.HintProviderTTL = 2 * time.Minute
	}
	if cfg.MaxHintProviders == 0 {
		cfg.MaxHintProviders = 4096
	}

	// Watermark configuration defaults
	if cfg.MinPeersForWatermark == 0 {
		cfg.MinPeersForWatermark = 2
	}
	if cfg.WatermarkSlack == 0 {
		cfg.WatermarkSlack = 30 * time.Second
	}
	if cfg.PeerActivityTTL == 0 {
		cfg.PeerActivityTTL = 5 * time.Minute
	}
	if cfg.EnableResubmission == nil {
		t := true
		cfg.EnableResubmission = &t
	}

	var (
		db     *DB
		ownsDB bool
		err    error
	)
	if cfg.DB != nil {
		db = cfg.DB
	} else {
		if cfg.Dir == "" {
			return nil, fmt.Errorf("Dir is required")
		}
		db, err = Open(cfg.Dir, cfg.Repo.RepoName, cfg.Log, cfg.Signer)
		if err != nil {
			return nil, err
		}
		ownsDB = true
	}

	// Register this repo for swarm:// dbfactory resolution.
	RegisterSwarmProviders(cfg.Repo, cfg.Transport.Providers())

	// Ensure the local DB has a `swarm` remote pointing at swarm://<repo>.
	// This is idempotent and best-effort; failures will be surfaced during fetch.
	_ = db.EnsureSwarmRemote(context.Background(), cfg.Repo)

	n := &Node{
		cfg:           cfg,
		db:            db,
		idx:           NewMemoryCommitIndex(),
		ownsDB:        ownsDB,
		hintProviders: make(map[HLCTimestamp]hintProviderEntry),
		peerHeads:     make(map[string]HLCTimestamp),
		peerActivity:  make(map[string]PeerActivity),
	}

	n.installReconcilerHooks()

	return n, nil
}

// SetCommitRejectedCallback registers a callback invoked when a commit is deterministically dropped
// during reconciliation (e.g. due to a conflict under FWW).
func (n *Node) SetCommitRejectedCallback(cb CommitRejectedCallback) {
	if n == nil || n.db == nil {
		return
	}
	n.db.SetCommitRejectedCallback(cb)
}

func (n *Node) Close() error {
	if n.idx != nil {
		_ = n.idx.Close()
	}
	UnregisterSwarmProviders(n.cfg.Repo)
	if n.db == nil || !n.ownsDB {
		return nil
	}
	return n.db.Close()
}

func (n *Node) installReconcilerHooks() {
	if n == nil || n.db == nil || n.db.reconciler == nil || n.idx == nil {
		return
	}

	// Keep the Node's CommitIndex authoritative for "handled vs pending" state. The reconciler itself
	// is local-only and does not track a durable queue of adverts.
	n.db.reconciler.setCommitRejectedInternalHook(func(ad *CommitAd, reason RejectionReason) {
		_ = reason
		if ad == nil || ad.HLC.IsZero() {
			return
		}
		_ = n.idx.Upsert(context.Background(), CommitIndexEntry{
			HLC:        ad.HLC,
			CommitHash: ad.CommitHash,
			Status:     CommitStatusRejected,
			UpdatedAt:  time.Now(),
		})
	})
}

func (n *Node) Commit(msg string) (string, error) {
	n.pullFirstBestEffort()

	// Check for and resubmit any offline commits that need resubmission
	ctx := context.Background()
	n.resubmitOfflineCommits(ctx)

	commitHash, err := n.db.Commit(msg)
	if err != nil {
		return "", err
	}
	n.publishLocalCommitAd(commitHash)
	// Publish digest immediately after commit to repair missed ads without waiting for RepairInterval
	n.publishDigestBestEffort()
	return commitHash, nil
}

func (n *Node) ExecAndCommit(exec ExecFunc, msg string) (string, error) {
	n.pullFirstBestEffort()

	// Check for and resubmit any offline commits that need resubmission
	ctx := context.Background()
	n.resubmitOfflineCommits(ctx)

	commitHash, err := n.db.ExecAndCommit(exec, msg)
	if err != nil {
		return "", err
	}
	n.publishLocalCommitAd(commitHash)
	// Publish digest immediately after commit to repair missed ads without waiting for RepairInterval
	n.publishDigestBestEffort()
	return commitHash, nil
}

func (n *Node) pullFirstBestEffort() {
	if n == nil || n.db == nil {
		return
	}
	if n.cfg.Transport == nil || n.cfg.Transport.Providers() == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), n.cfg.PullFirstTimeout)
	defer cancel()

	// If we know about a specific pending advertised commit, use it as a hint for the first pass.
	// This helps avoid committing on a stale base (which later forces replay and rewrites commit hashes).
	var hint HLCTimestamp
	if n.idx != nil {
		if entries, err := n.idx.ListAfter(ctx, HLCTimestamp{}, 0); err == nil {
			for _, e := range entries {
				if e.Status != CommitStatusPending {
					continue
				}
				if hint.IsZero() || hint.Less(e.HLC) {
					hint = e.HLC
				}
			}
		}
	}

	var lastErr error
	for i := 0; i < n.cfg.PullFirstPasses; i++ {
		if ctx.Err() != nil {
			return
		}
		h := HLCTimestamp{}
		if !hint.IsZero() {
			h = hint
		}
		if err := n.syncOnce(ctx, h); err != nil && ctx.Err() == nil {
			// Pull-first is best-effort; don't block local writes indefinitely. But do try a few times
			// because providers are picked opportunistically and may be stale.
			lastErr = err
			n.cfg.Log.Debugf("pull-first syncOnce failed: %v", err)
			time.Sleep(200 * time.Millisecond)
			continue
		}
		lastErr = nil
	}

	_ = lastErr
}

// SyncHint performs a single best-effort synchronization pass.
// It is safe to call concurrently with Run(); internal locking serializes work.
func (n *Node) SyncHint(ctx context.Context, hint HLCTimestamp) (changed bool, err error) {
	var before string
	if n != nil && n.db != nil {
		if c, e := n.db.GetLastCommit("main"); e == nil {
			before = c.Hash
		}
	}

	err = n.syncOnce(ctx, hint)
	if err != nil {
		return false, err
	}

	if n != nil && n.db != nil {
		if c, e := n.db.GetLastCommit("main"); e == nil {
			return before != "" && before != c.Hash, nil
		}
	}
	return false, nil
}

func (n *Node) Sync(ctx context.Context) (bool, error) {
	return n.SyncHint(ctx, HLCTimestamp{})
}

func (n *Node) publishLocalCommitAd(commitHash string) {
	if n == nil || n.db == nil || n.cfg.Transport == nil || n.cfg.Transport.Gossip() == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	head, err := n.db.GetLastCommit("main")
	if err != nil || head.Hash != commitHash {
		// Best effort: only publish when the commit is the current main head.
		return
	}

	meta, err := ParseCommitMetadata(head.Message)
	if err != nil || meta == nil {
		return
	}

	// Keep the local index fresh so digests/checkpoint negotiation work even when we
	// haven't imported anything yet.
	_ = n.idx.Upsert(ctx, CommitIndexEntry{
		HLC:        meta.HLC,
		CommitHash: head.Hash,
		Status:     CommitStatusApplied,
		UpdatedAt:  time.Now(),
	})
	_ = n.idx.SetHead(ctx, meta.HLC, head.Hash)
	_ = n.idx.AppendCheckpoint(ctx, Checkpoint{HLC: meta.HLC, CommitHash: head.Hash})

	ad := CommitAdV1{
		Repo:         n.cfg.Repo,
		HLC:          meta.HLC,
		MetadataJSON: []byte(head.Message),
		MetadataSig:  []byte(meta.Signature),
		ObservedAt:   time.Now(),
	}
	n.cfg.Log.Debugf("[publish] CommitAd hlc=%s repo=%s/%s", ad.HLC, ad.Repo.Org, ad.Repo.RepoName)
	if err := n.cfg.Transport.Gossip().PublishCommitAd(ctx, ad); err != nil {
		n.cfg.Log.Debugf("PublishCommitAd failed: %v", err)
	}
}

func (n *Node) GetLastCommit(branch string) (Commit, error) { return n.db.GetLastCommit(branch) }
func (n *Node) GetAllCommits() ([]Commit, error)            { return n.db.GetAllCommits() }

// Run starts the gossip-first synchronization loops.
//
// It ingests commit ads and digests and advances main by importing commit bundles and
// applying them deterministically (FF when possible, replay otherwise).
func (n *Node) Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if n.cfg.Transport == nil || n.cfg.Transport.Gossip() == nil || n.cfg.Transport.Providers() == nil {
		return fmt.Errorf("transport gossip and providers are required")
	}

	sub, err := n.cfg.Transport.Gossip().Subscribe(ctx)
	if err != nil {
		return err
	}
	defer sub.Close()

	n.cfg.Log.Infof("Node sync loop started (repo=%s)", n.cfg.Repo.RepoName)

	_ = n.initIndexFromDB(ctx)

	var hintMu sync.Mutex
	var hintMin HLCTimestamp
	var hintMax HLCTimestamp
	trigger := make(chan struct{}, 1)
	requestSync := func(h HLCTimestamp) {
		hintMu.Lock()
		if !h.IsZero() {
			if hintMin.IsZero() || h.Less(hintMin) {
				hintMin = h
			}
			if hintMax.IsZero() || hintMax.Less(h) {
				hintMax = h
			}
		}
		hintMu.Unlock()
		select {
		case trigger <- struct{}{}:
		default:
		}
	}

	syncLoopErr := make(chan error, 1)
	go func() {
		for {
			select {
			case <-ctx.Done():
				syncLoopErr <- ctx.Err()
				return
			case <-trigger:
				// Debounce: coalesce bursts of triggers (e.g. many commit ads) into one sync.
				if n.cfg.SyncDebounce > 0 {
					t := time.NewTimer(n.cfg.SyncDebounce)
					for {
						select {
						case <-ctx.Done():
							t.Stop()
							syncLoopErr <- ctx.Err()
							return
						case <-trigger:
							if !t.Stop() {
								select {
								case <-t.C:
								default:
								}
							}
							t.Reset(n.cfg.SyncDebounce)
						case <-t.C:
							goto DO_SYNC
						}
					}
				}
			DO_SYNC:
				hintMu.Lock()
				minH := hintMin
				maxH := hintMax
				hintMin = HLCTimestamp{}
				hintMax = HLCTimestamp{}
				hintMu.Unlock()
				n.cfg.Log.Debugf("[sync] Starting syncOnce (hint min=%s max=%s)", minH, maxH)
				syncStart := time.Now()
				syncErr := n.syncOnce(ctx, minH)
				syncDur := time.Since(syncStart).Round(time.Millisecond)
				if syncErr != nil {
					// This is the primary "why aren't we converging?" signal in integration runs,
					// which usually run at info level (debug is suppressed).
					if ctx.Err() == nil {
						n.cfg.Log.Warnf("syncOnce failed after %v: %v", syncDur, syncErr)
					} else {
						n.cfg.Log.Debugf("syncOnce failed (ctx done) after %v: %v", syncDur, syncErr)
					}
				} else {
					n.cfg.Log.Debugf("[sync] syncOnce completed in %v", syncDur)
				}
				if !maxH.IsZero() {
					// If we were triggered by specific adverts/digests and we're still behind the newest one,
					// schedule another pass soon (avoids waiting for RepairInterval when the first fetch hit a
					// provider that didn't yet have the objects).
					if _, headMeta, err := n.getHeadMeta(context.Background()); err == nil && headMeta != nil {
						if headMeta.HLC.Less(maxH) {
							go func(hint HLCTimestamp) {
								time.Sleep(500 * time.Millisecond)
								requestSync(hint)
							}(maxH)
						}
					}
				}
			}
		}
	}()

	digestTicker := time.NewTicker(n.cfg.RepairInterval)
	defer digestTicker.Stop()

	events := make(chan GossipEvent, 256)
	subErr := make(chan error, 1)
	go func() {
		for {
			evt, err := sub.Next(ctx)
			if err != nil {
				subErr <- err
				return
			}
			select {
			case events <- evt:
			default:
				// Drop if consumer is too slow.
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-subErr:
			return err
		case err := <-syncLoopErr:
			return err
		case <-digestTicker.C:
			n.cfg.Log.Debugf("[repair] RepairInterval tick - publishing digest and requesting sync")
			n.publishDigestBestEffort()
			// Anti-entropy: periodically attempt a sync even if we missed commit adverts.
			requestSync(HLCTimestamp{})
		case evt := <-events:
			if evt.CommitAd != nil {
				n.onCommitAd(ctx, evt.From, *evt.CommitAd, requestSync)
			}
			if evt.Digest != nil {
				n.onDigest(ctx, evt.From, *evt.Digest, requestSync)
			}
		}
	}
}

func (n *Node) publishDigestBestEffort() {
	if n == nil || n.cfg.Transport == nil || n.cfg.Transport.Gossip() == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	head, headMeta, err := n.getHeadMeta(ctx)
	if err != nil || headMeta == nil {
		return
	}
	cps, _ := n.idx.RecentCheckpoints(ctx, 64)
	d := DigestV1{
		Repo:        n.cfg.Repo,
		HeadHLC:     headMeta.HLC,
		HeadHash:    head.Hash,
		Checkpoints: cps,
		ObservedAt:  time.Now(),
	}
	if err := n.cfg.Transport.Gossip().PublishDigest(ctx, d); err != nil {
		n.cfg.Log.Debugf("PublishDigest failed: %v", err)
	}
}

func (n *Node) onCommitAd(ctx context.Context, from string, ad CommitAdV1, requestSync func(HLCTimestamp)) {
	if ctx == nil {
		ctx = context.Background()
	}
	if ad.Repo != n.cfg.Repo {
		return
	}

	shortFrom := from
	if len(shortFrom) > 12 {
		shortFrom = shortFrom[:12]
	}
	shortPeerID := ad.HLC.PeerID
	if len(shortPeerID) > 12 {
		shortPeerID = shortPeerID[:12]
	}

	// If we've already decided an HLC is applied or rejected, ignore repeated adverts.
	if n.idx != nil {
		if e, ok, _ := n.idx.Get(ctx, ad.HLC); ok {
			if e.Status == CommitStatusApplied || e.Status == CommitStatusRejected {
				n.cfg.Log.Debugf("[gossip] CommitAd from=%s hlc=%s (peer=%s) - already %v, ignoring",
					shortFrom, ad.HLC, shortPeerID, e.Status)
				return
			}
		}
	}

	n.observeProvider(from, ad.HLC)
	n.rememberHintProvider(ad.HLC, from)
	// Ignore our own adverts; local commits are already present and indexed.
	if n.cfg.Signer != nil && ad.HLC.PeerID == n.cfg.Signer.GetID() {
		n.cfg.Log.Debugf("[gossip] CommitAd from=%s hlc=%s - own advert, ignoring", shortFrom, ad.HLC)
		return
	}
	now := time.Now().UnixNano()
	if n.cfg.MaxClockSkew > 0 && ad.HLC.Wall > now+int64(n.cfg.MaxClockSkew) {
		n.cfg.Log.Debugf("[gossip] CommitAd from=%s hlc=%s - clock skew rejected", shortFrom, ad.HLC)
		_ = n.idx.Upsert(ctx, CommitIndexEntry{
			HLC:       ad.HLC,
			Status:    CommitStatusRejected,
			UpdatedAt: time.Now(),
		})
		return
	}

	meta, err := ParseCommitMetadata(string(ad.MetadataJSON))
	if err != nil || meta == nil {
		n.cfg.Log.Debugf("[gossip] CommitAd from=%s hlc=%s - metadata parse failed: %v", shortFrom, ad.HLC, err)
		return
	}
	if !meta.HLC.Equal(ad.HLC) {
		n.cfg.Log.Debugf("[gossip] CommitAd from=%s hlc=%s - HLC mismatch (meta=%s)", shortFrom, ad.HLC, meta.HLC)
		_ = n.idx.Upsert(ctx, CommitIndexEntry{
			HLC:       ad.HLC,
			Status:    CommitStatusRejected,
			UpdatedAt: time.Now(),
		})
		return
	}
	if len(ad.MetadataSig) > 0 && meta.Signature != string(ad.MetadataSig) {
		n.cfg.Log.Debugf("[gossip] CommitAd from=%s hlc=%s - signature mismatch", shortFrom, ad.HLC)
		_ = n.idx.Upsert(ctx, CommitIndexEntry{
			HLC:       ad.HLC,
			Status:    CommitStatusRejected,
			UpdatedAt: time.Now(),
		})
		return
	}

	// Verify remote metadata signatures when an identity resolver is configured.
	//
	// Without an IdentityResolver, the node accepts remote adverts (best-effort) to avoid
	// false rejections in deployments that haven't wired key distribution yet.
	if n.cfg.Identity != nil {
		pub, err := n.cfg.Identity.PublicKeyForPeerID(ctx, ad.HLC.PeerID)
		if err != nil || len(pub) == 0 {
			// Strict mode: when an identity resolver is configured, do not accept unverifiable adverts.
			n.cfg.Log.Debugf("[gossip] CommitAd from=%s hlc=%s - identity lookup failed: %v", shortFrom, ad.HLC, err)
			_ = n.idx.Upsert(ctx, CommitIndexEntry{
				HLC:       ad.HLC,
				Status:    CommitStatusRejected,
				UpdatedAt: time.Now(),
			})
			return
		}
		if n.cfg.Signer == nil || meta.Verify(n.cfg.Signer, string(pub)) != nil {
			n.cfg.Log.Debugf("[gossip] CommitAd from=%s hlc=%s - signature verification failed", shortFrom, ad.HLC)
			_ = n.idx.Upsert(ctx, CommitIndexEntry{
				HLC:       ad.HLC,
				Status:    CommitStatusRejected,
				UpdatedAt: time.Now(),
			})
			return
		}
	}

	// Update local HLC from remote timestamp to reduce clock drift and replay churn.
	// This is a key part of HLC hygiene: observing remote timestamps helps clocks converge.
	if rec := n.db.GetReconciler(); rec != nil {
		rec.GetHLC().Update(ad.HLC)
	}

	_ = n.idx.Upsert(ctx, CommitIndexEntry{
		HLC:       ad.HLC,
		Status:    CommitStatusPending,
		UpdatedAt: time.Now(),
	})

	n.cfg.Log.Debugf("[gossip] CommitAd from=%s hlc=%s (peer=%s) - accepted, requesting sync",
		shortFrom, ad.HLC, shortPeerID)
	requestSync(ad.HLC)
}

func (n *Node) onDigest(ctx context.Context, from string, d DigestV1, requestSync func(HLCTimestamp)) {
	if d.Repo != n.cfg.Repo {
		return
	}

	shortFrom := from
	if len(shortFrom) > 12 {
		shortFrom = shortFrom[:12]
	}

	// Observe provider with checkpoints for safe watermark computation
	n.observeProviderWithCheckpoints(from, d.HeadHLC, d.Checkpoints)
	if !d.HeadHLC.IsZero() {
		n.rememberHintProvider(d.HeadHLC, from)

		// Update local HLC from remote head to reduce clock drift and replay churn.
		if rec := n.db.GetReconciler(); rec != nil {
			rec.GetHLC().Update(d.HeadHLC)
		}
	}
	head, headMeta, err := n.getHeadMeta(ctx)
	if err != nil || headMeta == nil || head.Hash == "" {
		n.cfg.Log.Debugf("[gossip] Digest from=%s headHLC=%s - no local head, requesting sync",
			shortFrom, d.HeadHLC)
		requestSync(d.HeadHLC)
		return
	}
	if headMeta.HLC.Less(d.HeadHLC) {
		n.cfg.Log.Debugf("[gossip] Digest from=%s headHLC=%s - remote ahead (local=%s), requesting sync",
			shortFrom, d.HeadHLC, headMeta.HLC)
		requestSync(d.HeadHLC)
	}

	// Repair missed commit adverts: if the remote digest contains checkpoints we don't have,
	// trigger a sync even if our head is already ahead.
	for _, cp := range d.Checkpoints {
		if cp.CommitHash == "" || cp.HLC.IsZero() {
			continue
		}
		e, ok, _ := n.idx.Get(ctx, cp.HLC)
		if !ok || (e.Status != CommitStatusApplied && e.Status != CommitStatusRejected) || (e.CommitHash != "" && e.CommitHash != cp.CommitHash) {
			n.cfg.Log.Debugf("[gossip] Digest from=%s - missing checkpoint hlc=%s, requesting sync",
				shortFrom, cp.HLC)
			// Remember the provider that advertised this checkpoint so the first sync attempt
			// is more likely to contact a peer that has the missing commits.
			n.rememberHintProvider(cp.HLC, from)
			requestSync(cp.HLC)
		}
	}
}

func (n *Node) observeProvider(from string, hlc HLCTimestamp) {
	if n == nil || from == "" || hlc.IsZero() {
		return
	}
	n.providerMu.Lock()
	n.lastProvider = from
	if cur, ok := n.peerHeads[from]; !ok || cur.Less(hlc) {
		n.peerHeads[from] = hlc
	}
	n.providerMu.Unlock()

	// Also track peer activity for watermark computation (without checkpoints)
	n.observePeerActivityInternal(from, hlc, nil)
}

// observeProviderWithCheckpoints tracks provider with checkpoints from digest
func (n *Node) observeProviderWithCheckpoints(from string, hlc HLCTimestamp, checkpoints []Checkpoint) {
	if n == nil || from == "" {
		return
	}
	n.providerMu.Lock()
	n.lastProvider = from
	if !hlc.IsZero() {
		if cur, ok := n.peerHeads[from]; !ok || cur.Less(hlc) {
			n.peerHeads[from] = hlc
		}
	}
	n.providerMu.Unlock()

	// Track peer activity with checkpoints for safe watermark computation
	n.observePeerActivityInternal(from, hlc, checkpoints)
}

// observePeerActivityInternal tracks peer activity for watermark computation
func (n *Node) observePeerActivityInternal(peerID string, hlc HLCTimestamp, checkpoints []Checkpoint) {
	if n == nil || peerID == "" {
		return
	}
	n.peerActivityMu.Lock()
	defer n.peerActivityMu.Unlock()

	now := time.Now()
	activity, ok := n.peerActivity[peerID]
	if !ok {
		activity = PeerActivity{PeerID: peerID}
	}
	activity.LastSeenAt = now
	if !hlc.IsZero() && (activity.HeadHLC.IsZero() || activity.HeadHLC.Less(hlc)) {
		activity.HeadHLC = hlc
	}
	// Update checkpoints if provided (from digest)
	if len(checkpoints) > 0 {
		activity.Checkpoints = checkpoints
	}
	n.peerActivity[peerID] = activity
}

func (n *Node) rememberHintProvider(hlc HLCTimestamp, from string) {
	if n == nil || from == "" || hlc.IsZero() {
		return
	}
	n.providerMu.Lock()
	defer n.providerMu.Unlock()

	now := time.Now()
	if n.cfg.MaxHintProviders > 0 && len(n.hintProviders) >= n.cfg.MaxHintProviders {
		n.cleanupHintProvidersLocked(now)
		// If we still exceed the bound, drop arbitrary entries.
		for len(n.hintProviders) >= n.cfg.MaxHintProviders {
			for k := range n.hintProviders {
				delete(n.hintProviders, k)
				break
			}
		}
	}

	n.hintProviders[hlc] = hintProviderEntry{
		from:      from,
		expiresAt: now.Add(n.cfg.HintProviderTTL),
	}
}

func (n *Node) popHintProvider(hlc HLCTimestamp) string {
	if n == nil || hlc.IsZero() {
		return ""
	}
	n.providerMu.Lock()
	defer n.providerMu.Unlock()
	e, ok := n.hintProviders[hlc]
	if !ok {
		return ""
	}
	delete(n.hintProviders, hlc)
	if !e.expiresAt.IsZero() && time.Now().After(e.expiresAt) {
		return ""
	}
	return e.from
}

func (n *Node) cleanupHintProvidersLocked(now time.Time) {
	if n == nil {
		return
	}
	for k, v := range n.hintProviders {
		if !v.expiresAt.IsZero() && now.After(v.expiresAt) {
			delete(n.hintProviders, k)
		}
	}
}

func (n *Node) bestProvider() string {
	if n == nil {
		return ""
	}
	n.providerMu.Lock()
	defer n.providerMu.Unlock()
	best := n.lastProvider
	bestHLC := HLCTimestamp{}
	for id, h := range n.peerHeads {
		if best == "" || bestHLC.Less(h) {
			best = id
			bestHLC = h
		}
	}
	return best
}

// computeCurrentWatermark computes the finalization watermark from observed peer activity
func (n *Node) computeCurrentWatermark() (HLCTimestamp, bool) {
	if n == nil {
		return HLCTimestamp{}, false
	}

	n.peerActivityMu.RLock()
	activities := make([]PeerActivity, 0, len(n.peerActivity))
	for _, a := range n.peerActivity {
		activities = append(activities, a)
	}
	n.peerActivityMu.RUnlock()

	cfg := WatermarkConfig{
		MinPeersForWatermark: n.cfg.MinPeersForWatermark,
		SlackDuration:        n.cfg.WatermarkSlack,
		ActivityTTL:          n.cfg.PeerActivityTTL,
	}
	return ComputeWatermark(activities, cfg)
}

// computeCurrentWatermarkWithCheckpoint computes the watermark and returns the checkpoint to finalize
func (n *Node) computeCurrentWatermarkWithCheckpoint() (WatermarkResult, bool) {
	if n == nil {
		return WatermarkResult{}, false
	}

	n.peerActivityMu.RLock()
	activities := make([]PeerActivity, 0, len(n.peerActivity))
	for _, a := range n.peerActivity {
		activities = append(activities, a)
	}
	n.peerActivityMu.RUnlock()

	cfg := WatermarkConfig{
		MinPeersForWatermark: n.cfg.MinPeersForWatermark,
		SlackDuration:        n.cfg.WatermarkSlack,
		ActivityTTL:          n.cfg.PeerActivityTTL,
	}
	return ComputeWatermarkWithCheckpoint(activities, cfg)
}

// tryAdvanceFinalizedBase attempts to advance the finalized base based on current watermark.
// SAFETY: Only finalizes commonly-known checkpoints, never arbitrary local commits.
// This prevents accidentally finalizing local-only offline commits during quiet intervals.
func (n *Node) tryAdvanceFinalizedBase(ctx context.Context) {
	if n == nil || n.db == nil || n.idx == nil {
		return
	}

	result, ok := n.computeCurrentWatermarkWithCheckpoint()
	if !ok {
		return // Not enough peers
	}

	// SAFETY: Only finalize from commonly-known checkpoints
	if result.FinalizeCheckpoint.HLC.IsZero() {
		return // No commonly-known checkpoint at or before watermark
	}

	currentBase, hasBase, _ := n.idx.FinalizedBase(ctx)
	if hasBase && !currentBase.HLC.Less(result.FinalizeCheckpoint.HLC) {
		return // Checkpoint hasn't advanced past current base
	}

	// The checkpoint HLC is the key; commit hash may vary across peers due to replays.
	// CRITICAL: We must ALWAYS look up the local commit by HLC to get the correct local hash.
	// The checkpoint's CommitHash comes from a remote peer's digest and may be different
	// from our local hash for the same HLC due to replay/reconciliation.
	var localHash string
	commits, err := n.db.GetAllCommits()
	if err == nil {
		for _, c := range commits {
			meta, err := ParseCommitMetadata(c.Message)
			if err == nil && meta.HLC.Equal(result.FinalizeCheckpoint.HLC) {
				localHash = c.Hash
				break
			}
		}
	}

	if localHash == "" {
		// We don't have this checkpoint locally yet; wait for sync
		return
	}

	_ = n.idx.SetFinalizedBase(ctx, FinalizedBase{
		HLC:        result.FinalizeCheckpoint.HLC,
		CommitHash: localHash,
		UpdatedAt:  time.Now(),
	})

	n.cfg.Log.Debugf("Advanced finalized base to commonly-known checkpoint HLC=%s hash=%s",
		result.FinalizeCheckpoint.HLC, localHash[:min(8, len(localHash))])
}

// resubmitOfflineCommits checks for local commits that need to be resubmitted
// because their original HLC is at or before the watermark (late offline commits).
func (n *Node) resubmitOfflineCommits(ctx context.Context) {
	if n == nil || n.db == nil || n.idx == nil {
		return
	}
	if n.cfg.EnableResubmission == nil || !*n.cfg.EnableResubmission {
		return
	}

	watermark, ok := n.computeCurrentWatermark()
	if !ok {
		return // Not enough peers to determine watermark
	}

	finalizedBase, hasBase, _ := n.idx.FinalizedBase(ctx)
	if !hasBase {
		return // No finalized base yet
	}

	// Get all local commits
	commits, err := n.db.GetAllCommits()
	if err != nil {
		return
	}

	// Find commits that need resubmission:
	// - HLC is after finalized base (not yet finalized)
	// - HLC is at or before watermark (would be finalized by now if canonical)
	// - Not already in the canonical history via normal sync
	for _, c := range commits {
		meta, err := ParseCommitMetadata(c.Message)
		if err != nil || meta == nil {
			continue
		}

		// Skip if already finalized
		if !IsAfterFinalizedBase(meta.HLC, &finalizedBase) {
			continue
		}

		// Skip if after watermark (will be handled normally)
		if watermark.Less(meta.HLC) {
			continue
		}

		// Skip if this is already a resubmission
		if meta.IsResubmission() {
			continue
		}

		// This commit needs resubmission
		n.resubmitCommit(ctx, c, meta)
	}
}

// resubmitCommit creates an actual resubmission commit by cherry-picking
// the original commit onto current main with resubmission metadata.
func (n *Node) resubmitCommit(ctx context.Context, original Commit, originalMeta *CommitMetadata) {
	if n == nil || n.db == nil || n.cfg.Signer == nil {
		return
	}

	// Compute origin event ID for idempotency
	originJSON, err := originalMeta.Marshal()
	if err != nil {
		return
	}
	originEventID := ComputeOriginEventID(originJSON)

	// Check if already resubmitted (idempotency)
	if _, found, _ := n.idx.GetOriginEventIDMapping(ctx, originEventID); found {
		return // Already resubmitted
	}

	// Lock reconciler to prevent concurrent mutations
	n.db.reconciler.mu.Lock()
	defer n.db.reconciler.mu.Unlock()

	// Create resubmission with fresh HLC
	freshHLC := n.db.reconciler.GetHLC().Now()

	// Get a connection for the cherry-pick operation
	conn, err := n.db.sqldb.Conn(ctx)
	if err != nil {
		n.cfg.Log.Warnf("Failed to get connection for resubmission: %v", err)
		return
	}
	defer conn.Close()

	// Ensure we're on main
	if _, err := conn.ExecContext(ctx, "CALL DOLT_CHECKOUT('main');"); err != nil {
		n.cfg.Log.Warnf("Failed to checkout main for resubmission: %v", err)
		return
	}

	// Cherry-pick the original commit onto current main
	_, err = conn.ExecContext(ctx, fmt.Sprintf("CALL DOLT_CHERRY_PICK('%s');", original.Hash))
	if err != nil {
		// Check if it's a conflict - if so, abort and mark as rejected
		if isConflictError(err) {
			n.cfg.Log.Warnf("Conflict resubmitting %s - dropping (FWW)", original.Hash[:8])
			_, _ = conn.ExecContext(ctx, "CALL DOLT_CHERRY_PICK('--abort');")
			// Still record the mapping to prevent retry
			_ = n.idx.SetOriginEventIDMapping(ctx, originEventID, HLCTimestamp{})
			return
		}
		// Check if no changes (already applied)
		if isNoChangesError(err) {
			n.cfg.Log.Debugf("No changes to resubmit for %s - already applied", original.Hash[:8])
			_ = n.idx.SetOriginEventIDMapping(ctx, originEventID, HLCTimestamp{})
			return
		}
		n.cfg.Log.Warnf("Failed to cherry-pick for resubmission: %v", err)
		return
	}

	// Create resubmission metadata
	resubMeta, err := NewResubmissionMetadata(
		originalMeta.Message,
		freshHLC,
		originalMeta.ContentHash,
		n.cfg.Signer.GetID(),
		fmt.Sprintf("%s@doltswarm", n.cfg.Signer.GetID()),
		time.Now(),
		originalMeta,
		original.Hash,
	)
	if err != nil {
		n.cfg.Log.Warnf("Failed to create resubmission metadata: %v", err)
		return
	}

	if err := resubMeta.Sign(n.cfg.Signer); err != nil {
		n.cfg.Log.Warnf("Failed to sign resubmission: %v", err)
		return
	}

	metaJSON, err := resubMeta.Marshal()
	if err != nil {
		n.cfg.Log.Warnf("Failed to marshal resubmission metadata: %v", err)
		return
	}

	// Amend the cherry-picked commit with resubmission metadata
	_, err = conn.ExecContext(ctx, fmt.Sprintf(
		"CALL DOLT_COMMIT('--amend', '-m', '%s', '--author', '%s <%s>', '--date', '%s');",
		escapeSQL(metaJSON),
		escapeSQL(n.cfg.Signer.GetID()),
		escapeSQL(fmt.Sprintf("%s@doltswarm", n.cfg.Signer.GetID())),
		time.Now().Format(time.RFC3339Nano),
	))
	if err != nil {
		n.cfg.Log.Warnf("Failed to amend resubmission commit: %v", err)
		return
	}

	// Get the new commit hash
	var newCommitHash string
	if err := conn.QueryRowContext(ctx, "SELECT HASHOF('HEAD');").Scan(&newCommitHash); err != nil {
		n.cfg.Log.Warnf("Failed to get resubmission commit hash: %v", err)
		return
	}

	// Record idempotency mapping
	_ = n.idx.SetOriginEventIDMapping(ctx, originEventID, freshHLC)

	// Publish resubmission ad (now there's an actual commit to fetch)
	if n.cfg.Transport != nil && n.cfg.Transport.Gossip() != nil {
		if err := n.cfg.Transport.Gossip().PublishCommitAd(ctx, CommitAdV1{
			Repo:         n.cfg.Repo,
			HLC:          freshHLC,
			MetadataJSON: []byte(metaJSON),
			MetadataSig:  []byte(resubMeta.Signature),
			ObservedAt:   time.Now(),
		}); err != nil {
			n.cfg.Log.Debugf("Failed to publish resubmission ad: %v", err)
		}
	}

	n.cfg.Log.Infof("Resubmitted offline commit %s as %s with origin HLC=%s, new HLC=%s",
		original.Hash[:8], newCommitHash[:8], originalMeta.HLC, freshHLC)
}

func (n *Node) initIndexFromDB(ctx context.Context) error {
	if n == nil || n.db == nil || n.idx == nil {
		return nil
	}

	// Preserve non-applied entries (e.g. rejected conflict drops) across rebuilds.
	// initIndexFromDB is called after fetch/replay to refresh applied hashes, but it should not
	// re-open already-settled "handled" decisions like rejections.
	var preserved []CommitIndexEntry
	if entries, err := n.idx.ListAfter(ctx, HLCTimestamp{}, 0); err == nil {
		for _, e := range entries {
			if e.Status != CommitStatusApplied && e.Status != CommitStatusUnknown {
				preserved = append(preserved, e)
			}
		}
	}

	if mi, ok := n.idx.(*MemoryCommitIndex); ok {
		mi.mu.Lock()
		mi.resetForRebuild()
		mi.mu.Unlock()
	}

	commits, err := n.db.GetAllCommits()
	if err != nil {
		return err
	}

	type cp struct {
		h HLCTimestamp
		k string
	}
	cps := make([]cp, 0, len(commits))
	for _, c := range commits {
		meta, err := ParseCommitMetadata(c.Message)
		if err != nil || meta == nil {
			continue
		}
		_ = n.idx.Upsert(ctx, CommitIndexEntry{
			HLC:        meta.HLC,
			CommitHash: c.Hash,
			Status:     CommitStatusApplied,
			UpdatedAt:  time.Now(),
		})
		cps = append(cps, cp{h: meta.HLC, k: c.Hash})
	}

	sort.Slice(cps, func(i, j int) bool { return cps[i].h.Less(cps[j].h) })
	// AppendCheckpoint prepends; feed oldest->newest to end up with newest->oldest.
	for i := 0; i < len(cps); i++ {
		_ = n.idx.AppendCheckpoint(ctx, Checkpoint{HLC: cps[i].h, CommitHash: cps[i].k})
	}

	head, headMeta, err := n.getHeadMeta(ctx)
	if err == nil && headMeta != nil {
		_ = n.idx.SetHead(ctx, headMeta.HLC, head.Hash)
	}

	// Re-apply preserved non-applied entries unless they became applied during rebuild.
	for _, e := range preserved {
		cur, ok, _ := n.idx.Get(ctx, e.HLC)
		if ok && cur.Status == CommitStatusApplied {
			continue
		}
		_ = n.idx.Upsert(ctx, e)
	}

	return nil
}

func (n *Node) getHeadMeta(ctx context.Context) (Commit, *CommitMetadata, error) {
	head, err := n.db.GetLastCommit("main")
	if err != nil {
		return Commit{}, nil, err
	}
	meta, err := ParseCommitMetadata(head.Message)
	if err != nil {
		return head, nil, err
	}
	return head, meta, nil
}

// syncResult contains the outcome of a single sync attempt.
type syncResult struct {
	shouldResubmit bool   // true if we should resubmit offline commits
	gotNoCommits   bool   // true if we expected commits (had hints) but got none
	usedProvider   string // which provider was used for the fetch
}

// maxSyncRetries limits how many different providers we try before giving up.
const maxSyncRetries = 3

func (n *Node) syncOnce(ctx context.Context, hint HLCTimestamp) error {
	var excludedProviders []string

	for attempt := 0; attempt < maxSyncRetries; attempt++ {
		syncCtx := ctx
		if len(excludedProviders) > 0 {
			syncCtx = WithExcludedProviders(ctx, excludedProviders)
		}

		// Set up a tracker to capture which provider was used
		tracker := &UsedProviderTracker{}
		syncCtx = WithUsedProviderTracker(syncCtx, tracker)

		// Prefer fetching from the peer that told us about the hinted commit (if any).
		// This avoids getting stuck contacting a stale provider that doesn't yet have the objects.
		// We consume the hint on the first attempt only; retries use round-robin with exclusions.
		if !hint.IsZero() {
			if attempt == 0 {
				if from := n.popHintProvider(hint); from != "" {
					syncCtx = WithPreferredProvider(syncCtx, from)
				}
			}
		} else if from := n.bestProvider(); from != "" {
			syncCtx = WithPreferredProvider(syncCtx, from)
		}

		// Do the actual sync (with mutex held), track if we should resubmit after
		result, err := n.syncOnceInner(syncCtx, hint)
		if err != nil {
			return err
		}

		// Capture the provider that was used (set during fetch)
		usedProvider := tracker.Get()
		if result.usedProvider == "" {
			result.usedProvider = usedProvider
		}

		// Resubmit late offline commits AFTER releasing reconciler mutex to avoid deadlock.
		// resubmitCommit also takes the reconciler mutex internally.
		if result.shouldResubmit {
			n.resubmitOfflineCommits(ctx)
		}

		// If we got commits or we had no hint (nothing expected), we're done
		if !result.gotNoCommits {
			return nil
		}

		// We expected commits but got none - the provider might be stale.
		// Retry with a different provider if we haven't exceeded max retries.
		if result.usedProvider != "" {
			excludedProviders = append(excludedProviders, result.usedProvider)
			providerShort := result.usedProvider
			if len(providerShort) > 12 {
				providerShort = providerShort[:12]
			}
			n.cfg.Log.Debugf("[sync] Provider %s returned no commits despite hints, retrying with different provider (attempt %d/%d)",
				providerShort, attempt+1, maxSyncRetries)
		} else {
			// No provider info, can't meaningfully retry
			return nil
		}
	}

	n.cfg.Log.Debugf("[sync] Exhausted %d provider retries, giving up this sync pass", maxSyncRetries)
	return nil
}

// syncOnceInner performs the actual sync with mutexes held.
// Returns a syncResult indicating what happened during the sync.
func (n *Node) syncOnceInner(ctx context.Context, hint HLCTimestamp) (syncResult, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	// Get the provider tracker to report which provider was used
	var usedProvider string
	if tracker := UsedProviderTrackerFromContext(ctx); tracker != nil {
		defer func() {
			usedProvider = tracker.Get()
		}()
	}
	makeResult := func(shouldResubmit, gotNoCommits bool) syncResult {
		return syncResult{
			shouldResubmit: shouldResubmit,
			gotNoCommits:   gotNoCommits,
			usedProvider:   usedProvider,
		}
	}

	// Serialize against local commits and other Dolt mutations.
	// DB.Commit/ExecAndCommit already lock this; sync must do the same.
	if n != nil && n.db != nil && n.db.reconciler != nil {
		n.db.reconciler.mu.Lock()
		defer n.db.reconciler.mu.Unlock()
	}

	// Bound a single sync attempt.
	innerCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	if err := n.db.EnsureSwarmRemote(innerCtx, n.cfg.Repo); err != nil {
		return syncResult{}, err
	}
	if err := n.db.FetchSwarm(innerCtx); err != nil {
		return syncResult{}, err
	}

	// Now we can capture the used provider (fetch has completed)
	// Try context-based tracker first, then fall back to provider picker's LastUsedProvider
	if tracker := UsedProviderTrackerFromContext(ctx); tracker != nil {
		usedProvider = tracker.Get()
	}
	if usedProvider == "" {
		// Context tracking didn't work (e.g., context not propagated through Dolt SQL layer)
		// Fall back to the provider picker's LastUsedProvider if available
		if getter, ok := n.cfg.Transport.Providers().(LastUsedProviderGetter); ok {
			usedProvider = getter.LastUsedProvider()
		}
	}

	remoteRef := "remotes/swarm/main"
	remoteHead, ok, err := n.db.GetBranchHead(innerCtx, remoteRef)
	if err != nil {
		return syncResult{}, err
	}
	if !ok {
		// Nothing fetched yet (no remote branch).
		return makeResult(false, false), nil
	}

	// Get local head for diagnostic comparison
	localHead, _, _ := n.db.GetBranchHead(innerCtx, "main")
	providerShort := usedProvider
	if len(providerShort) > 12 {
		providerShort = providerShort[:12]
	}

	mergeBase, err := n.db.MergeBase(innerCtx, "main", remoteRef)
	if err != nil {
		return syncResult{}, err
	}
	if mergeBase == "" {
		// Bootstrap: no shared history. Force main to the remote head and rebuild.
		if err := n.forceResetMainTo(innerCtx, remoteHead); err != nil {
			return syncResult{}, err
		}
		_ = n.initIndexFromDB(innerCtx)
		return makeResult(false, false), nil
	}

	imported, err := n.db.reconciler.getCommitsSince(innerCtx, n.db.sqldb, remoteRef, mergeBase)
	if err != nil {
		return syncResult{}, err
	}
	if len(imported) == 0 {
		// No commits to import. If we had a hint, check whether we should retry.
		// If the hinted commit is already applied locally, we already have it - no retry needed.
		// If it's not applied, the provider might be stale - signal for retry.
		gotNoCommits := false
		if !hint.IsZero() && n.idx != nil {
			if e, ok, _ := n.idx.Get(innerCtx, hint); ok && e.Status == CommitStatusApplied {
				// Commit already applied locally, no need to retry
				n.cfg.Log.Debugf("[sync] Hint %s already applied locally, no retry needed", hint)
				gotNoCommits = false
			} else {
				// Commit not applied yet but provider returned nothing - provider may be stale
				localShort, remoteShort, baseShort := localHead[:8], remoteHead[:8], mergeBase[:8]
				n.cfg.Log.Debugf("[sync] No commits imported from provider=%s (remote=%s local=%s base=%s) but hint %s not applied (status=%v)",
					providerShort, remoteShort, localShort, baseShort, hint, e.Status)
				gotNoCommits = true
			}
		}
		return makeResult(false, gotNoCommits), nil
	}

	// Log successful import
	localShort, remoteShort, baseShort := localHead[:8], remoteHead[:8], mergeBase[:8]
	n.cfg.Log.Debugf("[sync] Importing %d commits from provider=%s (remote=%s local=%s base=%s)",
		len(imported), providerShort, remoteShort, localShort, baseShort)

	// Get finalized base for tail-only linearization
	var fb *FinalizedBase
	if base, ok, _ := n.idx.FinalizedBase(innerCtx); ok {
		fb = &base
	}

	// Try the cheap path first.
	if ok, err := n.tryFastForward(innerCtx, remoteHead); err == nil && ok {
		n.cfg.Log.Debugf("[sync] Fast-forward succeeded to %s", remoteShort)
		_ = n.initIndexFromDB(innerCtx)
		n.tryAdvanceFinalizedBase(innerCtx)
		return makeResult(true, false), nil // Signal to resubmit after releasing mutex
	}

	n.cfg.Log.Debugf("[sync] Replaying %d commits (fast-forward not possible)", len(imported))
	if err := n.db.reconciler.ReplayImported(innerCtx, mergeBase, imported, fb); err != nil {
		return syncResult{}, err
	}

	_ = n.initIndexFromDB(innerCtx)
	n.tryAdvanceFinalizedBase(innerCtx)
	return makeResult(true, false), nil // Signal to resubmit after releasing mutex
}

func (n *Node) updateHeadIndexBestEffort(ctx context.Context) {
	head, headMeta, err := n.getHeadMeta(ctx)
	if err != nil || headMeta == nil {
		return
	}
	_ = n.idx.SetHead(ctx, headMeta.HLC, head.Hash)
	_ = n.idx.AppendCheckpoint(ctx, Checkpoint{HLC: headMeta.HLC, CommitHash: head.Hash})
}

func (n *Node) tryFastForward(ctx context.Context, targetHash string) (bool, error) {
	conn, err := n.db.sqldb.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = conn.Close() }()

	tmp := fmt.Sprintf("swarm_ff_%d", time.Now().UnixNano())

	if _, err := conn.ExecContext(ctx, "CALL DOLT_CHECKOUT('main');"); err != nil {
		return false, err
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("CALL DOLT_BRANCH('-f', '%s', '%s');", tmp, targetHash)); err != nil {
		return false, err
	}
	defer func() {
		_, _ = conn.ExecContext(context.Background(), "CALL DOLT_CHECKOUT('main');")
		_, _ = conn.ExecContext(context.Background(), fmt.Sprintf("CALL DOLT_BRANCH('-D', '%s');", tmp))
	}()

	_, err = conn.ExecContext(ctx, fmt.Sprintf("CALL DOLT_MERGE('--ff-only', '%s');", tmp))
	if err != nil {
		return false, err
	}
	return true, nil
}

func (n *Node) forceResetMainTo(ctx context.Context, targetHash string) error {
	conn, err := n.db.sqldb.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	tmp := fmt.Sprintf("swarm_bootstrap_%d", time.Now().UnixNano())
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("CALL DOLT_BRANCH('-f', '%s', '%s');", tmp, targetHash)); err != nil {
		return err
	}
	defer func() {
		_, _ = conn.ExecContext(context.Background(), "CALL DOLT_CHECKOUT('main');")
		_, _ = conn.ExecContext(context.Background(), fmt.Sprintf("CALL DOLT_BRANCH('-D', '%s');", tmp))
	}()
	if _, err := conn.ExecContext(ctx, "CALL DOLT_CHECKOUT('main');"); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, fmt.Sprintf("CALL DOLT_RESET('--hard', '%s');", tmp))
	return err
}
