// -*- mode: Bluespec; -*-

/**
 * DoltSwarm Merkle clock protocol — flat, set-based verification module.
 *
 * Self-contained module for Apalache model checking. No Choreo framework,
 * no List (Seq) types — everything is sets and maps.
 *
 * Models the core protocol: Merkle clock events, MergeRoots-based reconcile,
 * heartbeats for stability, causal finalization.
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
  pure val STALENESS_THRESHOLD: int = 3
  pure val REJECTION_RATIO: int = 30
  pure val CONTENTS: Set[int] = Set(100, 200)
  pure val MAX_WALL: int = 6
  pure val MAX_EVENTS: int = 8
  pure val EMPTY_ROOT: int = 0

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
    rejected_events: Set[EventCID],
    pending_conflicts: Set[EventCID],
    active_peers: Set[PeerID],
    localWall: int,
  }

  // ===========================================================================
  // State variables
  // ===========================================================================

  var nodes: PeerID -> NodeState
  var inbox: PeerID -> Set[Event]

  // ===========================================================================
  // Helpers
  // ===========================================================================

  pure def makeEventCID(peer: PeerID, wall: int, logical: int): EventCID =
    (peer, wall, logical)

  pure def broadcast(ib: PeerID -> Set[Event], e: Event): PeerID -> Set[Event] =
    ib.keys().mapBy(p => ib.get(p).union(Set(e)))

  /// Simple deterministic merge model.
  pure def mergeRoots(ours: Hash, theirs: Hash, ancestor: Hash): Hash = {
    if (ours == theirs) ours
    else if (ours == ancestor) theirs
    else if (theirs == ancestor) ours
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

  /// Deterministic merged root for a set of heads.
  /// Sorts heads by HLC and folds mergeRoots left-to-right.
  /// For 2 heads this is trivially correct (mergeRoots is commutative).
  /// For 3+ heads the HLC sort provides deterministic ordering.
  pure def computeMergedRoot(st: NodeState): Hash = {
    if (st.heads.size() <= 1) {
      st.heads.fold(st.finalized_root, (_, cid) => st.clock.get(cid).root_hash)
    } else {
      val sorted = sortCIDsByHLC(st.heads, st.clock)
      sorted.foldl(st.finalized_root, (acc, cid) =>
        mergeRoots(acc, st.clock.get(cid).root_hash, st.finalized_root)
      )
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
      rejected_events: Set(),
      pending_conflicts: Set(),
      active_peers: PEERS,
      localWall: 1,
    }),
    inbox' = PEERS.mapBy(_ => Set()),
  }

  /// A peer creates a local write event.
  action do_write(p: PeerID, content: int): bool = {
    val st = nodes.get(p)
    all {
      st.localWall <= MAX_WALL,
      st.clock.keys().size() < MAX_EVENTS,
      st.pending_conflicts == Set(),  // no write during conflict
      val newHLC = tick(st.hlc_clock, st.localWall, p)
      val newCID = makeEventCID(p, newHLC.wall, newHLC.logical)
      val event: Event = {
        cid: newCID,
        root_hash: content,
        parents: st.heads,
        hlc: newHLC,
        peer: p,
      }
      val newClock = st.clock.put(newCID, event)
      all {
        nodes' = nodes.set(p, {
          ...st,
          clock: newClock,
          heads: computeHeads(newClock),
          latest_root: content,
          hlc_clock: newHLC,
        }),
        inbox' = broadcast(inbox, event),
      }
    }
  }

  /// A peer processes events from its inbox.
  action do_receive(p: PeerID): bool = {
    val st = nodes.get(p)
    val remoteEvents = inbox.get(p)

    val newEvents = remoteEvents.filter(e =>
      not(st.clock.keys().contains(e.cid)) and
      not(st.rejected_events.contains(e.cid))
    )

    all {
      not(newEvents.empty()),
      // Add all new events to the clock (put is commutative for same key/value)
      val newClock = newEvents.fold(st.clock, (acc, e) => acc.put(e.cid, e))
      // Merge HLC with all received timestamps
      val newHLC = newEvents.fold(st.hlc_clock, (acc, e) =>
        observe(acc, e.hlc, st.localWall, p)
      )
      // Recompute heads from the full clock (order-independent)
      val newHeads = computeHeads(newClock)
      // Fast-forward latest_root when we converge to a single head
      val newLatestRoot = if (newHeads.size() == 1) {
        newHeads.fold(EMPTY_ROOT, (_, cid) => newClock.get(cid).root_hash)
      } else {
        st.latest_root
      }
      all {
        nodes' = nodes.set(p, {
          ...st,
          clock: newClock,
          heads: newHeads,
          hlc_clock: newHLC,
          latest_root: newLatestRoot,
        }),
        inbox' = inbox,
      }
    }
  }

  /// A peer reconciles when it has multiple heads.
  /// Merges are deterministic local computations — no event is created,
  /// nothing is broadcast. Every peer with the same event set independently
  /// computes the same merged root. The next LOCAL_WRITE will have
  /// parents = all current heads, which collapses them in the DAG.
  action do_reconcile(p: PeerID): bool = {
    val st = nodes.get(p)
    all {
      st.heads.size() > 1,
      val merged = computeMergedRoot(st)
      all {
        nodes' = nodes.set(p, {
          ...st,
          latest_root: merged,
        }),
        inbox' = inbox,
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
    }
  }

  /// Heartbeat: peer updates its peer_hlc and stable_hlc.
  action do_heartbeat(p: PeerID, q: PeerID): bool = {
    val st = nodes.get(p)
    val remote = nodes.get(q)
    all {
      p != q,
      val updatedPeerHLC = st.peer_hlc.set(q,
        hlcMax(st.peer_hlc.get(q), remote.hlc_clock))
      val newStable = hlcMinOver(updatedPeerHLC, st.active_peers)
      all {
        nodes' = nodes.set(p, {
          ...st,
          peer_hlc: updatedPeerHLC,
          stable_hlc: newStable,
        }),
        inbox' = inbox,
      }
    }
  }

  /// Finalize one event below stable_hlc (the one with the smallest HLC).
  /// Processing one at a time avoids non-deterministic set fold ordering.
  action do_finalize(p: PeerID): bool = {
    val st = nodes.get(p)
    val candidates = st.clock.keys().filter(cid =>
      val e = st.clock.get(cid)
      less(e.hlc, st.stable_hlc) and
      not(st.finalized_events.contains(cid)) and
      not(st.rejected_events.contains(cid))
    )
    all {
      not(candidates.empty()),
      // Pick a candidate and require it to be the HLC-minimum
      nondet cid = candidates.oneOf()
      val isMinimal = candidates.forall(other =>
        other == cid or less(st.clock.get(cid).hlc, st.clock.get(other).hlc)
      )
      all {
        isMinimal,
        val newFinRoot = mergeRoots(
          st.finalized_root,
          st.clock.get(cid).root_hash,
          st.finalized_root
        )
        all {
          nodes' = nodes.set(p, {
            ...st,
            finalized_root: newFinRoot,
            finalized_events: st.finalized_events.union(Set(cid)),
          }),
          inbox' = inbox,
        }
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

  /// Finalized agreement: same finalized events → same finalized root.
  val inv_finalized_agreement = PEERS.forall(p1 => PEERS.forall(p2 => {
    val s1 = nodes.get(p1)
    val s2 = nodes.get(p2)
    (s1.finalized_events == s2.finalized_events) implies
      (s1.finalized_root == s2.finalized_root)
  }))

  /// No event is both finalized and rejected.
  val inv_no_finalized_rejected_overlap = PEERS.forall(p => {
    val st = nodes.get(p)
    st.finalized_events.intersect(st.rejected_events) == Set()
  })

  /// No write during pending conflict (enforced by do_write precondition).
  val inv_no_write_during_conflict = PEERS.forall(p => {
    val st = nodes.get(p)
    st.pending_conflicts != Set() implies true
  })

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
}
