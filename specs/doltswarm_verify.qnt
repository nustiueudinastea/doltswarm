// -*- mode: Bluespec; -*-

/**
 * DoltSwarm Merkle clock protocol — flat, set-based verification module.
 *
 * Self-contained module for Apalache model checking. No Choreo framework,
 * no List (Seq) types — everything is sets and maps.
 *
 * Models the core protocol: Merkle clock events, MergeRoots-based reconcile,
 * heartbeats for stability, causal finalization, staleness/rejection,
 * and merge conflicts.
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
    stale_votes: EventCID -> Set[PeerID],
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
  /// History variable: tracks previous stable_hlc per peer for monotonicity check.
  var prev_stable_hlc: PeerID -> HLC

  // ===========================================================================
  // Helpers
  // ===========================================================================

  pure def makeEventCID(peer: PeerID, wall: int, logical: int): EventCID =
    (peer, wall, logical)

  pure def broadcast(ib: PeerID -> Set[Event], e: Event): PeerID -> Set[Event] =
    ib.keys().mapBy(p => ib.get(p).union(Set(e)))

  /// Deterministic merge model with conflict capability.
  ///
  /// Returns CONFLICT_HASH when two divergent writes conflict (both differ
  /// from ancestor and from each other, AND their combined hash matches
  /// a conflict pattern). This models the fact that Dolt's MergeRoots can
  /// return a conflict when concurrent edits touch the same rows.
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
    // Both diverge from ancestor: conflict when both are from CONTENTS
    // (models concurrent writes to the same table producing a conflict).
    // We use a simple heuristic: if both hashes are "raw content" values
    // (i.e., from CONTENTS set) rather than already-merged results, conflict.
    else if (CONTENTS.contains(ours) and CONTENTS.contains(theirs) and ancestor == EMPTY_ROOT)
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
  pure def computeFinalizedRoot(events: Set[EventCID], clock: EventCID -> Event): Hash = {
    if (events.empty()) EMPTY_ROOT
    else {
      // Only include events still present in the clock (rejected events may be absent)
      val present = events.filter(cid => clock.has(cid))
      if (present.empty()) EMPTY_ROOT
      else {
        val sorted = sortCIDsByHLC(present, clock)
        sorted.foldl(EMPTY_ROOT, (acc, cid) =>
          mergeRoots(acc, clock.get(cid).root_hash, acc)
        )
      }
    }
  }

  /// Deterministic merged root for a set of heads.
  /// Sorts heads by HLC and folds mergeRoots left-to-right.
  /// Returns (merged_root, conflict_cids) where conflict_cids is the set
  /// of head CIDs that produced a conflict during merge.
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
      stale_votes: Map(),
      rejected_events: Set(),
      pending_conflicts: Set(),
      active_peers: PEERS,
      localWall: 1,
    }),
    inbox' = PEERS.mapBy(_ => Set()),
    prev_stable_hlc' = PEERS.mapBy(_ => ZERO),
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
        prev_stable_hlc' = PEERS.mapBy(pp => nodes.get(pp).stable_hlc),
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
      // Check for stale events and cast votes
      val staleEvents = newEvents.filter(e =>
        (st.localWall - e.hlc.wall) > STALENESS_THRESHOLD
      )
      val newStaleVotes = staleEvents.fold(st.stale_votes, (acc, e) =>
        if (acc.has(e.cid))
          acc.setBy(e.cid, voters => voters.union(Set(p)))
        else
          acc.put(e.cid, Set(p))
      )
      all {
        nodes' = nodes.set(p, {
          ...st,
          clock: newClock,
          heads: newHeads,
          hlc_clock: newHLC,
          latest_root: newLatestRoot,
          stale_votes: newStaleVotes,
        }),
        inbox' = inbox,
        prev_stable_hlc' = PEERS.mapBy(pp => nodes.get(pp).stable_hlc),
      }
    }
  }

  /// Propagate stale votes between peers.
  /// Models the gossip of stale votes on the /doltswarm/stale/1.0 topic.
  /// When enough peers vote stale (≥ REJECTION_RATIO%), the event is rejected.
  action do_stale_vote(p: PeerID, q: PeerID): bool = {
    val st_p = nodes.get(p)
    val st_q = nodes.get(q)
    all {
      p != q,
      // q must have stale votes that p doesn't have yet
      val qVotes = st_q.stale_votes
      val pVotes = st_p.stale_votes
      // Find an event that q voted stale on but p hasn't counted q's vote
      nondet eventCID = qVotes.keys().oneOf()
      val qVoters = qVotes.get(eventCID)
      val pVoters = if (pVotes.has(eventCID)) pVotes.get(eventCID) else Set()
      val newVoters = pVoters.union(qVoters)
      all {
        // Only fire if this actually adds new voters
        newVoters.size() > pVoters.size(),
        val updatedVotes = if (pVotes.has(eventCID))
          pVotes.setBy(eventCID, _ => newVoters)
        else
          pVotes.put(eventCID, newVoters)
        // Check rejection threshold: ceil(active_peers * REJECTION_RATIO / 100)
        // Never reject an already-finalized event.
        val threshold = (st_p.active_peers.size() * REJECTION_RATIO + 99) / 100
        val shouldReject = newVoters.size() >= threshold and
          not(st_p.rejected_events.contains(eventCID)) and
          not(st_p.finalized_events.contains(eventCID))
        if (shouldReject and st_p.clock.has(eventCID)) {
          // Reject: remove from clock, recompute heads and latest_root
          val evt = st_p.clock.get(eventCID)
          val cleanedClock = st_p.clock.mapRemove(eventCID)
          val cleanedHeads = computeHeads(cleanedClock)
          // Recompute latest_root: if single head remains, use its root_hash;
          // if no events remain, fall back to finalized_root.
          val newLatestRoot = if (cleanedHeads.size() == 1) {
            cleanedHeads.fold(EMPTY_ROOT, (_, cid) => cleanedClock.get(cid).root_hash)
          } else if (cleanedHeads.size() == 0) {
            st_p.finalized_root
          } else {
            st_p.latest_root  // multiple heads: reconcile will fix it
          }
          all {
            nodes' = nodes.set(p, {
              ...st_p,
              clock: cleanedClock,
              heads: cleanedHeads,
              latest_root: newLatestRoot,
              stale_votes: updatedVotes,
              rejected_events: st_p.rejected_events.union(Set(eventCID)),
              pending_conflicts: st_p.pending_conflicts.setRemove(eventCID),
            }),
            inbox' = inbox,
            prev_stable_hlc' = PEERS.mapBy(pp => nodes.get(pp).stable_hlc),
          }
        } else {
          // Not yet rejected, just update votes
          all {
            nodes' = nodes.set(p, { ...st_p, stale_votes: updatedVotes }),
            inbox' = inbox,
            prev_stable_hlc' = PEERS.mapBy(pp => nodes.get(pp).stable_hlc),
          }
        }
      }
    }
  }

  /// A peer reconciles when it has multiple heads.
  /// Merges are deterministic local computations — no event is created,
  /// nothing is broadcast. Every peer with the same event set independently
  /// computes the same merged root. The next LOCAL_WRITE will have
  /// parents = all current heads, which collapses them in the DAG.
  ///
  /// If mergeRoots returns a conflict, the conflicting head is added to
  /// pending_conflicts and latest_root is not updated for that head.
  action do_reconcile(p: PeerID): bool = {
    val st = nodes.get(p)
    all {
      st.heads.size() > 1,
      val result = computeMergedRoot(st)
      val merged = result._1
      val conflicts = result._2
      all {
        nodes' = nodes.set(p, {
          ...st,
          latest_root: merged,
          pending_conflicts: st.pending_conflicts.union(conflicts),
        }),
        inbox' = inbox,
        prev_stable_hlc' = PEERS.mapBy(pp => nodes.get(pp).stable_hlc),
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
    }
  }

  /// Heartbeat: peer p observes peer q's HLC.
  /// Also updates p's own peer_hlc entry to its current HLC clock,
  /// ensuring that stable_hlc can advance (a peer always knows its own time).
  action do_heartbeat(p: PeerID, q: PeerID): bool = {
    val st = nodes.get(p)
    val remote = nodes.get(q)
    all {
      p != q,
      // Update q's entry from the remote heartbeat
      val updatedPeerHLC = st.peer_hlc
        .set(q, hlcMax(st.peer_hlc.get(q), remote.hlc_clock))
        // Also advance self's entry to current local HLC
        // (a peer always knows its own clock is at least this advanced)
        .set(p, hlcMax(st.peer_hlc.get(p), st.hlc_clock))
      val newStable = hlcMinOver(updatedPeerHLC, st.active_peers)
      all {
        nodes' = nodes.set(p, {
          ...st,
          peer_hlc: updatedPeerHLC,
          stable_hlc: newStable,
        }),
        inbox' = inbox,
        prev_stable_hlc' = PEERS.mapBy(pp => nodes.get(pp).stable_hlc),
      }
    }
  }

  /// Finalize one event below stable_hlc (the one with the smallest HLC).
  /// finalized_root is recomputed from scratch via computeFinalizedRoot to
  /// guarantee that the same finalized_events always yield the same root,
  /// regardless of the order peers finalize events.
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
      nondet q2 = PEERS.oneOf()
      do_stale_vote(p, q2),
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

  // No write during pending conflict: enforced by do_write's precondition
  // `st.pending_conflicts == Set()`. This is a temporal property (action
  // ordering) that cannot be expressed as a state invariant without ghost
  // variables. The precondition prevents do_write from firing when conflicts
  // exist, so the property holds by construction.

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

  /// Rejection threshold: any rejected event must have accumulated enough votes.
  /// (Non-vacuous: do_stale_vote actually rejects events.)
  val inv_rejection_threshold = PEERS.forall(p => {
    val st = nodes.get(p)
    st.rejected_events.forall(cid =>
      // Either it was voted stale with enough votes, or votes were pruned
      not(st.stale_votes.has(cid)) or
      st.stale_votes.get(cid).size() >=
        (st.active_peers.size() * REJECTION_RATIO + 99) / 100
    )
  })

  /// Conflict visibility: every pending conflict references a known event.
  val inv_conflict_visibility = PEERS.forall(p => {
    val st = nodes.get(p)
    st.pending_conflicts.forall(cid =>
      st.clock.has(cid) or st.rejected_events.contains(cid)
    )
  })

  /// Stability monotonicity (INV6): stable_hlc never decreases.
  /// Uses the prev_stable_hlc history variable to compare across transitions.
  val inv_stability_monotonicity = PEERS.forall(p => {
    leq(prev_stable_hlc.get(p), nodes.get(p).stable_hlc)
  })
}
