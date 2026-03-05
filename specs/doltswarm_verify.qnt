// -*- mode: Bluespec; -*-

/**
 * DoltSwarm Merkle clock protocol — flat, set-based verification module.
 *
 * Self-contained module for Apalache model checking. No Choreo framework,
 * no List (Seq) types — everything is sets and maps.
 *
 * Models the core protocol: Merkle clock events, MergeRoots-based reconcile,
 * heartbeats for stability, causal finalization,
 * merge conflicts, tentative + finalized conflict resolution, network partitions,
 * and partition recovery via accept-the-fork finalized merge (§7.10).
 *
 * NOTE: The protocol uses frontier digests (heads_digest = hash(sort(heads)))
 * in heartbeats for bandwidth efficiency (§7.6). The spec reads full remote
 * state directly (skipping message-level digest compare mechanics), but models
 * the same closure-based pull target: remote heads plus recursive ancestors.
 * Partition conflicts are handled inline by SYNC_FINALIZED's divergence
 * ours-side root (no blocking), while conflict records are tracked in
 * finalized_conflicts until resolved locally or cleared by resolution refs
 * carried in received events.
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
  pure val CONTENTS: Set[int] = Set(100, 200)
  pure val MAX_WALL: int = 6
  pure val MAX_EVENTS: int = 8
  pure val EMPTY_ROOT: int = 0

  /// Sentinel hash indicating a merge conflict.
  /// mergeRoots returns this when two truly-divergent writes conflict.
  pure val CONFLICT_HASH: int = -1

  // ===========================================================================
  // Types
  // ===========================================================================

  type Hash = int
  /// EventCID is a (peer, wall, logical) tuple — unique event identifier.
  type EventCID = (int, int, int)

  /// Deterministic identity for a finalized-layer conflict from SYNC_FINALIZED.
  /// Canonical tuple: (ours_root, theirs_root, common_root).
  type FinalizedConflictID = (Hash, Hash, Hash)

  type Event = {
    cid: EventCID,
    root_hash: Hash,
    parents: Set[EventCID],
    resolved_finalized_conflicts: Set[FinalizedConflictID],
    hlc: HLC,
    peer: PeerID,
  }

  type FinalizedConflict = {
    id: FinalizedConflictID,
    ours_root: Hash,
    theirs_root: Hash,
    common_root: Hash,
  }

  type NodeState = {
    clock: EventCID -> Event,
    heads: Set[EventCID],
    latest_root: Hash,
    hlc_clock: HLC,
    peer_hlc: PeerID -> HLC,
    stable_hlc: HLC,
    finalized_root: Hash,
    finalized_events: Set[EventCID],
    finalized_parents: Hash -> Set[Hash],
    finalized_ancestors: Hash -> Set[Hash],
    finalized_depth: Hash -> int,
    parked_conflicts: Set[EventCID],
    finalized_conflicts: FinalizedConflictID -> FinalizedConflict,
    active_peers: Set[PeerID],
    localWall: int,
  }

  // ===========================================================================
  // State variables
  // ===========================================================================

  var nodes: PeerID -> NodeState
  var inbox: PeerID -> Set[Event]
  /// History variable: tracks previous stable_hlc per peer for monotonicity check.
  var prev_stable_hlc: PeerID -> HLC
  /// History variable: tracks previous active_peers per peer for monotonicity guard.
  var prev_active_peers: PeerID -> Set[PeerID]
  /// Network connectivity: set of directed (a, b) pairs meaning a can send to b.
  /// Symmetric: (a, b) ∈ connected ⟺ (b, a) ∈ connected.
  /// Models network partitions: removing pairs isolates peers.
  var connected: Set[(PeerID, PeerID)]

  // ===========================================================================
  // Helpers
  // ===========================================================================

  pure def makeEventCID(peer: PeerID, wall: int, logical: int): EventCID =
    (peer, wall, logical)

  /// All directed pairs between peers (symmetric).
  pure def allPairs(peers: Set[PeerID]): Set[(PeerID, PeerID)] =
    peers.fold(Set(), (acc, p) =>
      acc.union(peers.filter(q => p != q).map(q => (p, q)))
    )

  /// Check if two peers are connected (symmetric).
  pure def isConnected(conn: Set[(PeerID, PeerID)], a: PeerID, b: PeerID): bool =
    conn.contains((a, b))

  /// Broadcast to connected peers only. Events from peer e.peer are delivered
  /// only to peers that e.peer can reach via the connectivity relation.
  pure def broadcastConnected(ib: PeerID -> Set[Event], e: Event,
      conn: Set[(PeerID, PeerID)]): PeerID -> Set[Event] =
    ib.keys().mapBy(p =>
      if (p == e.peer or isConnected(conn, e.peer, p))
        ib.get(p).union(Set(e))
      else
        ib.get(p)
    )

  /// Deterministic merge model with conflict capability.
  ///
  /// Returns CONFLICT_HASH when two divergent writes conflict (both differ
  /// from ancestor and from each other, AND both are "raw content" values).
  /// This models the fact that Dolt's MergeRoots can return a conflict when
  /// concurrent edits touch the same rows — regardless of the ancestor.
  ///
  /// The conflict condition is: both ours and theirs are raw content values
  /// (from CONTENTS) that each differ from the ancestor. This fires with
  /// any ancestor, including post-finalization merged roots, so that
  /// partition conflicts and post-finalization tentative conflicts are
  /// exercised realistically.
  ///
  /// The merge ancestor for all reconciliation merges is finalized_root,
  /// matching the protocol (§2.4 RECONCILE step 3a).
  pure def mergeRoots(ours: Hash, theirs: Hash, ancestor: Hash): Hash = {
    if (ours == theirs) ours
    else if (ours == ancestor) theirs
    else if (theirs == ancestor) ours
    // Both diverge from ancestor: conflict when both are raw content values
    // (models concurrent writes to the same table producing a conflict).
    // Fires with any ancestor — not just EMPTY_ROOT — so that conflicts
    // are exercised in post-finalization and partition-recovery scenarios.
    else if (CONTENTS.contains(ours) and CONTENTS.contains(theirs))
      CONFLICT_HASH
    // Hash combination that cannot collide with sentinels:
    // Result is always >= 1000, well above CONTENTS (100, 200),
    // EMPTY_ROOT (0), and CONFLICT_HASH (-1).
    else 1000 + ours * 31 + theirs * 17 + ancestor * 7
  }

  /// Compute heads from the clock structure (order-independent).
  /// Heads = events whose CID is not a parent of any other event.
  pure def computeHeads(clock: EventCID -> Event): Set[EventCID] = {
    val allParents = clock.keys().fold(Set(), (acc, cid) =>
      acc.union(clock.get(cid).parents)
    )
    clock.keys().exclude(allParents)
  }

  /// Ancestor-ID closure from a seed set by repeatedly following parent links.
  ///
  /// Includes missing parent IDs even if those CIDs are not yet present in
  /// the local clock, so subset checks can detect orphan events.
  /// The unfold is bounded by MAX_EVENTS for a finite-state model.
  pure def ancestorIdClosure(seed: Set[EventCID], clock: EventCID -> Event): Set[EventCID] =
    range(0, MAX_EVENTS).foldl(seed, (acc, _) => {
      val parentStep = acc.fold(Set(), (parentsAcc, cid) => {
        if (clock.has(cid)) parentsAcc.union(clock.get(cid).parents)
        else parentsAcc
      })
      acc.union(parentStep)
    })

  /// Parent-closure gate: an event is mergeable only if all reachable ancestor
  /// CIDs are present in the local clock.
  pure def isParentClosed(clock: EventCID -> Event, cid: EventCID): bool =
    clock.has(cid) and ancestorIdClosure(Set(cid), clock).subseteq(clock.keys())

  /// Heads eligible for reconciliation (parent-closed in the local clock).
  /// Chunk readiness is not modeled in Quint, so parent-closure is the explicit
  /// mergeability gate.
  pure def mergeableHeads(st: NodeState): Set[EventCID] =
    st.heads.filter(cid => isParentClosed(st.clock, cid))

  /// Pull target on heartbeat mismatch: remote heads plus recursive ancestor
  /// closure, excluding CIDs already present locally.
  pure def pullClosureCIDs(localClock: EventCID -> Event, remote: NodeState): Set[EventCID] = {
    val remoteClosure = ancestorIdClosure(remote.heads, remote.clock)
    remoteClosure.filter(cid =>
      remote.clock.has(cid) and
      not(localClock.has(cid))
    )
  }

  /// Apply a set of remote event CIDs to a local clock map.
  pure def applyRemoteEvents(localClock: EventCID -> Event, remoteClock: EventCID -> Event,
      cids: Set[EventCID]): EventCID -> Event =
    cids.fold(localClock, (acc, cid) =>
      if (remoteClock.has(cid)) acc.put(cid, remoteClock.get(cid))
      else acc
    )

  /// Bounded anti-entropy catchup projection: one heartbeat pull plus enough
  /// receive steps to materialize all pulled events into local clock state.
  pure def heartbeatClosureCatchupClock(st_local: NodeState, st_remote: NodeState): EventCID -> Event = {
    val pulled = pullClosureCIDs(st_local.clock, st_remote)
    applyRemoteEvents(st_local.clock, st_remote.clock, pulled)
  }

  /// Sort CIDs by HLC (deterministic total order for multi-head merge).
  pure def sortCIDsByHLC(cids: Set[EventCID], clock: EventCID -> Event): List[EventCID] = {
    val cidList = cids.fold([], (acc, c) => acc.append(c))
    cidList.sortList((a, b) => less(clock.get(a).hlc, clock.get(b).hlc))
  }

  /// Deterministic merged root for mergeable heads.
  /// Operates on parent-closed heads only (mergeableHeads).
  /// Sorts mergeable heads by HLC and folds mergeRoots left-to-right.
  /// Returns (merged_root, parked_set) where parked_set is the COMPLETE set
  /// of parked events (not a delta). This makes parking a pure function of
  /// (heads, clock, finalized_root), independent of event receipt order.
  pure def computeMergedRoot(st: NodeState): (Hash, Set[EventCID]) = {
    val readyHeads = mergeableHeads(st)
    if (st.heads.empty()) {
      (st.finalized_root, Set())
    } else if (readyHeads.empty()) {
      (st.latest_root, Set())
    } else if (readyHeads.size() == 1) {
      val root = readyHeads.fold(st.finalized_root, (_, cid) => st.clock.get(cid).root_hash)
      (root, Set())
    } else {
      val sorted = sortCIDsByHLC(readyHeads, st.clock)
      // Fold left, accumulating both the merged root and conflict set
      sorted.foldl((st.finalized_root, Set()), (acc, cid) => {
        val currentRoot = acc._1
        val conflicts = acc._2
        val merged = mergeRoots(currentRoot, st.clock.get(cid).root_hash, st.finalized_root)
        if (merged == CONFLICT_HASH) {
          // Conflict: don't update root, record the conflicting head
          (currentRoot, conflicts.union(Set(cid)))
        } else {
          (merged, conflicts)
        }
      })
    }
  }

  /// Eligible events for finalization at the current stability boundary.
  pure def finalizationCandidates(st: NodeState): Set[EventCID] =
    st.clock.keys().filter(cid =>
      val e = st.clock.get(cid)
      less(e.hlc, st.stable_hlc) and
      not(st.finalized_events.contains(cid)) and
      not(st.parked_conflicts.contains(cid)) and
      isParentClosed(st.clock, cid)
    )

  /// Stable frontier heads: candidates that have no child in the same
  /// candidate set. Finalization merges only these frontier roots.
  pure def stableFrontier(candidates: Set[EventCID], clock: EventCID -> Event): Set[EventCID] =
    candidates.filter(cid =>
      not(candidates.exists(child => clock.get(child).parents.contains(cid)))
    )

  /// Advance finalized state in one deterministic batch:
  /// 1) snapshot anchor = finalized_root at batch start
  /// 2) merge stable frontier heads in HLC order with fixed ancestor = anchor
  /// 3) mark all candidates finalized for bookkeeping
  pure def finalizeBatch(st: NodeState): (Hash, Set[EventCID]) = {
    val candidates = finalizationCandidates(st)
    if (candidates.empty()) {
      (st.finalized_root, st.finalized_events)
    } else {
      val frontier = stableFrontier(candidates, st.clock)
      val sortedFrontier = sortCIDsByHLC(frontier, st.clock)
      val anchor = st.finalized_root
      val newRoot = sortedFrontier.foldl(st.finalized_root, (acc, cid) => {
        val merged = mergeRoots(acc, st.clock.get(cid).root_hash, anchor)
        // Skip frontier heads that still conflict at finalized layer (§7.1.1).
        if (merged == CONFLICT_HASH) acc else merged
      })
      (newRoot, st.finalized_events.union(candidates))
    }
  }

  /// Merge two maps Hash -> Set[Hash] by key union and value union.
  pure def mergeSetMap(m1: Hash -> Set[Hash], m2: Hash -> Set[Hash]): Hash -> Set[Hash] = {
    m1.keys().union(m2.keys()).mapBy(k => {
      val s1 = if (m1.has(k)) m1.get(k) else Set()
      val s2 = if (m2.has(k)) m2.get(k) else Set()
      s1.union(s2)
    })
  }

  /// Merge two maps Hash -> int by key union and max value.
  pure def mergeDepthMap(m1: Hash -> int, m2: Hash -> int): Hash -> int = {
    m1.keys().union(m2.keys()).mapBy(k => {
      val d1 = if (m1.has(k)) m1.get(k) else 0
      val d2 = if (m2.has(k)) m2.get(k) else 0
      if (d1 > d2) d1 else d2
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

  /// Add/update a finalized checkpoint root with given parent roots.
  ///
  /// Uses root-level checkpoint DAG metadata:
  /// - if child already exists, parents are unioned
  /// - ancestors/depth are recomputed from merged parents
  pure def lineageAddCheckpoint(parentsMap: Hash -> Set[Hash],
      ancestorsMap: Hash -> Set[Hash], depthMap: Hash -> int, child: Hash,
      parentRoots: Set[Hash]): ((Hash -> Set[Hash]), (Hash -> Set[Hash]), (Hash -> int)) = {
    val sanitizedParents = parentRoots.exclude(Set(child))
    val priorParents = if (parentsMap.has(child)) parentsMap.get(child) else Set()
    val allParents = priorParents.union(sanitizedParents)
    val parentAncestors = allParents.fold(Set(), (acc, p) => {
      val anc = if (ancestorsMap.has(p)) ancestorsMap.get(p) else Set(p, EMPTY_ROOT)
      acc.union(anc)
    })
    val allAncestors = Set(child).union(parentAncestors)
    val maxParentDepth = allParents.fold(0, (acc, p) => {
      val d = if (depthMap.has(p)) depthMap.get(p) else if (p == EMPTY_ROOT) 0 else 1
      if (d > acc) d else acc
    })
    val childDepth = if (child == EMPTY_ROOT) 0 else maxParentDepth + 1
    (
      parentsMap.put(child, allParents),
      ancestorsMap.put(child, allAncestors),
      depthMap.put(child, childDepth)
    )
  }

  /// Lowest common finalized-root ancestor using finalized checkpoint metadata.
  /// Picks the common ancestor with highest depth; ties break by root value.
  pure def lineageLcaRoot(rootA: Hash, rootB: Hash, ancestorsMap: Hash -> Set[Hash],
      depthMap: Hash -> int): Hash = {
    val ancA = if (ancestorsMap.has(rootA)) ancestorsMap.get(rootA) else Set(rootA, EMPTY_ROOT)
    val ancB = if (ancestorsMap.has(rootB)) ancestorsMap.get(rootB) else Set(rootB, EMPTY_ROOT)
    val common = ancA.intersect(ancB)
    if (common.empty()) EMPTY_ROOT
    else common.fold(EMPTY_ROOT, (best, candidate) => {
      val bestDepth = if (depthMap.has(best)) depthMap.get(best) else 0
      val candidateDepth = if (depthMap.has(candidate)) depthMap.get(candidate) else 0
      if (candidateDepth > bestDepth) candidate
      else if (candidateDepth < bestDepth) best
      else if (candidate < best) candidate
      else best
    })
  }

  pure def depthAt(depthMap: Hash -> int, root: Hash): int =
    if (depthMap.has(root)) depthMap.get(root) else 0

  pure def ancestorsAt(ancestorsMap: Hash -> Set[Hash], root: Hash): Set[Hash] =
    if (ancestorsMap.has(root)) ancestorsMap.get(root) else Set(root, EMPTY_ROOT)

  pure def finalizedConflictID(oursRoot: Hash, theirsRoot: Hash,
      commonRoot: Hash): FinalizedConflictID =
    (oursRoot, theirsRoot, commonRoot)

  /// Remove one key from a map via key reconstruction (portable Quint idiom).
  pure def dropFinalizedConflict(conflicts: FinalizedConflictID -> FinalizedConflict,
      conflictID: FinalizedConflictID): FinalizedConflictID -> FinalizedConflict =
    conflicts.keys().exclude(Set(conflictID)).mapBy(k => conflicts.get(k))

  /// Remove multiple finalized conflict ids from a map.
  pure def dropFinalizedConflicts(conflicts: FinalizedConflictID -> FinalizedConflict,
      conflictIDs: Set[FinalizedConflictID]): FinalizedConflictID -> FinalizedConflict =
    conflicts.keys().exclude(conflictIDs).mapBy(k => conflicts.get(k))

  /// Collect all finalized-conflict resolution references observed in a clock.
  pure def resolvedFinalizedConflictRefs(clock: EventCID -> Event): Set[FinalizedConflictID] =
    clock.keys().fold(Set(), (acc, cid) =>
      acc.union(clock.get(cid).resolved_finalized_conflicts)
    )

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
    val newCID = makeEventCID(p, newHLC.wall, newHLC.logical)
    val activeHeads = st.heads.exclude(st.parked_conflicts)
    val event: Event = {
      cid: newCID,
      root_hash: resolvedContent,
      parents: activeHeads,
      resolved_finalized_conflicts: Set(conflictID),
      hlc: newHLC,
      peer: p,
    }
    val newClock = st.clock.put(newCID, event)
    val newHeads = computeHeads(newClock)
    val postState: NodeState = {
      ...st,
      clock: newClock,
      heads: newHeads,
      hlc_clock: newHLC,
      finalized_conflicts: dropFinalizedConflict(st.finalized_conflicts, conflictID),
    }
    val reconcileResult = computeMergedRoot(postState)
    {
      ...postState,
      latest_root: reconcileResult._1,
      parked_conflicts: reconcileResult._2,
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
    val localSubsetRemote = st_local.finalized_events.subseteq(st_remote.finalized_events)
    val remoteSubsetLocal = st_remote.finalized_events.subseteq(st_local.finalized_events)
    if (localSubsetRemote and not(remoteSubsetLocal)) SYNC_ADOPT
    else if (remoteSubsetLocal and not(localSubsetRemote)) SYNC_NOOP
    else SYNC_MERGE
  }

  /// Metadata-completeness gate used by SYNC_FINALIZED.
  /// If local is behind and must adopt, it must already have remote finalized
  /// events in its local clock metadata.
  pure def syncAdoptionReady(st_local: NodeState, st_remote: NodeState): bool = {
    if (syncMode(st_local, st_remote) == SYNC_ADOPT)
      st_remote.finalized_events.subseteq(st_local.clock.keys())
    else
      true
  }

  /// Lineage metadata readiness for SYNC_FINALIZED: after exchanging finalized
  /// checkpoint metadata, both local and remote finalized roots must be present
  /// in the merged lineage view so common-ancestor lookup is well-defined.
  pure def syncLineageReady(st_local: NodeState, st_remote: NodeState): bool = {
    val mergedAncestors = mergeSetMap(
      st_local.finalized_ancestors,
      st_remote.finalized_ancestors
    )
    mergedAncestors.has(st_local.finalized_root) and
      mergedAncestors.has(st_remote.finalized_root)
  }

  /// Enable condition for SYNC_FINALIZED transition.
  pure def syncEnabled(conn: Set[(PeerID, PeerID)], p: PeerID, q: PeerID,
      st_local: NodeState, st_remote: NodeState): bool = all {
    p != q,
    isConnected(conn, p, q),
    st_local.finalized_root != st_remote.finalized_root,
    st_local.finalized_root != EMPTY_ROOT,
    st_remote.finalized_root != EMPTY_ROOT,
    syncAdoptionReady(st_local, st_remote),
    syncLineageReady(st_local, st_remote),
  }

  /// Finalized-state update for SYNC_FINALIZED (without latest_root re-anchoring).
  pure def syncFinalizedCore(st_local: NodeState, st_remote: NodeState): NodeState = {
    val mode = syncMode(st_local, st_remote)
    val knownResolvedRefs = resolvedFinalizedConflictRefs(st_local.clock)
      .union(resolvedFinalizedConflictRefs(st_remote.clock))
    // SYNC_FINALIZED exchanges finalized checkpoint metadata. The spec models
    // this by taking the union of local+remote lineage maps.
    val mergedParents = mergeSetMap(st_local.finalized_parents, st_remote.finalized_parents)
    val mergedAncestors = mergeSetMap(st_local.finalized_ancestors, st_remote.finalized_ancestors)
    val mergedDepth = mergeDepthMap(st_local.finalized_depth, st_remote.finalized_depth)
    val withMergedLineage: NodeState = {
      ...st_local,
      finalized_parents: mergedParents,
      finalized_ancestors: mergedAncestors,
      finalized_depth: mergedDepth,
    }
    if (mode == SYNC_ADOPT) {
      {
        ...withMergedLineage,
        finalized_root: st_remote.finalized_root,
        finalized_events: st_remote.finalized_events,
        finalized_conflicts: dropFinalizedConflicts(
          st_remote.finalized_conflicts,
          knownResolvedRefs
        ),
      }
    } else if (mode == SYNC_NOOP) {
      // Ahead path: finalized state no-op, but metadata exchange is retained.
      withMergedLineage
    } else {
      val commonRoot = lineageLcaRoot(
        st_local.finalized_root,
        st_remote.finalized_root,
        mergedAncestors,
        mergedDepth
      )
      // Deterministic ours/theirs: compare finalized roots by value.
      val oursRoot = if (st_local.finalized_root < st_remote.finalized_root)
        st_local.finalized_root else st_remote.finalized_root
      val theirsRoot = if (st_local.finalized_root < st_remote.finalized_root)
        st_remote.finalized_root else st_local.finalized_root
      val conflictID = finalizedConflictID(oursRoot, theirsRoot, commonRoot)
      val conflictInfo: FinalizedConflict = {
        id: conflictID,
        ours_root: oursRoot,
        theirs_root: theirsRoot,
        common_root: commonRoot,
      }
      val merged = mergeRoots(oursRoot, theirsRoot, commonRoot)
      val mergedEvents = st_local.finalized_events.union(st_remote.finalized_events)
      val baseFinalizedConflicts = mergeFinalizedConflictMap(
        st_local.finalized_conflicts,
        st_remote.finalized_conflicts
      )
      val unresolvedFinalizedConflicts = dropFinalizedConflicts(
        baseFinalizedConflicts,
        knownResolvedRefs
      )
      // Unified path: on conflict, use ours root (deterministic, no blocking),
      // and persist a finalized_conflicts entry for explicit resolution
      // unless already resolved by an observed resolution-reference event.
      val effectiveRoot = if (merged == CONFLICT_HASH) oursRoot else merged
      val mergedFinalizedConflicts = if (
        merged == CONFLICT_HASH and not(knownResolvedRefs.contains(conflictID))
      )
        unresolvedFinalizedConflicts.put(conflictID, conflictInfo)
      else
        unresolvedFinalizedConflicts
      val mergedState: NodeState = {
        ...withMergedLineage,
        finalized_root: effectiveRoot,
        finalized_events: mergedEvents,
        finalized_conflicts: mergedFinalizedConflicts,
      }
      val lineage = lineageAddCheckpoint(
        mergedState.finalized_parents,
        mergedState.finalized_ancestors,
        mergedState.finalized_depth,
        effectiveRoot,
        Set(st_local.finalized_root, st_remote.finalized_root)
      )
      {
        ...mergedState,
        finalized_parents: lineage._1,
        finalized_ancestors: lineage._2,
        finalized_depth: lineage._3,
      }
    }
  }

  /// Full SYNC_FINALIZED local result, including latest_root re-anchoring.
  pure def applySyncFinalized(st_local: NodeState, st_remote: NodeState): NodeState = {
    val syncedState = syncFinalizedCore(st_local, st_remote)
    val reconcileResult = computeMergedRoot(syncedState)
    {
      ...syncedState,
      latest_root: reconcileResult._1,
      parked_conflicts: reconcileResult._2,
    }
  }

  // ===========================================================================
  // Actions
  // ===========================================================================

  action init = all {
    nodes' = PEERS.mapBy(p => {
      clock: Map(),
      heads: Set(),
      latest_root: EMPTY_ROOT,
      hlc_clock: { wall: 1, logical: 0, peer: p },
      peer_hlc: PEERS.mapBy(q => { wall: 0, logical: 0, peer: q }),
      stable_hlc: ZERO,
      finalized_root: EMPTY_ROOT,
      finalized_events: Set(),
      finalized_parents: Map().put(EMPTY_ROOT, Set()),
      finalized_ancestors: Map().put(EMPTY_ROOT, Set(EMPTY_ROOT)),
      finalized_depth: Map().put(EMPTY_ROOT, 0),
      parked_conflicts: Set(),
      finalized_conflicts: Map(),
      active_peers: PEERS,
      localWall: 1,
    }),
    inbox' = PEERS.mapBy(_ => Set()),
    prev_stable_hlc' = PEERS.mapBy(_ => ZERO),
    prev_active_peers' = PEERS.mapBy(_ => PEERS),
    connected' = allPairs(PEERS),
  }

  /// A peer creates a local write event.
  /// Parents are active_heads (excluding parked). After writing, parked_conflicts
  /// are recomputed from the new head set to maintain the invariant that parking
  /// is a pure function of (heads, clock, finalized_root).
  action do_write(p: PeerID, content: int): bool = {
    val st = nodes.get(p)
    all {
      st.localWall <= MAX_WALL,
      st.clock.keys().size() < MAX_EVENTS,
      val newHLC = tick(st.hlc_clock, st.localWall, p)
      val newCID = makeEventCID(p, newHLC.wall, newHLC.logical)
      val activeHeads = st.heads.exclude(st.parked_conflicts)
      val event: Event = {
        cid: newCID,
        root_hash: content,
        parents: activeHeads,
        resolved_finalized_conflicts: Set(),
        hlc: newHLC,
        peer: p,
      }
      val newClock = st.clock.put(newCID, event)
      val newHeads = computeHeads(newClock)
      // Recompute parking for the new head set
      val postWriteState: NodeState = { ...st, clock: newClock, heads: newHeads, hlc_clock: newHLC }
      val reconcileResult = computeMergedRoot(postWriteState)
      val newLatestRoot = reconcileResult._1
      val newParkedConflicts = reconcileResult._2
      all {
        nodes' = nodes.set(p, {
          ...st,
          clock: newClock,
          heads: newHeads,
          latest_root: newLatestRoot,
          hlc_clock: newHLC,
          parked_conflicts: newParkedConflicts,
        }),
        inbox' = broadcastConnected(inbox, event, connected),
        prev_stable_hlc' = PEERS.mapBy(pp => nodes.get(pp).stable_hlc),
        prev_active_peers' = PEERS.mapBy(pp => nodes.get(pp).active_peers),

        connected' = connected,
      }
    }
  }

  /// A peer processes a single event from its inbox.
  /// Events are processed one at a time (not batched), matching the protocol's
  /// per-event processing with inline reconciliation between events.
  /// Reconciliation is inlined here (matching §2.4 RECEIVE_EVENT):
  /// merge/parking is computed from parent-closed heads only. Orphan heads
  /// stay announced until ancestors arrive via pull-on-demand.
  /// No local wall-time admission filter is modeled or permitted: any valid,
  /// non-duplicate received event is accepted into the clock (G-Set behavior).
  action do_receive(p: PeerID): bool = {
    val st = nodes.get(p)
    val processable = inbox.get(p).filter(e =>
      not(st.clock.keys().contains(e.cid))
    )

    all {
      not(processable.empty()),
      nondet e = processable.oneOf()
      // Add the single event to the clock
      val newClock = st.clock.put(e.cid, e)
      // Merge HLC with the received timestamp
      val newHLC = observe(st.hlc_clock, e.hlc, st.localWall, p)
      // Recompute heads from the full clock (order-independent)
      val newHeads = computeHeads(newClock)
      // Inline reconciliation: computeMergedRoot evaluates only parent-closed
      // heads and returns the complete parked set, making parking a pure
      // function of (heads, clock, finalized_root).
      val tempState: NodeState = {
        ...st,
        clock: newClock,
        heads: newHeads,
        finalized_conflicts: dropFinalizedConflicts(
          st.finalized_conflicts,
          e.resolved_finalized_conflicts
        ),
        hlc_clock: newHLC,
      }
      val reconcileResult = computeMergedRoot(tempState)
      val newLatestRoot = reconcileResult._1
      val newParkedConflicts = reconcileResult._2
      all {
        nodes' = nodes.set(p, {
          ...tempState,
          latest_root: newLatestRoot,
          parked_conflicts: newParkedConflicts,
        }),
        inbox' = inbox,
        prev_stable_hlc' = PEERS.mapBy(pp => nodes.get(pp).stable_hlc),
        prev_active_peers' = PEERS.mapBy(pp => nodes.get(pp).active_peers),

        connected' = connected,
      }
    }
  }

  /// A peer reconciles when it has multiple heads.
  /// Merges are deterministic local computations — no event is created,
  /// nothing is broadcast. Every peer with the same event set independently
  /// computes the same merged root. The next LOCAL_WRITE will have
  /// parents = active_heads (excluding parked), which collapses them in the DAG.
  ///
  /// computeMergedRoot operates on parent-closed heads only and returns the
  /// complete parked set (not a delta), making parking a pure function of
  /// (heads, clock, finalized_root). No peer is blocked from writing.
  action do_reconcile(p: PeerID): bool = {
    val st = nodes.get(p)
    all {
      st.heads.size() > 1,
      val result = computeMergedRoot(st)
      val merged = result._1
      val parked = result._2
      all {
        nodes' = nodes.set(p, {
          ...st,
          latest_root: merged,
          parked_conflicts: parked,
        }),
        inbox' = inbox,
        prev_stable_hlc' = PEERS.mapBy(pp => nodes.get(pp).stable_hlc),
        prev_active_peers' = PEERS.mapBy(pp => nodes.get(pp).active_peers),

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
      prev_stable_hlc' = PEERS.mapBy(pp => nodes.get(pp).stable_hlc),
      prev_active_peers' = PEERS.mapBy(pp => nodes.get(pp).active_peers),
      connected' = connected,
    }
  }

  /// Heartbeat: peer p observes peer q's HLC.
  /// Requires connectivity (heartbeats don't flow across partitions).
  /// Also updates p's own peer_hlc entry to its current HLC clock,
  /// ensuring that stable_hlc can advance (a peer always knows its own time).
  ///
  /// NOTE: The protocol uses frontier digests (heads_digest) for bandwidth
  /// efficiency — peers compare digests and only exchange full heads on
  /// mismatch (§7.6). The spec reads full remote state directly as a sound
  /// over-approximation, skipping the digest-compare step.
  ///
  /// Pull-on-demand (§2.4 HEARTBEAT step 5): on mismatch, p requests q's head
  /// set and recursively fetches missing ancestor closure for those heads.
  /// Fetched events are added to p's inbox for later processing via do_receive.
  /// This models closure-based event exchange after partitions.
  action do_heartbeat(p: PeerID, q: PeerID): bool = {
    val st = nodes.get(p)
    val remote = nodes.get(q)
    all {
      p != q,
      isConnected(connected, p, q),
      // Update q's entry from the remote heartbeat
      val updatedPeerHLC = st.peer_hlc
        .set(q, hlcMax(st.peer_hlc.get(q), remote.hlc_clock))
        // Also advance self's entry to current local HLC
        // (a peer always knows its own clock is at least this advanced)
        .set(p, hlcMax(st.peer_hlc.get(p), st.hlc_clock))
      // Re-add q to active_peers on heartbeat (reconnection).
      // Compute stable_hlc over the NEW active_peers so that the min
      // immediately accounts for q's entry. This prevents a delayed
      // stability decrease in the next step (which would violate
      // stability monotonicity within the same active_peers set).
      val newActivePeers = st.active_peers.union(Set(q))
      val newStable = hlcMinOver(updatedPeerHLC, newActivePeers)
      // Pull-on-demand closure: remote heads + recursive ancestors (§2.4 step 5)
      val pulledCIDs = pullClosureCIDs(st.clock, remote)
      val pulledEvents = pulledCIDs.map(cid => remote.clock.get(cid))
      all {
        nodes' = nodes.set(p, {
          ...st,
          peer_hlc: updatedPeerHLC,
          stable_hlc: newStable,
          active_peers: newActivePeers,
        }),
        // Add pulled closure events to p's inbox for processing via do_receive
        inbox' = inbox.set(p, inbox.get(p).union(pulledEvents)),
        prev_stable_hlc' = PEERS.mapBy(pp => nodes.get(pp).stable_hlc),
        prev_active_peers' = PEERS.mapBy(pp => nodes.get(pp).active_peers),

        connected' = connected,
      }
    }
  }

  /// Finalize all currently-stable candidates in one deterministic batch.
  /// Uses a fixed anchor = finalized_root at batch start and merges only
  /// stable frontier heads (candidates with no stable descendants).
  /// This avoids fast-forward overwrite of concurrent writes while preserving
  /// incremental advancement on top of SYNC_FINALIZED outputs.
  action do_finalize(p: PeerID): bool = {
    val st = nodes.get(p)
    val candidates = finalizationCandidates(st)
    all {
      not(candidates.empty()),
      val batch = finalizeBatch(st)
      val withBatch: NodeState = {
        ...st,
        finalized_root: batch._1,
        finalized_events: batch._2,
      }
      val withLineage: NodeState = if (batch._1 != st.finalized_root) {
        val lineage = lineageAddCheckpoint(
          st.finalized_parents,
          st.finalized_ancestors,
          st.finalized_depth,
          batch._1,
          Set(st.finalized_root)
        )
        {
          ...withBatch,
          finalized_parents: lineage._1,
          finalized_ancestors: lineage._2,
          finalized_depth: lineage._3,
        }
      } else {
        withBatch
      }
      val reconcileResult = computeMergedRoot(withLineage)
      nodes' = nodes.set(p, {
        ...withLineage,
        latest_root: reconcileResult._1,
        parked_conflicts: reconcileResult._2,
      }),
      inbox' = inbox,
      prev_stable_hlc' = PEERS.mapBy(pp => nodes.get(pp).stable_hlc),
      prev_active_peers' = PEERS.mapBy(pp => nodes.get(pp).active_peers),

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
    prev_stable_hlc' = PEERS.mapBy(pp => nodes.get(pp).stable_hlc),
    prev_active_peers' = PEERS.mapBy(pp => nodes.get(pp).active_peers),
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
    prev_stable_hlc' = PEERS.mapBy(pp => nodes.get(pp).stable_hlc),
    prev_active_peers' = PEERS.mapBy(pp => nodes.get(pp).active_peers),
  }

  /// Peer eviction: p removes q from its active_peers.
  /// Triggered after heartbeat timeout when q is unreachable.
  /// Precondition: q is not connected to p (models timeout detection).
  /// Recomputes stable_hlc without q, allowing finalization to resume.
  action do_evict(p: PeerID, q: PeerID): bool = {
    val st = nodes.get(p)
    all {
      p != q,
      st.active_peers.contains(q),
      not(isConnected(connected, p, q)),
      val newActivePeers = st.active_peers.exclude(Set(q))
      // Recompute stable_hlc over reduced membership
      val newStable = hlcMinOver(st.peer_hlc, newActivePeers)
      all {
        nodes' = nodes.set(p, {
          ...st,
          active_peers: newActivePeers,
          stable_hlc: newStable,
        }),
        inbox' = inbox,
        prev_stable_hlc' = PEERS.mapBy(pp => nodes.get(pp).stable_hlc),
        prev_active_peers' = PEERS.mapBy(pp => nodes.get(pp).active_peers),

        connected' = connected,
      }
    }
  }

  /// Synchronize finalized state after partition recovery (§7.10).
  ///
  /// Triggered when peer p detects that peer q has a different finalized_root
  /// (via heartbeat). Case split:
  /// - p behind (p.finalized_events ⊂ q.finalized_events): adopt q's finalized state
  /// - p ahead   (q.finalized_events ⊂ p.finalized_events): no-op
  /// - incomparable sets: deterministic three-way merge (accept-the-fork)
  ///
  /// In the merge branch, deterministic ours/theirs assignment picks the
  /// earlier finalized tip as "ours". On conflict, uses ours-side root
  /// (deterministic) with explicit tracking in finalized_conflicts.
  action do_sync_finalized(p: PeerID, q: PeerID): bool = {
    val st_p = nodes.get(p)
    val st_q = nodes.get(q)
    val mode = syncMode(st_p, st_q)
    all {
      syncEnabled(connected, p, q, st_p, st_q),
      val syncedState: NodeState = applySyncFinalized(st_p, st_q)
      all {
        // Branch tag is kept here so any future change to the action still
        // must satisfy the explicit SYNC_FINALIZED contract helpers.
        mode == syncMode(st_p, st_q),
        nodes' = nodes.set(p, syncedState),
        inbox' = inbox,
        prev_stable_hlc' = PEERS.mapBy(pp => nodes.get(pp).stable_hlc),
        prev_active_peers' = PEERS.mapBy(pp => nodes.get(pp).active_peers),

        connected' = connected,
      }
    }
  }

  /// Resolve a parked tentative conflict (§2.4 RESOLVE_CONFLICT).
  ///
  /// User (typically the parked event's author) provides resolution SQL
  /// (modeled as nondeterministic content). Creates a new event whose parents
  /// include ALL heads (including the parked one), subsuming it in the DAG.
  /// The resolution flows through LOCAL_WRITE semantics (steps 2–10).
  action do_resolve_conflict(p: PeerID, parkedCID: EventCID): bool = {
    val st = nodes.get(p)
    all {
      st.parked_conflicts.contains(parkedCID),
      st.clock.has(parkedCID),
      st.localWall <= MAX_WALL,
      st.clock.keys().size() < MAX_EVENTS,
      nondet resolvedContent = CONTENTS.oneOf()
      val newHLC = tick(st.hlc_clock, st.localWall, p)
      val newCID = makeEventCID(p, newHLC.wall, newHLC.logical)
      // Parents = ALL heads (including the parked one), subsuming it in the DAG
      val allHeads = st.heads
      val event: Event = {
        cid: newCID,
        root_hash: resolvedContent,
        parents: allHeads,
        resolved_finalized_conflicts: Set(),
        hlc: newHLC,
        peer: p,
      }
      val newClock = st.clock.put(newCID, event)
      val newHeads = computeHeads(newClock)
      // Recompute parking for the new head set
      val postState: NodeState = {
        ...st,
        clock: newClock,
        heads: newHeads,
        hlc_clock: newHLC,
      }
      val reconcileResult = computeMergedRoot(postState)
      val newLatestRoot = reconcileResult._1
      all {
        nodes' = nodes.set(p, {
          ...st,
          clock: newClock,
          heads: newHeads,
          latest_root: newLatestRoot,
          hlc_clock: newHLC,
          parked_conflicts: reconcileResult._2,
        }),
        inbox' = broadcastConnected(inbox, event, connected),
        prev_stable_hlc' = PEERS.mapBy(pp => nodes.get(pp).stable_hlc),
        prev_active_peers' = PEERS.mapBy(pp => nodes.get(pp).active_peers),

        connected' = connected,
      }
    }
  }

  /// Resolve a tracked finalized-layer conflict.
  ///
  /// This is explicit bookkeeping for SYNC_FINALIZED conflict outcomes:
  /// it removes one finalized_conflicts entry and emits a normal write event
  /// (modeled as nondeterministic content) through LOCAL_WRITE mechanics.
  /// The emitted event carries the resolved conflict id reference.
  action do_resolve_finalized_conflict(p: PeerID, conflictID: FinalizedConflictID): bool = {
    val st = nodes.get(p)
    all {
      resolveFinalizedConflictEnabled(st, p, conflictID),
      nondet resolvedContent = CONTENTS.oneOf()
      val nextState = resolveFinalizedConflictResult(st, p, conflictID, resolvedContent)
      val newCID = makeEventCID(
        p,
        nextState.hlc_clock.wall,
        nextState.hlc_clock.logical
      )
      val emittedEvent = nextState.clock.get(newCID)
      all {
        nodes' = nodes.set(p, {
          ...nextState,
        }),
        inbox' = broadcastConnected(inbox, emittedEvent, connected),
        prev_stable_hlc' = PEERS.mapBy(pp => nodes.get(pp).stable_hlc),
        prev_active_peers' = PEERS.mapBy(pp => nodes.get(pp).active_peers),
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
      do_reconcile(p),
      do_tick(p),
      nondet q = PEERS.oneOf()
      do_heartbeat(p, q),
      do_finalize(p),
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
      nondet parkedCID = nodes.get(p).parked_conflicts.oneOf()
      do_resolve_conflict(p, parkedCID),
      // Finalized-layer conflict resolution
      nondet finalizedConflictCID = nodes.get(p).finalized_conflicts.keys().oneOf()
      do_resolve_finalized_conflict(p, finalizedConflictCID),
    }
  }

  // ===========================================================================
  // Invariants
  // ===========================================================================

  /// Clock convergence: peers with the same event set have the same heads.
  val inv_clock_convergence = PEERS.forall(p1 => PEERS.forall(p2 => {
    val s1 = nodes.get(p1)
    val s2 = nodes.get(p2)
    (s1.clock.keys() == s2.clock.keys()) implies (s1.heads == s2.heads)
  }))

  /// Finalized agreement (INV3): same finalized events → same finalized root.
  /// During a partition, peers may have different finalized_events sets.
  /// After SYNC_FINALIZED, they converge to the same fork-and-join state.
  /// Unconditional: also covers partition merge convergence (formerly INV8).
  /// SYNC_FINALIZED divergence merges handle conflicts by using ours-side root
  /// (deterministic),
  /// so same finalized_events always implies same finalized_root.
  val inv_finalized_agreement = PEERS.forall(p1 => PEERS.forall(p2 => {
    val s1 = nodes.get(p1)
    val s2 = nodes.get(p2)
    (s1.finalized_events == s2.finalized_events) implies
      (s1.finalized_root == s2.finalized_root)
  }))

  /// Finalized lineage well-formedness:
  /// - EMPTY_ROOT is present with depth 0
  /// - finalized_root is present in all lineage maps
  /// - every known root is its own ancestor
  /// - every parent edge points to a strictly shallower root
  val inv_lineage_well_formed = PEERS.forall(p => {
    val st = nodes.get(p)
    val knownRoots = st.finalized_parents.keys()
      .union(st.finalized_ancestors.keys())
      .union(st.finalized_depth.keys())
    all {
      st.finalized_parents.has(EMPTY_ROOT),
      st.finalized_ancestors.has(EMPTY_ROOT),
      st.finalized_depth.has(EMPTY_ROOT),
      st.finalized_depth.get(EMPTY_ROOT) == 0,
      st.finalized_parents.has(st.finalized_root),
      st.finalized_ancestors.has(st.finalized_root),
      st.finalized_depth.has(st.finalized_root),
      knownRoots.forall(r =>
        st.finalized_parents.has(r) and
        st.finalized_ancestors.has(r) and
        st.finalized_depth.has(r) and
        st.finalized_ancestors.get(r).contains(r)
      ),
      st.finalized_parents.keys().forall(r =>
        st.finalized_parents.get(r).forall(parent =>
          parent != r and
          st.finalized_depth.has(parent) and
          st.finalized_depth.get(parent) < st.finalized_depth.get(r)
        )
      ),
    }
  })

  /// Finalized lineage closure:
  /// - parents of a root are included in its ancestor set
  /// - every referenced parent has lineage metadata entries
  val inv_lineage_closure = PEERS.forall(p => {
    val st = nodes.get(p)
    st.finalized_parents.keys().forall(r => {
      if (st.finalized_ancestors.has(r)) {
        st.finalized_parents.get(r).subseteq(st.finalized_ancestors.get(r)) and
        st.finalized_parents.get(r).forall(parent =>
          st.finalized_parents.has(parent) and
          st.finalized_ancestors.has(parent) and
          st.finalized_depth.has(parent)
        )
      } else {
        false
      }
    })
  })

  /// LCA soundness over lineage metadata:
  /// lineageLcaRoot(a, b) must be a common ancestor with maximal depth.
  val inv_lineage_lca_sound = PEERS.forall(p => {
    val st = nodes.get(p)
    val knownRoots = st.finalized_ancestors.keys().intersect(st.finalized_depth.keys())
    knownRoots.forall(a => knownRoots.forall(b => {
      val lca = lineageLcaRoot(a, b, st.finalized_ancestors, st.finalized_depth)
      val common = ancestorsAt(st.finalized_ancestors, a).intersect(
        ancestorsAt(st.finalized_ancestors, b)
      )
      if (common.empty()) {
        lca == EMPTY_ROOT
      } else {
        all {
          common.contains(lca),
          common.forall(c =>
            depthAt(st.finalized_depth, c) <= depthAt(st.finalized_depth, lca)
          ),
        }
      }
    }))
  })

  /// SYNC_FINALIZED metadata-union contract:
  /// output lineage metadata includes merged local+remote metadata.
  val inv_sync_metadata_union = PEERS.forall(p => PEERS.forall(q => {
    val st_p = nodes.get(p)
    val st_q = nodes.get(q)
    val mergedParents = mergeSetMap(st_p.finalized_parents, st_q.finalized_parents)
    val mergedAncestors = mergeSetMap(st_p.finalized_ancestors, st_q.finalized_ancestors)
    val mergedDepth = mergeDepthMap(st_p.finalized_depth, st_q.finalized_depth)
    val synced = applySyncFinalized(st_p, st_q)
    syncEnabled(connected, p, q, st_p, st_q) implies all {
      mergedParents.keys().forall(k => {
        if (synced.finalized_parents.has(k))
          mergedParents.get(k).subseteq(synced.finalized_parents.get(k))
        else
          false
      }),
      mergedAncestors.keys().forall(k => {
        if (synced.finalized_ancestors.has(k))
          mergedAncestors.get(k).subseteq(synced.finalized_ancestors.get(k))
        else
          false
      }),
      mergedDepth.keys().forall(k => {
        if (synced.finalized_depth.has(k))
          synced.finalized_depth.get(k) >= mergedDepth.get(k)
        else
          false
      }),
    }
  }))

  /// SYNC_FINALIZED merge branch must record lineage links to both input roots
  /// (excluding self-edge if resulting root equals one input root).
  val inv_sync_merge_parent_edges = PEERS.forall(p => PEERS.forall(q => {
    val st_p = nodes.get(p)
    val st_q = nodes.get(q)
    val synced = applySyncFinalized(st_p, st_q)
    val expectedParents = Set(st_p.finalized_root, st_q.finalized_root)
      .exclude(Set(synced.finalized_root))
    (syncEnabled(connected, p, q, st_p, st_q) and syncMode(st_p, st_q) == SYNC_MERGE) implies {
      if (synced.finalized_parents.has(synced.finalized_root))
        expectedParents.subseteq(synced.finalized_parents.get(synced.finalized_root))
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
      left.finalized_root == right.finalized_root,
      left.finalized_events == right.finalized_events,
    }
  })))

  /// SYNC_FINALIZED contract (adopt branch): behind peer adopts remote
  /// finalized state exactly once metadata is complete.
  val inv_sync_contract_adopt = PEERS.forall(p => PEERS.forall(q => {
    val st_p = nodes.get(p)
    val st_q = nodes.get(q)
    val knownResolvedRefs = resolvedFinalizedConflictRefs(st_p.clock)
      .union(resolvedFinalizedConflictRefs(st_q.clock))
    (syncEnabled(connected, p, q, st_p, st_q) and syncMode(st_p, st_q) == SYNC_ADOPT) implies all {
      st_q.finalized_events.subseteq(st_p.clock.keys()),
      applySyncFinalized(st_p, st_q).finalized_events == st_q.finalized_events,
      applySyncFinalized(st_p, st_q).finalized_root == st_q.finalized_root,
      applySyncFinalized(st_p, st_q).finalized_conflicts
        == dropFinalizedConflicts(st_q.finalized_conflicts, knownResolvedRefs),
    }
  }))

  /// SYNC_FINALIZED contract (no-op branch): ahead peer does not change
  /// finalized or tentative state.
  val inv_sync_contract_noop = PEERS.forall(p => PEERS.forall(q => {
    val st_p = nodes.get(p)
    val st_q = nodes.get(q)
    (syncEnabled(connected, p, q, st_p, st_q) and syncMode(st_p, st_q) == SYNC_NOOP) implies all {
      applySyncFinalized(st_p, st_q).finalized_events == st_p.finalized_events,
      applySyncFinalized(st_p, st_q).finalized_root == st_p.finalized_root,
      applySyncFinalized(st_p, st_q).finalized_conflicts == st_p.finalized_conflicts,
      applySyncFinalized(st_p, st_q).latest_root == st_p.latest_root,
      applySyncFinalized(st_p, st_q).parked_conflicts == st_p.parked_conflicts,
    }
  }))

  /// SYNC_FINALIZED contract (merge branch): divergence produces merged
  /// finalized events and never leaks CONFLICT_HASH to finalized_root.
  val inv_sync_contract_merge = PEERS.forall(p => PEERS.forall(q => {
    val st_p = nodes.get(p)
    val st_q = nodes.get(q)
    val knownResolvedRefs = resolvedFinalizedConflictRefs(st_p.clock)
      .union(resolvedFinalizedConflictRefs(st_q.clock))
    val unresolvedP = st_p.finalized_conflicts.keys().exclude(knownResolvedRefs)
    val unresolvedQ = st_q.finalized_conflicts.keys().exclude(knownResolvedRefs)
    (syncEnabled(connected, p, q, st_p, st_q) and syncMode(st_p, st_q) == SYNC_MERGE) implies all {
      applySyncFinalized(st_p, st_q).finalized_events == st_p.finalized_events.union(st_q.finalized_events),
      applySyncFinalized(st_p, st_q).finalized_root != CONFLICT_HASH,
      unresolvedP.subseteq(
        applySyncFinalized(st_p, st_q).finalized_conflicts.keys()
      ),
      unresolvedQ.subseteq(
        applySyncFinalized(st_p, st_q).finalized_conflicts.keys()
      ),
    }
  }))

  /// SYNC_FINALIZED conflict-tracking contract: when merge branch conflict occurs,
  /// the deterministic finalized conflict id must be present in the synced state.
  val inv_sync_conflict_recorded = PEERS.forall(p => PEERS.forall(q => {
    val st_p = nodes.get(p)
    val st_q = nodes.get(q)
    val mergedAncestors = mergeSetMap(st_p.finalized_ancestors, st_q.finalized_ancestors)
    val mergedDepth = mergeDepthMap(st_p.finalized_depth, st_q.finalized_depth)
    val knownResolvedRefs = resolvedFinalizedConflictRefs(st_p.clock)
      .union(resolvedFinalizedConflictRefs(st_q.clock))
    val commonRoot = lineageLcaRoot(st_p.finalized_root, st_q.finalized_root, mergedAncestors, mergedDepth)
    val oursRoot = if (st_p.finalized_root < st_q.finalized_root)
      st_p.finalized_root else st_q.finalized_root
    val theirsRoot = if (st_p.finalized_root < st_q.finalized_root)
      st_q.finalized_root else st_p.finalized_root
    val conflictID = finalizedConflictID(oursRoot, theirsRoot, commonRoot)
    val merged = mergeRoots(oursRoot, theirsRoot, commonRoot)
    (syncEnabled(connected, p, q, st_p, st_q) and
      syncMode(st_p, st_q) == SYNC_MERGE and
      merged == CONFLICT_HASH and
      not(knownResolvedRefs.contains(conflictID))) implies
      applySyncFinalized(st_p, st_q).finalized_conflicts.has(conflictID)
  }))

  /// SYNC_FINALIZED conflict-id symmetry: when both directions take merge+conflict,
  /// they derive and record the same deterministic conflict id.
  val inv_sync_conflict_id_symmetric = PEERS.forall(p => PEERS.forall(q => {
    val st_p = nodes.get(p)
    val st_q = nodes.get(q)
    val pCanSync = syncEnabled(connected, p, q, st_p, st_q)
    val qCanSync = syncEnabled(connected, q, p, st_q, st_p)
    val knownResolvedRefsPQ = resolvedFinalizedConflictRefs(st_p.clock)
      .union(resolvedFinalizedConflictRefs(st_q.clock))

    val mergedAncestorsPQ = mergeSetMap(st_p.finalized_ancestors, st_q.finalized_ancestors)
    val mergedDepthPQ = mergeDepthMap(st_p.finalized_depth, st_q.finalized_depth)
    val commonRootPQ = lineageLcaRoot(st_p.finalized_root, st_q.finalized_root, mergedAncestorsPQ, mergedDepthPQ)
    val oursRootPQ = if (st_p.finalized_root < st_q.finalized_root)
      st_p.finalized_root else st_q.finalized_root
    val theirsRootPQ = if (st_p.finalized_root < st_q.finalized_root)
      st_q.finalized_root else st_p.finalized_root
    val conflictIDPQ = finalizedConflictID(oursRootPQ, theirsRootPQ, commonRootPQ)
    val mergedPQ = mergeRoots(oursRootPQ, theirsRootPQ, commonRootPQ)

    val mergedAncestorsQP = mergeSetMap(st_q.finalized_ancestors, st_p.finalized_ancestors)
    val mergedDepthQP = mergeDepthMap(st_q.finalized_depth, st_p.finalized_depth)
    val commonRootQP = lineageLcaRoot(st_q.finalized_root, st_p.finalized_root, mergedAncestorsQP, mergedDepthQP)
    val oursRootQP = if (st_q.finalized_root < st_p.finalized_root)
      st_q.finalized_root else st_p.finalized_root
    val theirsRootQP = if (st_q.finalized_root < st_p.finalized_root)
      st_p.finalized_root else st_q.finalized_root
    val conflictIDQP = finalizedConflictID(oursRootQP, theirsRootQP, commonRootQP)
    val mergedQP = mergeRoots(oursRootQP, theirsRootQP, commonRootQP)

    (pCanSync and qCanSync and
      syncMode(st_p, st_q) == SYNC_MERGE and
      syncMode(st_q, st_p) == SYNC_MERGE and
      mergedPQ == CONFLICT_HASH and
      mergedQP == CONFLICT_HASH) implies all {
      conflictIDPQ == conflictIDQP,
      (not(knownResolvedRefsPQ.contains(conflictIDPQ))) implies
        applySyncFinalized(st_p, st_q).finalized_conflicts.has(conflictIDPQ),
      (not(knownResolvedRefsPQ.contains(conflictIDQP))) implies
        applySyncFinalized(st_q, st_p).finalized_conflicts.has(conflictIDQP),
    }
  }))

  /// RESOLVE_FINALIZED_CONFLICT contract: enabled resolution removes exactly one
  /// finalized conflict id and does not directly mutate finalized_root/events.
  val inv_resolve_finalized_conflict_contract = PEERS.forall(p => {
    val st = nodes.get(p)
    st.finalized_conflicts.keys().forall(conflictID => {
      resolveFinalizedConflictEnabled(st, p, conflictID) implies
        CONTENTS.forall(resolvedContent => {
          val resolvedState = resolveFinalizedConflictResult(st, p, conflictID, resolvedContent)
          val emittedCID = makeEventCID(
            p,
            resolvedState.hlc_clock.wall,
            resolvedState.hlc_clock.logical
          )
          val emitted = resolvedState.clock.get(emittedCID)
          all {
            resolvedState.finalized_root == st.finalized_root,
            resolvedState.finalized_events == st.finalized_events,
            resolvedState.finalized_conflicts.keys()
              == st.finalized_conflicts.keys().exclude(Set(conflictID)),
            emitted.resolved_finalized_conflicts == Set(conflictID),
          }
        })
    })
  })

  /// Scenario check: strict-superset pair converges when the ahead peer no-ops
  /// and the behind peer syncs back.
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
      applySyncFinalized(st_q, applySyncFinalized(st_p, st_q)).finalized_root
        == applySyncFinalized(st_p, st_q).finalized_root,
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
      applySyncFinalized(st_p, st_q).finalized_root
        == applySyncFinalized(st_q, st_p).finalized_root,
    }
  }))

  /// Data convergence: peers with the same events and the same single
  /// parent-closed head agree on latest_root.
  ///
  /// Without parent-closure, an orphan single head may remain ANNOUNCED and
  /// latest_root may still reflect the last mergeable state.
  val inv_data_convergence = PEERS.forall(p1 => PEERS.forall(p2 => {
    val s1 = nodes.get(p1)
    val s2 = nodes.get(p2)
    (s1.clock.keys() == s2.clock.keys() and
     s1.heads.size() == 1 and s2.heads.size() == 1 and
     s1.heads == s2.heads and
     s1.heads.forall(cid =>
       isParentClosed(s1.clock, cid) and
       isParentClosed(s2.clock, cid))) implies
      (s1.latest_root == s2.latest_root)
  }))

  /// Causal consistency (INV4): if e1 is a parent of e2 in the clock DAG,
  /// then e1 precedes e2 in the HLC total order. Since HLC total order is
  /// transitive, checking direct parents suffices for full ancestry chains.
  val inv_causal_consistency = PEERS.forall(p => {
    val st = nodes.get(p)
    st.clock.keys().forall(cid => {
      val e = st.clock.get(cid)
      e.parents.forall(parentCid =>
        // Guard: parent may not yet be in this peer's clock
        not(st.clock.has(parentCid)) or
        less(st.clock.get(parentCid).hlc, e.hlc)
      )
    })
  })

  /// No silent data loss (INV5, safety approximation): every event that any peer
  /// has ever seen is accounted for — still in the clock (pending finalization)
  /// or already finalized. No event disappears silently.
  ///
  /// The full INV5 ("every event *eventually* enters finalized set") is a
  /// liveness property that cannot be checked as a state invariant. This safety
  /// version ensures the precondition: events are never dropped without a trace.
  val inv_no_silent_data_loss = PEERS.forall(p => {
    val st = nodes.get(p)
    // Every finalized event is still in the clock (not silently removed)
    st.finalized_events.forall(cid => st.clock.has(cid))
  })

  /// No unhandled tentative conflict (INV7a): latest_root is never
  /// CONFLICT_HASH. Every conflict detected by mergeRoots during
  /// reconciliation is captured in parked_conflicts — none leaks
  /// into the working state.
  val inv_no_conflict_latest_root = PEERS.forall(p => {
    val st = nodes.get(p)
    st.latest_root != CONFLICT_HASH
  })

  /// No unhandled finalized conflict (INV7b): finalized_root is never
  /// CONFLICT_HASH. SYNC_FINALIZED divergence merges handle conflicts by using the
  /// ours-side root (deterministic) — no conflict sentinel leaks into
  /// the finalized state.
  val inv_no_conflict_finalized_root = PEERS.forall(p => {
    val st = nodes.get(p)
    st.finalized_root != CONFLICT_HASH
  })

  /// Parked event integrity (INV7c): every parked conflict references
  /// a known event in the clock. No conflict is silently discarded.
  val inv_conflict_visibility = PEERS.forall(p => {
    val st = nodes.get(p)
    st.parked_conflicts.forall(cid => st.clock.has(cid))
  })

  /// Finalized conflict integrity (INV7e): every tracked finalized conflict
  /// is well-formed and references known lineage roots.
  val inv_finalized_conflict_visibility = PEERS.forall(p => {
    val st = nodes.get(p)
    st.finalized_conflicts.keys().forall(conflictID => {
      val c = st.finalized_conflicts.get(conflictID)
      all {
        conflictID == finalizedConflictID(c.ours_root, c.theirs_root, c.common_root),
        c.ours_root != c.theirs_root,
        st.finalized_ancestors.has(c.ours_root),
        st.finalized_ancestors.has(c.theirs_root),
        st.finalized_ancestors.has(c.common_root),
      }
    })
  })

  /// Finalized conflict authenticity: tracked finalized conflicts must correspond
  /// to a real merge conflict under mergeRoots.
  val inv_finalized_conflict_authenticity = PEERS.forall(p => {
    val st = nodes.get(p)
    st.finalized_conflicts.keys().forall(conflictID => {
      val c = st.finalized_conflicts.get(conflictID)
      mergeRoots(c.ours_root, c.theirs_root, c.common_root) == CONFLICT_HASH
    })
  })

  /// Finalized conflict resolution propagation: once a peer has observed a
  /// resolution reference for a conflict id, that id must not remain tracked.
  val inv_finalized_conflict_no_resurrection = PEERS.forall(p => {
    val st = nodes.get(p)
    val resolvedRefs = resolvedFinalizedConflictRefs(st.clock)
    st.finalized_conflicts.keys().intersect(resolvedRefs).empty()
  })

  /// Parent-closure safety: only parent-closed heads may be parked by reconcile.
  val inv_parked_parent_closed = PEERS.forall(p => {
    val st = nodes.get(p)
    st.parked_conflicts.subseteq(mergeableHeads(st))
  })

  /// Finalization safety: every finalized event is parent-closed in the local clock.
  val inv_finalized_parent_closed = PEERS.forall(p => {
    val st = nodes.get(p)
    st.finalized_events.forall(cid => isParentClosed(st.clock, cid))
  })

  /// Reconcile fixed-point safety: latest_root is always equal to the
  /// deterministic merged-root projection from current state.
  val inv_latest_root_is_compute_merged_root = PEERS.forall(p => {
    val st = nodes.get(p)
    st.latest_root == computeMergedRoot(st)._1
  })

  /// Reconcile fixed-point safety: parked_conflicts is always equal to the
  /// deterministic parked-set projection from current state.
  val inv_parked_conflicts_is_compute_merged_root = PEERS.forall(p => {
    val st = nodes.get(p)
    st.parked_conflicts == computeMergedRoot(st)._2
  })

  /// Bounded anti-entropy scenario check: if p can heartbeat q and q's head has
  /// full ancestry in q's local clock, then one closure pull plus enough
  /// do_receive steps yields parent-closed remote heads in p's projected clock.
  val inv_heartbeat_closure_catchup_bounded = PEERS.forall(p => PEERS.forall(q => {
    val st_p = nodes.get(p)
    val st_q = nodes.get(q)
    val caughtUpClock = heartbeatClosureCatchupClock(st_p, st_q)
    (p != q and isConnected(connected, p, q)) implies
      st_q.heads.forall(h =>
        (st_q.clock.has(h) and
          ancestorIdClosure(Set(h), st_q.clock).subseteq(st_q.clock.keys())) implies
          isParentClosed(caughtUpClock, h)
      )
  }))

  /// Stability monotonicity (INV6): stable_hlc never decreases.
  /// Uses the prev_stable_hlc history variable to compare across transitions.
  ///
  /// NOTE: This invariant is conditioned on active_peers not changing.
  /// When a peer is re-added to active_peers (reconnection via heartbeat),
  /// stable_hlc can temporarily decrease if the returning peer's HLC is
  /// behind the current minimum. This is expected during partition recovery
  /// and does not compromise finalization safety.
  val inv_stability_monotonicity = PEERS.forall(p => {
    val sameActivePeers = nodes.get(p).active_peers == prev_active_peers.get(p)
    sameActivePeers implies leq(prev_stable_hlc.get(p), nodes.get(p).stable_hlc)
  })

  /// Parking agreement (INV7d): peers with the same events and same
  /// finalized_root deterministically agree on which events are parked.
  /// Parked set is a pure function of (heads, clock, finalized_root) via
  /// computeMergedRoot — no dependence on event receipt order.
  val inv_parking_agreement = PEERS.forall(p1 => PEERS.forall(p2 => {
    val s1 = nodes.get(p1)
    val s2 = nodes.get(p2)
    (s1.clock.keys() == s2.clock.keys() and
     s1.finalized_root == s2.finalized_root) implies
      (s1.parked_conflicts == s2.parked_conflicts)
  }))

  /// Heads are always derived from clock structure (no hidden divergence).
  val inv_heads_consistent = PEERS.forall(p => {
    val st = nodes.get(p)
    st.heads == computeHeads(st.clock)
  })

  /// Finalization and parking are disjoint: parked events are never finalized.
  val inv_finalized_parked_disjoint = PEERS.forall(p => {
    val st = nodes.get(p)
    st.finalized_events.intersect(st.parked_conflicts).empty()
  })

}
