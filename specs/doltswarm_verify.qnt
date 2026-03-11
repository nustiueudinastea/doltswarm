// -*- mode: Bluespec; -*-

/**
 * DoltSwarm Merkle clock protocol — flat, set-based verification module.
 *
 * Self-contained module for Apalache model checking. No Choreo framework,
 * no List (Seq) types — everything is sets and maps.
 *
 * Models the core protocol: Merkle clock events, MergeRoots-based reconcile,
 * heartbeats for witness-frontier exchange, causal finalization,
 * merge conflicts, tentative + finalized conflict resolution, network partitions,
 * and partition recovery via accept-the-fork finalized merge (§7.10).
 *
 * NOTE: The protocol boundary is abstracted into:
 *   (1) a control plane for event/frontier/finalized-metadata exchange, and
 *   (2) a content plane for content-addressed root-closure fetch.
 * The spec models control-plane Event dissemination with `inbox`,
 * heartbeat/finalized metadata exchange as direct state reads, and content-plane
 * completion as `chunks_ready`. It intentionally does not model topology,
 * provider selection, or wire mechanics. Protocol heartbeats still carry
 * `head_event_ids_digest = hash(sort(head_event_ids))`, the full seen frontier,
 * the finalized commit/prolly-root pair, and a finalized-metadata digest
 * (§7.6); the spec just skips the message-level head-digest compare mechanics
 * and compares the underlying finalized metadata payload directly.
 *
 * NOTE: The protocol defines admission via a swarm-uniform `ValidEvent(e)`
 * predicate and leaves any concrete auth envelope outside the core `Event`.
 * The spec abstracts that contract as `validEvent(e)` and models only
 * executions where every peer applies the same predicate.
 *
 * Events carry anchor_prolly_root_hash in addition to prolly_root_hash. Reconciliation and
 * finalization start from the current finalized content-root accumulator, but
 * each event is merged against its own recorded anchor_prolly_root_hash.
 *
 * Conceptual split mirrored from the protocol:
 * - Authoritative finalized history:
 *   finalized_commit_id/finalized_prolly_root_hash.
 * - Finalized sync metadata:
 *   finalized_commit_parents/finalized_checkpoint_events/
 *   finalized_conflicts/resolved_finalized_conflict_ids.
 *   finalized_commit_parents and finalized_checkpoint_events are synchronized
 *   metadata derived from materialized finalized commits, not the source of
 *   finalized commit identity. One finalized checkpoint commit may subsume
 *   multiple hardened event ids without changing the finalized commit DAG.
 * - Local finalized bookkeeping:
 *   finalized_events, reconstructed from the finalized tip plus reachable
 *   checkpoint descriptors rather than synchronized directly as the primary
 *   finalized payload.
 * - Tentative state:
 *   clock/head_event_ids/tentative_prolly_root_hash/parked_conflicts/projection_basis.
 * The tentative projection is deterministic but may be provisional under
 * closure lag (missing ancestors/chunks).
 * Partition conflicts are handled inline by SYNC_FINALIZED's divergence
 * ours-side root (no blocking), while conflict records are tracked in
 * finalized_conflicts until a resolving claim hardens into finalized state.
 *
 * NOTE: Bootstrap/join is outside the core state machine. The surrounding
 * transport/bootstrap adapter may restore a peer via snapshot transfer or any
 * other transport-specific mechanism, but it must hand the core protocol a
 * local state that already satisfies the bootstrapBoundaryReady contract below.
 * The executable model therefore has no join action; `init` is the degenerate
 * already-bootstrapped empty case.
 *
 * Reduced constants for tractable state space:
 *   - 3 peers
 *   - 2 content values
 *   - MAX_WALL = 6
 */
module doltswarm_verify {
  import basicSpells.* from "./spells/basicSpells"
  import hlc.* from "./hlc"

  // ===========================================================================
  // Configuration (small for verification)
  // ===========================================================================

  pure val PEERS: Set[PeerID] = Set(1, 2, 3)
  // Encoded two-dimension content states:
  // Dimension A has 3 values {0,1,2}, dimension B has 2 values {0,1}.
  // Three values in A are required so that concurrent writes to A can
  // produce genuinely different values (e.g., A=1 vs A=2 from ancestor A=0),
  // making same-dimension conflicts reachable and exercising the parking,
  // resolution, and subsumption paths.
  // 10 => (A0, B0), 11 => (A1, B0), 12 => (A0, B1), 13 => (A1, B1),
  // 14 => (A2, B0), 15 => (A2, B1)
  pure val CONTENTS: Set[int] = Set(10, 11, 12, 13, 14, 15)
  pure val MAX_WALL: int = 6
  pure val MAX_EVENTS: int = 8
  pure val EMPTY_ROOT: int = 0
  pure val FINALIZED_KIND_NONE: int = 0
  pure val FINALIZED_KIND_CHECKPOINT: int = 1
  pure val FINALIZED_KIND_MERGE: int = 2

  /// Sentinel hash indicating a merge conflict.
  /// mergeRoots returns this when two truly-divergent writes conflict.
  pure val CONFLICT_HASH: int = -1

  // ===========================================================================
  // Types
  // ===========================================================================

  type ProllyRootHash = int
  /// EventID is a (peer, wall, logical) tuple — unique event identifier.
  /// Protocol-level event-id bytes come from hash(CanonicalEncode(EventIDPayload));
  /// this tuple abstracts that identity without modeling byte encoding/hashing.
  type EventID = (int, int, int)

  /// Deterministic finalized shared-history identity.
  /// This abstracts hash(CanonicalEncode(FinalizedCommitPayload)), where the
  /// payload includes the finalized commit's prolly root, its ordered parent
  /// commit ids, and whether the commit is a linear checkpoint or a merge.
  type FinalizedCommitID = (ProllyRootHash, int, int)

  /// Deterministic identity for a finalized-layer conflict from SYNC_FINALIZED.
  /// Canonical tuple: (ours_finalized_commit_id, theirs_finalized_commit_id, common_finalized_commit_id).
  type FinalizedConflictID = (FinalizedCommitID, FinalizedCommitID, FinalizedCommitID)

  type Event = {
    event_id: EventID,
    prolly_root_hash: ProllyRootHash,
    anchor_prolly_root_hash: ProllyRootHash,
    parent_event_ids: Set[EventID],
    resolved_parked_event_id: Option[EventID],
    resolved_finalized_conflict_id: Option[FinalizedConflictID],
    hlc: HLC,
    peer: PeerID,
  }

  type FinalizedConflict = {
    id: FinalizedConflictID,
    ours_finalized_commit_id: FinalizedCommitID,
    theirs_finalized_commit_id: FinalizedCommitID,
    common_finalized_commit_id: FinalizedCommitID,
  }

  /// Descriptor for a checkpoint finalized commit. Merge commits carry no
  /// descriptor entry. One finalized checkpoint commit may subsume multiple
  /// hardened event ids that compiled to the same deterministic checkpoint
  /// payload; this map stores the resulting event_id -> prolly_root_hash set.
  type FinalizedCheckpoint = EventID -> ProllyRootHash

  type NodeState = {
    clock: EventID -> Event,
    head_event_ids: Set[EventID],
    chunks_ready: Set[EventID],
    tentative_prolly_root_hash: ProllyRootHash,
    projection_basis: Set[EventID],
    hlc_clock: HLC,
    peer_seen_frontier: PeerID -> Set[EventID],
    finalized_commit_id: FinalizedCommitID,
    finalized_prolly_root_hash: ProllyRootHash,
    finalized_events: EventID -> ProllyRootHash,
    finalized_commit_parents: FinalizedCommitID -> Set[FinalizedCommitID],
    finalized_checkpoint_events: FinalizedCommitID -> FinalizedCheckpoint,
    parked_conflicts: Set[EventID],
    finalized_conflicts: FinalizedConflictID -> FinalizedConflict,
    resolved_finalized_conflict_ids: Set[FinalizedConflictID],
    active_peers: Set[PeerID],
    localWall: int,
  }

  // ===========================================================================
  // State variables
  // ===========================================================================

  var nodes: PeerID -> NodeState
  /// Abstract control-plane Event deliveries. Heartbeats/finalized metadata are
  /// modeled as direct state reads instead of serialized messages.
  var inbox: PeerID -> Set[Event]
  /// Abstract reachability relation used for both control-plane exchange and
  /// content-plane fetch eligibility.
  /// Symmetric: (a, b) ∈ connected ⟺ (b, a) ∈ connected.
  /// Models network partitions: removing pairs isolates peers.
  var connected: Set[(PeerID, PeerID)]

  // ===========================================================================
  // Helpers
  // ===========================================================================

  pure def makeEventID(peer: PeerID, wall: int, logical: int): EventID =
    (peer, wall, logical)

  pure val EMPTY_FINALIZED_COMMIT_ID: FinalizedCommitID = (EMPTY_ROOT, 0, FINALIZED_KIND_NONE)
  pure val EMPTY_EVENT_ID: EventID = (0, 0, 0)
  pure val EVENT_ID_LOGICAL_BASE: int = MAX_EVENTS + 2
  pure val EVENT_ID_WALL_BASE: int = MAX_WALL + 2
  pure val EVENT_ID_ENCODING_BASE: int = 512

  pure def finalizedCommitProllyRootHash(finalized_commit: FinalizedCommitID): ProllyRootHash =
    finalized_commit._1

  pure def finalizedCommitParentFingerprint(finalized_commit: FinalizedCommitID): int =
    finalized_commit._2

  pure def finalizedCommitKind(finalized_commit: FinalizedCommitID): int =
    finalized_commit._3

  pure def eventIDLess(a: EventID, b: EventID): bool =
    if (a._1 != b._1) a._1 < b._1
    else if (a._2 != b._2) a._2 < b._2
    else a._3 < b._3

  pure def finalizedCommitLess(a: FinalizedCommitID, b: FinalizedCommitID): bool =
    if (finalizedCommitProllyRootHash(a) != finalizedCommitProllyRootHash(b))
      finalizedCommitProllyRootHash(a) < finalizedCommitProllyRootHash(b)
    else if (finalizedCommitParentFingerprint(a) != finalizedCommitParentFingerprint(b))
      finalizedCommitParentFingerprint(a) < finalizedCommitParentFingerprint(b)
    else
      finalizedCommitKind(a) < finalizedCommitKind(b)

  pure def eventIDOrdinal(event_id: EventID): int =
    ((event_id._1 * EVENT_ID_WALL_BASE) + event_id._2) * EVENT_ID_LOGICAL_BASE + event_id._3

  pure def encodeEventIDSet(event_ids: Set[EventID]): int = {
    val eventIDList = event_ids.fold([], (acc, event_id) => acc.append(event_id))
    val sorted = eventIDList.sortList((a, b) => eventIDLess(a, b))
    sorted.foldl(0, (acc, event_id) =>
      acc * EVENT_ID_ENCODING_BASE + eventIDOrdinal(event_id) + 1
    )
  }

  /// Injective pairing over natural numbers. This avoids the lossy fixed-base
  /// encoding that could alias distinct finalized parent sets once recursive
  /// parent fingerprints grew beyond the chosen radix.
  pure def natPair(a: int, b: int): int =
    if (a >= b) a * a + a + b
    else a + b * b

  pure def finalizedCommitKey(finalized_commit: FinalizedCommitID): int =
    natPair(
      finalizedCommitProllyRootHash(finalized_commit),
      natPair(
        finalizedCommitParentFingerprint(finalized_commit),
        finalizedCommitKind(finalized_commit)
      )
    )

  pure def encodeFinalizedCommitIDSet(finalized_commits: Set[FinalizedCommitID]): int = {
    val commitList = finalized_commits.fold([], (acc, finalized_commit) => acc.append(finalized_commit))
    val sorted = commitList.sortList((a, b) => finalizedCommitLess(a, b))
    sorted.foldl(1, (acc, finalized_commit) =>
      natPair(acc, finalizedCommitKey(finalized_commit) + 1)
    )
  }

  pure def makeFinalizedCheckpointCommitID(contentRoot: ProllyRootHash,
      parentFinalizedCommits: Set[FinalizedCommitID]): FinalizedCommitID =
    (contentRoot, encodeFinalizedCommitIDSet(parentFinalizedCommits), FINALIZED_KIND_CHECKPOINT)

  pure def makeFinalizedMergeCommitID(contentRoot: ProllyRootHash,
      parentFinalizedCommits: Set[FinalizedCommitID]): FinalizedCommitID =
    (contentRoot, encodeFinalizedCommitIDSet(parentFinalizedCommits), FINALIZED_KIND_MERGE)

  /// All directed pairs between peers (symmetric).
  pure def allPairs(peers: Set[PeerID]): Set[(PeerID, PeerID)] =
    peers.fold(Set(), (acc, p) =>
      acc.union(peers.filter(q => p != q).map(q => (p, q)))
    )

  /// Check if two peers are connected (symmetric).
  pure def isConnected(conn: Set[(PeerID, PeerID)], a: PeerID, b: PeerID): bool =
    conn.contains((a, b))

  /// Disseminate to connected peers only. Events from peer e.peer are delivered
  /// only to peers that e.peer can reach via the connectivity relation.
  pure def disseminateToConnected(ib: PeerID -> Set[Event], e: Event,
      conn: Set[(PeerID, PeerID)]): PeerID -> Set[Event] =
    ib.keys().mapBy(p =>
      if (p == e.peer or isConnected(conn, e.peer, p))
        ib.get(p).union(Set(e))
      else
        ib.get(p)
    )

  /// Encoded content-hash helpers for a 2-dimensional abstract row state.
  pure def isContentHash(h: ProllyRootHash): bool =
    h == EMPTY_ROOT or CONTENTS.contains(h)

  /// Root hashes used by protocol state are either empty, content roots,
  /// or finalized merge roots synthesized above the content range.
  pure def isProtocolRootHash(h: ProllyRootHash): bool =
    h == EMPTY_ROOT or CONTENTS.contains(h) or h >= 1000

  /// Abstract swarm-uniform admission predicate.
  /// This models structural validity plus "all peers use the same rule".
  /// Any concrete auth envelope remains outside the core model.
  pure def validEvent(e: Event): bool = all {
    PEERS.contains(e.peer),
    e.hlc.peer == e.peer,
    e.event_id == makeEventID(e.peer, e.hlc.wall, e.hlc.logical),
    isProtocolRootHash(e.prolly_root_hash),
    isProtocolRootHash(e.anchor_prolly_root_hash),
    e.prolly_root_hash != CONFLICT_HASH,
    e.anchor_prolly_root_hash != CONFLICT_HASH,
    // Mutual exclusivity: at most one resolution field is present.
    not(e.resolved_parked_event_id != None and e.resolved_finalized_conflict_id != None),
    // If resolving a parked event, that event must be a parent.
    match e.resolved_parked_event_id {
      | Some(pid) => e.parent_event_ids.contains(pid)
      | None => true
    },
    not(e.parent_event_ids.contains(e.event_id)),
  }

  pure def decodeDimA(h: ProllyRootHash): int =
    if (h == 11 or h == 13) 1
    else if (h == 14 or h == 15) 2
    else 0

  pure def decodeDimB(h: ProllyRootHash): int =
    if (h == 12 or h == 13 or h == 15) 1 else 0

  pure def encodeContent(a: int, b: int): ProllyRootHash = {
    if (a == 0 and b == 0) 10
    else if (a == 1 and b == 0) 11
    else if (a == 0 and b == 1) 12
    else if (a == 1 and b == 1) 13
    else if (a == 2 and b == 0) 14
    else 15  // a == 2 and b == 1
  }

  /// Deterministic merge model with 2 independent writable dimensions.
  ///
  /// Conflicts are generated only when both sides concurrently write different
  /// values to the same dimension relative to the ancestor. Disjoint writes
  /// (different dimensions) auto-merge.
  pure def mergeRoots(ours: ProllyRootHash, theirs: ProllyRootHash,
      ancestor: ProllyRootHash): ProllyRootHash = {
    if (ours == theirs) ours
    else if (ours == ancestor) theirs
    else if (theirs == ancestor) ours
    else if (isContentHash(ours) and isContentHash(theirs) and isContentHash(ancestor)) {
      val oursA = decodeDimA(ours)
      val oursB = decodeDimB(ours)
      val theirsA = decodeDimA(theirs)
      val theirsB = decodeDimB(theirs)
      val ancA = decodeDimA(ancestor)
      val ancB = decodeDimB(ancestor)
      val oursChangedA = oursA != ancA
      val oursChangedB = oursB != ancB
      val theirsChangedA = theirsA != ancA
      val theirsChangedB = theirsB != ancB
      val conflictA = oursChangedA and theirsChangedA and oursA != theirsA
      val conflictB = oursChangedB and theirsChangedB and oursB != theirsB
      if (conflictA or conflictB) {
        CONFLICT_HASH
      } else {
        val mergedA = if (theirsChangedA) theirsA else oursA
        val mergedB = if (theirsChangedB) theirsB else oursB
        encodeContent(mergedA, mergedB)
      }
    } else {
      // Fallback hash-combination for non-abstracted content values.
      // Result is always >= 1000, above CONTENTS (10..13), EMPTY_ROOT (0),
      // and CONFLICT_HASH (-1).
      1000 + ours * 31 + theirs * 17 + ancestor * 7
    }
  }

  /// Compute head event IDs from the clock structure (order-independent).
  /// Head event IDs are events whose ID is not a parent of any other event.
  pure def computeHeadEventIDs(clock: EventID -> Event): Set[EventID] = {
    val allParents = clock.keys().fold(Set(), (acc, event_id) =>
      acc.union(clock.get(event_id).parent_event_ids)
    )
    clock.keys().exclude(allParents)
  }

  /// Derived set of event IDs whose tentative conflict has been explicitly
  /// resolved. These events are finalized as bookkeeping-only entries.
  pure def subsumedEvents(clock: EventID -> Event): Set[EventID] =
    clock.keys().fold(Set(), (acc, event_id) =>
      match clock.get(event_id).resolved_parked_event_id {
        | Some(pid) => acc.union(Set(pid))
        | None => acc
      }
    )

  /// Ancestor-ID closure from a seed set by repeatedly following parent links.
  ///
  /// Includes missing parent IDs even if those event IDs are not yet present in
  /// the local clock, so subset checks can detect orphan events.
  /// The unfold is bounded by MAX_EVENTS for a finite-state model.
  pure def ancestorIdClosure(seed: Set[EventID], clock: EventID -> Event): Set[EventID] =
    range(0, MAX_EVENTS).foldl(seed, (acc, _) => {
      val parentStep = acc.fold(Set(), (parentsAcc, event_id) => {
        if (clock.has(event_id)) parentsAcc.union(clock.get(event_id).parent_event_ids)
        else parentsAcc
      })
      acc.union(parentStep)
    })

  /// Parent-closure gate: an event is parent-ready only if all reachable
  /// ancestor event IDs are present in the local clock.
  pure def isParentClosed(clock: EventID -> Event, event_id: EventID): bool =
    clock.has(event_id) and
      ancestorIdClosure(Set(event_id), clock).subseteq(clock.keys())

  /// Head event IDs eligible for reconciliation (MERGEABLE):
  /// parent-closed and chunk-ready.
  pure def mergeableHeadEventIDs(st: NodeState): Set[EventID] =
    st.head_event_ids.filter(event_id =>
      isParentClosed(st.clock, event_id) and st.chunks_ready.contains(event_id)
    )

  /// Locally observed frontier:
  /// maximal events whose full parent ancestry is present in the local clock.
  pure def observedFrontier(clock: EventID -> Event): Set[EventID] = {
    val observed = clock.keys().filter(event_id => isParentClosed(clock, event_id))
    observed.filter(event_id =>
      not(observed.exists(child => clock.get(child).parent_event_ids.contains(event_id)))
    )
  }

  /// A frontier covers event_id iff some frontier element has event_id in its
  /// ancestor-or-self closure within the local retained clock.
  pure def frontierCovers(clock: EventID -> Event,
      frontier: Set[EventID], event_id: EventID): bool =
    frontier.exists(fid =>
      clock.has(fid) and ancestorIdClosure(Set(fid), clock).contains(event_id)
    )

  /// Candidate event_id is witnessed by the current active peer set iff every
  /// active peer's last known seen frontier covers it.
  pure def coveredByActivePeers(st: NodeState, event_id: EventID): bool =
    st.active_peers.forall(q =>
      frontierCovers(st.clock, st.peer_seen_frontier.get(q), event_id)
    )

  /// Pull target on heartbeat refresh: remote head event IDs plus recursive
  /// ancestor closure for remote head event IDs and remote seen frontier,
  /// excluding event IDs already
  /// present locally.
  pure def pullClosureEventIDs(localClock: EventID -> Event,
      remoteClock: EventID -> Event, remoteHeads: Set[EventID],
      remoteSeenFrontier: Set[EventID]): Set[EventID] = {
    val remoteClosure = ancestorIdClosure(remoteHeads.union(remoteSeenFrontier), remoteClock)
    remoteClosure.filter(event_id =>
      remoteClock.has(event_id) and
      not(localClock.has(event_id))
    )
  }

  /// Apply a set of remote event IDs to a local clock map.
  pure def applyRemoteEvents(localClock: EventID -> Event, remoteClock: EventID -> Event,
      event_ids: Set[EventID]): EventID -> Event =
    event_ids.fold(localClock, (acc, event_id) =>
      if (remoteClock.has(event_id)) acc.put(event_id, remoteClock.get(event_id))
      else acc
    )

  /// Bounded anti-entropy catchup projection: one heartbeat pull plus enough
  /// receive steps to materialize all pulled events into local clock state.
  pure def heartbeatClosureCatchupClock(st_local: NodeState, st_remote: NodeState,
      remotePeer: PeerID): EventID -> Event = {
    val pulled = pullClosureEventIDs(
      st_local.clock,
      st_remote.clock,
      st_remote.head_event_ids,
      st_remote.peer_seen_frontier.get(remotePeer)
    )
    applyRemoteEvents(st_local.clock, st_remote.clock, pulled)
  }

  /// Sort event IDs by HLC (deterministic total order for multi-head merge).
  pure def sortEventIDsByHLC(event_ids: Set[EventID], clock: EventID -> Event): List[EventID] = {
    val eventIDList = event_ids.fold([], (acc, event_id) => acc.append(event_id))
    eventIDList.sortList((a, b) => less(clock.get(a).hlc, clock.get(b).hlc))
  }

  /// Shared anchored ordered merge fold used by RECONCILE and ADVANCE_STABILITY.
  /// Starts from start_prolly_root_hash and merges each head against its own recorded
  /// anchor_prolly_root_hash. Returns (merged_root, conflicts) where conflicts
  /// contains head event IDs whose merge against the running accumulator conflicted.
  pure def anchoredFold(head_event_ids: Set[EventID], st: NodeState,
      start_prolly_root_hash: ProllyRootHash): (ProllyRootHash, Set[EventID]) = {
    val sorted = sortEventIDsByHLC(head_event_ids, st.clock)
    sorted.foldl((start_prolly_root_hash, Set()), (acc, event_id) => {
      val currentRoot = acc._1
      val conflicts = acc._2
      val event = st.clock.get(event_id)
      val merged = mergeRoots(currentRoot, event.prolly_root_hash, event.anchor_prolly_root_hash)
      if (merged == CONFLICT_HASH) {
        (currentRoot, conflicts.union(Set(event_id)))
      } else {
        (merged, conflicts)
      }
    })
  }

  /// Deterministic tentative projection for head_event_ids:
  /// - full closure path: mergeable heads are folded with per-event anchors
  /// - closure-lag path: projection is provisional and retains unresolved
  ///   parked head IDs conservatively until closure completes
  ///
  /// Returns (merged_root, parked_set, projection_basis). Single-head and
  /// multi-head use the same anchored fold semantics (no single-head bypass).
  pure def computeTentativeProjection(st: NodeState):
      (ProllyRootHash, Set[EventID], Set[EventID]) = {
    val readyHeads = mergeableHeadEventIDs(st)
    if (st.head_event_ids.empty()) {
      (st.finalized_prolly_root_hash, Set(), Set())
    } else if (readyHeads.empty()) {
      (st.tentative_prolly_root_hash, st.parked_conflicts.intersect(st.head_event_ids), st.projection_basis)
    } else {
      val folded = anchoredFold(readyHeads, st, st.finalized_prolly_root_hash)
      val unreadyHeads = st.head_event_ids.exclude(readyHeads)
      val retainedUnreadyParked = st.parked_conflicts.intersect(unreadyHeads)
      val parkedSet = folded._2.union(retainedUnreadyParked)
      val projectionBasis = readyHeads.exclude(folded._2)
      (folded._1, parkedSet, projectionBasis)
    }
  }

  pure def finalizationEventLess(a: EventID, b: EventID, clock: EventID -> Event): bool =
    if (less(clock.get(a).hlc, clock.get(b).hlc)) true
    else if (less(clock.get(b).hlc, clock.get(a).hlc)) false
    else eventIDLess(a, b)

  /// Stable events at the current stability boundary.
  pure def finalizationCandidates(st: NodeState): Set[EventID] =
    st.clock.keys().filter(event_id =>
      not(st.finalized_events.has(event_id)) and
      not(st.parked_conflicts.contains(event_id)) and
      isParentClosed(st.clock, event_id) and
      st.chunks_ready.contains(event_id) and
      coveredByActivePeers(st, event_id)
    )

  /// Canonical stable frontier for finalized compilation: stable events whose
  /// parents are already in finalized history.
  pure def finalizationEligible(st: NodeState): Set[EventID] =
    finalizationCandidates(st).filter(event_id =>
      st.clock.get(event_id).parent_event_ids.subseteq(st.finalized_events.keys())
    )

  pure def nextFinalizationEventID(st: NodeState): EventID =
    finalizationEligible(st).fold(EMPTY_EVENT_ID, (best, event_id) =>
      if (best == EMPTY_EVENT_ID or finalizationEventLess(event_id, best, st.clock)) event_id
      else best
    )

  pure def nextFinalizationMergeRoot(st: NodeState): ProllyRootHash = {
    val event_id = nextFinalizationEventID(st)
    if (event_id == EMPTY_EVENT_ID) st.finalized_prolly_root_hash
    else {
      val event = st.clock.get(event_id)
      mergeRoots(st.finalized_prolly_root_hash, event.prolly_root_hash, event.anchor_prolly_root_hash)
    }
  }

  /// Finalized events safe to compact from clock.
  /// A finalized event id is pruneable only if no other retained clock event
  /// depends on it through parent ancestry. This preserves parent-closure of
  /// the retained subgraph after compaction.
  pure def retainedFinalizedEventIDs(st: NodeState): Set[EventID] =
    st.finalized_events.keys().intersect(st.clock.keys())

  pure def gcPruneable(st: NodeState): Set[EventID] =
    retainedFinalizedEventIDs(st).filter(finalized_event_id =>
      not(st.clock.keys().exists(event_id =>
        event_id != finalized_event_id and
        ancestorIdClosure(Set(event_id), st.clock).contains(finalized_event_id)
      ))
    )

  /// Optional compaction pass used after finalized sync to restore the
  /// retained finalized subgraph to a parent-closed shape before reconcile.
  pure def compactRetainedFinalized(st: NodeState): NodeState = {
    val compacted = range(0, MAX_EVENTS).foldl(st, (accSt, _) => {
      val pruneable = gcPruneable(accSt)
      val newClock = pruneable.fold(accSt.clock, (clockAcc, event_id) =>
        dropEvent(clockAcc, event_id)
      )
      val newChunksReady = pruneable.fold(accSt.chunks_ready, (readyAcc, event_id) =>
        readyAcc.exclude(Set(event_id))
      )
      {
        ...accSt,
        clock: newClock,
        chunks_ready: newChunksReady,
      }
    })
    {
      ...compacted,
      head_event_ids: computeHeadEventIDs(compacted.clock),
    }
  }

  /// Merge two finalized-commit-parent maps by key union and value union.
  pure def mergeSetMap(m1: FinalizedCommitID -> Set[FinalizedCommitID],
      m2: FinalizedCommitID -> Set[FinalizedCommitID]): FinalizedCommitID -> Set[FinalizedCommitID] = {
    m1.keys().union(m2.keys()).mapBy(k => {
      val s1 = if (m1.has(k)) m1.get(k) else Set()
      val s2 = if (m2.has(k)) m2.get(k) else Set()
      s1.union(s2)
    })
  }

  /// Merge two event-id -> root-hash maps by key union.
  pure def mergeFinalizedEventMap(m1: EventID -> ProllyRootHash,
      m2: EventID -> ProllyRootHash): EventID -> ProllyRootHash =
    m1.keys().union(m2.keys()).mapBy(k =>
      if (m1.has(k)) m1.get(k) else m2.get(k)
    )

  /// Merge checkpoint finalized-commit descriptors by outer-key union and
  /// inner event-map union.
  pure def mergeFinalizedCheckpointMap(m1: FinalizedCommitID -> FinalizedCheckpoint,
      m2: FinalizedCommitID -> FinalizedCheckpoint): FinalizedCommitID -> FinalizedCheckpoint = {
    m1.keys().union(m2.keys()).mapBy(k => {
      val c1 = if (m1.has(k)) m1.get(k) else Map()
      val c2 = if (m2.has(k)) m2.get(k) else Map()
      mergeFinalizedEventMap(c1, c2)
    })
  }

  /// Merge two finalized-conflict maps by key union.
  pure def mergeFinalizedConflictMap(
      m1: FinalizedConflictID -> FinalizedConflict,
      m2: FinalizedConflictID -> FinalizedConflict): FinalizedConflictID -> FinalizedConflict = {
    m1.keys().union(m2.keys()).mapBy(k =>
      if (m1.has(k)) m1.get(k) else m2.get(k)
    )
  }

  /// Add/update derived finalized-commit lineage metadata with the parent links
  /// extracted from a materialized finalized commit payload.
  pure def lineageAddFinalizedCommit(parentsMap: FinalizedCommitID -> Set[FinalizedCommitID],
      childFinalizedCommit: FinalizedCommitID, parentFinalizedCommits: Set[FinalizedCommitID]):
      (FinalizedCommitID -> Set[FinalizedCommitID]) = {
    val sanitizedParents = parentFinalizedCommits.exclude(Set(childFinalizedCommit))
    val acyclicParents = sanitizedParents.filter(parent =>
      not(lineageAncestors(parentsMap, parent).contains(childFinalizedCommit))
    )
    val priorParents = if (parentsMap.has(childFinalizedCommit)) parentsMap.get(childFinalizedCommit) else Set()
    val allParents = priorParents.union(acyclicParents)
    parentsMap.put(childFinalizedCommit, allParents)
  }

  pure def lineageKnownFinalizedCommits(parentsMap: FinalizedCommitID -> Set[FinalizedCommitID]):
      Set[FinalizedCommitID] =
    parentsMap.keys().union(
      parentsMap.keys().fold(Set(), (acc, finalized_commit) =>
        if (parentsMap.has(finalized_commit)) acc.union(parentsMap.get(finalized_commit)) else acc
      )
    )

  pure def parentEdges(parentsMap: FinalizedCommitID -> Set[FinalizedCommitID]):
      Set[(FinalizedCommitID, FinalizedCommitID)] =
    parentsMap.keys().fold(Set(), (acc, child) =>
      acc.union(parentsMap.get(child).map(parent => (child, parent)))
    )

  /// Derived transitive ancestors (including self and EMPTY_FINALIZED_COMMIT_ID).
  pure def lineageAncestors(parentsMap: FinalizedCommitID -> Set[FinalizedCommitID],
      finalized_commit: FinalizedCommitID): Set[FinalizedCommitID] =
    range(0, MAX_EVENTS + 4).foldl(Set(finalized_commit), (acc, _) => {
      val parentStep = acc.fold(Set(), (parentsAcc, current) => {
        if (parentsMap.has(current)) parentsAcc.union(parentsMap.get(current))
        else parentsAcc
      })
      acc.union(parentStep)
    }).union(Set(EMPTY_FINALIZED_COMMIT_ID))

  pure def lineageDepthMap(parentsMap: FinalizedCommitID -> Set[FinalizedCommitID]):
      FinalizedCommitID -> int = {
    val finalizedCommits = lineageKnownFinalizedCommits(parentsMap).union(Set(EMPTY_FINALIZED_COMMIT_ID))
    val initDepth = finalizedCommits.mapBy(finalized_commit =>
      if (finalized_commit == EMPTY_FINALIZED_COMMIT_ID) 0 else 1
    )
    range(0, MAX_EVENTS + 4).foldl(initDepth, (depths, _) =>
      finalizedCommits.mapBy(finalized_commit => {
        if (finalized_commit == EMPTY_FINALIZED_COMMIT_ID) 0
        else {
          val parents = if (parentsMap.has(finalized_commit) and not(parentsMap.get(finalized_commit).empty()))
            parentsMap.get(finalized_commit)
          else
            Set(EMPTY_FINALIZED_COMMIT_ID)
          val maxParentDepth = parents.fold(0, (acc, p) => {
            val d = if (depths.has(p)) depths.get(p)
              else if (p == EMPTY_FINALIZED_COMMIT_ID) 0
              else 1
            if (d > acc) d else acc
          })
          maxParentDepth + 1
        }
      })
    )
  }

  pure def finalizedCommitIsAncestor(ancestor: FinalizedCommitID, descendant: FinalizedCommitID,
      parentsMap: FinalizedCommitID -> Set[FinalizedCommitID]): bool =
    lineageAncestors(parentsMap, descendant).contains(ancestor)

  pure def finalizedCommitDepthAt(parentsMap: FinalizedCommitID -> Set[FinalizedCommitID],
      finalized_commit: FinalizedCommitID): int = {
    val depthMap = lineageDepthMap(parentsMap)
    if (depthMap.has(finalized_commit)) depthMap.get(finalized_commit) else 0
  }

  /// Lowest common finalized-commit ancestor using finalized-commit metadata.
  /// Picks the common ancestor with highest depth; ties break by finalized-commit id.
  pure def lineageLcaFinalizedCommit(finalizedCommitA: FinalizedCommitID, finalizedCommitB: FinalizedCommitID,
      parentsMap: FinalizedCommitID -> Set[FinalizedCommitID]): FinalizedCommitID = {
    val ancA = lineageAncestors(parentsMap, finalizedCommitA)
    val ancB = lineageAncestors(parentsMap, finalizedCommitB)
    val depthMap = lineageDepthMap(parentsMap)
    val common = ancA.intersect(ancB)
    if (common.empty()) EMPTY_FINALIZED_COMMIT_ID
    else common.fold(EMPTY_FINALIZED_COMMIT_ID, (best, candidate) => {
      val bestDepth = if (depthMap.has(best)) depthMap.get(best) else 0
      val candidateDepth = if (depthMap.has(candidate)) depthMap.get(candidate) else 0
      if (candidateDepth > bestDepth) candidate
      else if (candidateDepth < bestDepth) best
      else if (finalizedCommitLess(candidate, best)) candidate
      else best
    })
  }

  pure def ancestorsAt(parentsMap: FinalizedCommitID -> Set[FinalizedCommitID],
      finalized_commit: FinalizedCommitID): Set[FinalizedCommitID] =
    lineageAncestors(parentsMap, finalized_commit)

  pure def finalizedConflictID(oursFinalizedCommit: FinalizedCommitID,
      theirsFinalizedCommit: FinalizedCommitID,
      commonFinalizedCommit: FinalizedCommitID): FinalizedConflictID =
    (oursFinalizedCommit, theirsFinalizedCommit, commonFinalizedCommit)

  /// Remove one key from a map via key reconstruction (portable Quint idiom).
  pure def dropFinalizedConflict(conflicts: FinalizedConflictID -> FinalizedConflict,
      conflictID: FinalizedConflictID): FinalizedConflictID -> FinalizedConflict =
    conflicts.keys().exclude(Set(conflictID)).mapBy(k => conflicts.get(k))

  /// Remove multiple finalized conflict ids from a map.
  pure def dropFinalizedConflicts(conflicts: FinalizedConflictID -> FinalizedConflict,
      conflictIDs: Set[FinalizedConflictID]): FinalizedConflictID -> FinalizedConflict =
    conflicts.keys().exclude(conflictIDs).mapBy(k => conflicts.get(k))

  /// Remove one event from the clock map.
  pure def dropEvent(clock: EventID -> Event, event_id: EventID): EventID -> Event =
    clock.keys().exclude(Set(event_id)).mapBy(k => clock.get(k))

  /// Canonical finalized-event catalog ids.
  pure def finalizedEventCatalog(st: NodeState): Set[EventID] =
    st.finalized_events.keys()

  pure def currentFinalizedCommitParents(st: NodeState): Set[FinalizedCommitID] =
    if (st.finalized_commit_id == EMPTY_FINALIZED_COMMIT_ID) Set()
    else if (st.finalized_commit_parents.has(st.finalized_commit_id))
      st.finalized_commit_parents.get(st.finalized_commit_id)
    else
      Set()

  pure def derivedFinalizedCommitID(st: NodeState): FinalizedCommitID =
    if (st.finalized_commit_id == EMPTY_FINALIZED_COMMIT_ID)
      EMPTY_FINALIZED_COMMIT_ID
    else {
      val parents = currentFinalizedCommitParents(st)
      if (parents.size() >= 2)
        makeFinalizedMergeCommitID(st.finalized_prolly_root_hash, parents)
      else
        makeFinalizedCheckpointCommitID(st.finalized_prolly_root_hash, parents)
    }

  pure def logicalEventIDs(st: NodeState): Set[EventID] =
    st.clock.keys().union(st.finalized_events.keys())

  /// Reconstruct the local finalized-event bookkeeping view from the finalized
  /// commit DAG plus checkpoint descriptors reachable from the current tip.
  pure def derivedFinalizedEventMap(tip: FinalizedCommitID,
      parentsMap: FinalizedCommitID -> Set[FinalizedCommitID],
      checkpoints: FinalizedCommitID -> FinalizedCheckpoint): EventID -> ProllyRootHash =
    lineageAncestors(parentsMap, tip)
      .intersect(checkpoints.keys())
      .fold(Map(), (acc, finalized_commit) => {
        mergeFinalizedEventMap(acc, checkpoints.get(finalized_commit))
      })

  /// Out-of-model bootstrap/join contract. Any transport/bootstrap state
  /// transfer that creates or restores a peer must establish these local
  /// steady-state obligations before the core protocol begins executing
  /// transitions.
  pure def bootstrapBoundaryReady(self: PeerID, st: NodeState): bool = {
    val knownFinalizedCommits = lineageKnownFinalizedCommits(st.finalized_commit_parents)
      .union(Set(EMPTY_FINALIZED_COMMIT_ID))
    all {
      st.head_event_ids == computeHeadEventIDs(st.clock),
      st.peer_seen_frontier.get(self) == observedFrontier(st.clock),
      st.chunks_ready.subseteq(st.clock.keys()),
      retainedFinalizedEventIDs(st).forall(event_id =>
        isParentClosed(st.clock, event_id) and st.chunks_ready.contains(event_id)
      ),
      st.finalized_commit_parents.has(EMPTY_FINALIZED_COMMIT_ID),
      knownFinalizedCommits.contains(st.finalized_commit_id),
      st.finalized_checkpoint_events.keys().subseteq(knownFinalizedCommits),
      st.finalized_events == derivedFinalizedEventMap(
        st.finalized_commit_id,
        st.finalized_commit_parents,
        st.finalized_checkpoint_events
      ),
      st.active_peers.contains(self),
    }
  }

  pure def resolveFinalizedConflictEnabled(st: NodeState, p: PeerID,
      conflictID: FinalizedConflictID): bool = all {
    st.finalized_conflicts.has(conflictID),
    st.localWall <= MAX_WALL,
    st.clock.keys().size() < MAX_EVENTS,
  }

  /// Pure post-state helper mirroring do_resolve_finalized_conflict local state update.
  pure def resolveFinalizedConflictResult(st: NodeState, p: PeerID,
      conflictID: FinalizedConflictID, resolvedContent: int): NodeState = {
    val newHLC = tick(st.hlc_clock, st.localWall, p)
    val newEventID = makeEventID(p, newHLC.wall, newHLC.logical)
    val activeHeads = st.projection_basis
    val event: Event = {
      event_id: newEventID,
      prolly_root_hash: resolvedContent,
      anchor_prolly_root_hash: st.finalized_prolly_root_hash,
      parent_event_ids: activeHeads,
      resolved_parked_event_id: None,
      resolved_finalized_conflict_id: Some(conflictID),
      hlc: newHLC,
      peer: p,
    }
    val newClock = st.clock.put(newEventID, event)
    val newHeads = computeHeadEventIDs(newClock)
    val postState: NodeState = {
      ...st,
      clock: newClock,
      head_event_ids: newHeads,
      chunks_ready: st.chunks_ready.union(Set(newEventID)),
      hlc_clock: newHLC,
      peer_seen_frontier: st.peer_seen_frontier.set(p, observedFrontier(newClock)),
    }
    val reconcileResult = computeTentativeProjection(postState)
    {
      ...postState,
      tentative_prolly_root_hash: reconcileResult._1,
      parked_conflicts: reconcileResult._2,
      projection_basis: reconcileResult._3,
    }
  }

  /// LOCAL_WRITE enable gate and pure local post-state helper.
  pure def writeEnabled(st: NodeState): bool = all {
    st.localWall <= MAX_WALL,
    st.clock.keys().size() < MAX_EVENTS,
  }

  pure def writeResult(st: NodeState, p: PeerID, content: int): NodeState = {
    val newHLC = tick(st.hlc_clock, st.localWall, p)
    val newEventID = makeEventID(p, newHLC.wall, newHLC.logical)
    val event: Event = {
      event_id: newEventID,
      prolly_root_hash: content,
      anchor_prolly_root_hash: st.finalized_prolly_root_hash,
      parent_event_ids: st.projection_basis,
      resolved_parked_event_id: None,
      resolved_finalized_conflict_id: None,
      hlc: newHLC,
      peer: p,
    }
    val newClock = st.clock.put(newEventID, event)
    val postWriteState: NodeState = {
      ...st,
      clock: newClock,
      head_event_ids: computeHeadEventIDs(newClock),
      chunks_ready: st.chunks_ready.union(Set(newEventID)),
      hlc_clock: newHLC,
      peer_seen_frontier: st.peer_seen_frontier.set(p, observedFrontier(newClock)),
    }
    val reconcile = computeTentativeProjection(postWriteState)
    {
      ...postWriteState,
      tentative_prolly_root_hash: reconcile._1,
      parked_conflicts: reconcile._2,
      projection_basis: reconcile._3,
    }
  }

  /// RECONCILE enable gate and pure local post-state helper.
  pure def reconcileResult(st: NodeState): NodeState = {
    val result = computeTentativeProjection(st)
    {
      ...st,
      tentative_prolly_root_hash: result._1,
      parked_conflicts: result._2,
      projection_basis: result._3,
    }
  }

  pure def reconcileEnabled(st: NodeState): bool = {
    val post = reconcileResult(st)
    st.head_event_ids.size() > 1 and
      (post.tentative_prolly_root_hash != st.tentative_prolly_root_hash or post.parked_conflicts != st.parked_conflicts)
  }

  /// HEARTBEAT pure local metadata-refresh result.
  pure def heartbeatResult(st: NodeState, remote: NodeState, q: PeerID): NodeState = {
    val newActivePeers = st.active_peers.union(Set(q))
    {
      ...st,
      peer_seen_frontier: st.peer_seen_frontier.set(q, remote.peer_seen_frontier.get(q)),
      active_peers: newActivePeers,
    }
  }

  /// ADVANCE_STABILITY enable gate and pure local post-state helper.
  pure def finalizeEnabled(st: NodeState): bool =
    nextFinalizationEventID(st) != EMPTY_EVENT_ID and
      (nextFinalizationIsSubsumed(st) or nextFinalizationMergeRoot(st) != CONFLICT_HASH)

  /// Check if the next finalization event is subsumed.
  pure def nextFinalizationIsSubsumed(st: NodeState): bool = {
    val nextEventID = nextFinalizationEventID(st)
    nextEventID != EMPTY_EVENT_ID and subsumedEvents(st.clock).contains(nextEventID)
  }

  pure def finalizeResult(st: NodeState): NodeState = {
    val nextEventID = nextFinalizationEventID(st)
    if (nextEventID == EMPTY_EVENT_ID) st
    else {
      val nextEvent = st.clock.get(nextEventID)
      val isSubsumed = subsumedEvents(st.clock).contains(nextEventID)
      if (isSubsumed) {
        // Bookkeeping-only finalization: no MergeRoots replay, no new checkpoint.
        // Attach to the current checkpoint descriptor.
        val currentCheckpoint = if (st.finalized_checkpoint_events.has(st.finalized_commit_id))
          st.finalized_checkpoint_events.get(st.finalized_commit_id).put(nextEventID, nextEvent.prolly_root_hash)
        else
          Map().put(nextEventID, nextEvent.prolly_root_hash)
        val nextResolvedClaims = match nextEvent.resolved_finalized_conflict_id {
          | Some(cid) => Set(cid)
          | None => Set()
        }
        val withSubsumed: NodeState = {
          ...st,
          // finalized_commit_id unchanged
          // finalized_prolly_root_hash unchanged
          finalized_events: st.finalized_events.put(nextEventID, nextEvent.prolly_root_hash),
          finalized_checkpoint_events: st.finalized_checkpoint_events.put(st.finalized_commit_id, currentCheckpoint),
          resolved_finalized_conflict_ids: st.resolved_finalized_conflict_ids.union(nextResolvedClaims),
          finalized_conflicts: dropFinalizedConflicts(
            st.finalized_conflicts,
            st.resolved_finalized_conflict_ids.union(nextResolvedClaims)
          ),
        }
        val reconcile = computeTentativeProjection(withSubsumed)
        {
          ...withSubsumed,
          tentative_prolly_root_hash: reconcile._1,
          parked_conflicts: reconcile._2,
          projection_basis: reconcile._3,
        }
      } else {
        // Normal replay finalization.
        val nextRoot = nextFinalizationMergeRoot(st)
        if (nextRoot == CONFLICT_HASH) st
        else {
          val nextParents = if (st.finalized_commit_id == EMPTY_FINALIZED_COMMIT_ID)
            Set()
          else
            Set(st.finalized_commit_id)
          val nextFinalizedCommit = makeFinalizedCheckpointCommitID(nextRoot, nextParents)
          val nextResolvedClaims = match nextEvent.resolved_finalized_conflict_id {
            | Some(cid) => Set(cid)
            | None => Set()
          }
          val nextCheckpoint = if (st.finalized_checkpoint_events.has(nextFinalizedCommit))
            st.finalized_checkpoint_events.get(nextFinalizedCommit).put(nextEventID, nextEvent.prolly_root_hash)
          else
            Map().put(nextEventID, nextEvent.prolly_root_hash)
          val withBatch: NodeState = {
            ...st,
            finalized_commit_id: nextFinalizedCommit,
            finalized_prolly_root_hash: nextRoot,
            finalized_events: st.finalized_events.put(nextEventID, nextEvent.prolly_root_hash),
            finalized_checkpoint_events: st.finalized_checkpoint_events.put(nextFinalizedCommit, nextCheckpoint),
            resolved_finalized_conflict_ids: st.resolved_finalized_conflict_ids.union(nextResolvedClaims),
            finalized_conflicts: dropFinalizedConflicts(
              st.finalized_conflicts,
              st.resolved_finalized_conflict_ids.union(nextResolvedClaims)
            ),
          }
          val withLineage: NodeState = if (nextFinalizedCommit != st.finalized_commit_id) {
            val lineage = lineageAddFinalizedCommit(
              st.finalized_commit_parents,
              nextFinalizedCommit,
              nextParents
            )
            {
              ...withBatch,
              finalized_commit_parents: lineage,
            }
          } else {
            withBatch
          }
          val reconcile = computeTentativeProjection(withLineage)
          {
            ...withLineage,
            tentative_prolly_root_hash: reconcile._1,
            parked_conflicts: reconcile._2,
            projection_basis: reconcile._3,
          }
        }
      }
    }
  }

  /// RESOLVE_CONFLICT enable gate and pure local post-state helper.
  pure def resolveConflictEnabled(st: NodeState, parkedEventID: EventID): bool = all {
    st.parked_conflicts.contains(parkedEventID),
    st.clock.has(parkedEventID),
    writeEnabled(st),
  }

  pure def resolveConflictResult(st: NodeState, p: PeerID,
      parkedEventID: EventID, resolvedContent: int): NodeState = {
    val newHLC = tick(st.hlc_clock, st.localWall, p)
    val newEventID = makeEventID(p, newHLC.wall, newHLC.logical)
    val resolveParents = st.projection_basis.union(Set(parkedEventID))
    val event: Event = {
      event_id: newEventID,
      prolly_root_hash: resolvedContent,
      anchor_prolly_root_hash: st.finalized_prolly_root_hash,
      parent_event_ids: resolveParents,
      resolved_parked_event_id: Some(parkedEventID),
      resolved_finalized_conflict_id: None,
      hlc: newHLC,
      peer: p,
    }
    val newClock = st.clock.put(newEventID, event)
    val postState: NodeState = {
      ...st,
      clock: newClock,
      head_event_ids: computeHeadEventIDs(newClock),
      chunks_ready: st.chunks_ready.union(Set(newEventID)),
      hlc_clock: newHLC,
      peer_seen_frontier: st.peer_seen_frontier.set(p, observedFrontier(newClock)),
    }
    val reconcile = computeTentativeProjection(postState)
    {
      ...postState,
      tentative_prolly_root_hash: reconcile._1,
      parked_conflicts: reconcile._2,
      projection_basis: reconcile._3,
    }
  }

  // ===========================================================================
  // SYNC_FINALIZED contract helpers
  // ===========================================================================

  /// SYNC_FINALIZED branch tags.
  pure val SYNC_ADOPT: int = 1
  pure val SYNC_NOOP: int = 2
  pure val SYNC_MERGE: int = 3

  /// Branch classifier for SYNC_FINALIZED.
  pure def syncMode(st_local: NodeState, st_remote: NodeState): int = {
    val mergedParents = mergeSetMap(st_local.finalized_commit_parents, st_remote.finalized_commit_parents)
    if (st_local.finalized_commit_id == st_remote.finalized_commit_id)
      SYNC_NOOP
    else {
      val localAncestorRemote = finalizedCommitIsAncestor(
        st_local.finalized_commit_id,
        st_remote.finalized_commit_id,
        mergedParents
      )
      val remoteAncestorLocal = finalizedCommitIsAncestor(
        st_remote.finalized_commit_id,
        st_local.finalized_commit_id,
        mergedParents
      )
      if (localAncestorRemote and not(remoteAncestorLocal)) SYNC_ADOPT
      else if (remoteAncestorLocal and not(localAncestorRemote)) SYNC_NOOP
      else SYNC_MERGE
    }
  }

  /// Heartbeats carry a finalized-metadata digest in the protocol. The spec
  /// abstracts that digest comparison as direct equality over the finalized
  /// metadata payload that SYNC_FINALIZED would exchange.
  pure def finalizedSyncNeeded(st_local: NodeState, st_remote: NodeState): bool = {
    st_local.finalized_commit_id != st_remote.finalized_commit_id or
      st_local.finalized_prolly_root_hash != st_remote.finalized_prolly_root_hash or
      st_local.finalized_commit_parents != st_remote.finalized_commit_parents or
      st_local.finalized_checkpoint_events != st_remote.finalized_checkpoint_events or
      st_local.finalized_conflicts != st_remote.finalized_conflicts or
      st_local.resolved_finalized_conflict_ids != st_remote.resolved_finalized_conflict_ids
  }

  /// Lineage-based adoption gate for SYNC_FINALIZED.
  pure def syncAdoptionReady(st_local: NodeState, st_remote: NodeState): bool = {
    if (syncMode(st_local, st_remote) == SYNC_ADOPT) {
      val mergedParents = mergeSetMap(st_local.finalized_commit_parents, st_remote.finalized_commit_parents)
      finalizedCommitIsAncestor(st_local.finalized_commit_id, st_remote.finalized_commit_id, mergedParents)
    } else
      true
  }

  /// Lineage metadata readiness for SYNC_FINALIZED: after exchanging finalized
  /// commit metadata, both local and remote finalized commits must be present
  /// in the merged lineage view so common-ancestor lookup is well-defined.
  pure def syncLineageReady(st_local: NodeState, st_remote: NodeState): bool = {
    val mergedParents = mergeSetMap(st_local.finalized_commit_parents, st_remote.finalized_commit_parents)
    val knownFinalizedCommits = lineageKnownFinalizedCommits(mergedParents).union(Set(EMPTY_FINALIZED_COMMIT_ID))
    mergedParents.has(EMPTY_FINALIZED_COMMIT_ID) and
      knownFinalizedCommits.contains(st_local.finalized_commit_id) and
      knownFinalizedCommits.contains(st_remote.finalized_commit_id)
  }

  /// Enable condition for SYNC_FINALIZED transition. This fires for either
  /// finalized-tip divergence or equal-tip finalized-metadata divergence.
  pure def syncEnabled(conn: Set[(PeerID, PeerID)], p: PeerID, q: PeerID,
      st_local: NodeState, st_remote: NodeState): bool = all {
    p != q,
    isConnected(conn, p, q),
    finalizedSyncNeeded(st_local, st_remote),
    syncAdoptionReady(st_local, st_remote),
    syncLineageReady(st_local, st_remote),
  }

  /// Finalized-state update for SYNC_FINALIZED (without tentative_prolly_root_hash re-anchoring).
  pure def syncFinalizedCore(st_local: NodeState, st_remote: NodeState): NodeState = {
    val mode = syncMode(st_local, st_remote)
    val mergedResolvedIDs = st_local.resolved_finalized_conflict_ids
      .union(st_remote.resolved_finalized_conflict_ids)
    // SYNC_FINALIZED exchanges finalized-commit metadata and checkpoint
    // descriptors. finalized_events is reconstructed locally from the resulting
    // finalized tip plus reachable checkpoint commits.
    val mergedParents = mergeSetMap(st_local.finalized_commit_parents, st_remote.finalized_commit_parents)
    val mergedCheckpoints = mergeFinalizedCheckpointMap(
      st_local.finalized_checkpoint_events,
      st_remote.finalized_checkpoint_events
    )
    val unresolvedFinalizedConflicts = dropFinalizedConflicts(
      mergeFinalizedConflictMap(st_local.finalized_conflicts, st_remote.finalized_conflicts),
      mergedResolvedIDs
    )
    val withMergedLineage: NodeState = {
      ...st_local,
      finalized_commit_parents: mergedParents,
      finalized_checkpoint_events: mergedCheckpoints,
      resolved_finalized_conflict_ids: mergedResolvedIDs,
    }
    if (mode == SYNC_ADOPT) {
      {
        ...withMergedLineage,
        finalized_commit_id: st_remote.finalized_commit_id,
        finalized_prolly_root_hash: st_remote.finalized_prolly_root_hash,
        finalized_events: derivedFinalizedEventMap(
          st_remote.finalized_commit_id,
          mergedParents,
          mergedCheckpoints
        ),
        finalized_conflicts: unresolvedFinalizedConflicts,
      }
    } else if (mode == SYNC_NOOP) {
      // Ahead path: finalized root/tip no-op, but lineage/checkpoint metadata
      // exchange is retained with resolution-aware filtering.
      {
        ...withMergedLineage,
        finalized_commit_id: st_local.finalized_commit_id,
        finalized_prolly_root_hash: st_local.finalized_prolly_root_hash,
        finalized_events: derivedFinalizedEventMap(
          st_local.finalized_commit_id,
          mergedParents,
          mergedCheckpoints
        ),
        finalized_conflicts: unresolvedFinalizedConflicts,
      }
    } else {
      val commonFinalizedCommit = lineageLcaFinalizedCommit(
        st_local.finalized_commit_id,
        st_remote.finalized_commit_id,
        mergedParents
      )
      // Deterministic ours/theirs: compare finalized-commit ids lexicographically.
      val oursFinalizedCommit = if (finalizedCommitLess(st_local.finalized_commit_id, st_remote.finalized_commit_id))
        st_local.finalized_commit_id else st_remote.finalized_commit_id
      val theirsFinalizedCommit = if (finalizedCommitLess(st_local.finalized_commit_id, st_remote.finalized_commit_id))
        st_remote.finalized_commit_id else st_local.finalized_commit_id
      val conflictID = finalizedConflictID(oursFinalizedCommit, theirsFinalizedCommit, commonFinalizedCommit)
      val conflictInfo: FinalizedConflict = {
        id: conflictID,
        ours_finalized_commit_id: oursFinalizedCommit,
        theirs_finalized_commit_id: theirsFinalizedCommit,
        common_finalized_commit_id: commonFinalizedCommit,
      }
      val merged = mergeRoots(
        finalizedCommitProllyRootHash(oursFinalizedCommit),
        finalizedCommitProllyRootHash(theirsFinalizedCommit),
        finalizedCommitProllyRootHash(commonFinalizedCommit)
      )
      // Unified path: on conflict, use ours root (deterministic, no blocking),
      // and persist a finalized_conflicts entry for explicit resolution
      // unless already durably resolved in finalized state.
      val effectiveRoot = if (merged == CONFLICT_HASH)
        finalizedCommitProllyRootHash(oursFinalizedCommit)
      else
        merged
      val mergeParents = Set(st_local.finalized_commit_id, st_remote.finalized_commit_id)
      val effectiveFinalizedCommit = makeFinalizedMergeCommitID(effectiveRoot, mergeParents)
      val mergedFinalizedConflicts = if (
        merged == CONFLICT_HASH and not(mergedResolvedIDs.contains(conflictID))
      )
        unresolvedFinalizedConflicts.put(conflictID, conflictInfo)
      else
        unresolvedFinalizedConflicts
      val mergedState: NodeState = {
        ...withMergedLineage,
        finalized_commit_id: effectiveFinalizedCommit,
        finalized_prolly_root_hash: effectiveRoot,
        finalized_conflicts: mergedFinalizedConflicts,
      }
      val lineage = lineageAddFinalizedCommit(
        mergedState.finalized_commit_parents,
        effectiveFinalizedCommit,
        mergeParents
      )
      {
        ...mergedState,
        finalized_events: derivedFinalizedEventMap(
          effectiveFinalizedCommit,
          lineage,
          mergedCheckpoints
        ),
        finalized_commit_parents: lineage,
      }
    }
  }

  /// Full SYNC_FINALIZED local result, including retained-local closure
  /// hydration from the remote clock, retained-finalized compaction,
  /// and tentative_prolly_root_hash re-anchoring.
  pure def applySyncFinalized(st_local: NodeState, st_remote: NodeState): NodeState = {
    val syncedState = syncFinalizedCore(st_local, st_remote)
    val tipChanged =
      syncedState.finalized_commit_id != st_local.finalized_commit_id or
      syncedState.finalized_prolly_root_hash != st_local.finalized_prolly_root_hash
    val bookkeepingChanged =
      syncedState.finalized_events != st_local.finalized_events
    // Step 4: retained-finalized cleanup runs whenever finalized bookkeeping
    // changed, even in SYNC_NOOP (equal-tip metadata repair can expand
    // finalized_events via merged checkpoint metadata).
    val cleaned =
      if (bookkeepingChanged) compactRetainedFinalized(syncedState)
      else syncedState
    if (tipChanged) {
      // ADOPT or MERGE: full hydration + reconcile.
      val hydratedClosure = pullClosureEventIDs(
        cleaned.clock,
        st_remote.clock,
        cleaned.clock.keys(),
        Set()
      )
      val hydratedClock = applyRemoteEvents(
        cleaned.clock,
        st_remote.clock,
        hydratedClosure
      )
      val retainedFinalizedReady = hydratedClock.keys().intersect(cleaned.finalized_events.keys())
      val withRetainedFinalizedReady: NodeState = {
        ...cleaned,
        clock: hydratedClock,
        head_event_ids: computeHeadEventIDs(hydratedClock),
        chunks_ready: cleaned.chunks_ready.union(retainedFinalizedReady),
      }
      val compactedState = compactRetainedFinalized(withRetainedFinalizedReady)
      val reconcileResult = computeTentativeProjection(compactedState)
      {
        ...compactedState,
        tentative_prolly_root_hash: reconcileResult._1,
        parked_conflicts: reconcileResult._2,
        projection_basis: reconcileResult._3,
      }
    } else {
      // NOOP: tip unchanged, preserve tentative state. Compaction (if any)
      // already handled above via bookkeepingChanged.
      {
        ...cleaned,
        tentative_prolly_root_hash: st_local.tentative_prolly_root_hash,
        parked_conflicts: st_local.parked_conflicts,
        projection_basis: st_local.projection_basis,
      }
    }
  }

  // ===========================================================================
  // Actions
  // ===========================================================================

  /// Already-bootstrapped empty steady state. Join/bootstrap itself is an
  /// out-of-model transport/bootstrap concern represented by
  /// bootstrapBoundaryReady.
  action init = all {
    nodes' = PEERS.mapBy(p => {
      clock: Map(),
      head_event_ids: Set(),
      chunks_ready: Set(),
      tentative_prolly_root_hash: EMPTY_ROOT,
      projection_basis: Set(),
      hlc_clock: { wall: 1, logical: 0, peer: p },
      peer_seen_frontier: PEERS.mapBy(_ => Set()),
      finalized_commit_id: EMPTY_FINALIZED_COMMIT_ID,
      finalized_prolly_root_hash: EMPTY_ROOT,
      finalized_events: Map(),
      finalized_commit_parents: Map().put(EMPTY_FINALIZED_COMMIT_ID, Set()),
      finalized_checkpoint_events: Map(),
      parked_conflicts: Set(),
      finalized_conflicts: Map(),
      resolved_finalized_conflict_ids: Set(),
      active_peers: PEERS,
      localWall: 1,
    }),
    inbox' = PEERS.mapBy(_ => Set()),
    connected' = allPairs(PEERS),
  }

  /// A peer creates a local write event.
  /// Parents are the current projection_basis so the emitted event's causal
  /// parents match the content state embodied in the emitted prolly_root_hash.
  action do_write(p: PeerID, content: int): bool = {
    val st = nodes.get(p)
    all {
      st.localWall <= MAX_WALL,
      st.clock.keys().size() < MAX_EVENTS,
      val newHLC = tick(st.hlc_clock, st.localWall, p)
      val newEventID = makeEventID(p, newHLC.wall, newHLC.logical)
      val event: Event = {
        event_id: newEventID,
        prolly_root_hash: content,
        anchor_prolly_root_hash: st.finalized_prolly_root_hash,
        parent_event_ids: st.projection_basis,
        resolved_parked_event_id: None,
        resolved_finalized_conflict_id: None,
        hlc: newHLC,
        peer: p,
      }
      val newClock = st.clock.put(newEventID, event)
      val newHeads = computeHeadEventIDs(newClock)
      // Recompute parking for the new head set
      val postWriteState: NodeState = {
        ...st,
        clock: newClock,
        head_event_ids: newHeads,
        chunks_ready: st.chunks_ready.union(Set(newEventID)),
        hlc_clock: newHLC,
        peer_seen_frontier: st.peer_seen_frontier.set(p, observedFrontier(newClock)),
      }
      val reconcileResult = computeTentativeProjection(postWriteState)
      val newLatestRoot = reconcileResult._1
      val newParkedConflicts = reconcileResult._2
      all {
        nodes' = nodes.set(p, {
          ...postWriteState,
          tentative_prolly_root_hash: newLatestRoot,
          parked_conflicts: newParkedConflicts,
          projection_basis: reconcileResult._3,
        }),
        inbox' = disseminateToConnected(inbox, event, connected),
        connected' = connected,
      }
    }
  }

  /// A peer processes a single event from its inbox.
  /// Events are processed one at a time (not batched), matching the protocol's
  /// per-event processing with inline reconciliation between events.
  /// Reconciliation is inlined here (matching §2.4 RECEIVE_EVENT):
  /// merge/parking is computed only from MERGEABLE heads
  /// (parent-closed + chunk-ready). Orphan/non-ready heads stay announced
  /// until ancestors/chunks arrive.
  /// No local wall-time admission filter is modeled or permitted: any
  /// `validEvent(e)` that is new is accepted into the clock (G-Set behavior).
  action do_receive(p: PeerID): bool = {
    val st = nodes.get(p)
    val processable = inbox.get(p).filter(e =>
      validEvent(e) and
      not(st.clock.keys().contains(e.event_id)) and
      not(st.finalized_events.has(e.event_id))
    )

    all {
      not(processable.empty()),
      nondet e = processable.oneOf()
      // Add the single event to the clock
      val newClock = st.clock.put(e.event_id, e)
      // Merge HLC with the received timestamp
      val newHLC = observe(st.hlc_clock, e.hlc, st.localWall, p)
      // Recompute heads from the full clock (order-independent)
      val newHeads = computeHeadEventIDs(newClock)
      // Inline reconciliation with closure/chunk gates.
      val tempState: NodeState = {
        ...st,
        clock: newClock,
        head_event_ids: newHeads,
        hlc_clock: newHLC,
        peer_seen_frontier: st.peer_seen_frontier.set(p, observedFrontier(newClock)),
      }
      val reconcileResult = computeTentativeProjection(tempState)
      val newLatestRoot = reconcileResult._1
      val newParkedConflicts = reconcileResult._2
      all {
        nodes' = nodes.set(p, {
          ...tempState,
          tentative_prolly_root_hash: newLatestRoot,
          parked_conflicts: newParkedConflicts,
          projection_basis: reconcileResult._3,
        }),
        inbox' = inbox,
        connected' = connected,
      }
    }
  }

  /// Mark one announced event as CHUNKS_READY.
  /// This abstracts completion of all chunks needed to evaluate the event,
  /// including both prolly_root_hash and anchor_prolly_root_hash, and immediately
  /// re-evaluates tentative projection.
  action do_chunks_ready(p: PeerID, event_id: EventID): bool = {
    val st = nodes.get(p)
    all {
      st.clock.has(event_id),
      not(st.chunks_ready.contains(event_id)),
      val readyState: NodeState = {
        ...st,
        chunks_ready: st.chunks_ready.union(Set(event_id)),
      }
      val reconcileResult = computeTentativeProjection(readyState)
      nodes' = nodes.set(p, {
        ...readyState,
        tentative_prolly_root_hash: reconcileResult._1,
        parked_conflicts: reconcileResult._2,
        projection_basis: reconcileResult._3,
      }),
      inbox' = inbox,
      connected' = connected,
    }
  }

  /// A peer reconciles when it has multiple heads.
  /// Merges are deterministic local computations — no event is created,
  /// nothing is disseminated. Every peer with the same event set independently
  /// computes the same merged root. The next LOCAL_WRITE will have
  /// parents = projection_basis, which collapses only the exact basis that
  /// produced the tentative root.
  /// Primarily used to exercise re-anchoring after finalized_prolly_root_hash advances
  /// (via do_finalize or do_sync_finalized). Steady-state reconciliation is
  /// inlined in do_receive and do_write.
  ///
  /// computeTentativeProjection operates on mergeable heads (parent-closed + chunk-ready)
  /// and retains unresolved parked heads conservatively under closure lag.
  /// No peer is blocked from writing.
  action do_reconcile(p: PeerID): bool = {
    val st = nodes.get(p)
    all {
      st.head_event_ids.size() > 1,
      val result = computeTentativeProjection(st)
      val newLatestRoot = result._1
      val newParkedConflicts = result._2
      all {
        (newLatestRoot != st.tentative_prolly_root_hash or newParkedConflicts != st.parked_conflicts),
        nodes' = nodes.set(p, {
          ...st,
          tentative_prolly_root_hash: newLatestRoot,
          parked_conflicts: newParkedConflicts,
          projection_basis: result._3,
        }),
        inbox' = inbox,
        connected' = connected,
      }
    }
  }

  /// A peer's wall clock advances.
  action do_tick(p: PeerID): bool = {
    val st = nodes.get(p)
    all {
      st.localWall < MAX_WALL,
      nodes' = nodes.set(p, { ...st, localWall: st.localWall + 1 }),
      inbox' = inbox,
      connected' = connected,
    }
  }

  /// Heartbeat: peer p refreshes its witness view of peer q.
  /// Requires connectivity (heartbeats don't flow across partitions).
  ///
  /// NOTE: The protocol heartbeat sends the full seen frontier and only uses
  /// a digest for the remote head set plus a finalized-metadata digest for
  /// finalized-state sync triggering. The spec reads full remote state
  /// directly, matching the frontier-exchange semantics and abstracting the
  /// digest compare mechanics as direct payload equality.
  ///
  /// Pull-on-demand: p requests q's head set and seen frontier and recursively
  /// fetches missing ancestor closure for their union.
  /// Fetched events are added to p's inbox for later processing via do_receive.
  /// Chunk completion is modeled separately by do_chunks_ready.
  /// This models closure-based event exchange after partitions.
  action do_heartbeat(p: PeerID, q: PeerID): bool = {
    val st = nodes.get(p)
    val remote = nodes.get(q)
    all {
      p != q,
      isConnected(connected, p, q),
      val newActivePeers = st.active_peers.union(Set(q))
      val remoteSeenFrontier = remote.peer_seen_frontier.get(q)
      // Pull-on-demand closure: remote heads + remote seen frontier.
      val pulledEventIDs = pullClosureEventIDs(
        st.clock,
        remote.clock,
        remote.head_event_ids,
        remoteSeenFrontier
      )
      val pulledEvents = pulledEventIDs.map(event_id => remote.clock.get(event_id))
      all {
        nodes' = nodes.set(p, {
          ...st,
          peer_seen_frontier: st.peer_seen_frontier.set(q, remoteSeenFrontier),
          active_peers: newActivePeers,
        }),
        // Add pulled closure events to p's inbox for processing via do_receive
        inbox' = inbox.set(p, inbox.get(p).union(pulledEvents)),
        connected' = connected,
      }
    }
  }

  /// Finalize exactly one canonical stable event.
  /// Candidates are closure-ready, chunk-ready, covered by all active peers,
  /// not parked, not already finalized, and have all parents already finalized.
  /// The chosen event is the minimum eligible one by deterministic HLC/EventID
  /// order. The checkpoint merge starts from finalized_prolly_root_hash and
  /// merges only that event against its recorded anchor, preserving a canonical
  /// finalized checkpoint sequence independent of scheduler timing.
  ///
  /// In the executable model this standalone action is a scheduling abstraction
  /// for all candidate-enabling transitions (receive, chunk-ready, heartbeat,
  /// sync, conflict resolution, GC, membership change).
  action do_finalize(p: PeerID): bool = {
    val st = nodes.get(p)
    val candidates = finalizationCandidates(st)
    all {
      finalizeEnabled(st),
      val post = finalizeResult(st)
      nodes' = nodes.set(p, {
        ...post,
      }),
      inbox' = inbox,
      connected' = connected,
    }
  }

  /// Compact finalized event bodies from clock when no non-finalized event
  /// depends on them.
  action do_gc(p: PeerID): bool = {
    val st = nodes.get(p)
    val pruneable = gcPruneable(st)
    all {
      not(pruneable.empty()),
      nondet event_id = pruneable.oneOf()
      val newClock = dropEvent(st.clock, event_id)
      val newHeads = computeHeadEventIDs(newClock)
      val newChunksReady = st.chunks_ready.intersect(newClock.keys())
      val gcState: NodeState = {
        ...st,
        clock: newClock,
        head_event_ids: newHeads,
        chunks_ready: newChunksReady,
        peer_seen_frontier: st.peer_seen_frontier.set(p, observedFrontier(newClock)),
      }
      val reconcileResult = computeTentativeProjection(gcState)
      nodes' = nodes.set(p, {
        ...gcState,
        tentative_prolly_root_hash: reconcileResult._1,
        parked_conflicts: reconcileResult._2,
        projection_basis: reconcileResult._3,
      }),
      inbox' = inbox,
      connected' = connected,
    }
  }

  // ===========================================================================
  // Partition and membership actions
  // ===========================================================================

  /// Network partition: disconnect two peers.
  /// Events and heartbeats no longer flow between them.
  action do_partition(p: PeerID, q: PeerID): bool = all {
    p != q,
    isConnected(connected, p, q),
    nodes' = nodes,
    inbox' = inbox,
    connected' = connected.exclude(Set((p, q), (q, p))),
  }

  /// Network heal: reconnect two peers.
  /// Heartbeats and events can flow again. Event exchange and partition
  /// merge happen via subsequent do_heartbeat and do_sync_finalized.
  action do_heal(p: PeerID, q: PeerID): bool = all {
    p != q,
    not(isConnected(connected, p, q)),
    nodes' = nodes,
    inbox' = inbox,
    connected' = connected.union(Set((p, q), (q, p))),
  }

  /// Peer eviction: p removes q from its active_peers.
  /// Triggered after heartbeat timeout when q is unreachable.
  /// Precondition: q is not connected to p (models timeout detection).
  /// Cached frontier metadata for q is retained, but q no longer participates
  /// in finalization once evicted.
  action do_evict(p: PeerID, q: PeerID): bool = {
    val st = nodes.get(p)
    all {
      p != q,
      st.active_peers.contains(q),
      not(isConnected(connected, p, q)),
      val newActivePeers = st.active_peers.exclude(Set(q))
      all {
        nodes' = nodes.set(p, {
          ...st,
          active_peers: newActivePeers,
        }),
        inbox' = inbox,
        connected' = connected,
      }
    }
  }

  /// Synchronize finalized state after partition recovery (§7.10).
  ///
  /// Triggered when peer p detects that peer q has a different finalized
  /// commit or different finalized metadata (via heartbeat). Case split on
  /// finalized-commit lineage:
  /// - p finalized commit ancestor of q finalized commit: adopt q's finalized state
  /// - q finalized commit ancestor of p finalized commit: no-op for finalized root/events
  /// - neither ancestor of the other: deterministic three-way merge (accept-the-fork)
  ///
  /// In the merge branch, deterministic ours/theirs assignment picks the
  /// lower finalized-commit id as "ours". On conflict, uses ours-side root
  /// (deterministic) with explicit tracking in finalized_conflicts.
  action do_sync_finalized(p: PeerID, q: PeerID): bool = {
    val st_p = nodes.get(p)
    val st_q = nodes.get(q)
    val mode = syncMode(st_p, st_q)
    all {
      syncEnabled(connected, p, q, st_p, st_q),
      val syncedState: NodeState = applySyncFinalized(st_p, st_q)
      val withLocalFrontier = {
        ...syncedState,
        peer_seen_frontier: syncedState.peer_seen_frontier.set(
          p,
          observedFrontier(syncedState.clock)
        ),
      }
      all {
        // Branch tag is kept here so any future change to the action still
        // must satisfy the explicit SYNC_FINALIZED contract helpers.
        mode == syncMode(st_p, st_q),
        nodes' = nodes.set(p, withLocalFrontier),
        inbox' = inbox,
        connected' = connected,
      }
    }
  }

  /// Resolve a parked tentative conflict (§2.4 RESOLVE_CONFLICT).
  ///
  /// User (typically the parked event's author) provides resolution SQL
  /// (modeled as nondeterministic content). Creates a new event whose parents
  /// are projection_basis plus the explicitly-resolved parked event id.
  /// The resolution flows through LOCAL_WRITE semantics.
  action do_resolve_conflict(p: PeerID, parkedEventID: EventID): bool = {
    val st = nodes.get(p)
    all {
      st.parked_conflicts.contains(parkedEventID),
      st.clock.has(parkedEventID),
      st.localWall <= MAX_WALL,
      st.clock.keys().size() < MAX_EVENTS,
      nondet resolvedContent = CONTENTS.oneOf()
      val newHLC = tick(st.hlc_clock, st.localWall, p)
      val newEventID = makeEventID(p, newHLC.wall, newHLC.logical)
      // Resolve one parked conflict explicitly:
      // parents = projection_basis ∪ {parkedEventID}
      val resolveParents = st.projection_basis.union(Set(parkedEventID))
      val event: Event = {
        event_id: newEventID,
        prolly_root_hash: resolvedContent,
        anchor_prolly_root_hash: st.finalized_prolly_root_hash,
        parent_event_ids: resolveParents,
        resolved_parked_event_id: Some(parkedEventID),
        resolved_finalized_conflict_id: None,
        hlc: newHLC,
        peer: p,
      }
      val newClock = st.clock.put(newEventID, event)
      val newHeads = computeHeadEventIDs(newClock)
      // Recompute parking for the new head set
      val postState: NodeState = {
        ...st,
        clock: newClock,
        head_event_ids: newHeads,
        chunks_ready: st.chunks_ready.union(Set(newEventID)),
        hlc_clock: newHLC,
        peer_seen_frontier: st.peer_seen_frontier.set(p, observedFrontier(newClock)),
      }
      val reconcileResult = computeTentativeProjection(postState)
      val newLatestRoot = reconcileResult._1
      all {
        nodes' = nodes.set(p, {
          ...postState,
          tentative_prolly_root_hash: newLatestRoot,
          parked_conflicts: reconcileResult._2,
          projection_basis: reconcileResult._3,
        }),
        inbox' = disseminateToConnected(inbox, event, connected),
        connected' = connected,
      }
    }
  }

  /// Resolve a tracked finalized-layer conflict.
  ///
  /// This emits a normal write event carrying a finalized-conflict resolution
  /// claim. The conflict remains open until that claim hardens into finalized
  /// state or arrives via finalized-state sync.
  /// (modeled as nondeterministic content) through LOCAL_WRITE mechanics.
  /// The emitted event carries the resolved conflict id reference.
  action do_resolve_finalized_conflict(p: PeerID, conflictID: FinalizedConflictID): bool = {
    val st = nodes.get(p)
    all {
      resolveFinalizedConflictEnabled(st, p, conflictID),
      nondet resolvedContent = CONTENTS.oneOf()
      val nextState = resolveFinalizedConflictResult(st, p, conflictID, resolvedContent)
      val newEventID = makeEventID(
        p,
        nextState.hlc_clock.wall,
        nextState.hlc_clock.logical
      )
      val emittedEvent = nextState.clock.get(newEventID)
      all {
        nodes' = nodes.set(p, {
          ...nextState,
        }),
        inbox' = disseminateToConnected(inbox, emittedEvent, connected),
        connected' = connected,
      }
    }
  }

  /// Nondeterministic step.
  action step = {
    nondet p = PEERS.oneOf()
    any {
      nondet content = CONTENTS.oneOf()
      do_write(p, content),
      do_receive(p),
      nondet chunkEventID = nodes.get(p).clock.keys().oneOf()
      do_chunks_ready(p, chunkEventID),
      do_reconcile(p),
      do_tick(p),
      nondet q = PEERS.oneOf()
      do_heartbeat(p, q),
      do_finalize(p),
      do_gc(p),
      // Partition and membership actions
      nondet q3 = PEERS.oneOf()
      do_partition(p, q3),
      nondet q4 = PEERS.oneOf()
      do_heal(p, q4),
      nondet q5 = PEERS.oneOf()
      do_evict(p, q5),
      nondet q6 = PEERS.oneOf()
      do_sync_finalized(p, q6),
      // Tentative conflict resolution (precondition in action guards empty set)
      nondet parkedEventID = nodes.get(p).parked_conflicts.oneOf()
      do_resolve_conflict(p, parkedEventID),
      // Finalized-layer conflict resolution
      nondet finalizedConflictID = nodes.get(p).finalized_conflicts.keys().oneOf()
      do_resolve_finalized_conflict(p, finalizedConflictID),
    }
  }

  // ===========================================================================
  // Invariants
  // ===========================================================================

  /// INV1b retained-structure convergence:
  /// peers with the same retained clock keys have the same derived heads.
  /// INV1a logical identity is represented by logicalEventIDs(clock ∪ finalized).
  val inv_retained_structure_convergence = PEERS.forall(p1 => PEERS.forall(p2 => {
    val s1 = nodes.get(p1)
    val s2 = nodes.get(p2)
    (s1.clock.keys() == s2.clock.keys()) implies (s1.head_event_ids == s2.head_event_ids)
  }))

  /// Current finalized commit must match the finalized content root plus the
  /// actual parent set of the materialized finalized commit at the tip.
  val inv_finalized_commit_id_matches_state = PEERS.forall(p => {
    val st = nodes.get(p)
    st.finalized_commit_id == derivedFinalizedCommitID(st)
  })

  /// Finalized agreement (INV3, tip form): same finalized commit id implies
  /// the same finalized content root. History equality itself is carried by the
  /// finalized commit id plus derived lineage metadata, not by finalized_events.
  val inv_finalized_agreement = PEERS.forall(p1 => PEERS.forall(p2 => {
    val s1 = nodes.get(p1)
    val s2 = nodes.get(p2)
    (s1.finalized_commit_id == s2.finalized_commit_id) implies
      (s1.finalized_prolly_root_hash == s2.finalized_prolly_root_hash)
  }))

  /// Finalized lineage well-formedness:
  /// - EMPTY_FINALIZED_COMMIT_ID is present
  /// - finalized_commit_id is present in lineage known finalized commits
  /// - every known finalized commit is its own ancestor
  /// - every parent edge points to a strictly shallower finalized commit (derived depth)
  val inv_lineage_well_formed = PEERS.forall(p => {
    val st = nodes.get(p)
    val knownFinalizedCommits = lineageKnownFinalizedCommits(st.finalized_commit_parents)
      .union(Set(EMPTY_FINALIZED_COMMIT_ID))
    all {
      st.finalized_commit_parents.has(EMPTY_FINALIZED_COMMIT_ID),
      knownFinalizedCommits.contains(st.finalized_commit_id),
      knownFinalizedCommits.forall(finalized_commit =>
        ancestorsAt(st.finalized_commit_parents, finalized_commit).contains(finalized_commit)
      ),
      st.finalized_commit_parents.keys().forall(finalized_commit =>
        st.finalized_commit_parents.get(finalized_commit).forall(parent =>
          parent != finalized_commit and
          knownFinalizedCommits.contains(parent) and
          finalizedCommitDepthAt(st.finalized_commit_parents, parent)
            < finalizedCommitDepthAt(st.finalized_commit_parents, finalized_commit)
        )
      ),
    }
  })

  /// Finalized lineage closure:
  /// - parents of a finalized commit are included in its ancestor set
  /// - every referenced parent is a known lineage finalized commit
  val inv_lineage_closure = PEERS.forall(p => {
    val st = nodes.get(p)
    val knownFinalizedCommits = lineageKnownFinalizedCommits(st.finalized_commit_parents)
      .union(Set(EMPTY_FINALIZED_COMMIT_ID))
    st.finalized_commit_parents.keys().forall(finalized_commit => {
      st.finalized_commit_parents.get(finalized_commit)
        .subseteq(ancestorsAt(st.finalized_commit_parents, finalized_commit)) and
      st.finalized_commit_parents.get(finalized_commit)
        .forall(parent => knownFinalizedCommits.contains(parent))
    })
  })

  /// LCA soundness over lineage metadata:
  /// lineageLcaFinalizedCommit(a, b) must be a common ancestor with maximal depth.
  val inv_lineage_lca_sound = PEERS.forall(p => {
    val st = nodes.get(p)
    val knownFinalizedCommits = lineageKnownFinalizedCommits(st.finalized_commit_parents)
      .union(Set(EMPTY_FINALIZED_COMMIT_ID))
    knownFinalizedCommits.forall(a => knownFinalizedCommits.forall(b => {
      val lca = lineageLcaFinalizedCommit(a, b, st.finalized_commit_parents)
      val common = ancestorsAt(st.finalized_commit_parents, a).intersect(
        ancestorsAt(st.finalized_commit_parents, b)
      )
      if (common.empty()) {
        lca == EMPTY_FINALIZED_COMMIT_ID
      } else {
        all {
          common.contains(lca),
          common.forall(c =>
            finalizedCommitDepthAt(st.finalized_commit_parents, c)
              <= finalizedCommitDepthAt(st.finalized_commit_parents, lca)
          ),
        }
      }
    }))
  })

  /// SYNC_FINALIZED metadata-union contract:
  /// output lineage metadata includes merged local+remote parent metadata.
  val inv_sync_metadata_union = PEERS.forall(p => PEERS.forall(q => {
    val st_p = nodes.get(p)
    val st_q = nodes.get(q)
    val mergedParents = mergeSetMap(st_p.finalized_commit_parents, st_q.finalized_commit_parents)
    val mergedCheckpoints = mergeFinalizedCheckpointMap(
      st_p.finalized_checkpoint_events,
      st_q.finalized_checkpoint_events
    )
    val synced = applySyncFinalized(st_p, st_q)
    syncEnabled(connected, p, q, st_p, st_q) implies all {
      mergedParents.keys().forall(k => {
        if (synced.finalized_commit_parents.has(k))
          mergedParents.get(k).subseteq(synced.finalized_commit_parents.get(k))
        else
          false
      }),
      mergedCheckpoints.keys().forall(k =>
        synced.finalized_checkpoint_events.has(k) and
          synced.finalized_checkpoint_events.get(k) == mergedCheckpoints.get(k)
      ),
    }
  }))

  /// SYNC_FINALIZED merge branch must record lineage links to both input
  /// finalized commits (excluding self-edge if resulting finalized commit equals one input).
  val inv_sync_merge_parent_edges = PEERS.forall(p => PEERS.forall(q => {
    val st_p = nodes.get(p)
    val st_q = nodes.get(q)
    val synced = applySyncFinalized(st_p, st_q)
    val expectedParents = Set(st_p.finalized_commit_id, st_q.finalized_commit_id)
      .exclude(Set(synced.finalized_commit_id))
    (syncEnabled(connected, p, q, st_p, st_q) and syncMode(st_p, st_q) == SYNC_MERGE) implies {
      if (synced.finalized_commit_parents.has(synced.finalized_commit_id))
        expectedParents.subseteq(synced.finalized_commit_parents.get(synced.finalized_commit_id))
      else
        false
    }
  }))

  /// Nested-merge determinism check:
  /// sync(sync(A,B),C) and sync(A,sync(B,C)) converge on finalized state
  /// under merge-mode preconditions.
  val inv_sync_nested_merge_determinism = PEERS.forall(p => PEERS.forall(q => PEERS.forall(r => {
    val st_p = nodes.get(p)
    val st_q = nodes.get(q)
    val st_r = nodes.get(r)
    val left = applySyncFinalized(applySyncFinalized(st_p, st_q), st_r)
    val right = applySyncFinalized(st_p, applySyncFinalized(st_q, st_r))
    (p != q and q != r and p != r and
      syncEnabled(connected, p, q, st_p, st_q) and
      syncEnabled(connected, q, r, st_q, st_r) and
      syncEnabled(connected, p, r, st_p, st_r) and
      syncMode(st_p, st_q) == SYNC_MERGE and
      syncMode(st_q, st_r) == SYNC_MERGE and
      syncMode(st_p, st_r) == SYNC_MERGE) implies all {
      left.finalized_commit_id == right.finalized_commit_id,
      left.finalized_prolly_root_hash == right.finalized_prolly_root_hash,
      left.finalized_events == right.finalized_events,
      left.finalized_checkpoint_events == right.finalized_checkpoint_events,
      left.resolved_finalized_conflict_ids == right.resolved_finalized_conflict_ids,
    }
  })))

  /// SYNC_FINALIZED contract (adopt branch): behind peer adopts remote
  /// finalized root/events while preserving unresolved conflict visibility.
  val inv_sync_contract_adopt = PEERS.forall(p => PEERS.forall(q => {
    val st_p = nodes.get(p)
    val st_q = nodes.get(q)
    val mergedParents = mergeSetMap(st_p.finalized_commit_parents, st_q.finalized_commit_parents)
    val mergedCheckpoints = mergeFinalizedCheckpointMap(
      st_p.finalized_checkpoint_events,
      st_q.finalized_checkpoint_events
    )
    val mergedResolvedIDs = st_p.resolved_finalized_conflict_ids
      .union(st_q.resolved_finalized_conflict_ids)
    val unresolvedUnion = dropFinalizedConflicts(
      mergeFinalizedConflictMap(st_p.finalized_conflicts, st_q.finalized_conflicts),
      mergedResolvedIDs
    )
    val synced = applySyncFinalized(st_p, st_q)
    (syncEnabled(connected, p, q, st_p, st_q) and syncMode(st_p, st_q) == SYNC_ADOPT) implies all {
      synced.finalized_commit_id == st_q.finalized_commit_id,
      synced.finalized_prolly_root_hash == st_q.finalized_prolly_root_hash,
      synced.finalized_commit_parents == mergedParents,
      synced.finalized_checkpoint_events == mergedCheckpoints,
      synced.finalized_events == derivedFinalizedEventMap(
        st_q.finalized_commit_id,
        mergedParents,
        mergedCheckpoints
      ),
      synced.finalized_conflicts == unresolvedUnion,
      synced.resolved_finalized_conflict_ids == mergedResolvedIDs,
    }
  }))

  /// SYNC_FINALIZED empty-root adoption contract: a peer at EMPTY_ROOT adopts
  /// a connected peer's non-empty finalized state through the normal ADOPT path.
  val inv_sync_empty_root_adopts = PEERS.forall(p => PEERS.forall(q => {
    val st_p = nodes.get(p)
    val st_q = nodes.get(q)
    val mergedParents = mergeSetMap(st_p.finalized_commit_parents, st_q.finalized_commit_parents)
    val mergedCheckpoints = mergeFinalizedCheckpointMap(
      st_p.finalized_checkpoint_events,
      st_q.finalized_checkpoint_events
    )
    val synced = applySyncFinalized(st_p, st_q)
    (syncEnabled(connected, p, q, st_p, st_q) and
      st_p.finalized_commit_id == EMPTY_FINALIZED_COMMIT_ID and
      st_q.finalized_commit_id != EMPTY_FINALIZED_COMMIT_ID) implies all {
      syncMode(st_p, st_q) == SYNC_ADOPT,
      synced.finalized_commit_id == st_q.finalized_commit_id,
      synced.finalized_prolly_root_hash == st_q.finalized_prolly_root_hash,
      synced.finalized_events == derivedFinalizedEventMap(
        st_q.finalized_commit_id,
        mergedParents,
        mergedCheckpoints
      ),
    }
  }))

  /// SYNC_FINALIZED contract (no-op branch): ahead peer does not change
  /// finalized root/events, but unresolved finalized conflicts still propagate
  /// monotonically (minus durable resolved ids).
  val inv_sync_contract_noop = PEERS.forall(p => PEERS.forall(q => {
    val st_p = nodes.get(p)
    val st_q = nodes.get(q)
    val mergedParents = mergeSetMap(st_p.finalized_commit_parents, st_q.finalized_commit_parents)
    val mergedCheckpoints = mergeFinalizedCheckpointMap(
      st_p.finalized_checkpoint_events,
      st_q.finalized_checkpoint_events
    )
    val mergedResolvedIDs = st_p.resolved_finalized_conflict_ids
      .union(st_q.resolved_finalized_conflict_ids)
    val unresolvedUnion = dropFinalizedConflicts(
      mergeFinalizedConflictMap(st_p.finalized_conflicts, st_q.finalized_conflicts),
      mergedResolvedIDs
    )
    val synced = applySyncFinalized(st_p, st_q)
    (syncEnabled(connected, p, q, st_p, st_q) and syncMode(st_p, st_q) == SYNC_NOOP) implies all {
      synced.finalized_commit_id == st_p.finalized_commit_id,
      synced.finalized_prolly_root_hash == st_p.finalized_prolly_root_hash,
      synced.finalized_commit_parents == mergedParents,
      synced.finalized_checkpoint_events == mergedCheckpoints,
      synced.finalized_events == derivedFinalizedEventMap(
        st_p.finalized_commit_id,
        mergedParents,
        mergedCheckpoints
      ),
      synced.finalized_conflicts == unresolvedUnion,
      synced.resolved_finalized_conflict_ids == mergedResolvedIDs,
      synced.tentative_prolly_root_hash == st_p.tentative_prolly_root_hash,
      synced.parked_conflicts == st_p.parked_conflicts,
      synced.projection_basis == st_p.projection_basis,
    }
  }))

  /// Equal-tip finalized sync is a metadata repair no-op for the finalized
  /// commit DAG: tip/root stay fixed while lineage/checkpoint/conflict metadata
  /// is merged.
  val inv_sync_equal_tip_metadata_repair = PEERS.forall(p => PEERS.forall(q => {
    val st_p = nodes.get(p)
    val st_q = nodes.get(q)
    val mergedParents = mergeSetMap(st_p.finalized_commit_parents, st_q.finalized_commit_parents)
    val mergedCheckpoints = mergeFinalizedCheckpointMap(
      st_p.finalized_checkpoint_events,
      st_q.finalized_checkpoint_events
    )
    val mergedResolvedIDs = st_p.resolved_finalized_conflict_ids
      .union(st_q.resolved_finalized_conflict_ids)
    val unresolvedUnion = dropFinalizedConflicts(
      mergeFinalizedConflictMap(st_p.finalized_conflicts, st_q.finalized_conflicts),
      mergedResolvedIDs
    )
    val synced = applySyncFinalized(st_p, st_q)
    (syncEnabled(connected, p, q, st_p, st_q) and
      st_p.finalized_commit_id == st_q.finalized_commit_id) implies all {
      syncMode(st_p, st_q) == SYNC_NOOP,
      synced.finalized_commit_id == st_p.finalized_commit_id,
      synced.finalized_prolly_root_hash == st_p.finalized_prolly_root_hash,
      synced.finalized_commit_parents == mergedParents,
      synced.finalized_checkpoint_events == mergedCheckpoints,
      synced.finalized_events == derivedFinalizedEventMap(
        st_p.finalized_commit_id,
        mergedParents,
        mergedCheckpoints
      ),
      synced.finalized_conflicts == unresolvedUnion,
      synced.resolved_finalized_conflict_ids == mergedResolvedIDs,
    }
  }))

  /// Regression invariant: after any SYNC_NOOP (including equal-tip metadata
  /// repair), retained finalized events in clock must satisfy parent-closure
  /// and chunk-readiness. Compaction from bookkeeping changes must not leave
  /// finalized-but-unready or finalized-but-orphaned events in clock.
  val inv_sync_noop_retained_finalized_clean = PEERS.forall(p => PEERS.forall(q => {
    val st_p = nodes.get(p)
    val st_q = nodes.get(q)
    val synced = applySyncFinalized(st_p, st_q)
    (syncEnabled(connected, p, q, st_p, st_q) and syncMode(st_p, st_q) == SYNC_NOOP) implies
      retainedFinalizedEventIDs(synced).forall(event_id =>
        isParentClosed(synced.clock, event_id) and synced.chunks_ready.contains(event_id)
      )
  }))

  /// SYNC_FINALIZED contract (merge branch): divergence produces merged
  /// finalized events and never leaks CONFLICT_HASH to finalized_prolly_root_hash.
  val inv_sync_contract_merge = PEERS.forall(p => PEERS.forall(q => {
    val st_p = nodes.get(p)
    val st_q = nodes.get(q)
    val mergedCheckpoints = mergeFinalizedCheckpointMap(
      st_p.finalized_checkpoint_events,
      st_q.finalized_checkpoint_events
    )
    val mergedResolvedIDs = st_p.resolved_finalized_conflict_ids
      .union(st_q.resolved_finalized_conflict_ids)
    val unresolvedP = st_p.finalized_conflicts.keys().exclude(mergedResolvedIDs)
    val unresolvedQ = st_q.finalized_conflicts.keys().exclude(mergedResolvedIDs)
    val synced = applySyncFinalized(st_p, st_q)
    (syncEnabled(connected, p, q, st_p, st_q) and syncMode(st_p, st_q) == SYNC_MERGE) implies all {
      synced.finalized_events == mergeFinalizedEventMap(st_p.finalized_events, st_q.finalized_events),
      synced.finalized_checkpoint_events == mergedCheckpoints,
      synced.finalized_commit_id == derivedFinalizedCommitID(synced),
      synced.finalized_prolly_root_hash != CONFLICT_HASH,
      synced.resolved_finalized_conflict_ids == mergedResolvedIDs,
      unresolvedP.subseteq(
        synced.finalized_conflicts.keys()
      ),
      unresolvedQ.subseteq(
        synced.finalized_conflicts.keys()
      ),
    }
  }))

  /// SYNC_FINALIZED unresolved-conflict monotonicity:
  /// unresolved conflict ids cannot disappear without entering the durable
  /// resolved set.
  val inv_sync_unresolved_conflicts_monotone = PEERS.forall(p => PEERS.forall(q => {
    val st_p = nodes.get(p)
    val st_q = nodes.get(q)
    val mergedResolvedIDs = st_p.resolved_finalized_conflict_ids
      .union(st_q.resolved_finalized_conflict_ids)
    val unresolvedIDs = st_p.finalized_conflicts.keys()
      .union(st_q.finalized_conflicts.keys())
      .exclude(mergedResolvedIDs)
    val synced = applySyncFinalized(st_p, st_q)
    syncEnabled(connected, p, q, st_p, st_q) implies
      unresolvedIDs.subseteq(synced.finalized_conflicts.keys())
  }))

  /// SYNC_FINALIZED conflict-tracking contract: when merge branch conflict occurs,
  /// the deterministic finalized conflict id must be present in the synced state.
  val inv_sync_conflict_recorded = PEERS.forall(p => PEERS.forall(q => {
    val st_p = nodes.get(p)
    val st_q = nodes.get(q)
    val mergedParents = mergeSetMap(st_p.finalized_commit_parents, st_q.finalized_commit_parents)
    val mergedResolvedIDs = st_p.resolved_finalized_conflict_ids
      .union(st_q.resolved_finalized_conflict_ids)
    val commonFinalizedCommit = lineageLcaFinalizedCommit(
      st_p.finalized_commit_id,
      st_q.finalized_commit_id,
      mergedParents
    )
    val oursFinalizedCommit = if (finalizedCommitLess(st_p.finalized_commit_id, st_q.finalized_commit_id))
      st_p.finalized_commit_id else st_q.finalized_commit_id
    val theirsFinalizedCommit = if (finalizedCommitLess(st_p.finalized_commit_id, st_q.finalized_commit_id))
      st_q.finalized_commit_id else st_p.finalized_commit_id
    val conflictID = finalizedConflictID(oursFinalizedCommit, theirsFinalizedCommit, commonFinalizedCommit)
    val merged = mergeRoots(
      finalizedCommitProllyRootHash(oursFinalizedCommit),
      finalizedCommitProllyRootHash(theirsFinalizedCommit),
      finalizedCommitProllyRootHash(commonFinalizedCommit)
    )
    (syncEnabled(connected, p, q, st_p, st_q) and
      syncMode(st_p, st_q) == SYNC_MERGE and
      merged == CONFLICT_HASH and
      not(mergedResolvedIDs.contains(conflictID))) implies
      applySyncFinalized(st_p, st_q).finalized_conflicts.has(conflictID)
  }))

  /// SYNC_FINALIZED conflict-id symmetry: when both directions take merge+conflict,
  /// they derive and record the same deterministic conflict id.
  val inv_sync_conflict_id_symmetric = PEERS.forall(p => PEERS.forall(q => {
    val st_p = nodes.get(p)
    val st_q = nodes.get(q)
    val pCanSync = syncEnabled(connected, p, q, st_p, st_q)
    val qCanSync = syncEnabled(connected, q, p, st_q, st_p)
    val mergedResolvedIDs = st_p.resolved_finalized_conflict_ids
      .union(st_q.resolved_finalized_conflict_ids)

    val mergedParentsPQ = mergeSetMap(st_p.finalized_commit_parents, st_q.finalized_commit_parents)
    val commonFinalizedCommitPQ = lineageLcaFinalizedCommit(
      st_p.finalized_commit_id,
      st_q.finalized_commit_id,
      mergedParentsPQ
    )
    val oursFinalizedCommitPQ = if (finalizedCommitLess(st_p.finalized_commit_id, st_q.finalized_commit_id))
      st_p.finalized_commit_id else st_q.finalized_commit_id
    val theirsFinalizedCommitPQ = if (finalizedCommitLess(st_p.finalized_commit_id, st_q.finalized_commit_id))
      st_q.finalized_commit_id else st_p.finalized_commit_id
    val conflictIDPQ = finalizedConflictID(
      oursFinalizedCommitPQ,
      theirsFinalizedCommitPQ,
      commonFinalizedCommitPQ
    )
    val mergedPQ = mergeRoots(
      finalizedCommitProllyRootHash(oursFinalizedCommitPQ),
      finalizedCommitProllyRootHash(theirsFinalizedCommitPQ),
      finalizedCommitProllyRootHash(commonFinalizedCommitPQ)
    )

    val mergedParentsQP = mergeSetMap(st_q.finalized_commit_parents, st_p.finalized_commit_parents)
    val commonFinalizedCommitQP = lineageLcaFinalizedCommit(
      st_q.finalized_commit_id,
      st_p.finalized_commit_id,
      mergedParentsQP
    )
    val oursFinalizedCommitQP = if (finalizedCommitLess(st_q.finalized_commit_id, st_p.finalized_commit_id))
      st_q.finalized_commit_id else st_p.finalized_commit_id
    val theirsFinalizedCommitQP = if (finalizedCommitLess(st_q.finalized_commit_id, st_p.finalized_commit_id))
      st_p.finalized_commit_id else st_q.finalized_commit_id
    val conflictIDQP = finalizedConflictID(
      oursFinalizedCommitQP,
      theirsFinalizedCommitQP,
      commonFinalizedCommitQP
    )
    val mergedQP = mergeRoots(
      finalizedCommitProllyRootHash(oursFinalizedCommitQP),
      finalizedCommitProllyRootHash(theirsFinalizedCommitQP),
      finalizedCommitProllyRootHash(commonFinalizedCommitQP)
    )

    (pCanSync and qCanSync and
      syncMode(st_p, st_q) == SYNC_MERGE and
      syncMode(st_q, st_p) == SYNC_MERGE and
      mergedPQ == CONFLICT_HASH and
      mergedQP == CONFLICT_HASH) implies all {
      conflictIDPQ == conflictIDQP,
      (not(mergedResolvedIDs.contains(conflictIDPQ))) implies
        applySyncFinalized(st_p, st_q).finalized_conflicts.has(conflictIDPQ),
      (not(mergedResolvedIDs.contains(conflictIDQP))) implies
        applySyncFinalized(st_q, st_p).finalized_conflicts.has(conflictIDQP),
    }
  }))

  /// RESOLVE_FINALIZED_CONFLICT contract: enabled resolution emits an anchored
  /// write claim, leaves finalized history untouched, and does not immediately
  /// mark the conflict durably resolved.
  val inv_resolve_finalized_conflict_contract = PEERS.forall(p => {
    val st = nodes.get(p)
    st.finalized_conflicts.keys().forall(conflictID => {
      resolveFinalizedConflictEnabled(st, p, conflictID) implies
        CONTENTS.forall(resolvedContent => {
          val resolvedState = resolveFinalizedConflictResult(st, p, conflictID, resolvedContent)
          val emittedEventID = makeEventID(
            p,
            resolvedState.hlc_clock.wall,
            resolvedState.hlc_clock.logical
          )
          val emitted = resolvedState.clock.get(emittedEventID)
          all {
            emitted.parent_event_ids == st.projection_basis,
            resolvedState.finalized_prolly_root_hash == st.finalized_prolly_root_hash,
            resolvedState.finalized_commit_id == st.finalized_commit_id,
            resolvedState.finalized_events == st.finalized_events,
            resolvedState.finalized_checkpoint_events == st.finalized_checkpoint_events,
            resolvedState.finalized_conflicts == st.finalized_conflicts,
            resolvedState.resolved_finalized_conflict_ids == st.resolved_finalized_conflict_ids,
            emitted.resolved_finalized_conflict_id == Some(conflictID),
            emitted.anchor_prolly_root_hash == st.finalized_prolly_root_hash,
            resolvedState.chunks_ready.contains(emittedEventID),
            resolvedState.peer_seen_frontier.get(p) == observedFrontier(resolvedState.clock),
          }
        })
    })
  })

  /// Liveness-oriented bounded scenario checks (fairness approximations):
  /// - heartbeats/sync actions eventually fire on connected peers
  /// - closure/chunk readiness actions eventually complete for reachable events
  /// These are not full temporal liveness proofs; they are regression guards.

  /// Scenario check: ancestor/descendant finalized-root pair converges when the
  /// ahead peer no-ops and the behind peer syncs back.
  val inv_sync_subset_roundtrip_converges = PEERS.forall(p => PEERS.forall(q => {
    val st_p = nodes.get(p)
    val st_q = nodes.get(q)
    val pCanSync = syncEnabled(connected, p, q, st_p, st_q)
    val qCanSync = syncEnabled(connected, q, p, st_q, st_p)
    (pCanSync and qCanSync and
      syncMode(st_p, st_q) == SYNC_NOOP and
      syncMode(st_q, st_p) == SYNC_ADOPT) implies all {
      applySyncFinalized(st_q, applySyncFinalized(st_p, st_q)).finalized_events
        == applySyncFinalized(st_p, st_q).finalized_events,
      applySyncFinalized(st_q, applySyncFinalized(st_p, st_q)).finalized_commit_id
        == applySyncFinalized(st_p, st_q).finalized_commit_id,
      applySyncFinalized(st_q, applySyncFinalized(st_p, st_q)).finalized_prolly_root_hash
        == applySyncFinalized(st_p, st_q).finalized_prolly_root_hash,
    }
  }))

  /// Scenario check: incomparable peers independently converge to the same
  /// merged finalized state under deterministic merge tie-breaking.
  val inv_sync_incomparable_pairwise_converges = PEERS.forall(p => PEERS.forall(q => {
    val st_p = nodes.get(p)
    val st_q = nodes.get(q)
    val pCanSync = syncEnabled(connected, p, q, st_p, st_q)
    val qCanSync = syncEnabled(connected, q, p, st_q, st_p)
    (pCanSync and qCanSync and
      syncMode(st_p, st_q) == SYNC_MERGE and
      syncMode(st_q, st_p) == SYNC_MERGE) implies all {
      applySyncFinalized(st_p, st_q).finalized_events
        == applySyncFinalized(st_q, st_p).finalized_events,
      applySyncFinalized(st_p, st_q).finalized_commit_id
        == applySyncFinalized(st_q, st_p).finalized_commit_id,
      applySyncFinalized(st_p, st_q).finalized_prolly_root_hash
        == applySyncFinalized(st_q, st_p).finalized_prolly_root_hash,
      applySyncFinalized(st_p, st_q).resolved_finalized_conflict_ids
        == applySyncFinalized(st_q, st_p).resolved_finalized_conflict_ids,
    }
  }))

  /// Data convergence: peers with the same retained clock state and the same
  /// finalized root must derive the same reconcile projection.
  val inv_data_convergence = PEERS.forall(p1 => PEERS.forall(p2 => {
    val s1 = nodes.get(p1)
    val s2 = nodes.get(p2)
    (s1.clock == s2.clock and
     s1.chunks_ready == s2.chunks_ready and
     s1.finalized_prolly_root_hash == s2.finalized_prolly_root_hash and
     not(mergeableHeadEventIDs(s1).empty())) implies
      (s1.tentative_prolly_root_hash == s2.tentative_prolly_root_hash and
       s1.parked_conflicts == s2.parked_conflicts and
       s1.projection_basis == s2.projection_basis)
  }))

  /// Causal consistency (INV4): if e1 is a parent of e2 in the clock DAG,
  /// then e1 precedes e2 in the HLC total order. Since HLC total order is
  /// transitive, checking direct parents suffices for full ancestry chains.
  val inv_causal_consistency = PEERS.forall(p => {
    val st = nodes.get(p)
    st.clock.keys().forall(event_id => {
      val e = st.clock.get(event_id)
      e.parent_event_ids.forall(parent_event_id =>
        // Guard: parent may not yet be in this peer's clock
        not(st.clock.has(parent_event_id)) or
        less(st.clock.get(parent_event_id).hlc, e.hlc)
      )
    })
  })

  /// No silent data loss (INV5, safety approximation): finalized catalog entries
  /// that are still retained in clock must agree on root hash.
  ///
  /// The full INV5 ("every event *eventually* enters finalized set") is a
  /// liveness property that cannot be checked as a state invariant. This safety
  /// version ensures the precondition: events are never dropped without a trace.
  val inv_no_silent_data_loss = PEERS.forall(p => {
    val st = nodes.get(p)
    st.finalized_events.keys().forall(event_id =>
      not(st.clock.has(event_id)) or st.finalized_events.get(event_id) == st.clock.get(event_id).prolly_root_hash
    )
  })

  /// Finalized-event bookkeeping is derived from the finalized commit DAG plus
  /// checkpoint descriptors, not synchronized as an authoritative catalog.
  val inv_finalized_events_derived_from_checkpoints = PEERS.forall(p => {
    val st = nodes.get(p)
    st.finalized_events == derivedFinalizedEventMap(
      st.finalized_commit_id,
      st.finalized_commit_parents,
      st.finalized_checkpoint_events
    )
  })

  /// LOCAL_WRITE must not directly mutate finalized state. Concurrent writes
  /// remain below the finalized-history boundary.
  val inv_write_preserves_finalized_state = PEERS.forall(p => {
    val st = nodes.get(p)
    writeEnabled(st) implies CONTENTS.forall(content => {
      val post = writeResult(st, p, content)
      all {
        post.finalized_commit_id == st.finalized_commit_id,
        post.finalized_prolly_root_hash == st.finalized_prolly_root_hash,
        post.finalized_events == st.finalized_events,
        post.finalized_commit_parents == st.finalized_commit_parents,
        post.finalized_checkpoint_events == st.finalized_checkpoint_events,
        post.finalized_conflicts == st.finalized_conflicts,
        post.resolved_finalized_conflict_ids == st.resolved_finalized_conflict_ids,
      }
    })
  })

  /// RECONCILE is a tentative-layer projection only; it must never mutate the
  /// finalized catalog, lineage, or finalized conflict state.
  val inv_reconcile_preserves_finalized_state = PEERS.forall(p => {
    val st = nodes.get(p)
    val post = reconcileResult(st)
    not(reconcileEnabled(st)) or all {
      post.finalized_commit_id == st.finalized_commit_id,
      post.finalized_prolly_root_hash == st.finalized_prolly_root_hash,
      post.finalized_events == st.finalized_events,
      post.finalized_commit_parents == st.finalized_commit_parents,
      post.finalized_checkpoint_events == st.finalized_checkpoint_events,
      post.finalized_conflicts == st.finalized_conflicts,
      post.resolved_finalized_conflict_ids == st.resolved_finalized_conflict_ids,
    }
  })

  /// Normal finalization extends finalized lineage with at most one new edge:
  /// from the new finalized_commit_id back to the prior finalized_commit_id.
  /// This is the model-level form of "merge commits only when finalized
  /// lineages meet" — do_finalize itself must not synthesize multi-parent joins.
  val inv_finalize_linear_extension_only = PEERS.forall(p => {
    val st = nodes.get(p)
    val post = finalizeResult(st)
    val oldKnownFinalizedCommits = lineageKnownFinalizedCommits(st.finalized_commit_parents)
      .union(Set(EMPTY_FINALIZED_COMMIT_ID))
    val newEdges = parentEdges(post.finalized_commit_parents).exclude(parentEdges(st.finalized_commit_parents))
    val expectedNewParents = if (st.finalized_commit_id == EMPTY_FINALIZED_COMMIT_ID)
      Set()
    else
      Set(st.finalized_commit_id)
    finalizeEnabled(st) implies all {
      newEdges.subseteq(Set((post.finalized_commit_id, st.finalized_commit_id))),
      ((post.finalized_commit_id != st.finalized_commit_id) and
        not(oldKnownFinalizedCommits.contains(post.finalized_commit_id))) implies
          (post.finalized_commit_parents.has(post.finalized_commit_id) and
            post.finalized_commit_parents.get(post.finalized_commit_id)
              == expectedNewParents),
    }
  })

  /// Normal finalization materializes exactly one canonical stable event per
  /// checkpoint, not an arbitrary stable batch.
  val inv_finalize_one_event_per_checkpoint = PEERS.forall(p => {
    val st = nodes.get(p)
    val post = finalizeResult(st)
    val nextEventID = nextFinalizationEventID(st)
    finalizeEnabled(st) implies all {
      post.finalized_events.keys().exclude(st.finalized_events.keys()) == Set(nextEventID),
      post.finalized_events.has(nextEventID),
      post.finalized_events.get(nextEventID) == st.clock.get(nextEventID).prolly_root_hash,
    }
  })

  /// Tentative conflict resolution emits a normal anchored write that
  /// references the resolved parked head and leaves finalized state untouched.
  val inv_resolve_conflict_contract = PEERS.forall(p => {
    val st = nodes.get(p)
    st.parked_conflicts.forall(parkedEventID => {
      resolveConflictEnabled(st, parkedEventID) implies CONTENTS.forall(resolvedContent => {
        val post = resolveConflictResult(st, p, parkedEventID, resolvedContent)
        val emittedEventID = makeEventID(
          p,
          post.hlc_clock.wall,
          post.hlc_clock.logical
        )
        val emitted = post.clock.get(emittedEventID)
        all {
          post.clock.has(emittedEventID),
          emitted.parent_event_ids == st.projection_basis.union(Set(parkedEventID)),
          emitted.anchor_prolly_root_hash == st.finalized_prolly_root_hash,
          emitted.resolved_parked_event_id == Some(parkedEventID),
          emitted.resolved_finalized_conflict_id == None,
          not(post.head_event_ids.contains(parkedEventID)),
          not(post.parked_conflicts.contains(parkedEventID)),
          post.finalized_commit_id == st.finalized_commit_id,
          post.finalized_prolly_root_hash == st.finalized_prolly_root_hash,
          post.finalized_events == st.finalized_events,
          post.finalized_commit_parents == st.finalized_commit_parents,
          post.finalized_checkpoint_events == st.finalized_checkpoint_events,
          post.finalized_conflicts == st.finalized_conflicts,
          post.resolved_finalized_conflict_ids == st.resolved_finalized_conflict_ids,
          post.peer_seen_frontier.get(p) == observedFrontier(post.clock),
        }
      })
    })
  })

  /// All retained events in clocks satisfy the abstract swarm-wide admission
  /// predicate.
  val inv_clock_events_valid = PEERS.forall(p => {
    val st = nodes.get(p)
    st.clock.keys().forall(event_id => validEvent(st.clock.get(event_id)))
  })

  /// Any event still waiting in an inbox is also admissible under the same
  /// uniform predicate.
  val inv_inbox_events_valid = PEERS.forall(p =>
    inbox.get(p).forall(e => validEvent(e))
  )

  /// Event roots in clocks must never use the conflict sentinel.
  val inv_event_root_not_conflict = PEERS.forall(p => {
    val st = nodes.get(p)
    st.clock.keys().forall(event_id => st.clock.get(event_id).prolly_root_hash != CONFLICT_HASH)
  })

  /// Disjoint concurrent writes must auto-merge and be commutative.
  /// Scenario checks over the abstract 2D state space.
  val inv_merge_disjoint_commutative = all {
    // Disjoint writes: A and B dimensions changed independently auto-merge.
    mergeRoots(11, 12, 10) == 13,
    mergeRoots(12, 11, 10) == 13,
    mergeRoots(11, 12, 10) == mergeRoots(12, 11, 10),
    mergeRoots(10, 13, 11) == 12,
    mergeRoots(13, 10, 11) == 12,
    mergeRoots(10, 13, 11) == mergeRoots(13, 10, 11),
    // Same-dimension conflict: concurrent A=1 vs A=2 from A=0 => CONFLICT.
    mergeRoots(11, 14, 10) == CONFLICT_HASH,
    mergeRoots(14, 11, 10) == CONFLICT_HASH,
    mergeRoots(11, 14, 10) == mergeRoots(14, 11, 10),
    // Same-dimension conflict with B also changed: (A1,B1) vs (A2,B1) from (A0,B0) => CONFLICT.
    mergeRoots(13, 15, 10) == CONFLICT_HASH,
    mergeRoots(15, 13, 10) == CONFLICT_HASH,
    // Disjoint with expanded values: A=2 and B=1 from (A0,B0) => (A2,B1).
    mergeRoots(14, 12, 10) == 15,
    mergeRoots(12, 14, 10) == 15,
    mergeRoots(14, 12, 10) == mergeRoots(12, 14, 10),
  }

  /// No unhandled tentative conflict (INV7a): tentative_prolly_root_hash is never
  /// CONFLICT_HASH. Every conflict detected by mergeRoots during
  /// reconciliation is captured in parked_conflicts — none leaks
  /// into the working state.
  val inv_no_conflict_tentative_root = PEERS.forall(p => {
    val st = nodes.get(p)
    st.tentative_prolly_root_hash != CONFLICT_HASH
  })

  /// No unhandled finalized conflict (INV7b): finalized_prolly_root_hash is never
  /// CONFLICT_HASH. SYNC_FINALIZED divergence merges handle conflicts by using the
  /// ours-side root (deterministic) — no conflict sentinel leaks into
  /// the finalized state.
  val inv_no_conflict_finalized_prolly_root = PEERS.forall(p => {
    val st = nodes.get(p)
    st.finalized_prolly_root_hash != CONFLICT_HASH
  })

  /// Parked event integrity (INV7c): every parked conflict references
  /// a known event in the clock. No conflict is silently discarded.
  val inv_conflict_visibility = PEERS.forall(p => {
    val st = nodes.get(p)
    st.parked_conflicts.forall(event_id => st.clock.has(event_id))
  })

  /// Finalized conflict integrity (INV7e): every tracked finalized conflict
  /// is well-formed and references known lineage finalized commits.
  val inv_finalized_conflict_visibility = PEERS.forall(p => {
    val st = nodes.get(p)
    val knownFinalizedCommits = lineageKnownFinalizedCommits(st.finalized_commit_parents)
      .union(Set(EMPTY_FINALIZED_COMMIT_ID))
    st.finalized_conflicts.keys().forall(conflictID => {
      val c = st.finalized_conflicts.get(conflictID)
      all {
        conflictID == finalizedConflictID(c.ours_finalized_commit_id, c.theirs_finalized_commit_id, c.common_finalized_commit_id),
        c.ours_finalized_commit_id != c.theirs_finalized_commit_id,
        knownFinalizedCommits.contains(c.ours_finalized_commit_id),
        knownFinalizedCommits.contains(c.theirs_finalized_commit_id),
        knownFinalizedCommits.contains(c.common_finalized_commit_id),
      }
    })
  })

  /// Finalized conflict authenticity: tracked finalized conflicts must correspond
  /// to a real merge conflict under mergeRoots.
  val inv_finalized_conflict_authenticity = PEERS.forall(p => {
    val st = nodes.get(p)
    st.finalized_conflicts.keys().forall(conflictID => {
      val c = st.finalized_conflicts.get(conflictID)
      mergeRoots(
        finalizedCommitProllyRootHash(c.ours_finalized_commit_id),
        finalizedCommitProllyRootHash(c.theirs_finalized_commit_id),
        finalizedCommitProllyRootHash(c.common_finalized_commit_id)
      ) == CONFLICT_HASH
    })
  })

  /// Finalized conflict resolution propagation: once a conflict id is durably
  /// resolved at the finalized layer, that id must not remain tracked.
  val inv_finalized_conflict_no_resurrection = PEERS.forall(p => {
    val st = nodes.get(p)
    st.finalized_conflicts.keys().intersect(st.resolved_finalized_conflict_ids).empty()
  })

  /// Chunk-readiness integrity: only known clock events can be chunk-ready.
  val inv_chunks_ready_known = PEERS.forall(p => {
    val st = nodes.get(p)
    st.chunks_ready.subseteq(st.clock.keys())
  })

  /// Self must always remain in the current witness set.
  val inv_self_in_active_peers = PEERS.forall(p => {
    val st = nodes.get(p)
    st.active_peers.contains(p)
  })

  /// Parked integrity: parked conflicts always reference current heads.
  val inv_parked_are_heads = PEERS.forall(p => {
    val st = nodes.get(p)
    st.parked_conflicts.subseteq(st.head_event_ids)
  })

  /// Closure-ready parking safety: once all heads are mergeable, parked events
  /// are a subset of mergeable heads.
  val inv_parked_mergeable_when_closure_ready = PEERS.forall(p => {
    val st = nodes.get(p)
    (mergeableHeadEventIDs(st) == st.head_event_ids) implies st.parked_conflicts.subseteq(mergeableHeadEventIDs(st))
  })

  /// Retained finalized subgraph closure: any finalized event body retained in
  /// clock keeps its required retained ancestry.
  val inv_retained_finalized_subgraph_parent_closed = PEERS.forall(p => {
    val st = nodes.get(p)
    retainedFinalizedEventIDs(st).forall(event_id =>
      isParentClosed(st.clock, event_id)
    )
  })

  /// Finalization safety: every retained finalized event in clock is
  /// parent-closed and chunk-ready.
  val inv_finalized_parent_closed = PEERS.forall(p => {
    val st = nodes.get(p)
    retainedFinalizedEventIDs(st).forall(event_id =>
      isParentClosed(st.clock, event_id) and st.chunks_ready.contains(event_id)
    )
  })

  /// Reconcile fixed-point safety: tentative_prolly_root_hash is always equal to the
  /// deterministic merged-root projection from current state.
  val inv_tentative_root_is_compute_projection = PEERS.forall(p => {
    val st = nodes.get(p)
    st.tentative_prolly_root_hash == computeTentativeProjection(st)._1
  })

  /// Reconcile fixed-point safety: parked_conflicts is always equal to the
  /// deterministic parked-set projection from current state.
  val inv_parked_conflicts_is_compute_projection = PEERS.forall(p => {
    val st = nodes.get(p)
    st.parked_conflicts == computeTentativeProjection(st)._2
  })

  /// Projection-basis safety: emitted-parent basis is always the deterministic
  /// basis associated with the current tentative root.
  val inv_projection_basis_is_compute_projection = PEERS.forall(p => {
    val st = nodes.get(p)
    st.projection_basis == computeTentativeProjection(st)._3
  })

  /// Bounded anti-entropy scenario check: if p can heartbeat q and q's head has
  /// full ancestry in q's local clock, then one closure pull plus enough
  /// do_receive steps yields parent-closed remote heads in p's projected clock.
  val inv_heartbeat_closure_catchup_bounded = PEERS.forall(p => PEERS.forall(q => {
    val st_p = nodes.get(p)
    val st_q = nodes.get(q)
    val caughtUpClock = heartbeatClosureCatchupClock(st_p, st_q, q)
    (p != q and isConnected(connected, p, q)) implies
      st_q.head_event_ids.forall(h =>
        (st_q.clock.has(h) and
          ancestorIdClosure(Set(h), st_q.clock).subseteq(st_q.clock.keys())) implies
          isParentClosed(caughtUpClock, h)
      )
  }))

  /// Re-anchoring conservative retention check: when SYNC_FINALIZED changes the
  /// anchor and mergeable heads are still missing, unresolved parked heads that
  /// remain heads must not disappear from parked_conflicts.
  val inv_reanchor_provisional_parked_retained = PEERS.forall(p => PEERS.forall(q => {
    val st_p = nodes.get(p)
    val st_q = nodes.get(q)
    val synced = applySyncFinalized(st_p, st_q)
    val unresolvedHeadIDs = st_p.parked_conflicts.intersect(synced.head_event_ids)
    (syncEnabled(connected, p, q, st_p, st_q) and
      synced.finalized_commit_id != st_p.finalized_commit_id and
      mergeableHeadEventIDs(synced).empty()) implies
      unresolvedHeadIDs.subseteq(synced.parked_conflicts)
  }))

  /// Re-anchoring under closure lag: after SYNC_FINALIZED changes finalized_prolly_root_hash,
  /// projection may remain provisional while mergeable heads are missing.
  /// Once required closure and chunks are available and introduce mergeable
  /// heads whose anchored merge step is not a no-op, recomputation must change
  /// the stale projection.
  val inv_reanchor_after_closure_lag = PEERS.forall(p => PEERS.forall(q => {
    val st_p = nodes.get(p)
    val st_q = nodes.get(q)
    val synced = applySyncFinalized(st_p, st_q)
    val caughtClock = heartbeatClosureCatchupClock(synced, st_q, q)
    val caughtChunksReady = synced.chunks_ready.union(caughtClock.keys())
    val caughtState: NodeState = {
      ...synced,
      clock: caughtClock,
      head_event_ids: computeHeadEventIDs(caughtClock),
      chunks_ready: caughtChunksReady,
    }
    val recomputed = computeTentativeProjection(caughtState)
    (syncEnabled(connected, p, q, st_p, st_q) and
      synced.finalized_commit_id != st_p.finalized_commit_id and
      not(synced.head_event_ids.empty()) and
      mergeableHeadEventIDs(synced).empty() and
      not(mergeableHeadEventIDs(caughtState).empty()) and
      mergeableHeadEventIDs(caughtState).exists(h => {
        val candidate = mergeRoots(
          synced.tentative_prolly_root_hash,
          caughtState.clock.get(h).prolly_root_hash,
          caughtState.clock.get(h).anchor_prolly_root_hash
        )
        candidate == CONFLICT_HASH or candidate != synced.tentative_prolly_root_hash
      })
    ) implies
      (recomputed._1 != synced.tentative_prolly_root_hash or recomputed._2 != synced.parked_conflicts)
  }))

  /// Heartbeats should not keep requesting closure once the remote frontier
  /// and head ancestry are already present locally.
  val inv_heartbeat_no_redundant_pull_when_closed = PEERS.forall(p => PEERS.forall(q => {
    val st_p = nodes.get(p)
    val st_q = nodes.get(q)
    val remoteClosure = ancestorIdClosure(
      st_q.head_event_ids.union(st_q.peer_seen_frontier.get(q)),
      st_q.clock
    )
    (p != q and isConnected(connected, p, q) and
      remoteClosure.subseteq(st_p.clock.keys())) implies
        pullClosureEventIDs(
          st_p.clock,
          st_q.clock,
          st_q.head_event_ids,
          st_q.peer_seen_frontier.get(q)
        ).empty()
  }))

  /// Heartbeat witness refresh is exact and must not mutate finalized state.
  val inv_heartbeat_refreshes_remote_witness_exactly = PEERS.forall(p => PEERS.forall(q => {
    val st_p = nodes.get(p)
    val st_q = nodes.get(q)
    val post = heartbeatResult(st_p, st_q, q)
    (p != q and isConnected(connected, p, q)) implies all {
      post.peer_seen_frontier.get(q) == st_q.peer_seen_frontier.get(q),
      post.active_peers == st_p.active_peers.union(Set(q)),
      post.finalized_commit_id == st_p.finalized_commit_id,
      post.finalized_prolly_root_hash == st_p.finalized_prolly_root_hash,
      post.finalized_events == st_p.finalized_events,
      post.finalized_commit_parents == st_p.finalized_commit_parents,
      post.finalized_checkpoint_events == st_p.finalized_checkpoint_events,
      post.finalized_conflicts == st_p.finalized_conflicts,
      post.resolved_finalized_conflict_ids == st_p.resolved_finalized_conflict_ids,
    }
  }))

  /// Witness-frontier soundness (INV6): each peer records its own observed
  /// frontier exactly, and finalization with singleton membership reduces to
  /// local readiness because self coverage is immediate.
  val inv_self_seen_frontier_exact = PEERS.forall(p => {
    val st = nodes.get(p)
    st.peer_seen_frontier.get(p) == observedFrontier(st.clock)
  })

  /// The local seen frontier is an antichain: no frontier element is an
  /// ancestor of any other frontier element.
  val inv_self_seen_frontier_antichain = PEERS.forall(p => {
    val st = nodes.get(p)
    val frontier = st.peer_seen_frontier.get(p)
    frontier.forall(f1 => frontier.forall(f2 =>
      (f1 == f2) or all {
        not(ancestorIdClosure(Set(f1), st.clock).contains(f2)),
        not(ancestorIdClosure(Set(f2), st.clock).contains(f1)),
      }
    ))
  })

  /// Every locally parent-closed event is covered by the local seen frontier.
  val inv_self_seen_frontier_covers_parent_closed_clock = PEERS.forall(p => {
    val st = nodes.get(p)
    st.clock.keys().forall(event_id =>
      isParentClosed(st.clock, event_id) implies
        frontierCovers(st.clock, st.peer_seen_frontier.get(p), event_id)
    )
  })

  /// The model treats bootstrap/join as external to the core protocol, so all
  /// reachable states must already satisfy the bootstrap boundary contract.
  val inv_bootstrap_boundary_ready = PEERS.forall(p => {
    val st = nodes.get(p)
    bootstrapBoundaryReady(p, st)
  })

  pure def localReadyUnfinalized(st: NodeState): Set[EventID] =
    st.clock.keys().filter(event_id =>
      not(st.finalized_events.has(event_id)) and
      not(st.parked_conflicts.contains(event_id)) and
      isParentClosed(st.clock, event_id) and
      st.chunks_ready.contains(event_id)
    )

  val inv_singleton_membership_self_coverage = PEERS.forall(p => {
    val st = nodes.get(p)
    (st.active_peers == Set(p)) implies
      finalizationCandidates(st) == localReadyUnfinalized(st)
  })

  /// Conflicts do not block LOCAL_WRITE when model resource bounds permit it.
  val inv_write_enabled_despite_conflicts = PEERS.forall(p => {
    val st = nodes.get(p)
    (((not(st.parked_conflicts.empty())) or
      not(st.finalized_conflicts.keys().empty())) and
      writeEnabled(st)) implies CONTENTS.forall(content => {
        val post = writeResult(st, p, content)
        val emittedEventID = makeEventID(
          p,
          post.hlc_clock.wall,
          post.hlc_clock.logical
        )
        all {
          post.clock.keys().size() == st.clock.keys().size() + 1,
          post.clock.has(emittedEventID),
        }
      })
  })

  /// Re-adding a lagging peer to active_peers via heartbeat may delay future
  /// finalization, but it must not rewind or rewrite already-finalized history.
  val inv_rejoin_does_not_rewind_finalized = PEERS.forall(p => PEERS.forall(q => {
    val st_p = nodes.get(p)
    val st_q = nodes.get(q)
    val post = heartbeatResult(st_p, st_q, q)
    (p != q and isConnected(connected, p, q) and
      not(st_p.active_peers.contains(q))) implies all {
        post.active_peers.contains(q),
        post.finalized_commit_id == st_p.finalized_commit_id,
        post.finalized_prolly_root_hash == st_p.finalized_prolly_root_hash,
        post.finalized_events == st_p.finalized_events,
        post.finalized_commit_parents == st_p.finalized_commit_parents,
        post.finalized_checkpoint_events == st_p.finalized_checkpoint_events,
        post.finalized_conflicts == st_p.finalized_conflicts,
        post.resolved_finalized_conflict_ids == st_p.resolved_finalized_conflict_ids,
      }
  }))

  /// Parking agreement (INV7d, closure-ready):
  /// peers with the same events/chunk-readiness and same finalized_prolly_root_hash
  /// deterministically agree on parked events once head closure is complete.
  val inv_parking_agreement = PEERS.forall(p1 => PEERS.forall(p2 => {
    val s1 = nodes.get(p1)
    val s2 = nodes.get(p2)
    (s1.clock.keys() == s2.clock.keys() and
     s1.chunks_ready == s2.chunks_ready and
     mergeableHeadEventIDs(s1) == s1.head_event_ids and
     mergeableHeadEventIDs(s2) == s2.head_event_ids and
     s1.finalized_prolly_root_hash == s2.finalized_prolly_root_hash) implies
      (s1.parked_conflicts == s2.parked_conflicts and
       s1.projection_basis == s2.projection_basis)
  }))

  /// Heads are always derived from clock structure (no hidden divergence).
  val inv_heads_consistent = PEERS.forall(p => {
    val st = nodes.get(p)
    st.head_event_ids == computeHeadEventIDs(st.clock)
  })

  /// INV9a: Parked and subsumed events are disjoint.
  /// A subsumed event has been resolved; it must not remain parked.
  val inv_parked_subsumed_disjoint = PEERS.forall(p => {
    val st = nodes.get(p)
    st.parked_conflicts.intersect(subsumedEvents(st.clock)).empty()
  })

  /// INV9b: Every subsumed event has a parent link from its resolver.
  /// The event that carries resolved_parked_event_id must include the
  /// subsumed event in its parent_event_ids.
  val inv_subsumed_parent_link = PEERS.forall(p => {
    val st = nodes.get(p)
    st.clock.keys().forall(event_id => {
      val ev = st.clock.get(event_id)
      match ev.resolved_parked_event_id {
        | Some(sub_id) => ev.parent_event_ids.contains(sub_id)
        | None => true
      }
    })
  })

  /// INV9c: Subsumed events finalize as bookkeeping-only.
  /// When a subsumed event is finalized, the finalized_prolly_root_hash and
  /// finalized_commit_id must not change (they stay at the pre-subsumption
  /// values). We check this indirectly: a subsumed finalized event's
  /// prolly_root_hash is recorded in finalized_events but must not equal
  /// the finalized_prolly_root_hash (unless coincidentally the same content).
  /// More precisely: subsumed events attach to the existing checkpoint, so
  /// the checkpoint descriptor that contains them must have been created
  /// by a non-subsumed event.
  val inv_subsumed_finalize_bookkeeping_only = PEERS.forall(p => {
    val st = nodes.get(p)
    val subsumed = subsumedEvents(st.clock)
    val finalizedSubsumed = st.finalized_events.keys().intersect(subsumed)
    // Each finalized subsumed event must be attached to a checkpoint that
    // also contains at least one non-subsumed event (the original creator).
    finalizedSubsumed.forall(sub_id =>
      st.finalized_checkpoint_events.keys().exists(commit_id =>
        st.finalized_checkpoint_events.get(commit_id).has(sub_id) and
        st.finalized_checkpoint_events.get(commit_id).keys().exists(eid =>
          not(subsumed.contains(eid))
        )
      )
    )
  })

  /// Finalization and parking are disjoint: parked events are never finalized.
  val inv_finalized_parked_disjoint = PEERS.forall(p => {
    val st = nodes.get(p)
    st.finalized_events.keys().intersect(st.parked_conflicts).empty()
  })

  /// Three-peer partition-heal regression: if three peers have pairwise
  /// divergent finalized histories and all pairwise finalized syncs are in
  /// merge mode, healing/sync order must converge on the same finalized state.
  val inv_three_peer_partition_heal_converges = PEERS.forall(p => PEERS.forall(q => PEERS.forall(r => {
    val st_p = nodes.get(p)
    val st_q = nodes.get(q)
    val st_r = nodes.get(r)
    val left = applySyncFinalized(applySyncFinalized(st_p, st_q), st_r)
    val right = applySyncFinalized(applySyncFinalized(st_p, st_r), st_q)
    (p != q and q != r and p != r and
      syncEnabled(connected, p, q, st_p, st_q) and
      syncEnabled(connected, p, r, st_p, st_r) and
      syncEnabled(connected, q, r, st_q, st_r) and
      syncMode(st_p, st_q) == SYNC_MERGE and
      syncMode(st_p, st_r) == SYNC_MERGE and
      syncMode(st_q, st_r) == SYNC_MERGE) implies all {
        left.finalized_commit_id == right.finalized_commit_id,
        left.finalized_prolly_root_hash == right.finalized_prolly_root_hash,
        left.finalized_events == right.finalized_events,
        left.finalized_commit_parents == right.finalized_commit_parents,
        left.finalized_checkpoint_events == right.finalized_checkpoint_events,
        left.finalized_conflicts == right.finalized_conflicts,
        left.resolved_finalized_conflict_ids == right.resolved_finalized_conflict_ids,
      }
  })))

}
