package core

import (
	"context"
	"fmt"
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

	Identity IdentityResolver

	// HeartbeatInterval controls how often the node publishes its HLC as a heartbeat.
	// Zero uses the default (5s).
	HeartbeatInterval time.Duration
}

type Node struct {
	cfg NodeConfig
	db  *DB
	idx CommitIndex

	ownsDB bool

	mu sync.Mutex

	// peerHLC tracks the latest HLC observed from each peer (from heartbeats).
	// Matches spec's peer_hlc map.
	peerHLCMu sync.RWMutex
	peerHLC   map[string]HLCTimestamp
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
	if cfg.HeartbeatInterval == 0 {
		cfg.HeartbeatInterval = 5 * time.Second
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
	_ = db.EnsureSwarmRemote(context.Background(), cfg.Repo)

	n := &Node{
		cfg:     cfg,
		db:      db,
		idx:     NewMemoryCommitIndex(),
		ownsDB:  ownsDB,
		peerHLC: make(map[string]HLCTimestamp),
	}

	n.installReconcilerHooks()

	return n, nil
}

// SetCommitRejectedCallback registers a callback invoked when a commit is deterministically dropped
// during reconciliation (e.g. due to a conflict).
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
	commitHash, err := n.db.Commit(msg)
	if err != nil {
		return "", err
	}
	n.publishLocalCommitAd(commitHash)
	return commitHash, nil
}

func (n *Node) ExecAndCommit(exec ExecFunc, msg string) (string, error) {
	commitHash, err := n.db.ExecAndCommit(exec, msg)
	if err != nil {
		return "", err
	}
	n.publishLocalCommitAd(commitHash)
	return commitHash, nil
}

// SyncHint performs a single best-effort synchronization pass.
// It is safe to call concurrently with Run(); internal locking serializes work.
func (n *Node) SyncHint(ctx context.Context, hint HLCTimestamp) (changed bool, err error) {
	var before string
	if n != nil && n.db != nil {
		if c, _, e := n.db.GetLastUserCommit("main"); e == nil {
			before = c.Hash
		}
	}

	_, err = n.syncOnce(ctx)
	if err != nil {
		return false, err
	}

	if n != nil && n.db != nil {
		if c, _, e := n.db.GetLastUserCommit("main"); e == nil {
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

	head, meta, err := n.db.GetLastUserCommit("main")
	if err != nil || head.Hash != commitHash {
		return
	}
	if meta.Kind != CommitKindUser {
		return
	}

	_ = n.idx.Upsert(ctx, CommitIndexEntry{
		HLC:        meta.HLC,
		CommitHash: head.Hash,
		Status:     CommitStatusApplied,
		UpdatedAt:  time.Now(),
	})

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
// It ingests commit ads and heartbeats, and advances main by importing commits and
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

	trigger := make(chan struct{}, 1)
	requestSync := func() {
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
				n.cfg.Log.Debugf("[sync] Starting syncOnce")
				syncStart := time.Now()
				_, syncErr := n.syncOnce(ctx)
				syncDur := time.Since(syncStart).Round(time.Millisecond)
				if syncErr != nil {
					if ctx.Err() == nil {
						n.cfg.Log.Warnf("syncOnce failed after %v: %v", syncDur, syncErr)
					}
				} else {
					n.cfg.Log.Debugf("[sync] syncOnce completed in %v", syncDur)
				}
			}
		}
	}()

	heartbeatTicker := time.NewTicker(n.cfg.HeartbeatInterval)
	defer heartbeatTicker.Stop()

	repairTicker := time.NewTicker(n.cfg.RepairInterval)
	defer repairTicker.Stop()

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
		case <-heartbeatTicker.C:
			n.publishHeartbeat()
			n.tryFinalize(ctx)
		case <-repairTicker.C:
			n.cfg.Log.Debugf("[repair] RepairInterval tick - requesting sync")
			requestSync()
		case evt := <-events:
			if evt.CommitAd != nil {
				n.onCommitAd(ctx, evt.From, *evt.CommitAd, requestSync)
			}
			if evt.Heartbeat != nil {
				n.onHeartbeat(evt.From, *evt.Heartbeat)
			}
		}
	}
}

// publishHeartbeat publishes our current HLC as a heartbeat (matches spec's do_heartbeat).
func (n *Node) publishHeartbeat() {
	if n == nil || n.cfg.Transport == nil || n.cfg.Transport.Gossip() == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rec := n.db.GetReconciler()
	if rec == nil {
		return
	}

	hb := HeartbeatV1{
		Repo:   n.cfg.Repo,
		HLC:    rec.GetHLC().Now(),
		PeerID: n.cfg.Signer.GetID(),
	}
	if err := n.cfg.Transport.Gossip().PublishHeartbeat(ctx, hb); err != nil {
		n.cfg.Log.Debugf("PublishHeartbeat failed: %v", err)
	}
}

// onHeartbeat updates peerHLC, computes stable_hlc = min(peerHLC), triggers finalization.
// Matches spec's do_heartbeat action.
func (n *Node) onHeartbeat(from string, hb HeartbeatV1) {
	if hb.Repo != n.cfg.Repo {
		return
	}
	// Ignore our own heartbeats
	if n.cfg.Signer != nil && hb.PeerID == n.cfg.Signer.GetID() {
		return
	}

	// Update peer HLC (max of current and received)
	n.peerHLCMu.Lock()
	cur, ok := n.peerHLC[hb.PeerID]
	if !ok || cur.Less(hb.HLC) {
		n.peerHLC[hb.PeerID] = hb.HLC
	}
	n.peerHLCMu.Unlock()

	// Update local HLC from remote timestamp
	if rec := n.db.GetReconciler(); rec != nil {
		rec.GetHLC().Update(hb.HLC)
	}
}

// tryFinalize implements spec's do_finalize: stable_hlc = min(peerHLC[active_peers]),
// then finalize one event at a time with HLC < stable_hlc.
func (n *Node) tryFinalize(ctx context.Context) {
	if n == nil || n.db == nil || n.idx == nil {
		return
	}

	// Compute stable_hlc = min(peerHLC) over all tracked peers
	n.peerHLCMu.RLock()
	if len(n.peerHLC) == 0 {
		n.peerHLCMu.RUnlock()
		return
	}
	var stableHLC HLCTimestamp
	first := true
	for _, hlc := range n.peerHLC {
		if first || hlc.Less(stableHLC) {
			stableHLC = hlc
			first = false
		}
	}
	n.peerHLCMu.RUnlock()

	if stableHLC.IsZero() {
		return
	}

	// Find the candidate with the minimum HLC that is below stable_hlc and not yet finalized
	entries, err := n.idx.ListAfter(ctx, HLCTimestamp{}, 0)
	if err != nil {
		return
	}

	currentBase, hasBase, _ := n.idx.FinalizedBase(ctx)

	// Find the highest applied commit below stable_hlc
	var bestEntry *CommitIndexEntry
	for i := range entries {
		e := &entries[i]
		if e.Status != CommitStatusApplied {
			continue
		}
		if !e.HLC.Less(stableHLC) {
			continue
		}
		// Must be after current finalized base
		if hasBase && !currentBase.HLC.Less(e.HLC) {
			continue
		}
		if bestEntry == nil || bestEntry.HLC.Less(e.HLC) {
			bestEntry = e
		}
	}

	if bestEntry == nil {
		return
	}

	_ = n.idx.SetFinalizedBase(ctx, FinalizedBase{
		HLC:        bestEntry.HLC,
		CommitHash: bestEntry.CommitHash,
		UpdatedAt:  time.Now(),
	})

	shortHash := bestEntry.CommitHash
	if len(shortHash) > 8 {
		shortHash = shortHash[:8]
	}
	n.cfg.Log.Debugf("Advanced finalized base to HLC=%s hash=%s (stable_hlc=%s)",
		bestEntry.HLC, shortHash, stableHLC)
}

func (n *Node) onCommitAd(ctx context.Context, from string, ad CommitAdV1, requestSync func()) {
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
	if meta.Kind != CommitKindUser {
		n.cfg.Log.Debugf("[gossip] CommitAd from=%s hlc=%s - non-user commit kind %q rejected", shortFrom, ad.HLC, meta.Kind)
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
	if n.cfg.Identity != nil {
		pub, err := n.cfg.Identity.PublicKeyForPeerID(ctx, ad.HLC.PeerID)
		if err != nil || len(pub) == 0 {
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

	// Update local HLC from remote timestamp
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
	requestSync()
}

func (n *Node) initIndexFromDB(ctx context.Context) error {
	if n == nil || n.db == nil || n.idx == nil {
		return nil
	}

	// Preserve non-applied entries across rebuilds.
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

	for _, c := range commits {
		meta, err := ParseCommitMetadata(c.Message)
		if err != nil || meta == nil {
			continue
		}
		if meta.Kind != CommitKindUser {
			continue
		}
		_ = n.idx.Upsert(ctx, CommitIndexEntry{
			HLC:        meta.HLC,
			CommitHash: c.Hash,
			Status:     CommitStatusApplied,
			UpdatedAt:  time.Now(),
		})
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
	head, meta, err := n.db.GetLastUserCommit("main")
	if err != nil {
		return Commit{}, nil, err
	}
	return head, meta, nil
}

// syncOnce performs a single sync pass: fetch from swarm remote, check for new commits, FF or merge.
func (n *Node) syncOnce(ctx context.Context) (bool, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	// Serialize against local commits and other Dolt mutations.
	if n != nil && n.db != nil && n.db.reconciler != nil {
		n.db.reconciler.mu.Lock()
		defer n.db.reconciler.mu.Unlock()
	}

	// Bound a single sync attempt.
	innerCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	if err := n.db.EnsureSwarmRemote(innerCtx, n.cfg.Repo); err != nil {
		return false, err
	}
	if err := n.db.FetchSwarm(innerCtx); err != nil {
		return false, err
	}

	remoteRef := "remotes/swarm/main"
	remoteHead, ok, err := n.db.GetBranchHead(innerCtx, remoteRef)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}

	mergeBase, err := n.db.MergeBase(innerCtx, "main", remoteRef)
	if err != nil {
		return false, err
	}
	if mergeBase == "" {
		// Bootstrap: no shared history.
		if err := n.forceResetMainTo(innerCtx, remoteHead); err != nil {
			return false, err
		}
		_ = n.initIndexFromDB(innerCtx)
		return true, nil
	}

	imported, err := n.db.reconciler.getCommitsSince(innerCtx, n.db.sqldb, remoteRef, mergeBase)
	if err != nil {
		return false, err
	}
	if len(imported) == 0 {
		return false, nil
	}

	// Get finalized base for tail-only linearization
	var fb *FinalizedBase
	if base, ok, _ := n.idx.FinalizedBase(innerCtx); ok {
		fb = &base
	}

	// Try the cheap path first.
	if ok, err := n.tryFastForward(innerCtx, remoteHead); err == nil && ok {
		n.cfg.Log.Debugf("[sync] Fast-forward succeeded")
		_ = n.initIndexFromDB(innerCtx)
		return true, nil
	}

	n.cfg.Log.Infof("[sync] Merging %d commits (fast-forward not possible)", len(imported))
	mergeCount, err := n.db.reconciler.ReplayImportedCommits(innerCtx, mergeBase, imported, fb)
	if err != nil {
		return false, err
	}
	n.cfg.Log.Infof("[sync] Merge commits: %d", mergeCount)

	_ = n.initIndexFromDB(innerCtx)
	return true, nil
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
