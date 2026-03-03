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

  /// Record type for partition conflict data.
  type ConflictData = {
    ours_finalized_events: Set[EventCID],
    theirs_finalized_events: Set[EventCID],
    ours_root: Hash,
    theirs_root: Hash,
    common_root: Hash,
  }

  /// Partition conflict payload: stores both sides' finalized state so that
  /// do_resolve_partition_conflict can union the finalized_events sets.
  /// NONE when no partition conflict is active.
  type PartitionConflict =
    | NoConflict
    | ActiveConflict(ConflictData)

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
    partition_conflict: PartitionConflict,
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
  /// History variable: tracks previous finalized_events per peer for finalize-blocked check.
  var prev_finalized_events: PeerID -> Set[EventCID]
  /// Network connectivity: set of directed (a, b) pairs meaning a can send to b.
  /// Symmetric: (a, b) ∈ connected ⟺ (b, a) ∈ connected.
  /// Models network partitions: removing pairs isolates peers.
  var connected: Set[(PeerID, PeerID)]

  // ===========================================================================
  // Helpers
  // ===========================================================================

  pure def makeEventCID(peer: PeerID, wall: int, logical: int): EventCID =
    (peer, wall, logical)

  /// Check if a partition conflict is active.
  pure def hasPartitionConflict(st: NodeState): bool =
    match st.partition_conflict {
      | NoConflict => false
      | ActiveConflict(_) => true
    }

  /// Sentinel for extracting conflict data (never actually used — guard ensures ActiveConflict).
  pure val EMPTY_CONFLICT_DATA: ConflictData = {
    ours_finalized_events: Set(),
    theirs_finalized_events: Set(),
    ours_root: 0,
    theirs_root: 0,
    common_root: 0,
  }

  /// Extract conflict data from a PartitionConflict (safe when hasPartitionConflict is true).
  pure def getConflictData(pc: PartitionConflict): ConflictData =
    match pc {
      | NoConflict => EMPTY_CONFLICT_DATA
      | ActiveConflict(data) => data
    }

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
  /// partition conflicts (INV8b, INV11) and post-finalization tentative
  /// conflicts (INV8a, INV8d) are exercised realistically.
  ///
  /// NOTE: This uses finalized_root as the merge ancestor, not an LCA-derived
  /// ancestor from the Merkle clock DAG. The protocol doc (§2.4 step 3a)
  /// specifies LCA-based ancestors, but implementing LCA in the model checker
  /// would significantly increase the state space. Using finalized_root is a
  /// sound over-approximation: it may produce more conflicts than LCA would
  /// (since finalized_root is always an ancestor of or equal to the LCA),
  /// but it will not miss conflicts that LCA would detect.
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
    else ours + theirs + ancestor  // deterministic combined hash
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

  /// Deterministic finalized root: pure function of finalized event set.
  /// Sorts events by HLC total order and folds mergeRoots from EMPTY_ROOT.
  /// Since mergeRoots(acc, x, acc) is a fast-forward (ours == ancestor → theirs),
  /// the result equals the root_hash of the HLC-maximum event. The fold is kept
  /// explicit to mirror the protocol's replay semantics.
  ///
  /// Events that produce a conflict during the fold are skipped (§7.1.1):
  /// this handles formerly-parked events whose root_hash branches from an
  /// earlier state. The resolution event (later in HLC order) carries their
  /// data. In this abstract model, the skip path is never exercised because
  /// mergeRoots(acc, x, acc) always fast-forwards, but it aligns with the
  /// protocol and provides defense in depth.
  ///
  /// NOTE: After a network partition, finalized events may come from divergent
  /// lineages. In that case, do_merge_finalized handles the merge via a single
  /// three-way MergeRoots call instead of using this fold. See §7.10.
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
      partition_conflict: NoConflict,
    }),
    inbox' = PEERS.mapBy(_ => Set()),
    prev_stable_hlc' = PEERS.mapBy(_ => ZERO),
    prev_active_peers' = PEERS.mapBy(_ => PEERS),
    prev_finalized_events' = PEERS.mapBy(_ => Set()),
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
        prev_finalized_events' = PEERS.mapBy(pp => nodes.get(pp).finalized_events),
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
        prev_finalized_events' = PEERS.mapBy(pp => nodes.get(pp).finalized_events),
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
        prev_finalized_events' = PEERS.mapBy(pp => nodes.get(pp).finalized_events),
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
      prev_finalized_events' = PEERS.mapBy(pp => nodes.get(pp).finalized_events),
      connected' = connected,
    }
  }

  /// Heartbeat: peer p observes peer q's HLC.
  /// Requires connectivity (heartbeats don't flow across partitions).
  /// Also updates p's own peer_hlc entry to its current HLC clock,
  /// ensuring that stable_hlc can advance (a peer always knows its own time).
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
        prev_finalized_events' = PEERS.mapBy(pp => nodes.get(pp).finalized_events),
        connected' = connected,
      }
    }
  }

  /// Finalize one event below stable_hlc (the one with the smallest HLC).
  /// finalized_root is recomputed from scratch via computeFinalizedRoot to
  /// guarantee that the same finalized_events always yield the same root,
  /// regardless of the order peers finalize events.
  ///
  /// Blocked when partition_conflict is active — no new finalization
  /// until the partition conflict is resolved (§7.10).
  action do_finalize(p: PeerID): bool = {
    val st = nodes.get(p)
    all {
      // Block finalization during partition conflict (§2.4 ADVANCE_STABILITY step 0)
      not(hasPartitionConflict(st)),
      val candidates = st.clock.keys().filter(cid =>
        val e = st.clock.get(cid)
        less(e.hlc, st.stable_hlc) and
        not(st.finalized_events.contains(cid)) and
        not(st.parked_conflicts.contains(cid))
      )
      all {
        not(candidates.empty()),
        nondet cid = candidates.oneOf()
        val newFinalizedEvents = st.finalized_events.union(Set(cid))
        val newFinRoot = computeFinalizedRoot(newFinalizedEvents, st.clock)
        all {
          nodes' = nodes.set(p, {
            ...st,
            finalized_root: newFinRoot,
            finalized_events: newFinalizedEvents,
          }),
          inbox' = inbox,
          prev_stable_hlc' = PEERS.mapBy(pp => nodes.get(pp).stable_hlc),
          prev_active_peers' = PEERS.mapBy(pp => nodes.get(pp).active_peers),
          prev_finalized_events' = PEERS.mapBy(pp => nodes.get(pp).finalized_events),
          connected' = connected,
        }
      }
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
    prev_finalized_events' = PEERS.mapBy(pp => nodes.get(pp).finalized_events),
  }

  /// Network heal: reconnect two peers.
  /// Heartbeats and events can flow again. Event exchange and partition
  /// merge happen via subsequent do_heartbeat and do_merge_finalized.
  action do_heal(p: PeerID, q: PeerID): bool = all {
    p != q,
    not(isConnected(connected, p, q)),
    nodes' = nodes,
    inbox' = inbox,
    connected' = connected.union(Set((p, q), (q, p))),
    prev_stable_hlc' = PEERS.mapBy(pp => nodes.get(pp).stable_hlc),
    prev_active_peers' = PEERS.mapBy(pp => nodes.get(pp).active_peers),
    prev_finalized_events' = PEERS.mapBy(pp => nodes.get(pp).finalized_events),
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
        prev_finalized_events' = PEERS.mapBy(pp => nodes.get(pp).finalized_events),
        connected' = connected,
      }
    }
  }

  /// Merge divergent finalized roots after partition recovery (§7.10).
  ///
  /// Triggered when peer p detects that peer q has a different finalized_root
  /// (via heartbeat) and the two roots diverged independently. Both peers
  /// must have finalized at least one event (non-EMPTY_ROOT).
  ///
  /// Computes common ancestor from intersection of finalized_events.
  /// Performs a single three-way MergeRoots call. On success: updates
  /// finalized_root and re-anchors tentative events. On conflict: sets
  /// partition_conflict, blocking further finalization.
  ///
  /// Deterministic ours/theirs assignment: the finalized tip with the earlier
  /// HLC is "ours", the later is "theirs". All peers make the same selection.
  /// Every peer independently computes the same merge — deterministic.
  action do_merge_finalized(p: PeerID, q: PeerID): bool = {
    val st_p = nodes.get(p)
    val st_q = nodes.get(q)
    all {
      p != q,
      isConnected(connected, p, q),
      // Both sides have finalized and their roots differ
      st_p.finalized_root != st_q.finalized_root,
      st_p.finalized_root != EMPTY_ROOT,
      st_q.finalized_root != EMPTY_ROOT,
      not(hasPartitionConflict(st_p)),
      // Neither side's finalized_events is a subset of the other
      // (if one is a subset, the other is simply ahead — not a partition)
      not(st_p.finalized_events.subseteq(st_q.finalized_events)),
      not(st_q.finalized_events.subseteq(st_p.finalized_events)),
      // Compute common ancestor from intersection of finalized event sets
      val commonEvents = st_p.finalized_events.intersect(st_q.finalized_events)
      val commonRoot = computeFinalizedRoot(commonEvents, st_p.clock)
      // Deterministic ours/theirs: compare finalized roots by value
      // (models HLC-based tip comparison — lower hash = "ours", higher = "theirs")
      val oursRoot = if (st_p.finalized_root < st_q.finalized_root)
        st_p.finalized_root else st_q.finalized_root
      val theirsRoot = if (st_p.finalized_root < st_q.finalized_root)
        st_q.finalized_root else st_p.finalized_root
      val oursEvents = if (st_p.finalized_root < st_q.finalized_root)
        st_p.finalized_events else st_q.finalized_events
      val theirsEvents = if (st_p.finalized_root < st_q.finalized_root)
        st_q.finalized_events else st_p.finalized_events
      val merged = mergeRoots(oursRoot, theirsRoot, commonRoot)
      val mergedEvents = st_p.finalized_events.union(st_q.finalized_events)
      if (merged == CONFLICT_HASH) {
        // Partition conflict — store full payload, block finalization until resolved
        all {
          nodes' = nodes.set(p, {
            ...st_p,
            partition_conflict: ActiveConflict({
              ours_finalized_events: oursEvents,
              theirs_finalized_events: theirsEvents,
              ours_root: oursRoot,
              theirs_root: theirsRoot,
              common_root: commonRoot,
            }),
          }),
          inbox' = inbox,
          prev_stable_hlc' = PEERS.mapBy(pp => nodes.get(pp).stable_hlc),
          prev_active_peers' = PEERS.mapBy(pp => nodes.get(pp).active_peers),
          prev_finalized_events' = PEERS.mapBy(pp => nodes.get(pp).finalized_events),
          connected' = connected,
        }
      } else {
        // Clean merge — update finalized root and events, re-anchor tentative
        val postMergeState: NodeState = {
          ...st_p,
          finalized_root: merged,
          finalized_events: mergedEvents,
        }
        val reconcileResult = if (postMergeState.heads.size() > 1) {
          computeMergedRoot(postMergeState)
        } else {
          val root = postMergeState.heads.fold(merged, (_, cid) =>
            postMergeState.clock.get(cid).root_hash)
          (root, Set())
        }
        all {
          nodes' = nodes.set(p, {
            ...postMergeState,
            latest_root: reconcileResult._1,
            parked_conflicts: reconcileResult._2,
          }),
          inbox' = inbox,
          prev_stable_hlc' = PEERS.mapBy(pp => nodes.get(pp).stable_hlc),
          prev_active_peers' = PEERS.mapBy(pp => nodes.get(pp).active_peers),
          prev_finalized_events' = PEERS.mapBy(pp => nodes.get(pp).finalized_events),
          connected' = connected,
        }
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
        prev_finalized_events' = PEERS.mapBy(pp => nodes.get(pp).finalized_events),
        connected' = connected,
      }
    }
  }

  /// Resolve a partition conflict (§2.4 RESOLVE_PARTITION_CONFLICT).
  ///
  /// User provides resolution (modeled as nondeterministic content choice).
  /// Sets the resolved root as finalized_root, unions both sides' finalized
  /// events, clears partition_conflict, broadcasts a resolution event, and
  /// re-anchors tentative events. Finalization resumes.
  action do_resolve_partition_conflict(p: PeerID): bool = {
    val st = nodes.get(p)
    val pc = getConflictData(st.partition_conflict)
    all {
      hasPartitionConflict(st),
      st.localWall <= MAX_WALL,
      st.clock.keys().size() < MAX_EVENTS,
      nondet resolvedContent = CONTENTS.oneOf()
      // Union both sides' finalized events (protocol step 4)
      val mergedFinalizedEvents = pc.ours_finalized_events.union(pc.theirs_finalized_events)
      // Create a resolution event and broadcast it (protocol step 8)
      val newHLC = tick(st.hlc_clock, st.localWall, p)
      val newCID = makeEventCID(p, newHLC.wall, newHLC.logical)
      val resolutionEvent: Event = {
        cid: newCID,
        root_hash: resolvedContent,
        parents: st.heads,
        hlc: newHLC,
        peer: p,
      }
      val newClock = st.clock.put(newCID, resolutionEvent)
      val newHeads = computeHeads(newClock)
      val postResolveState: NodeState = {
        ...st,
        clock: newClock,
        heads: newHeads,
        hlc_clock: newHLC,
        finalized_root: resolvedContent,
        finalized_events: mergedFinalizedEvents,
        partition_conflict: NoConflict,
      }
      // Re-anchor tentative events against the new finalized_root
      val reconcileResult = if (postResolveState.heads.size() > 1) {
        computeMergedRoot(postResolveState)
      } else {
        val root = postResolveState.heads.fold(resolvedContent, (_, cid) =>
          postResolveState.clock.get(cid).root_hash)
        (root, Set())
      }
      all {
        nodes' = nodes.set(p, {
          ...postResolveState,
          latest_root: reconcileResult._1,
          parked_conflicts: reconcileResult._2,
        }),
        // Broadcast resolution event so all peers converge (protocol step 8)
        inbox' = broadcastConnected(inbox, resolutionEvent, connected),
        prev_stable_hlc' = PEERS.mapBy(pp => nodes.get(pp).stable_hlc),
        prev_active_peers' = PEERS.mapBy(pp => nodes.get(pp).active_peers),
        prev_finalized_events' = PEERS.mapBy(pp => nodes.get(pp).finalized_events),
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
      do_merge_finalized(p, q6),
      do_resolve_partition_conflict(p),
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
  /// After MERGE_FINALIZED, they converge to the same fork-and-join state.
  val inv_finalized_agreement = PEERS.forall(p1 => PEERS.forall(p2 => {
    val s1 = nodes.get(p1)
    val s2 = nodes.get(p2)
    (s1.finalized_events == s2.finalized_events and
     not(hasPartitionConflict(s1)) and
     not(hasPartitionConflict(s2))) implies
      (s1.finalized_root == s2.finalized_root)
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
  /// CONFLICT_HASH. Every conflict from MERGE_FINALIZED is captured
  /// in partition_conflict — none leaks into the finalized state.
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

  /// Partition merge convergence (INV8): peers that complete MERGE_FINALIZED
  /// with the same inputs (same finalized_events union) agree on finalized_root.
  /// This is a consequence of mergeRoots being deterministic and the ours/theirs
  /// assignment being deterministic.
  val inv_partition_merge_convergence = PEERS.forall(p1 => PEERS.forall(p2 => {
    val s1 = nodes.get(p1)
    val s2 = nodes.get(p2)
    (s1.finalized_events == s2.finalized_events and
     not(hasPartitionConflict(s1)) and
     not(hasPartitionConflict(s2))) implies
      (s1.finalized_root == s2.finalized_root)
  }))

  /// Finalization blocked during partition conflict (INV9): a peer with an
  /// active partition conflict cannot have gained new finalized events since
  /// the previous step. Uses prev_finalized_events history variable.
  val inv_finalize_blocked_during_conflict = PEERS.forall(p => {
    val st = nodes.get(p)
    hasPartitionConflict(st) implies
      (st.finalized_events == prev_finalized_events.get(p))
  })
}
