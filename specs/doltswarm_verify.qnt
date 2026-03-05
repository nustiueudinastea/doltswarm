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
  // Encoded two-dimension content states:
  // 10 => (A0, B0), 11 => (A1, B0), 12 => (A0, B1), 13 => (A1, B1)
  pure val CONTENTS: Set[int] = Set(10, 11, 12, 13)
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
  /// Protocol-level CID bytes come from hash(CanonicalEncode(CIDPayload));
  /// this tuple abstracts that identity without modeling byte encoding/hashing.
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
    finalized_events: EventCID -> Hash,
    finalized_parents: Hash -> Set[Hash],
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

  /// Encoded content-hash helpers for a 2-dimensional abstract row state.
  pure def isContentHash(h: Hash): bool =
    h == EMPTY_ROOT or CONTENTS.contains(h)

  pure def decodeDimA(h: Hash): int =
    if (h == 11 or h == 13) 1 else 0

  pure def decodeDimB(h: Hash): int =
    if (h == 12 or h == 13) 1 else 0

  pure def encodeContent(a: int, b: int): Hash = {
    if (a == 0 and b == 0) 10
    else if (a == 1 and b == 0) 11
    else if (a == 0 and b == 1) 12
    else 13
  }

  /// Deterministic merge model with 2 independent writable dimensions.
  ///
  /// Conflicts are generated only when both sides concurrently write different
  /// values to the same dimension relative to the ancestor. Disjoint writes
  /// (different dimensions) auto-merge.
  pure def mergeRoots(ours: Hash, theirs: Hash, ancestor: Hash): Hash = {
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

  /// Shared fixed-anchor ordered merge fold used by RECONCILE and ADVANCE_STABILITY.
  /// Returns (merged_root, conflicts) where conflicts contains heads whose merge
  /// against the running accumulator conflicted under the fixed anchor.
  pure def fixedAnchorFold(heads: Set[EventCID], st: NodeState, anchor: Hash): (Hash, Set[EventCID]) = {
    val sorted = sortCIDsByHLC(heads, st.clock)
    sorted.foldl((anchor, Set()), (acc, cid) => {
      val currentRoot = acc._1
      val conflicts = acc._2
      val merged = mergeRoots(currentRoot, st.clock.get(cid).root_hash, anchor)
      if (merged == CONFLICT_HASH) {
        (currentRoot, conflicts.union(Set(cid)))
      } else {
        (merged, conflicts)
      }
    })
  }

  /// Deterministic merged root for mergeable heads.
  /// Operates on parent-closed heads only (mergeableHeads).
  /// Uses fixedAnchorFold with anchor = finalized_root.
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
      fixedAnchorFold(readyHeads, st, st.finalized_root)
    }
  }

  /// Eligible events for finalization at the current stability boundary.
  pure def finalizationCandidates(st: NodeState): Set[EventCID] =
    st.clock.keys().filter(cid =>
      val e = st.clock.get(cid)
      less(e.hlc, st.stable_hlc) and
      not(st.finalized_events.has(cid)) and
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
  pure def finalizeBatch(st: NodeState): (Hash, EventCID -> Hash) = {
    val candidates = finalizationCandidates(st)
    if (candidates.empty()) {
      (st.finalized_root, st.finalized_events)
    } else {
      val frontier = stableFrontier(candidates, st.clock)
      val folded = fixedAnchorFold(frontier, st, st.finalized_root)
      val newRoot = folded._1
      val updatedFinalized = candidates.fold(st.finalized_events, (acc, cid) =>
        if (st.clock.has(cid)) acc.put(cid, st.clock.get(cid).root_hash) else acc
      )
      (newRoot, updatedFinalized)
    }
  }

  /// Finalized events safe to compact from clock.
  /// A finalized CID is pruneable only if no non-finalized event depends on it
  /// through parent ancestry.
  pure def gcPruneable(st: NodeState): Set[EventCID] =
    st.finalized_events.keys().intersect(st.clock.keys()).filter(cid =>
      not(st.clock.keys().exists(eid =>
        not(st.finalized_events.has(eid)) and
        ancestorIdClosure(Set(eid), st.clock).contains(cid)
      ))
    )

  /// Merge two maps Hash -> Set[Hash] by key union and value union.
  pure def mergeSetMap(m1: Hash -> Set[Hash], m2: Hash -> Set[Hash]): Hash -> Set[Hash] = {
    m1.keys().union(m2.keys()).mapBy(k => {
      val s1 = if (m1.has(k)) m1.get(k) else Set()
      val s2 = if (m2.has(k)) m2.get(k) else Set()
      s1.union(s2)
    })
  }

  /// Merge finalized event catalogs (cid -> root hash) by key union.
  pure def mergeFinalizedEventMap(m1: EventCID -> Hash, m2: EventCID -> Hash): EventCID -> Hash =
    m1.keys().union(m2.keys()).mapBy(k =>
      if (m1.has(k)) m1.get(k) else m2.get(k)
    )

  /// Merge two finalized-conflict maps by key union.
  pure def mergeFinalizedConflictMap(
      m1: FinalizedConflictID -> FinalizedConflict,
      m2: FinalizedConflictID -> FinalizedConflict): FinalizedConflictID -> FinalizedConflict = {
    m1.keys().union(m2.keys()).mapBy(k =>
      if (m1.has(k)) m1.get(k) else m2.get(k)
    )
  }

  /// Add/update a finalized checkpoint root with given parent roots.
  /// finalized_parents is authoritative lineage state.
  pure def lineageAddCheckpoint(parentsMap: Hash -> Set[Hash], child: Hash,
      parentRoots: Set[Hash]): (Hash -> Set[Hash]) = {
    val sanitizedParents = parentRoots.exclude(Set(child))
    val priorParents = if (parentsMap.has(child)) parentsMap.get(child) else Set()
    val allParents = priorParents.union(sanitizedParents)
    parentsMap.put(child, allParents)
  }

  pure def lineageKnownRoots(parentsMap: Hash -> Set[Hash]): Set[Hash] =
    parentsMap.keys().union(
      parentsMap.keys().fold(Set(), (acc, r) =>
        if (parentsMap.has(r)) acc.union(parentsMap.get(r)) else acc
      )
    )

  /// Derived transitive ancestors (including self and EMPTY_ROOT).
  pure def lineageAncestors(parentsMap: Hash -> Set[Hash], root: Hash): Set[Hash] =
    range(0, MAX_EVENTS + 4).foldl(Set(root), (acc, _) => {
      val parentStep = acc.fold(Set(), (parentsAcc, r) =>
        if (parentsMap.has(r)) parentsAcc.union(parentsMap.get(r))
        else parentsAcc
      )
      acc.union(parentStep)
    }).union(Set(EMPTY_ROOT))

  pure def lineageDepthMap(parentsMap: Hash -> Set[Hash]): Hash -> int = {
    val roots = lineageKnownRoots(parentsMap).union(Set(EMPTY_ROOT))
    val initDepth = roots.mapBy(r =>
      if (r == EMPTY_ROOT) 0 else 1
    )
    range(0, MAX_EVENTS + 4).foldl(initDepth, (depths, _) =>
      roots.mapBy(r => {
        if (r == EMPTY_ROOT) 0
        else {
          val parents = if (parentsMap.has(r) and not(parentsMap.get(r).empty()))
            parentsMap.get(r)
          else
            Set(EMPTY_ROOT)
          val maxParentDepth = parents.fold(0, (acc, p) => {
            val d = if (depths.has(p)) depths.get(p) else if (p == EMPTY_ROOT) 0 else 1
            if (d > acc) d else acc
          })
          maxParentDepth + 1
        }
      })
    )
  }

  pure def rootIsAncestor(ancestor: Hash, descendant: Hash,
      parentsMap: Hash -> Set[Hash]): bool =
    lineageAncestors(parentsMap, descendant).contains(ancestor)

  pure def depthAt(parentsMap: Hash -> Set[Hash], root: Hash): int = {
    val depthMap = lineageDepthMap(parentsMap)
    if (depthMap.has(root)) depthMap.get(root) else 0
  }

  /// Lowest common finalized-root ancestor using finalized checkpoint metadata.
  /// Picks the common ancestor with highest depth; ties break by root value.
  pure def lineageLcaRoot(rootA: Hash, rootB: Hash, parentsMap: Hash -> Set[Hash]): Hash = {
    val ancA = lineageAncestors(parentsMap, rootA)
    val ancB = lineageAncestors(parentsMap, rootB)
    val depthMap = lineageDepthMap(parentsMap)
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

  pure def ancestorsAt(parentsMap: Hash -> Set[Hash], root: Hash): Set[Hash] =
    lineageAncestors(parentsMap, root)

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

  /// Remove one event from the clock map.
  pure def dropEvent(clock: EventCID -> Event, cid: EventCID): EventCID -> Event =
    clock.keys().exclude(Set(cid)).mapBy(k => clock.get(k))

  /// Collect all finalized-conflict resolution references observed in a clock.
  pure def resolvedFinalizedConflictRefs(clock: EventCID -> Event): Set[FinalizedConflictID] =
    clock.keys().fold(Set(), (acc, cid) =>
      acc.union(clock.get(cid).resolved_finalized_conflicts)
    )

  /// Canonical finalized-event catalog (cid -> root hash).
  pure def finalizedEventCatalog(st: NodeState): Set[EventCID] =
    st.finalized_events.keys()

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
    val mergedParents = mergeSetMap(st_local.finalized_parents, st_remote.finalized_parents)
    val localAncestorRemote = rootIsAncestor(
      st_local.finalized_root,
      st_remote.finalized_root,
      mergedParents
    )
    val remoteAncestorLocal = rootIsAncestor(
      st_remote.finalized_root,
      st_local.finalized_root,
      mergedParents
    )
    if (localAncestorRemote and not(remoteAncestorLocal)) SYNC_ADOPT
    else if (remoteAncestorLocal and not(localAncestorRemote)) SYNC_NOOP
    else SYNC_MERGE
  }

  /// Lineage-based adoption gate for SYNC_FINALIZED.
  pure def syncAdoptionReady(st_local: NodeState, st_remote: NodeState): bool = {
    if (syncMode(st_local, st_remote) == SYNC_ADOPT) {
      val mergedParents = mergeSetMap(st_local.finalized_parents, st_remote.finalized_parents)
      rootIsAncestor(st_local.finalized_root, st_remote.finalized_root, mergedParents)
    } else
      true
  }

  /// Lineage metadata readiness for SYNC_FINALIZED: after exchanging finalized
  /// checkpoint metadata, both local and remote finalized roots must be present
  /// in the merged lineage view so common-ancestor lookup is well-defined.
  pure def syncLineageReady(st_local: NodeState, st_remote: NodeState): bool = {
    val mergedParents = mergeSetMap(st_local.finalized_parents, st_remote.finalized_parents)
    val knownRoots = lineageKnownRoots(mergedParents).union(Set(EMPTY_ROOT))
    mergedParents.has(EMPTY_ROOT) and
      knownRoots.contains(st_local.finalized_root) and
      knownRoots.contains(st_remote.finalized_root)
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
    // SYNC_FINALIZED exchanges finalized checkpoint metadata and finalized-event
    // catalogs. finalized_parents is authoritative lineage state.
    val mergedParents = mergeSetMap(st_local.finalized_parents, st_remote.finalized_parents)
    val mergedFinalizedEvents = mergeFinalizedEventMap(
      st_local.finalized_events,
      st_remote.finalized_events
    )
    val unresolvedFinalizedConflicts = dropFinalizedConflicts(
      mergeFinalizedConflictMap(st_local.finalized_conflicts, st_remote.finalized_conflicts),
      knownResolvedRefs
    )
    val withMergedLineage: NodeState = {
      ...st_local,
      finalized_parents: mergedParents,
    }
    if (mode == SYNC_ADOPT) {
      {
        ...withMergedLineage,
        finalized_root: st_remote.finalized_root,
        finalized_events: st_remote.finalized_events,
        finalized_conflicts: unresolvedFinalizedConflicts,
      }
    } else if (mode == SYNC_NOOP) {
      // Ahead path: finalized root/events no-op, but lineage/conflict metadata
      // exchange is retained with resolution-aware filtering.
      {
        ...withMergedLineage,
        finalized_events: st_local.finalized_events,
        finalized_conflicts: unresolvedFinalizedConflicts,
      }
    } else {
      val commonRoot = lineageLcaRoot(
        st_local.finalized_root,
        st_remote.finalized_root,
        mergedParents
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
        finalized_events: mergedFinalizedEvents,
        finalized_conflicts: mergedFinalizedConflicts,
      }
      val lineage = lineageAddCheckpoint(
        mergedState.finalized_parents,
        effectiveRoot,
        Set(st_local.finalized_root, st_remote.finalized_root)
      )
      {
        ...mergedState,
        finalized_parents: lineage,
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
      finalized_events: Map(),
      finalized_parents: Map().put(EMPTY_ROOT, Set()),
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
      not(st.clock.keys().contains(e.cid)) and
      not(st.finalized_events.has(e.cid))
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
  /// Primarily used to exercise re-anchoring after finalized_root advances
  /// (via do_finalize or do_sync_finalized). Steady-state reconciliation is
  /// inlined in do_receive and do_write.
  ///
  /// computeMergedRoot operates on parent-closed heads only and returns the
  /// complete parked set (not a delta), making parking a pure function of
  /// (heads, clock, finalized_root). No peer is blocked from writing.
  action do_reconcile(p: PeerID): bool = {
    val st = nodes.get(p)
    all {
      st.heads.size() > 1,
      val result = computeMergedRoot(st)
      val newLatestRoot = result._1
      val newParkedConflicts = result._2
      all {
        (newLatestRoot != st.latest_root or newParkedConflicts != st.parked_conflicts),
        nodes' = nodes.set(p, {
          ...st,
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
          batch._1,
          Set(st.finalized_root)
        )
        {
          ...withBatch,
          finalized_parents: lineage,
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

  /// Compact finalized event bodies from clock when no non-finalized event
  /// depends on them.
  action do_gc(p: PeerID): bool = {
    val st = nodes.get(p)
    val pruneable = gcPruneable(st)
    all {
      not(pruneable.empty()),
      nondet cid = pruneable.oneOf()
      val newClock = dropEvent(st.clock, cid)
      val newHeads = computeHeads(newClock)
      val gcState: NodeState = {
        ...st,
        clock: newClock,
        heads: newHeads,
      }
      val reconcileResult = computeMergedRoot(gcState)
      nodes' = nodes.set(p, {
        ...gcState,
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
  /// (via heartbeat). Case split on finalized-root lineage:
  /// - p root ancestor of q root: adopt q's finalized state
  /// - q root ancestor of p root: no-op for finalized root/events
  /// - neither ancestor of the other: deterministic three-way merge (accept-the-fork)
  ///
  /// In the merge branch, deterministic ours/theirs assignment picks the
  /// lower finalized-root value as "ours". On conflict, uses ours-side root
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
  /// are active_heads plus the explicitly-resolved parked CID.
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
      // Resolve one parked conflict explicitly:
      // parents = active_heads ∪ {parkedCID}
      val resolveParents = st.heads.exclude(st.parked_conflicts).union(Set(parkedCID))
      val event: Event = {
        cid: newCID,
        root_hash: resolvedContent,
        parents: resolveParents,
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

  /// Finalized agreement (INV3): same finalized catalog → same finalized root.
  /// During a partition, peers may have different finalized catalogs.
  /// After SYNC_FINALIZED, they converge to the same fork-and-join state.
  /// Unconditional: also covers partition merge convergence (formerly INV8).
  /// SYNC_FINALIZED divergence merges handle conflicts by using ours-side root
  /// (deterministic),
  /// so same finalized catalog always implies same finalized_root.
  val inv_finalized_agreement = PEERS.forall(p1 => PEERS.forall(p2 => {
    val s1 = nodes.get(p1)
    val s2 = nodes.get(p2)
    (s1.finalized_events == s2.finalized_events) implies
      (s1.finalized_root == s2.finalized_root)
  }))

  /// Finalized lineage well-formedness:
  /// - EMPTY_ROOT is present
  /// - finalized_root is present in lineage known roots
  /// - every known root is its own ancestor
  /// - every parent edge points to a strictly shallower root (derived depth)
  val inv_lineage_well_formed = PEERS.forall(p => {
    val st = nodes.get(p)
    val knownRoots = lineageKnownRoots(st.finalized_parents).union(Set(EMPTY_ROOT))
    all {
      st.finalized_parents.has(EMPTY_ROOT),
      knownRoots.contains(st.finalized_root),
      knownRoots.forall(r =>
        ancestorsAt(st.finalized_parents, r).contains(r)
      ),
      st.finalized_parents.keys().forall(r =>
        st.finalized_parents.get(r).forall(parent =>
          parent != r and
          knownRoots.contains(parent) and
          depthAt(st.finalized_parents, parent) < depthAt(st.finalized_parents, r)
        )
      ),
    }
  })

  /// Finalized lineage closure:
  /// - parents of a root are included in its ancestor set
  /// - every referenced parent is a known lineage root
  val inv_lineage_closure = PEERS.forall(p => {
    val st = nodes.get(p)
    val knownRoots = lineageKnownRoots(st.finalized_parents).union(Set(EMPTY_ROOT))
    st.finalized_parents.keys().forall(r => {
      st.finalized_parents.get(r).subseteq(ancestorsAt(st.finalized_parents, r)) and
      st.finalized_parents.get(r).forall(parent => knownRoots.contains(parent))
    })
  })

  /// LCA soundness over lineage metadata:
  /// lineageLcaRoot(a, b) must be a common ancestor with maximal depth.
  val inv_lineage_lca_sound = PEERS.forall(p => {
    val st = nodes.get(p)
    val knownRoots = lineageKnownRoots(st.finalized_parents).union(Set(EMPTY_ROOT))
    knownRoots.forall(a => knownRoots.forall(b => {
      val lca = lineageLcaRoot(a, b, st.finalized_parents)
      val common = ancestorsAt(st.finalized_parents, a).intersect(
        ancestorsAt(st.finalized_parents, b)
      )
      if (common.empty()) {
        lca == EMPTY_ROOT
      } else {
        all {
          common.contains(lca),
          common.forall(c =>
            depthAt(st.finalized_parents, c) <= depthAt(st.finalized_parents, lca)
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
    val mergedParents = mergeSetMap(st_p.finalized_parents, st_q.finalized_parents)
    val synced = applySyncFinalized(st_p, st_q)
    syncEnabled(connected, p, q, st_p, st_q) implies all {
      mergedParents.keys().forall(k => {
        if (synced.finalized_parents.has(k))
          mergedParents.get(k).subseteq(synced.finalized_parents.get(k))
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
  /// finalized root/events while preserving unresolved conflict visibility.
  val inv_sync_contract_adopt = PEERS.forall(p => PEERS.forall(q => {
    val st_p = nodes.get(p)
    val st_q = nodes.get(q)
    val knownResolvedRefs = resolvedFinalizedConflictRefs(st_p.clock)
      .union(resolvedFinalizedConflictRefs(st_q.clock))
    val unresolvedUnion = dropFinalizedConflicts(
      mergeFinalizedConflictMap(st_p.finalized_conflicts, st_q.finalized_conflicts),
      knownResolvedRefs
    )
    val synced = applySyncFinalized(st_p, st_q)
    (syncEnabled(connected, p, q, st_p, st_q) and syncMode(st_p, st_q) == SYNC_ADOPT) implies all {
      synced.finalized_events == st_q.finalized_events,
      synced.finalized_root == st_q.finalized_root,
      synced.finalized_conflicts == unresolvedUnion,
    }
  }))

  /// SYNC_FINALIZED contract (no-op branch): ahead peer does not change
  /// finalized root/events, but unresolved finalized conflicts still propagate
  /// monotonically (minus known resolution references).
  val inv_sync_contract_noop = PEERS.forall(p => PEERS.forall(q => {
    val st_p = nodes.get(p)
    val st_q = nodes.get(q)
    val knownResolvedRefs = resolvedFinalizedConflictRefs(st_p.clock)
      .union(resolvedFinalizedConflictRefs(st_q.clock))
    val unresolvedUnion = dropFinalizedConflicts(
      mergeFinalizedConflictMap(st_p.finalized_conflicts, st_q.finalized_conflicts),
      knownResolvedRefs
    )
    (syncEnabled(connected, p, q, st_p, st_q) and syncMode(st_p, st_q) == SYNC_NOOP) implies all {
      applySyncFinalized(st_p, st_q).finalized_events == st_p.finalized_events,
      applySyncFinalized(st_p, st_q).finalized_root == st_p.finalized_root,
      applySyncFinalized(st_p, st_q).finalized_conflicts == unresolvedUnion,
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
    val synced = applySyncFinalized(st_p, st_q)
    (syncEnabled(connected, p, q, st_p, st_q) and syncMode(st_p, st_q) == SYNC_MERGE) implies all {
      synced.finalized_events == mergeFinalizedEventMap(st_p.finalized_events, st_q.finalized_events),
      synced.finalized_root != CONFLICT_HASH,
      unresolvedP.subseteq(
        synced.finalized_conflicts.keys()
      ),
      unresolvedQ.subseteq(
        synced.finalized_conflicts.keys()
      ),
    }
  }))

  /// SYNC_FINALIZED unresolved-conflict monotonicity:
  /// unresolved conflict ids cannot disappear without observed resolution refs.
  val inv_sync_unresolved_conflicts_monotone = PEERS.forall(p => PEERS.forall(q => {
    val st_p = nodes.get(p)
    val st_q = nodes.get(q)
    val knownResolvedRefs = resolvedFinalizedConflictRefs(st_p.clock)
      .union(resolvedFinalizedConflictRefs(st_q.clock))
    val unresolvedIDs = st_p.finalized_conflicts.keys()
      .union(st_q.finalized_conflicts.keys())
      .exclude(knownResolvedRefs)
    val synced = applySyncFinalized(st_p, st_q)
    syncEnabled(connected, p, q, st_p, st_q) implies
      unresolvedIDs.subseteq(synced.finalized_conflicts.keys())
  }))

  /// SYNC_FINALIZED conflict-tracking contract: when merge branch conflict occurs,
  /// the deterministic finalized conflict id must be present in the synced state.
  val inv_sync_conflict_recorded = PEERS.forall(p => PEERS.forall(q => {
    val st_p = nodes.get(p)
    val st_q = nodes.get(q)
    val mergedParents = mergeSetMap(st_p.finalized_parents, st_q.finalized_parents)
    val knownResolvedRefs = resolvedFinalizedConflictRefs(st_p.clock)
      .union(resolvedFinalizedConflictRefs(st_q.clock))
    val commonRoot = lineageLcaRoot(st_p.finalized_root, st_q.finalized_root, mergedParents)
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

    val mergedParentsPQ = mergeSetMap(st_p.finalized_parents, st_q.finalized_parents)
    val commonRootPQ = lineageLcaRoot(st_p.finalized_root, st_q.finalized_root, mergedParentsPQ)
    val oursRootPQ = if (st_p.finalized_root < st_q.finalized_root)
      st_p.finalized_root else st_q.finalized_root
    val theirsRootPQ = if (st_p.finalized_root < st_q.finalized_root)
      st_q.finalized_root else st_p.finalized_root
    val conflictIDPQ = finalizedConflictID(oursRootPQ, theirsRootPQ, commonRootPQ)
    val mergedPQ = mergeRoots(oursRootPQ, theirsRootPQ, commonRootPQ)

    val mergedParentsQP = mergeSetMap(st_q.finalized_parents, st_p.finalized_parents)
    val commonRootQP = lineageLcaRoot(st_q.finalized_root, st_p.finalized_root, mergedParentsQP)
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

  /// Data convergence: peers with the same retained clock state and the same
  /// finalized root must derive the same reconcile projection.
  val inv_data_convergence = PEERS.forall(p1 => PEERS.forall(p2 => {
    val s1 = nodes.get(p1)
    val s2 = nodes.get(p2)
    (s1.clock == s2.clock and
     s1.finalized_root == s2.finalized_root and
     not(mergeableHeads(s1).empty())) implies
      (s1.latest_root == s2.latest_root and
       s1.parked_conflicts == s2.parked_conflicts)
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

  /// No silent data loss (INV5, safety approximation): finalized catalog entries
  /// that are still retained in clock must agree on root hash.
  ///
  /// The full INV5 ("every event *eventually* enters finalized set") is a
  /// liveness property that cannot be checked as a state invariant. This safety
  /// version ensures the precondition: events are never dropped without a trace.
  val inv_no_silent_data_loss = PEERS.forall(p => {
    val st = nodes.get(p)
    st.finalized_events.keys().forall(cid =>
      not(st.clock.has(cid)) or st.finalized_events.get(cid) == st.clock.get(cid).root_hash
    )
  })

  /// Event roots in clocks must never use the conflict sentinel.
  val inv_event_root_not_conflict = PEERS.forall(p => {
    val st = nodes.get(p)
    st.clock.keys().forall(cid => st.clock.get(cid).root_hash != CONFLICT_HASH)
  })

  /// Disjoint concurrent writes must auto-merge and be commutative.
  /// Scenario checks over the abstract 2D state space.
  val inv_merge_disjoint_commutative = all {
    mergeRoots(11, 12, 10) == 13,
    mergeRoots(12, 11, 10) == 13,
    mergeRoots(11, 12, 10) == mergeRoots(12, 11, 10),
    mergeRoots(10, 13, 11) == 12,
    mergeRoots(13, 10, 11) == 12,
    mergeRoots(10, 13, 11) == mergeRoots(13, 10, 11),
  }

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
    val knownRoots = lineageKnownRoots(st.finalized_parents).union(Set(EMPTY_ROOT))
    st.finalized_conflicts.keys().forall(conflictID => {
      val c = st.finalized_conflicts.get(conflictID)
      all {
        conflictID == finalizedConflictID(c.ours_root, c.theirs_root, c.common_root),
        c.ours_root != c.theirs_root,
        knownRoots.contains(c.ours_root),
        knownRoots.contains(c.theirs_root),
        knownRoots.contains(c.common_root),
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

  /// Finalization safety: every retained finalized event in clock is parent-closed.
  val inv_finalized_parent_closed = PEERS.forall(p => {
    val st = nodes.get(p)
    st.finalized_events.keys().intersect(st.clock.keys()).forall(cid => isParentClosed(st.clock, cid))
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

  /// Re-anchoring under closure lag: after SYNC_FINALIZED changes finalized_root,
  /// projection may remain provisional while mergeable heads are missing.
  /// Once required closure is available and introduces mergeable heads with
  /// differing roots, recomputation must change the stale projection.
  val inv_reanchor_after_closure_lag = PEERS.forall(p => PEERS.forall(q => {
    val st_p = nodes.get(p)
    val st_q = nodes.get(q)
    val synced = applySyncFinalized(st_p, st_q)
    val caughtClock = heartbeatClosureCatchupClock(synced, st_q)
    val caughtState: NodeState = {
      ...synced,
      clock: caughtClock,
      heads: computeHeads(caughtClock),
    }
    val recomputed = computeMergedRoot(caughtState)
    (syncEnabled(connected, p, q, st_p, st_q) and
      synced.finalized_root != st_p.finalized_root and
      not(synced.heads.empty()) and
      mergeableHeads(synced).empty() and
      not(mergeableHeads(caughtState).empty()) and
      mergeableHeads(caughtState).exists(h => caughtState.clock.get(h).root_hash != synced.latest_root)
    ) implies
      (recomputed._1 != synced.latest_root or recomputed._2 != synced.parked_conflicts)
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
    st.finalized_events.keys().intersect(st.parked_conflicts).empty()
  })

}
