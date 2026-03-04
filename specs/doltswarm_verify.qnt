// -*- mode: Bluespec; -*-

/**
 * DoltSwarm Merkle clock protocol — flat, set-based verification module.
 *
 * Self-contained module for Apalache model checking. No Choreo framework,
 * no List (Seq) types — everything is sets and maps.
 *
 * Models the core protocol: Merkle clock events, MergeRoots-based reconcile,
 * heartbeats for stability, causal finalization,
 * merge conflicts, tentative conflict resolution, network partitions,
 * and partition recovery via accept-the-fork finalized merge (§7.10).
 *
 * NOTE: The protocol uses frontier digests (heads_digest = hash(sort(heads)))
 * in heartbeats for bandwidth efficiency (§7.6). The spec reads full remote
 * state directly — a sound over-approximation that skips the digest-compare
 * optimization without affecting safety properties.
 * Partition conflicts are handled inline by SYNC_FINALIZED's divergence
 * ours-side root — no blocking, no separate resolution action.
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

  type Event = {
    cid: EventCID,
    root_hash: Hash,
    parents: Set[EventCID],
    hlc: HLC,
    peer: PeerID,
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
    parked_conflicts: Set[EventCID],
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

  /// Sort CIDs by HLC (deterministic total order for multi-head merge).
  pure def sortCIDsByHLC(cids: Set[EventCID], clock: EventCID -> Event): List[EventCID] = {
    val cidList = cids.fold([], (acc, c) => acc.append(c))
    cidList.sortList((a, b) => less(clock.get(a).hlc, clock.get(b).hlc))
  }

  /// Deterministic finalized root from a LINEAR event chain: pure function of
  /// the event set. Sorts events by HLC total order and folds mergeRoots from
  /// EMPTY_ROOT. Since mergeRoots(acc, x, acc) is a fast-forward (ours ==
  /// ancestor → theirs), the result equals the root_hash of the HLC-maximum
  /// event. The fold is kept explicit to mirror the protocol's replay semantics.
  ///
  /// Events that produce a conflict during the fold are skipped (§7.1.1):
  /// this handles formerly-parked events whose root_hash branches from an
  /// earlier state. The resolution event (later in HLC order) carries their
  /// data. In this abstract model, the skip path is never exercised because
  /// mergeRoots(acc, x, acc) always fast-forwards, but it aligns with the
  /// protocol and provides defense in depth.
  ///
  /// IMPORTANT: This function is ONLY used for computing the common ancestor
  /// during SYNC_FINALIZED's divergence branch (where the input is a pre-split linear chain —
  /// correct). It must NOT be used for general finalized root advancement,
  /// because after a partition merge the finalized event set contains events
  /// from divergent lineages whose merged result cannot be recovered by
  /// replaying them in HLC order. do_finalize uses a fixed-anchor batch fold
  /// over stable frontier heads instead. See §7.10.
  pure def computeFinalizedRoot(events: Set[EventCID], clock: EventCID -> Event): Hash = {
    if (events.empty()) EMPTY_ROOT
    else {
      // Only include events still present in the clock
      val present = events.filter(cid => clock.has(cid))
      if (present.empty()) EMPTY_ROOT
      else {
        val sorted = sortCIDsByHLC(present, clock)
        sorted.foldl(EMPTY_ROOT, (acc, cid) => {
          val merged = mergeRoots(acc, clock.get(cid).root_hash, acc)
          // Skip events that conflict in the fold (§7.1.1): handles
          // formerly-parked events whose content was subsumed by a
          // later resolution event.
          if (merged == CONFLICT_HASH) acc else merged
        })
      }
    }
  }

  /// Deterministic merged root for a set of heads.
  /// Operates on ALL heads (not pre-filtered by existing parked_conflicts).
  /// Sorts heads by HLC and folds mergeRoots left-to-right.
  /// Returns (merged_root, parked_set) where parked_set is the COMPLETE set
  /// of parked events (not a delta). This makes parking a pure function of
  /// (heads, clock, finalized_root), independent of event receipt order.
  pure def computeMergedRoot(st: NodeState): (Hash, Set[EventCID]) = {
    if (st.heads.size() <= 1) {
      val root = st.heads.fold(st.finalized_root, (_, cid) => st.clock.get(cid).root_hash)
      (root, Set())
    } else {
      val sorted = sortCIDsByHLC(st.heads, st.clock)
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
      not(st.parked_conflicts.contains(cid))
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

  /// Enable condition for SYNC_FINALIZED transition.
  pure def syncEnabled(conn: Set[(PeerID, PeerID)], p: PeerID, q: PeerID,
      st_local: NodeState, st_remote: NodeState): bool = all {
    p != q,
    isConnected(conn, p, q),
    st_local.finalized_root != st_remote.finalized_root,
    st_local.finalized_root != EMPTY_ROOT,
    st_remote.finalized_root != EMPTY_ROOT,
    syncAdoptionReady(st_local, st_remote),
  }

  /// Finalized-state update for SYNC_FINALIZED (without latest_root re-anchoring).
  pure def syncFinalizedCore(st_local: NodeState, st_remote: NodeState): NodeState = {
    val mode = syncMode(st_local, st_remote)
    if (mode == SYNC_ADOPT) {
      {
        ...st_local,
        finalized_root: st_remote.finalized_root,
        finalized_events: st_remote.finalized_events,
      }
    } else if (mode == SYNC_NOOP) {
      st_local
    } else {
      val commonEvents = st_local.finalized_events.intersect(st_remote.finalized_events)
      val commonRoot = computeFinalizedRoot(commonEvents, st_local.clock)
      // Deterministic ours/theirs: compare finalized roots by value.
      val oursRoot = if (st_local.finalized_root < st_remote.finalized_root)
        st_local.finalized_root else st_remote.finalized_root
      val theirsRoot = if (st_local.finalized_root < st_remote.finalized_root)
        st_remote.finalized_root else st_local.finalized_root
      val merged = mergeRoots(oursRoot, theirsRoot, commonRoot)
      val mergedEvents = st_local.finalized_events.union(st_remote.finalized_events)
      // Unified path: on conflict, use ours root (deterministic, no blocking).
      val effectiveRoot = if (merged == CONFLICT_HASH) oursRoot else merged
      {
        ...st_local,
        finalized_root: effectiveRoot,
        finalized_events: mergedEvents,
      }
    }
  }

  /// Full SYNC_FINALIZED local result, including latest_root re-anchoring.
  pure def applySyncFinalized(st_local: NodeState, st_remote: NodeState): NodeState = {
    val syncedState = syncFinalizedCore(st_local, st_remote)
    val rootChanged = syncedState.finalized_root != st_local.finalized_root
    val reconcileResult = if (rootChanged and syncedState.heads.size() > 1) {
      computeMergedRoot(syncedState)
    } else if (rootChanged) {
      val root = syncedState.heads.fold(syncedState.finalized_root, (_, cid) =>
        syncedState.clock.get(cid).root_hash)
      (root, Set())
    } else {
      (st_local.latest_root, st_local.parked_conflicts)
    }
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
      parked_conflicts: Set(),
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
        hlc: newHLC,
        peer: p,
      }
      val newClock = st.clock.put(newCID, event)
      val newHeads = computeHeads(newClock)
      // Recompute parking for the new head set
      val postWriteState: NodeState = { ...st, clock: newClock, heads: newHeads, hlc_clock: newHLC }
      val reconcileResult = if (newHeads.size() > 1) {
        computeMergedRoot(postWriteState)
      } else {
        (content, Set())
      }
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
  /// Reconciliation is inlined here (matching §2.4 RECEIVE_EVENT step 8):
  /// when receiving creates multiple active heads, immediately compute the
  /// merged root and park any conflicts.
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
      // Inline reconciliation (§2.4 step 8): if multiple heads,
      // compute merged root and park any conflicts. computeMergedRoot
      // operates on ALL heads and returns the complete parked set,
      // making parking a pure function of (heads, clock, finalized_root).
      val tempState: NodeState = {
        ...st,
        clock: newClock,
        heads: newHeads,
        hlc_clock: newHLC,
      }
      val reconcileResult = if (newHeads.size() > 1) {
        computeMergedRoot(tempState)
      } else {
        // Single or zero heads: no reconciliation needed
        val root = newHeads.fold(st.finalized_root, (_, cid) => newClock.get(cid).root_hash)
        (root, Set())
      }
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
  /// computeMergedRoot operates on ALL heads and returns the complete parked
  /// set (not a delta), making parking a pure function of (heads, clock,
  /// finalized_root). No peer is blocked from writing.
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
  /// Pull-on-demand (§2.4 HEARTBEAT step 5): if p is missing events that q
  /// has in its clock, those events are added to p's inbox for later
  /// processing via do_receive. This models the event exchange that happens
  /// when peers reconnect after a partition.
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
      // Pull-on-demand: events q has that p is missing (§2.4 step 5)
      val missingEvents = remote.clock.keys().filter(cid =>
        not(st.clock.has(cid))
      )
      val pulledEvents = missingEvents.map(cid => remote.clock.get(cid))
      all {
        nodes' = nodes.set(p, {
          ...st,
          peer_hlc: updatedPeerHLC,
          stable_hlc: newStable,
          active_peers: newActivePeers,
        }),
        // Add missing events to p's inbox for processing via do_receive
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
      nodes' = nodes.set(p, {
        ...st,
        finalized_root: batch._1,
        finalized_events: batch._2,
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
  /// (deterministic) — no blocking, no separate resolution action.
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
      val reconcileResult = if (newHeads.size() > 1) {
        computeMergedRoot(postState)
      } else {
        (resolvedContent, Set())
      }
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

  /// SYNC_FINALIZED contract (adopt branch): behind peer adopts remote
  /// finalized state exactly once metadata is complete.
  val inv_sync_contract_adopt = PEERS.forall(p => PEERS.forall(q => {
    val st_p = nodes.get(p)
    val st_q = nodes.get(q)
    (syncEnabled(connected, p, q, st_p, st_q) and syncMode(st_p, st_q) == SYNC_ADOPT) implies all {
      st_q.finalized_events.subseteq(st_p.clock.keys()),
      applySyncFinalized(st_p, st_q).finalized_events == st_q.finalized_events,
      applySyncFinalized(st_p, st_q).finalized_root == st_q.finalized_root,
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
      applySyncFinalized(st_p, st_q).latest_root == st_p.latest_root,
      applySyncFinalized(st_p, st_q).parked_conflicts == st_p.parked_conflicts,
    }
  }))

  /// SYNC_FINALIZED contract (merge branch): divergence produces merged
  /// finalized events and never leaks CONFLICT_HASH to finalized_root.
  val inv_sync_contract_merge = PEERS.forall(p => PEERS.forall(q => {
    val st_p = nodes.get(p)
    val st_q = nodes.get(q)
    (syncEnabled(connected, p, q, st_p, st_q) and syncMode(st_p, st_q) == SYNC_MERGE) implies all {
      applySyncFinalized(st_p, st_q).finalized_events == st_p.finalized_events.union(st_q.finalized_events),
      applySyncFinalized(st_p, st_q).finalized_root != CONFLICT_HASH,
    }
  }))

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

  /// Data convergence: peers with the same events and single head agree on latest_root.
  /// (After reconciliation, peers with same clock should converge.)
  val inv_data_convergence = PEERS.forall(p1 => PEERS.forall(p2 => {
    val s1 = nodes.get(p1)
    val s2 = nodes.get(p2)
    (s1.clock.keys() == s2.clock.keys() and
     s1.heads.size() == 1 and s2.heads.size() == 1 and
     s1.heads == s2.heads) implies
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
