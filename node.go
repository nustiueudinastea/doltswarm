package doltswarm

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

	Identity IdentityResolver
}

type Node struct {
	cfg NodeConfig
	db  *DB
	idx CommitIndex

	ownsDB bool

	mu sync.Mutex

	providerMu    sync.Mutex
	hintProviders map[HLCTimestamp]string
	peerHeads     map[string]HLCTimestamp
	lastProvider  string
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

	return &Node{
		cfg:           cfg,
		db:            db,
		idx:           NewMemoryCommitIndex(),
		ownsDB:        ownsDB,
		hintProviders: make(map[HLCTimestamp]string),
		peerHeads:     make(map[string]HLCTimestamp),
	}, nil
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

func (n *Node) Commit(msg string) (string, error) {
	n.pullFirstBestEffort()
	commitHash, err := n.db.Commit(msg)
	if err != nil {
		return "", err
	}
	n.publishLocalCommitAd(commitHash)
	return commitHash, nil
}

func (n *Node) ExecAndCommit(exec ExecFunc, msg string) (string, error) {
	n.pullFirstBestEffort()
	commitHash, err := n.db.ExecAndCommit(exec, msg)
	if err != nil {
		return "", err
	}
	n.publishLocalCommitAd(commitHash)
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

	if err := n.cfg.Transport.Gossip().PublishCommitAd(ctx, CommitAdV1{
		Repo:         n.cfg.Repo,
		HLC:          meta.HLC,
		MetadataJSON: []byte(head.Message),
		MetadataSig:  []byte(meta.Signature),
		ObservedAt:   time.Now(),
	}); err != nil {
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
				if err := n.syncOnce(ctx, minH); err != nil {
					// This is the primary "why aren't we converging?" signal in integration runs,
					// which usually run at info level (debug is suppressed).
					if ctx.Err() == nil {
						n.cfg.Log.Warnf("syncOnce failed: %v", err)
					} else {
						n.cfg.Log.Debugf("syncOnce failed (ctx done): %v", err)
					}
				} else if !maxH.IsZero() {
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
	if ad.Repo != n.cfg.Repo {
		return
	}
	n.observeProvider(from, ad.HLC)
	n.rememberHintProvider(ad.HLC, from)
	// Ignore our own adverts; local commits are already present and indexed.
	if n.cfg.Signer != nil && ad.HLC.PeerID == n.cfg.Signer.GetID() {
		return
	}
	now := time.Now().UnixNano()
	if n.cfg.MaxClockSkew > 0 && ad.HLC.Wall > now+int64(n.cfg.MaxClockSkew) {
		_ = n.idx.Upsert(ctx, CommitIndexEntry{
			HLC:       ad.HLC,
			Status:    CommitStatusRejected,
			UpdatedAt: time.Now(),
		})
		return
	}

	meta, err := ParseCommitMetadata(string(ad.MetadataJSON))
	if err != nil || meta == nil {
		return
	}
	if !meta.HLC.Equal(ad.HLC) {
		return
	}
	if len(ad.MetadataSig) > 0 && meta.Signature != string(ad.MetadataSig) {
		return
	}

	// Best-effort: accept remote commits until key distribution is wired.
	_ = n.idx.Upsert(ctx, CommitIndexEntry{
		HLC:       ad.HLC,
		Status:    CommitStatusPending,
		UpdatedAt: time.Now(),
	})

	requestSync(ad.HLC)
}

func (n *Node) onDigest(ctx context.Context, from string, d DigestV1, requestSync func(HLCTimestamp)) {
	if d.Repo != n.cfg.Repo {
		return
	}
	n.observeProvider(from, d.HeadHLC)
	if !d.HeadHLC.IsZero() {
		n.rememberHintProvider(d.HeadHLC, from)
	}
	head, headMeta, err := n.getHeadMeta(ctx)
	if err != nil || headMeta == nil || head.Hash == "" {
		requestSync(d.HeadHLC)
		return
	}
	if headMeta.HLC.Less(d.HeadHLC) {
		requestSync(d.HeadHLC)
	}

	// Repair missed commit adverts: if the remote digest contains checkpoints we don't have,
	// trigger a sync even if our head is already ahead.
	for _, cp := range d.Checkpoints {
		if cp.CommitHash == "" || cp.HLC.IsZero() {
			continue
		}
		e, ok, _ := n.idx.Get(ctx, cp.HLC)
		if !ok || e.Status != CommitStatusApplied || (e.CommitHash != "" && e.CommitHash != cp.CommitHash) {
			requestSync(cp.HLC)
		}
	}
}

func (n *Node) observeProvider(from string, hlc HLCTimestamp) {
	if n == nil || from == "" || hlc.IsZero() {
		return
	}
	n.providerMu.Lock()
	defer n.providerMu.Unlock()
	n.lastProvider = from
	if cur, ok := n.peerHeads[from]; !ok || cur.Less(hlc) {
		n.peerHeads[from] = hlc
	}
}

func (n *Node) rememberHintProvider(hlc HLCTimestamp, from string) {
	if n == nil || from == "" || hlc.IsZero() {
		return
	}
	n.providerMu.Lock()
	defer n.providerMu.Unlock()
	n.hintProviders[hlc] = from
}

func (n *Node) popHintProvider(hlc HLCTimestamp) string {
	if n == nil || hlc.IsZero() {
		return ""
	}
	n.providerMu.Lock()
	defer n.providerMu.Unlock()
	return n.hintProviders[hlc]
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

func (n *Node) initIndexFromDB(ctx context.Context) error {
	if n == nil || n.db == nil || n.idx == nil {
		return nil
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

func (n *Node) syncOnce(ctx context.Context, hint HLCTimestamp) error {
	// Prefer fetching from the peer that told us about the hinted commit (if any).
	// This avoids getting stuck contacting a stale provider that doesn't yet have the objects.
	if !hint.IsZero() {
		if from := n.popHintProvider(hint); from != "" {
			ctx = WithPreferredProvider(ctx, from)
		}
	} else if from := n.bestProvider(); from != "" {
		ctx = WithPreferredProvider(ctx, from)
	}

	n.mu.Lock()
	defer n.mu.Unlock()

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
		return err
	}
	if err := n.db.FetchSwarm(innerCtx); err != nil {
		return err
	}

	remoteRef := "remotes/swarm/main"
	remoteHead, ok, err := n.db.GetBranchHead(innerCtx, remoteRef)
	if err != nil {
		return err
	}
	if !ok {
		// Nothing fetched yet (no remote branch).
		return nil
	}

	mergeBase, err := n.db.MergeBase(innerCtx, "main", remoteRef)
	if err != nil {
		return err
	}
	if mergeBase == "" {
		// Bootstrap: no shared history. Force main to the remote head and rebuild.
		if err := n.forceResetMainTo(innerCtx, remoteHead); err != nil {
			return err
		}
		_ = n.initIndexFromDB(innerCtx)
		return nil
	}

	imported, err := n.db.reconciler.getCommitsSince(innerCtx, n.db.sqldb, remoteRef, mergeBase)
	if err != nil {
		return err
	}
	if len(imported) == 0 {
		return nil
	}

	// Try the cheap path first.
	if ok, err := n.tryFastForward(innerCtx, remoteHead); err == nil && ok {
		_ = n.initIndexFromDB(innerCtx)
		return nil
	}

	if err := n.db.reconciler.ReplayImported(innerCtx, mergeBase, imported); err != nil {
		return err
	}

	_ = n.initIndexFromDB(innerCtx)
	return nil
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
