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
 * Events carry anchor_prolly_root_hash in addition to prolly_root_hash.
 * Reconciliation folds tentative heads directly against their recorded anchors.
 * Finalization still starts from the current finalized content-root accumulator,
 * but replays each chosen event against its reconstructed parent-basis replay
 * base (recorded anchor for root events, deterministic parent-basis fold
 * otherwise).
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
 *   finalized_conflicts is structural-only metadata keyed by the finalized
 *   conflict triple; any table names, row details, or human-readable
 *   diagnostics are local advisory metadata and remain outside the model.
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
module doltswarm_model {
  import basicSpells.* from "../spells/basicSpells"
  import hlc.* from "../hlc"

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
  /// The first and third fields retain the finalized commit's prolly root and
  /// kind directly. The middle field is a canonical token list built from the
  /// exact sorted parent ids, replacing the old arithmetic fingerprinting and
  /// the later mutable catalog-slot encoding.
  type FinalizedCommitID = (ProllyRootHash, List[int], int)

  /// Deterministic identity for a finalized-layer conflict from SYNC_FINALIZED.
  /// Canonical tuple: (ours_finalized_commit_id, theirs_finalized_commit_id,
  /// common_finalized_commit_id). Only this structural triple participates in
  /// identity; local diagnostics are intentionally out of model.
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

  /// Structural finalized-conflict record synchronized by the protocol.
  /// Human-readable diagnostics are intentionally omitted from replicated state.
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

  pure val EMPTY_FINALIZED_COMMIT_ID: FinalizedCommitID =
    (EMPTY_ROOT, [EMPTY_ROOT, FINALIZED_KIND_NONE, 0], FINALIZED_KIND_NONE)
  pure val EMPTY_EVENT_ID: EventID = (0, 0, 0)
  pure val EVENT_ID_LOGICAL_BASE: int = MAX_EVENTS + 2
  pure val EVENT_ID_WALL_BASE: int = MAX_WALL + 2
  pure val EVENT_ID_ENCODING_BASE: int = 512

  pure def finalizedCommitProllyRootHash(finalized_commit: FinalizedCommitID): ProllyRootHash =
    finalized_commit._1

  pure def finalizedCommitTokens(finalized_commit: FinalizedCommitID): List[int] =
    finalized_commit._2

  pure def finalizedCommitKind(finalized_commit: FinalizedCommitID): int =
    finalized_commit._3

  pure def eventIDLess(a: EventID, b: EventID): bool =
    if (a._1 != b._1) a._1 < b._1
    else if (a._2 != b._2) a._2 < b._2
    else a._3 < b._3

  pure def appendIntList(acc: List[int], suffix: List[int]): List[int] =
    suffix.foldl(acc, (result, value) => result.append(value))

  pure def appendTaggedIntList(acc: List[int], suffix: List[int]): List[int] =
    appendIntList(acc.append(suffix.length()), suffix)

  pure def intListLess(a: List[int], b: List[int]): bool = {
    // Keep the lexicographic comparison exact while avoiding range(0, Len(...)),
    // which Apalache cannot encode when the bound is state-dependent.
    val comparison = a.foldl((0, false, false), (acc, value) =>
      if (acc._2) acc
      else if (acc._1 >= b.length()) (acc._1, true, false)
      else if (value < b[acc._1]) (acc._1, true, true)
      else if (value > b[acc._1]) (acc._1, true, false)
      else (acc._1 + 1, false, false)
    )
    if (comparison._2) comparison._3
    else comparison._1 < b.length()
  }

  pure def finalizedCommitLess(a: FinalizedCommitID, b: FinalizedCommitID): bool =
    if (finalizedCommitProllyRootHash(a) != finalizedCommitProllyRootHash(b))
      finalizedCommitProllyRootHash(a) < finalizedCommitProllyRootHash(b)
    else if (finalizedCommitKind(a) != finalizedCommitKind(b))
      finalizedCommitKind(a) < finalizedCommitKind(b)
    else
      intListLess(finalizedCommitTokens(a), finalizedCommitTokens(b))

  pure def eventIDOrdinal(event_id: EventID): int =
    ((event_id._1 * EVENT_ID_WALL_BASE) + event_id._2) * EVENT_ID_LOGICAL_BASE + event_id._3

  pure def encodeEventIDSet(event_ids: Set[EventID]): int = {
    val eventIDList = event_ids.fold([], (acc, event_id) => acc.append(event_id))
    val sorted = eventIDList.sortList((a, b) => eventIDLess(a, b))
    sorted.foldl(0, (acc, event_id) =>
      acc * EVENT_ID_ENCODING_BASE + eventIDOrdinal(event_id) + 1
    )
  }

  pure def makeFinalizedCommitID(contentRoot: ProllyRootHash,
      parentFinalizedCommits: Set[FinalizedCommitID], kind: int): FinalizedCommitID = {
    val parentList = parentFinalizedCommits.fold([], (acc, finalized_commit) => acc.append(finalized_commit))
    val sortedParents = parentList.sortList((a, b) => finalizedCommitLess(a, b))
    val tokens = sortedParents.foldl([contentRoot, kind, sortedParents.length()], (acc, parent) =>
      appendTaggedIntList(acc, finalizedCommitTokens(parent))
    )
    (contentRoot, tokens, kind)
  }

  pure def makeFinalizedCheckpointCommitID(contentRoot: ProllyRootHash,
      parentFinalizedCommits: Set[FinalizedCommitID]): FinalizedCommitID =
    makeFinalizedCommitID(contentRoot, parentFinalizedCommits, FINALIZED_KIND_CHECKPOINT)

  pure def makeFinalizedMergeCommitID(contentRoot: ProllyRootHash,
      parentFinalizedCommits: Set[FinalizedCommitID]): FinalizedCommitID =
    makeFinalizedCommitID(contentRoot, parentFinalizedCommits, FINALIZED_KIND_MERGE)

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

  /// Derived set of event IDs whose conflicted tentative branch has been
  /// explicitly carried forward by a resolution event. This includes the
  /// resolved parked head and its retained tentative ancestors, all of which
  /// finalize as bookkeeping-only entries rather than by replay.
  pure def subsumedEvents(clock: EventID -> Event): Set[EventID] = {
    val resolvedParkedRoots = clock.keys().fold(Set(), (acc, event_id) =>
      match clock.get(event_id).resolved_parked_event_id {
        | Some(pid) => acc.union(Set(pid))
        | None => acc
      }
    )
    ancestorIdClosure(resolvedParkedRoots, clock)
  }

  /// Ancestors of unresolved parked heads remain blocked from finalization
  /// until that parked branch is either resolved or compacted away.
  pure def parkedBranchEvents(st: NodeState): Set[EventID] =
    ancestorIdClosure(st.parked_conflicts, st.clock)

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

  /// Retained finalized event bodies can remain in the local clock for
  /// closure/bookkeeping, but they are already represented by the finalized
  /// accumulator and must not re-enter tentative reconciliation.
  pure def tentativeHeadEventIDs(st: NodeState): Set[EventID] =
    st.head_event_ids.exclude(st.finalized_events.keys())

  /// Tentative head event IDs eligible for reconciliation (MERGEABLE):
  /// parent-closed and chunk-ready.
  pure def mergeableHeadEventIDs(st: NodeState): Set[EventID] =
    tentativeHeadEventIDs(st).filter(event_id =>
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

  /// Shared anchored ordered merge fold used by RECONCILE and by
  /// ADVANCE_STABILITY's replay-base reconstruction.
  /// Starts from start_prolly_root_hash and merges each head against its own
  /// recorded anchor_prolly_root_hash. Returns (merged_root, conflicts) where
  /// conflicts contains head event IDs whose merge against the running
  /// accumulator conflicted.
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

  /// Parents that define the actual replay basis for finalization. Conflict
  /// resolution events carry the resolved parked head as a causal/bookkeeping
  /// parent, but replay against the current non-parked basis rather than
  /// treating the parked head as the write ancestor.
  pure def finalizationReplayParents(event: Event): Set[EventID] =
    match event.resolved_parked_event_id {
      | Some(parkedEventID) => event.parent_event_ids.exclude(Set(parkedEventID))
      | None => event.parent_event_ids
    }

  /// Deterministic replay base for finalization. Root events replay against
  /// their recorded finalized anchor; descendant events replay against the
  /// reconstructed parent-basis root they were written on.
  pure def finalizationReplayBase(st: NodeState, event_id: EventID): ProllyRootHash = {
    val event = st.clock.get(event_id)
    val replayParents = finalizationReplayParents(event)
    if (replayParents.empty()) event.anchor_prolly_root_hash
    else if (not(replayParents.subseteq(st.clock.keys()))) CONFLICT_HASH
    else {
      val parentFold = anchoredFold(replayParents, st, event.anchor_prolly_root_hash)
      if (parentFold._2.empty()) parentFold._1
      else CONFLICT_HASH
    }
  }

  /// Replay safety for non-subsumed finalization. Causal stability and
  /// parent-finalization are necessary but not sufficient: a replay candidate is
  /// only admissible if its reconstructed replay base is defined and replaying
  /// it into the current finalized accumulator is conflict-free.
  pure def replayFinalizationSafe(st: NodeState, event_id: EventID): bool = {
    val event = st.clock.get(event_id)
    val replayBase = finalizationReplayBase(st, event_id)
    replayBase != CONFLICT_HASH and
      mergeRoots(
        st.finalized_prolly_root_hash,
        event.prolly_root_hash,
        replayBase
      ) != CONFLICT_HASH
  }

  /// Eligible ready heads that are already unsafe to replay against the
  /// current finalized accumulator must remain parked, even if they become the
  /// only tentative head after a competing branch hardens.
  pure def replayUnsafeEligibleHeads(st: NodeState, readyHeads: Set[EventID]): Set[EventID] =
    readyHeads.filter(event_id => {
      val event = st.clock.get(event_id)
      not(subsumedEvents(st.clock).contains(event_id)) and
        event.parent_event_ids.subseteq(st.finalized_events.keys()) and
        not(replayFinalizationSafe(st, event_id))
    })

  /// Deterministic tentative projection for head_event_ids:
  /// - full closure path: mergeable heads are folded with per-event anchors
  /// - closure-lag path: projection is provisional and retains unresolved
  ///   parked head IDs conservatively until closure completes
  /// - immediately eligible heads that are replay-unsafe against the current
  ///   finalized accumulator stay parked instead of re-entering projection
  ///
  /// Returns (merged_root, parked_set, projection_basis). Single-head and
  /// multi-head use the same anchored fold semantics (no single-head bypass).
  pure def computeTentativeProjection(st: NodeState):
      (ProllyRootHash, Set[EventID], Set[EventID]) = {
    val tentativeHeads = tentativeHeadEventIDs(st)
    val readyHeads = mergeableHeadEventIDs(st)
    if (tentativeHeads.empty()) {
      (st.finalized_prolly_root_hash, Set(), Set())
    } else if (readyHeads.empty()) {
      (
        st.tentative_prolly_root_hash,
        st.parked_conflicts.intersect(tentativeHeads),
        st.projection_basis.intersect(tentativeHeads)
      )
    } else {
      val replayUnsafeHeads = replayUnsafeEligibleHeads(st, readyHeads)
      val projectionReadyHeads = readyHeads.exclude(replayUnsafeHeads)
      val folded = anchoredFold(projectionReadyHeads, st, st.finalized_prolly_root_hash)
      val unreadyHeads = tentativeHeads.exclude(readyHeads)
      val retainedUnreadyParked = st.parked_conflicts.intersect(unreadyHeads)
      val parkedSet = folded._2.union(retainedUnreadyParked).union(replayUnsafeHeads)
      val projectionBasis = projectionReadyHeads.exclude(folded._2)
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
      not(parkedBranchEvents(st).contains(event_id)) and
      isParentClosed(st.clock, event_id) and
      st.chunks_ready.contains(event_id) and
      coveredByActivePeers(st, event_id)
    )

  /// Canonical stable frontier for finalized compilation: stable events whose
  /// parents are already in finalized history, and whose non-subsumed replay is
  /// already known to be safe against the current finalized accumulator.
  pure def finalizationEligible(st: NodeState): Set[EventID] =
    finalizationCandidates(st).filter(event_id =>
      st.clock.get(event_id).parent_event_ids.subseteq(st.finalized_events.keys()) and
        (
          subsumedEvents(st.clock).contains(event_id) or
            replayFinalizationSafe(st, event_id)
        )
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
      val replayBase = finalizationReplayBase(st, event_id)
      if (replayBase == CONFLICT_HASH) CONFLICT_HASH
      else mergeRoots(st.finalized_prolly_root_hash, event.prolly_root_hash, replayBase)
    }
  }

  /// Finalized events safe to compact from clock.
  /// A finalized event id is pruneable only if no other retained clock event
  /// depends on it through parent ancestry. This preserves parent-closure of
  /// the retained subgraph after compaction.
  pure def retainedFinalizedEventIDs(st: NodeState): Set[EventID] =
    st.finalized_events.keys().intersect(st.clock.keys())

  /// Retained finalized events that cannot safely remain materialized locally.
  /// These must be compacted even if some non-finalized retained event still
  /// points at them; the tentative side then remains provisional until closure
  /// is repaired later.
  pure def invalidRetainedFinalizedEventIDs(st: NodeState): Set[EventID] =
    retainedFinalizedEventIDs(st).filter(event_id =>
      not(isParentClosed(st.clock, event_id) and st.chunks_ready.contains(event_id))
    )

  pure def gcPruneable(st: NodeState): Set[EventID] =
    retainedFinalizedEventIDs(st).filter(finalized_event_id =>
      not(st.clock.keys().exists(event_id =>
        event_id != finalized_event_id and
        ancestorIdClosure(Set(event_id), st.clock).contains(finalized_event_id)
      ))
    )

  /// Optional compaction pass used after finalized sync to restore the
  /// retained finalized subgraph to a parent-closed, ready-only shape before
  /// reconcile. Finalized event bodies that remain orphaned or unready are
  /// compacted even if a non-finalized retained event still depends on them.
  pure def compactRetainedFinalized(st: NodeState): NodeState = {
    val compacted = range(0, MAX_EVENTS).foldl(st, (accSt, _) => {
      val pruneable = gcPruneable(accSt).union(invalidRetainedFinalizedEventIDs(accSt))
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
  /// Because records are structural-only, equal keys imply equal logical facts.
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

  /// Maximal finalized tips in a candidate set under the known finalized
  /// lineage. Used by bounded partition-heal regressions to check that
  /// repeated binary merges collapse the remaining incomparable frontier.
  pure def finalizedTipFrontier(candidateTips: Set[FinalizedCommitID],
      parentsMap: FinalizedCommitID -> Set[FinalizedCommitID]): Set[FinalizedCommitID] =
    candidateTips.filter(tip =>
      not(candidateTips.exists(other =>
        other != tip and finalizedCommitIsAncestor(tip, other, parentsMap)
      ))
    )

  pure def finalizedDescendsFromAll(finalized_commit: FinalizedCommitID,
      requiredAncestors: Set[FinalizedCommitID],
      parentsMap: FinalizedCommitID -> Set[FinalizedCommitID]): bool =
    requiredAncestors.forall(ancestor =>
      ancestorsAt(parentsMap, finalized_commit).contains(ancestor)
    )

  pure def finalizedConflictID(oursFinalizedCommit: FinalizedCommitID,
      theirsFinalizedCommit: FinalizedCommitID,
      commonFinalizedCommit: FinalizedCommitID): FinalizedConflictID =
    // Structural-only identity: no presentation metadata participates.
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
      val kind = if (parents.size() >= 2)
        FINALIZED_KIND_MERGE
      else
        FINALIZED_KIND_CHECKPOINT
      makeFinalizedCommitID(st.finalized_prolly_root_hash, parents, kind)
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

  /// Deterministic helper for synthetic seeded bootstrap scenarios.
  pure def seedEvent(peer: PeerID, wall: int, logical: int,
      prollyRootHash: ProllyRootHash, anchorRootHash: ProllyRootHash,
      parentEventIDs: Set[EventID]): Event = {
    val eventID = makeEventID(peer, wall, logical)
    {
      event_id: eventID,
      prolly_root_hash: prollyRootHash,
      anchor_prolly_root_hash: anchorRootHash,
      parent_event_ids: parentEventIDs,
      resolved_parked_event_id: None,
      resolved_finalized_conflict_id: None,
      hlc: { wall: wall, logical: logical, peer: peer },
      peer: peer,
    }
  }

  /// Deterministic helper for the empty already-bootstrapped steady state of
  /// one peer. This mirrors the per-peer local state from `init`.
  pure def emptyNodeState(self: PeerID): NodeState = {
    clock: Map(),
    head_event_ids: Set(),
    chunks_ready: Set(),
    tentative_prolly_root_hash: EMPTY_ROOT,
    projection_basis: Set(),
    hlc_clock: { wall: 1, logical: 0, peer: self },
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
  }

  /// Seeded bootstrapped checkpoint state used only by scenario invariants.
  /// retainFinalizedBody toggles whether the current checkpoint event body is
  /// still materialized locally. includeExtraCheckpoint toggles extra known but
  /// unreachable checkpoint metadata so equal-tip metadata repair can be
  /// checked without introducing bootstrap as a core action.
  pure def seedBootstrappedCheckpointState(self: PeerID,
      retainFinalizedBody: bool, includeExtraCheckpoint: bool): NodeState = {
    val event1 = seedEvent(1, 1, 0, 11, EMPTY_ROOT, Set())
    val event2 = seedEvent(2, 1, 0, 12, EMPTY_ROOT, Set())
    val tip1 = makeFinalizedCheckpointCommitID(11, Set())
    val tip2 = makeFinalizedCheckpointCommitID(12, Set())
    val parentsBase = Map()
      .put(EMPTY_FINALIZED_COMMIT_ID, Set())
      .put(tip1, Set())
    val parents =
      if (includeExtraCheckpoint)
        lineageAddFinalizedCommit(parentsBase, tip2, Set())
      else
        parentsBase
    val checkpointsBase = Map().put(tip1, Map().put(event1.event_id, event1.prolly_root_hash))
    val checkpoints =
      if (includeExtraCheckpoint)
        checkpointsBase.put(tip2, Map().put(event2.event_id, event2.prolly_root_hash))
      else
        checkpointsBase
    val clock =
      if (retainFinalizedBody)
        Map().put(event1.event_id, event1)
      else
        Map()
    val chunksReady =
      if (retainFinalizedBody)
        Set(event1.event_id)
      else
        Set()
    {
      clock: clock,
      head_event_ids: computeHeadEventIDs(clock),
      chunks_ready: chunksReady,
      tentative_prolly_root_hash: 11,
      projection_basis: Set(),
      hlc_clock: { wall: 2, logical: 0, peer: self },
      peer_seen_frontier: PEERS.mapBy(peer =>
        if (peer == self) observedFrontier(clock) else Set()
      ),
      finalized_commit_id: tip1,
      finalized_prolly_root_hash: 11,
      finalized_events: derivedFinalizedEventMap(tip1, parents, checkpoints),
      finalized_commit_parents: parents,
      finalized_checkpoint_events: checkpoints,
      parked_conflicts: Set(),
      finalized_conflicts: Map(),
      resolved_finalized_conflict_ids: Set(),
      active_peers: Set(self),
      localWall: 2,
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

  pure def writeBaseResult(st: NodeState, p: PeerID, content: int): NodeState = {
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
    postWriteState
  }

  pure def writeResult(st: NodeState, p: PeerID, content: int): NodeState = {
    val postWriteState = writeBaseResult(st, p, content)
    val reconcile = computeTentativeProjection(postWriteState)
    {
      ...postWriteState,
      tentative_prolly_root_hash: reconcile._1,
      parked_conflicts: reconcile._2,
      projection_basis: reconcile._3,
    }
  }

  /// RECONCILE enable gate and pure local post-state helper.
  pure def applyTentativeProjection(st: NodeState, root: ProllyRootHash,
      parked: Set[EventID], basis: Set[EventID]): NodeState = {
    {
      ...st,
      tentative_prolly_root_hash: root,
      parked_conflicts: parked,
      projection_basis: basis,
    }
  }

  pure def reconcileResult(st: NodeState): NodeState = {
    val result = computeTentativeProjection(st)
    applyTentativeProjection(st, result._1, result._2, result._3)
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

  /// RECEIVE_EVENT pre-reconcile local ingestion step for one admissible event.
  pure def receiveBaseResult(st: NodeState, p: PeerID, e: Event): NodeState = {
    val newClock = st.clock.put(e.event_id, e)
    val newHLC = observe(st.hlc_clock, e.hlc, st.localWall, p)
    {
      ...st,
      clock: newClock,
      head_event_ids: computeHeadEventIDs(newClock),
      hlc_clock: newHLC,
      peer_seen_frontier: st.peer_seen_frontier.set(p, observedFrontier(newClock)),
    }
  }

  /// RECEIVE_EVENT pure local result for one admissible event.
  pure def receiveResult(st: NodeState, p: PeerID, e: Event): NodeState = {
    val tempState = receiveBaseResult(st, p, e)
    val reconcile = computeTentativeProjection(tempState)
    {
      ...tempState,
      tentative_prolly_root_hash: reconcile._1,
      parked_conflicts: reconcile._2,
      projection_basis: reconcile._3,
    }
  }

  /// GC_CLOCK pre-reconcile local result for pruning one retained finalized
  /// event body.
  pure def gcBaseResult(st: NodeState, p: PeerID, event_id: EventID): NodeState = {
    val newClock = dropEvent(st.clock, event_id)
    val newHeads = computeHeadEventIDs(newClock)
    val newChunksReady = st.chunks_ready.intersect(newClock.keys())
    {
      ...st,
      clock: newClock,
      head_event_ids: newHeads,
      chunks_ready: newChunksReady,
      peer_seen_frontier: st.peer_seen_frontier.set(p, observedFrontier(newClock)),
    }
  }

  /// GC_CLOCK pure local result for pruning one retained finalized event body.
  pure def gcResult(st: NodeState, p: PeerID, event_id: EventID): NodeState = {
    val gcState = gcBaseResult(st, p, event_id)
    val reconcile = computeTentativeProjection(gcState)
    {
      ...gcState,
      tentative_prolly_root_hash: reconcile._1,
      parked_conflicts: reconcile._2,
      projection_basis: reconcile._3,
    }
  }

  /// EVICT_PEER pure local result.
  pure def evictResult(st: NodeState, q: PeerID): NodeState = {
    val newActivePeers = st.active_peers.exclude(Set(q))
    {
      ...st,
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

  pure def sameFinalizedTipState(st_a: NodeState, st_b: NodeState): bool = all {
    st_a.finalized_commit_id == st_b.finalized_commit_id,
    st_a.finalized_prolly_root_hash == st_b.finalized_prolly_root_hash,
  }

  pure def sameFinalizedBookkeepingState(st_a: NodeState, st_b: NodeState): bool =
    st_a.finalized_events == st_b.finalized_events

  pure def sameFinalizedMetadataState(st_a: NodeState, st_b: NodeState): bool = all {
    st_a.finalized_checkpoint_events == st_b.finalized_checkpoint_events,
    st_a.finalized_commit_parents == st_b.finalized_commit_parents,
    st_a.finalized_conflicts == st_b.finalized_conflicts,
    st_a.resolved_finalized_conflict_ids == st_b.resolved_finalized_conflict_ids,
  }

  pure def sameFinalizedState(st_a: NodeState, st_b: NodeState): bool = all {
    sameFinalizedTipState(st_a, st_b),
    sameFinalizedBookkeepingState(st_a, st_b),
    sameFinalizedMetadataState(st_a, st_b),
  }

  /// Inputs that determine the non-provisional tentative projection.
  /// `finalized_events` participates because finalized retained bodies are
  /// excluded from tentative heads.
  pure def sameTentativeProjectionInputs(s1: NodeState, s2: NodeState): bool = all {
    s1.clock == s2.clock,
    s1.chunks_ready == s2.chunks_ready,
    s1.finalized_prolly_root_hash == s2.finalized_prolly_root_hash,
    s1.finalized_events == s2.finalized_events,
  }

  pure def sameFinalizedStateAcross(peerStatesA: PeerID -> NodeState,
      peerStatesB: PeerID -> NodeState): bool =
    PEERS.forall(p => sameFinalizedState(peerStatesA.get(p), peerStatesB.get(p)))

  pure def peersShareSameFinalizedState(peerStates: PeerID -> NodeState,
      peers: Set[PeerID]): bool =
    peers.forall(p => peers.forall(q =>
      sameFinalizedState(peerStates.get(p), peerStates.get(q))
    ))

  pure def allPeersSameFinalizedState(peerStates: PeerID -> NodeState): bool =
    peersShareSameFinalizedState(peerStates, PEERS)

  pure def connectedComponent(conn: Set[(PeerID, PeerID)], seed: PeerID): Set[PeerID] =
    range(0, PEERS.size()).foldl(Set(seed), (acc, _) =>
      acc.union(PEERS.filter(q =>
        acc.exists(p => p == q or isConnected(conn, p, q))
      ))
    )

  pure def mergedFinalizedParentsFor(peerStates: PeerID -> NodeState,
      peers: Set[PeerID]):
      FinalizedCommitID -> Set[FinalizedCommitID] =
    peers.fold(Map().put(EMPTY_FINALIZED_COMMIT_ID, Set()), (acc, p) =>
      mergeSetMap(acc, peerStates.get(p).finalized_commit_parents)
    )

  pure def mergedFinalizedParentsAcross(peerStates: PeerID -> NodeState):
      FinalizedCommitID -> Set[FinalizedCommitID] =
    mergedFinalizedParentsFor(peerStates, PEERS)

  pure def finalizedTipSetFor(peerStates: PeerID -> NodeState,
      peers: Set[PeerID]): Set[FinalizedCommitID] =
    peers.fold(Set(), (acc, p) =>
      acc.union(Set(peerStates.get(p).finalized_commit_id))
    )

  pure def finalizedTipSetAcross(peerStates: PeerID -> NodeState): Set[FinalizedCommitID] =
    finalizedTipSetFor(peerStates, PEERS)

  pure def finalizedFrontierFor(peerStates: PeerID -> NodeState,
      peers: Set[PeerID]): Set[FinalizedCommitID] =
    finalizedTipFrontier(
      finalizedTipSetFor(peerStates, peers),
      mergedFinalizedParentsFor(peerStates, peers)
    )

  pure def finalizedFrontierRankFor(peerStates: PeerID -> NodeState,
      peers: Set[PeerID]): int =
    finalizedFrontierFor(peerStates, peers).size() - 1

  pure def globalFinalizedFrontier(peerStates: PeerID -> NodeState): Set[FinalizedCommitID] =
    finalizedFrontierFor(peerStates, PEERS)

  pure def globalFinalizedFrontierRank(peerStates: PeerID -> NodeState): int =
    finalizedFrontierRankFor(peerStates, PEERS)

  def syncPeerIfEnabled(peerStates: PeerID -> NodeState,
      p: PeerID, q: PeerID): PeerID -> NodeState = {
    val st_p = peerStates.get(p)
    val st_q = peerStates.get(q)
    if (syncEnabled(allPairs(PEERS), p, q, st_p, st_q))
      peerStates.set(p, applySyncFinalized(st_p, st_q))
    else
      peerStates
  }

  /// Deterministic fairness approximation for finalized healing in a fully
  /// connected 3-peer swarm: every ordered pair gets one sync chance.
  def healRoundFullMesh(peerStates: PeerID -> NodeState): PeerID -> NodeState = {
    val after12 = syncPeerIfEnabled(peerStates, 1, 2)
    val after21 = syncPeerIfEnabled(after12, 2, 1)
    val after13 = syncPeerIfEnabled(after21, 1, 3)
    val after31 = syncPeerIfEnabled(after13, 3, 1)
    val after23 = syncPeerIfEnabled(after31, 2, 3)
    syncPeerIfEnabled(after23, 3, 2)
  }

  pure val FAIR_HEAL_SCHEDULE_REVERSE: List[(PeerID, PeerID)] =
    [(3, 2), (2, 3), (3, 1), (1, 3), (2, 1), (1, 2)]

  pure val FAIR_HEAL_SCHEDULE_ROTATED: List[(PeerID, PeerID)] =
    [(2, 3), (3, 2), (1, 3), (3, 1), (1, 2), (2, 1)]

  /// Alternate fair schedules used by bounded convergence regressions.
  def healRoundScheduled(peerStates: PeerID -> NodeState,
      schedule: List[(PeerID, PeerID)]): PeerID -> NodeState =
    schedule.foldl(peerStates, (acc, pair) =>
      syncPeerIfEnabled(acc, pair._1, pair._2)
    )

  pure val FULL_MESH_HEAL_ROUNDS: int = 3

  def healRoundsFullMesh(peerStates: PeerID -> NodeState, rounds: int):
      PeerID -> NodeState =
    range(0, rounds).foldl(peerStates, (acc, _) => healRoundFullMesh(acc))

  def healRoundsScheduled(peerStates: PeerID -> NodeState,
      schedule: List[(PeerID, PeerID)], rounds: int): PeerID -> NodeState =
    range(0, rounds).foldl(peerStates, (acc, _) => healRoundScheduled(acc, schedule))

  /// Finalized-state update for SYNC_FINALIZED (without tentative_prolly_root_hash re-anchoring).
  pure def syncFinalizedCoreResult(st_local: NodeState, st_remote: NodeState): NodeState = {
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
      // One binary finalized frontier-reduction step. This is deterministic for
      // the current pair of incomparable tips, but nested partition recovery is
      // not modeled as one-shot associativity over >2 tips. Later sync rounds
      // may merge these merge commits again without rewriting history.
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

  def syncFinalizedCore(st_local: NodeState, st_remote: NodeState): NodeState =
    syncFinalizedCoreResult(st_local, st_remote)

  /// Full SYNC_FINALIZED local result, including retained-local closure
  /// hydration from the remote clock, retained-finalized compaction,
  /// and tentative_prolly_root_hash re-anchoring.
  pure def applySyncFinalizedResult(st_local: NodeState, st_remote: NodeState): NodeState = {
    val syncedState = syncFinalizedCoreResult(st_local, st_remote)
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
      // NOOP: tip unchanged, but equal-tip metadata repair can still expose
      // retained finalized event bodies that must be hydrated/marked ready
      // before local compaction + tentative projection recomputation.
      val noopHydratedClosure =
        if (bookkeepingChanged)
          pullClosureEventIDs(
            cleaned.clock,
            st_remote.clock,
            retainedFinalizedEventIDs(cleaned),
            Set()
          )
        else
          Set()
      val noopHydratedClock = applyRemoteEvents(
        cleaned.clock,
        st_remote.clock,
        noopHydratedClosure
      )
      val noopRetainedFinalizedReady = noopHydratedClock.keys().intersect(cleaned.finalized_events.keys())
      val withNoopRetainedFinalizedReady: NodeState = {
        ...cleaned,
        clock: noopHydratedClock,
        head_event_ids: computeHeadEventIDs(noopHydratedClock),
        chunks_ready: cleaned.chunks_ready.union(noopRetainedFinalizedReady),
      }
      val noopCompactedState =
        if (bookkeepingChanged) compactRetainedFinalized(withNoopRetainedFinalizedReady)
        else withNoopRetainedFinalizedReady
      val noopReconcile = computeTentativeProjection(noopCompactedState)
      {
        ...noopCompactedState,
        tentative_prolly_root_hash: noopReconcile._1,
        parked_conflicts: noopReconcile._2,
        projection_basis: noopReconcile._3,
      }
    }
  }

  def applySyncFinalized(st_local: NodeState, st_remote: NodeState): NodeState =
    applySyncFinalizedResult(st_local, st_remote)

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
      val post = receiveResult(st, p, e)
      all {
        nodes' = nodes.set(p, post),
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
  /// nothing is disseminated. Every peer with the same tentative-projection
  /// inputs independently computes the same merged root whenever at least one
  /// tentative head is mergeable. The next LOCAL_WRITE will have
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
      reconcileEnabled(st),
      val post = reconcileResult(st)
      all {
        nodes' = nodes.set(p, post),
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
  /// merges only that event against its reconstructed replay base (recorded
  /// anchor for root events, deterministic parent-basis fold otherwise),
  /// preserving a canonical finalized checkpoint sequence independent of
  /// scheduler timing.
  ///
  /// In the executable model this standalone action is a scheduling abstraction
  /// for all candidate-enabling transitions (receive, chunk-ready, heartbeat,
  /// sync, conflict resolution, GC, membership change).
  action do_finalize(p: PeerID): bool = {
    val st = nodes.get(p)
    val candidates = finalizationCandidates(st)
    all {
      finalizeEnabled(st),
      val postState = finalizeResult(st)
      all {
        nodes' = nodes.set(p, {
          ...postState,
        }),
        inbox' = inbox,
        connected' = connected,
      }
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
      val post = gcResult(st, p, event_id)
      nodes' = nodes.set(p, {
        ...post,
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
      val post = evictResult(st, q)
      all {
        nodes' = nodes.set(p, post),
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
      val syncedState = applySyncFinalizedResult(st_p, st_q)
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

}
