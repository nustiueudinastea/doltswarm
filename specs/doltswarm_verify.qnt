// -*- mode: Bluespec; -*-

module doltswarm_verify {
  import basicSpells.* from "./spells/basicSpells"
  import hlc.* from "./hlc"
  import doltswarm_model.* from "./model/doltswarm_model"

  /// Bounded whole-model regression wrapper over `./model/doltswarm_model.qnt`.
  /// Protocol semantics, state, and actions live in the model module; this
  /// wrapper keeps the executable invariant suite.

  // ===========================================================================
  // Invariants
  // ===========================================================================
  // @default_steps annotations are benchmark-derived suite defaults consumed by
  // specs/tooling/quint_invariants.py and task quint:run. Pass STEPS=... to override them.

  /// INV1b retained-structure convergence:
  /// peers with the same retained clock keys have the same derived heads.
  /// INV1a logical identity is represented by logicalEventIDs(clock ∪ finalized).
  /// @default_steps 20
  val inv_retained_structure_convergence = PEERS.forall(p1 => PEERS.forall(p2 => {
    val s1 = nodes.get(p1)
    val s2 = nodes.get(p2)
    (s1.clock.keys() == s2.clock.keys()) implies (s1.head_event_ids == s2.head_event_ids)
  }))

  /// Current finalized commit must match the finalized content root plus the
  /// actual parent set of the materialized finalized commit at the tip.
  /// @default_steps 20
  val inv_finalized_commit_id_matches_state = PEERS.forall(p => {
    val st = nodes.get(p)
    st.finalized_commit_id == derivedFinalizedCommitID(st)
  })

  /// Finalized agreement (INV3, tip form): same finalized commit id implies
  /// the same finalized content root. History equality itself is carried by the
  /// finalized commit id plus derived lineage metadata, not by finalized_events.
  /// @default_steps 20
  val inv_finalized_agreement = PEERS.forall(p1 => PEERS.forall(p2 => {
    val s1 = nodes.get(p1)
    val s2 = nodes.get(p2)
    (s1.finalized_commit_id == s2.finalized_commit_id) implies
      (s1.finalized_prolly_root_hash == s2.finalized_prolly_root_hash)
  }))

  /// Identity-separation regression: finalized-history identity depends on the
  /// exact parent set, not only on the prolly-root hash.
  /// @default_steps 12
  val inv_same_root_different_parents_distinct_finalized_commit_ids = all {
    val parentA = makeFinalizedCheckpointCommitID(11, Set())
    val parentB = makeFinalizedCheckpointCommitID(12, Set())
    val childOnA = makeFinalizedCheckpointCommitID(13, Set(parentA))
    val childOnB = makeFinalizedCheckpointCommitID(13, Set(parentB))
    val mergeChild = makeFinalizedMergeCommitID(13, Set(parentA, parentB))
    all {
      childOnA != childOnB,
      childOnA != mergeChild,
      childOnB != mergeChild,
    }
  }

  /// Identity-separation regression: two distinct events may legitimately
  /// carry the same prolly-root hash without collapsing event history.
  /// @default_steps 12
  val inv_same_prolly_root_does_not_collapse_event_history = all {
    val event1 = seedEvent(1, 1, 0, 11, EMPTY_ROOT, Set())
    val event2 = seedEvent(2, 1, 0, 11, EMPTY_ROOT, Set(event1.event_id))
    val seedClock = Map()
      .put(event1.event_id, event1)
      .put(event2.event_id, event2)
    all {
      event1.event_id != event2.event_id,
      event1.prolly_root_hash == event2.prolly_root_hash,
      seedClock.keys().size() == 2,
      computeHeadEventIDs(seedClock) == Set(event2.event_id),
      ancestorIdClosure(Set(event2.event_id), seedClock) == Set(event1.event_id, event2.event_id),
    }
  }

  /// Finalized lineage well-formedness:
  /// - EMPTY_FINALIZED_COMMIT_ID is present
  /// - finalized_commit_id is present in lineage known finalized commits
  /// - every known finalized commit is its own ancestor
  /// - every parent edge points to a strictly shallower finalized commit (derived depth)
  /// @default_steps 20
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
  /// @default_steps 20
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
  /// @default_steps 10
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
  /// @default_steps 20
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

  /// SYNC_FINALIZED only adds finalized-lineage knowledge:
  /// parent metadata is monotone, checkpoint descriptors are union-preserving,
  /// and durable resolved finalized-conflict ids never move backward.
  /// @default_steps 20
  val inv_sync_lineage_union_monotone = PEERS.forall(p => PEERS.forall(q => {
    val st_p = nodes.get(p)
    val st_q = nodes.get(q)
    val mergedParents = mergeSetMap(st_p.finalized_commit_parents, st_q.finalized_commit_parents)
    val mergedCheckpoints = mergeFinalizedCheckpointMap(
      st_p.finalized_checkpoint_events,
      st_q.finalized_checkpoint_events
    )
    val mergedResolvedIDs = st_p.resolved_finalized_conflict_ids
      .union(st_q.resolved_finalized_conflict_ids)
    val synced = applySyncFinalized(st_p, st_q)
    syncEnabled(connected, p, q, st_p, st_q) implies all {
      mergedParents.keys().forall(k =>
        synced.finalized_commit_parents.has(k) and
          mergedParents.get(k).subseteq(synced.finalized_commit_parents.get(k))
      ),
      mergedCheckpoints.keys().forall(k =>
        synced.finalized_checkpoint_events.has(k) and
          synced.finalized_checkpoint_events.get(k) == mergedCheckpoints.get(k)
      ),
      mergedResolvedIDs.subseteq(synced.resolved_finalized_conflict_ids),
    }
  }))

  /// SYNC_FINALIZED merge branch must record lineage links to both input
  /// finalized commits (excluding self-edge if resulting finalized commit equals one input).
  /// @default_steps 20
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

  /// SYNC_FINALIZED merge branch reduces the pairwise finalized frontier to a
  /// single descendant tip while preserving both inputs as ancestors.
  /// @default_steps 20
  val inv_sync_merge_frontier_reduction = PEERS.forall(p => PEERS.forall(q => {
    val st_p = nodes.get(p)
    val st_q = nodes.get(q)
    val synced = applySyncFinalized(st_p, st_q)
    val frontierAfter = finalizedTipFrontier(
      Set(st_p.finalized_commit_id, st_q.finalized_commit_id, synced.finalized_commit_id),
      synced.finalized_commit_parents
    )
    (syncEnabled(connected, p, q, st_p, st_q) and syncMode(st_p, st_q) == SYNC_MERGE) implies all {
      frontierAfter == Set(synced.finalized_commit_id),
      finalizedDescendsFromAll(
        synced.finalized_commit_id,
        Set(st_p.finalized_commit_id, st_q.finalized_commit_id),
        synced.finalized_commit_parents
      ),
    }
  }))

  /// Nested partition-heal regression:
  /// different 3-way heal orders may produce different intermediate merge
  /// commits, but those commits must themselves be merge-reconcilable later
  /// without rewriting earlier finalized history.
  /// @default_steps 20
  val inv_sync_nested_merge_reconcilable = PEERS.forall(p => PEERS.forall(q => PEERS.forall(r => {
    val st_p = nodes.get(p)
    val st_q = nodes.get(q)
    val st_r = nodes.get(r)
    val left = applySyncFinalized(applySyncFinalized(st_p, st_q), st_r)
    val right = applySyncFinalized(st_p, applySyncFinalized(st_q, st_r))
    val reconciled = applySyncFinalized(left, right)
    val reconciledRev = applySyncFinalized(right, left)
    val frontierAfter = finalizedTipFrontier(
      Set(
        st_p.finalized_commit_id,
        st_q.finalized_commit_id,
        st_r.finalized_commit_id,
        reconciled.finalized_commit_id
      ),
      reconciled.finalized_commit_parents
    )
    (p != q and q != r and p != r and
      syncEnabled(connected, p, q, st_p, st_q) and
      syncEnabled(connected, q, r, st_q, st_r) and
      syncEnabled(connected, p, r, st_p, st_r) and
      syncMode(st_p, st_q) == SYNC_MERGE and
      syncMode(st_q, st_r) == SYNC_MERGE and
      syncMode(st_p, st_r) == SYNC_MERGE) implies all {
      sameFinalizedState(reconciled, reconciledRev),
      frontierAfter == Set(reconciled.finalized_commit_id),
      finalizedDescendsFromAll(
        reconciled.finalized_commit_id,
        Set(st_p.finalized_commit_id, st_q.finalized_commit_id, st_r.finalized_commit_id),
        reconciled.finalized_commit_parents
      ),
    }
  })))

  /// SYNC_FINALIZED contract (adopt branch): behind peer adopts remote
  /// finalized root/events while preserving unresolved conflict visibility.
  /// @default_steps 20
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
  /// @default_steps 20
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
  /// @default_steps 20
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
      // Tentative projection is recomputed from post-compaction state, not
      // preserved from pre-sync. Equal-tip metadata repair can expand
      // finalized_events, causing compaction of retained finalized heads,
      // which changes head_event_ids and invalidates the old projection.
      synced.tentative_prolly_root_hash == computeTentativeProjection(synced)._1,
      synced.parked_conflicts == computeTentativeProjection(synced)._2,
      synced.projection_basis == computeTentativeProjection(synced)._3,
    }
  }))

  /// Equal-tip finalized sync is a metadata repair no-op for the finalized
  /// commit DAG: tip/root stay fixed while lineage/checkpoint/conflict metadata
  /// is merged.
  /// @default_steps 20
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
  /// @default_steps 20
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
  /// @default_steps 20
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
  /// @default_steps 20
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
  /// @default_steps 20
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
  /// @default_steps 20
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
  /// @default_steps 20
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

  /// Finalized-conflict lifecycle regression: once a resolving claim hardens
  /// into finalized state, the durable resolved-id set carries the truth and
  /// later finalized sync must preserve that outcome.
  /// @default_steps 20
  val inv_resolved_finalized_conflict_hardens_and_syncs = PEERS.forall(p => PEERS.forall(q => {
    val st = nodes.get(p)
    val st_q = nodes.get(q)
    val nextEventID = nextFinalizationEventID(st)
    val post = finalizeResult(st)
    (finalizeEnabled(st) and nextEventID != EMPTY_EVENT_ID) implies
      match st.clock.get(nextEventID).resolved_finalized_conflict_id {
        | Some(conflictID) => all {
          post.resolved_finalized_conflict_ids.contains(conflictID),
          not(post.finalized_conflicts.has(conflictID)),
          (syncEnabled(allPairs(PEERS), p, q, post, st_q)) implies all {
            applySyncFinalized(post, st_q).resolved_finalized_conflict_ids.contains(conflictID),
            not(applySyncFinalized(post, st_q).finalized_conflicts.has(conflictID)),
          },
          (syncEnabled(allPairs(PEERS), q, p, st_q, post)) implies all {
            applySyncFinalized(st_q, post).resolved_finalized_conflict_ids.contains(conflictID),
            not(applySyncFinalized(st_q, post).finalized_conflicts.has(conflictID)),
          },
        }
        | None => true
      }
  }))

  /// Liveness-oriented bounded scenario checks (fairness approximations):
  /// - heartbeats/sync actions eventually fire on connected peers
  /// - closure/chunk readiness actions eventually complete for reachable events
  /// These are not full temporal liveness proofs; they are regression guards.

  /// Scenario check: ancestor/descendant finalized-root pair converges when the
  /// ahead peer no-ops and the behind peer syncs back.
  /// @default_steps 20
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
  /// @default_steps 20
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

  /// Deterministic fair-healing approximation for the 3-peer model:
  /// in a fully connected swarm with no new writes, one full ordered-pair sync
  /// round must not increase the incomparable finalized frontier.
  /// @default_steps 20
  val inv_fullmesh_heal_round_frontier_nonincreasing = {
    val healed = healRoundFullMesh(nodes)
    allPairs(PEERS).subseteq(connected) implies
      globalFinalizedFrontier(healed).size() <= globalFinalizedFrontier(nodes).size()
  }

  /// Rank/progress phase 1 for deterministic full-mesh healing:
  /// while more than one incomparable finalized tip remains, one heal round
  /// must strictly reduce the finalized frontier rank.
  /// @default_steps 20
  val inv_fullmesh_frontier_rank_strict_progress = {
    val healed = healRoundFullMesh(nodes)
    (allPairs(PEERS).subseteq(connected) and
      globalFinalizedFrontierRank(nodes) > 0) implies
      globalFinalizedFrontierRank(healed) < globalFinalizedFrontierRank(nodes)
  }

  /// Connected-component bounded-growth check for one heal step:
  /// a single SYNC_FINALIZED application may materialize one fresh
  /// intermediate merge tip before later rounds reconcile it, so the
  /// component frontier may grow transiently, but by at most one incomparable
  /// tip. If it grows, the new local tip must descend from both synced inputs.
  /// @default_steps 20
  val inv_heal_frontier_growth_bounded = PEERS.forall(p => PEERS.forall(q => {
    val st_p = nodes.get(p)
    val st_q = nodes.get(q)
    val component = connectedComponent(connected, p)
    val before = finalizedFrontierFor(nodes, component)
    val afterNodes = nodes.set(p, applySyncFinalized(st_p, st_q))
    val after = finalizedFrontierFor(afterNodes, component)
    val afterTip = afterNodes.get(p).finalized_commit_id
    val afterParents = mergedFinalizedParentsFor(afterNodes, component)
    syncEnabled(connected, p, q, st_p, st_q) implies
      all {
        after.size() <= before.size() + 1,
        (after.size() > before.size()) implies all {
          syncMode(st_p, st_q) == SYNC_MERGE,
          after.contains(afterTip),
          finalizedDescendsFromAll(
            afterTip,
            Set(st_p.finalized_commit_id, st_q.finalized_commit_id),
            afterParents
          ),
        },
      }
  }))

  /// If SYNC_MERGE joins two current maximal incomparable frontier tips in a
  /// connected component, that component frontier size drops by exactly one.
  /// @default_steps 20
  val inv_frontier_merge_strictly_decreases = PEERS.forall(p => PEERS.forall(q => {
    val st_p = nodes.get(p)
    val st_q = nodes.get(q)
    val component = connectedComponent(connected, p)
    val before = finalizedFrontierFor(nodes, component)
    val afterNodes = nodes.set(p, applySyncFinalized(st_p, st_q))
    val after = finalizedFrontierFor(afterNodes, component)
    (syncEnabled(connected, p, q, st_p, st_q) and
      syncMode(st_p, st_q) == SYNC_MERGE and
      before.contains(st_p.finalized_commit_id) and
      before.contains(st_q.finalized_commit_id)) implies
      after.size() + 1 == before.size()
  }))

  /// Bounded eventual-convergence regression for the 3-peer model:
  /// under a deterministic full-mesh sync schedule and no new writes, repeated
  /// heal rounds should collapse the finalized frontier to one shared tip while
  /// preserving all initially-visible finalized tips as ancestors.
  /// @default_steps 20
  val inv_fullmesh_heal_bounded_converges = {
    val initialTips = finalizedTipSetAcross(nodes)
    val healed = healRoundsFullMesh(nodes, FULL_MESH_HEAL_ROUNDS)
    val healedParents = mergedFinalizedParentsAcross(healed)
    allPairs(PEERS).subseteq(connected) implies all {
      allPeersSameFinalizedState(healed),
      globalFinalizedFrontier(healed) == Set(healed.get(1).finalized_commit_id),
      finalizedDescendsFromAll(
        healed.get(1).finalized_commit_id,
        initialTips,
        healedParents
      ),
    }
  }

  /// Alternate fair schedules should also avoid frontier growth in one round.
  /// This does not prove arbitrary-schedule liveness, but it checks that the
  /// bounded healing story is not an artifact of one hard-coded pair order.
  /// @default_steps 20
  val inv_alt_scheduled_heal_round_frontier_nonincreasing = {
    val healedReverse = healRoundScheduled(nodes, FAIR_HEAL_SCHEDULE_REVERSE)
    val healedRotated = healRoundScheduled(nodes, FAIR_HEAL_SCHEDULE_ROTATED)
    allPairs(PEERS).subseteq(connected) implies all {
      globalFinalizedFrontier(healedReverse).size() <= globalFinalizedFrontier(nodes).size(),
      globalFinalizedFrontier(healedRotated).size() <= globalFinalizedFrontier(nodes).size(),
    }
  }

  /// Bounded convergence regression for two alternate fair schedules.
  /// This strengthens the partition-heal checks beyond the default full-mesh
  /// ordered-pair sequence without claiming a full temporal fairness proof.
  /// @default_steps 20
  val inv_alt_scheduled_heal_bounded_converges = {
    val initialTips = finalizedTipSetAcross(nodes)
    val healedReverse = healRoundsScheduled(nodes, FAIR_HEAL_SCHEDULE_REVERSE, FULL_MESH_HEAL_ROUNDS)
    val healedRotated = healRoundsScheduled(nodes, FAIR_HEAL_SCHEDULE_ROTATED, FULL_MESH_HEAL_ROUNDS)
    val reverseParents = mergedFinalizedParentsAcross(healedReverse)
    val rotatedParents = mergedFinalizedParentsAcross(healedRotated)
    allPairs(PEERS).subseteq(connected) implies all {
      allPeersSameFinalizedState(healedReverse),
      globalFinalizedFrontier(healedReverse) == Set(healedReverse.get(1).finalized_commit_id),
      finalizedDescendsFromAll(
        healedReverse.get(1).finalized_commit_id,
        initialTips,
        reverseParents
      ),
      allPeersSameFinalizedState(healedRotated),
      globalFinalizedFrontier(healedRotated) == Set(healedRotated.get(1).finalized_commit_id),
      finalizedDescendsFromAll(
        healedRotated.get(1).finalized_commit_id,
        initialTips,
        rotatedParents
      ),
    }
  }

  /// Once every peer in a connected component already shares the same
  /// finalized state, one more local heal attempt inside that component must
  /// keep the component converged and preserve its unique frontier tip.
  /// @default_steps 20
  val inv_component_converged_frontier_absorbing = PEERS.forall(p => PEERS.forall(q => {
    val st_p = nodes.get(p)
    val st_q = nodes.get(q)
    val component = connectedComponent(connected, p)
    val before = finalizedFrontierFor(nodes, component)
    val afterNodes =
      if (syncEnabled(connected, p, q, st_p, st_q))
        nodes.set(p, applySyncFinalized(st_p, st_q))
      else
        nodes
    val after = finalizedFrontierFor(afterNodes, component)
    (before.size() == 1 and peersShareSameFinalizedState(nodes, component)) implies
      all {
        after.size() == 1,
        peersShareSameFinalizedState(afterNodes, component),
      }
  }))

  /// Rank/progress phase 2 for deterministic full-mesh healing:
  /// once the finalized frontier rank reaches zero (one maximal tip remains),
  /// one more full-mesh round must make all peers share the same finalized state.
  /// @default_steps 20
  val inv_fullmesh_unique_frontier_round_converges = {
    val healed = healRoundFullMesh(nodes)
    (allPairs(PEERS).subseteq(connected) and
      globalFinalizedFrontierRank(nodes) == 0) implies
      allPeersSameFinalizedState(healed)
  }

  /// Sync-only absorbing property for a converged healed full mesh:
  /// if all peers already share the same finalized state, one deterministic
  /// full-mesh heal round leaves finalized state unchanged.
  /// @default_steps 20
  val inv_converged_finalized_state_absorbing = {
    val healed = healRoundFullMesh(nodes)
    (allPairs(PEERS).subseteq(connected) and allPeersSameFinalizedState(nodes)) implies
      sameFinalizedStateAcross(healed, nodes)
  }

  /// Once the bounded heal schedule has converged, one more full-mesh heal round
  /// should be a finalized-state fixpoint.
  /// @default_steps 20
  val inv_fullmesh_heal_converged_absorbing = {
    val healed = healRoundsFullMesh(nodes, FULL_MESH_HEAL_ROUNDS)
    val healedAgain = healRoundFullMesh(healed)
    allPairs(PEERS).subseteq(connected) implies
      sameFinalizedStateAcross(healedAgain, healed)
  }

  /// Data convergence: peers with the same tentative-projection inputs
  /// (retained clock/chunks plus finalized boundary inputs) must derive the
  /// same reconcile projection whenever at least one tentative head is mergeable.
  /// @default_steps 20
  val inv_data_convergence = PEERS.forall(p1 => PEERS.forall(p2 => {
    val s1 = nodes.get(p1)
    val s2 = nodes.get(p2)
    (sameTentativeProjectionInputs(s1, s2) and
     not(mergeableHeadEventIDs(s1).empty())) implies
      (s1.tentative_prolly_root_hash == s2.tentative_prolly_root_hash and
       s1.parked_conflicts == s2.parked_conflicts and
       s1.projection_basis == s2.projection_basis)
  }))

  /// Causal consistency (INV4): if e1 is a parent of e2 in the clock DAG,
  /// then e1 precedes e2 in the HLC total order. Since HLC total order is
  /// transitive, checking direct parents suffices for full ancestry chains.
  /// @default_steps 20
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
  /// @default_steps 20
  val inv_no_silent_data_loss = PEERS.forall(p => {
    val st = nodes.get(p)
    st.finalized_events.keys().forall(event_id =>
      not(st.clock.has(event_id)) or st.finalized_events.get(event_id) == st.clock.get(event_id).prolly_root_hash
    )
  })

  /// Finalized-event bookkeeping is derived from the finalized commit DAG plus
  /// checkpoint descriptors, not synchronized as an authoritative catalog.
  /// @default_steps 20
  val inv_finalized_events_derived_from_checkpoints = PEERS.forall(p => {
    val st = nodes.get(p)
    st.finalized_events == derivedFinalizedEventMap(
      st.finalized_commit_id,
      st.finalized_commit_parents,
      st.finalized_checkpoint_events
    )
  })

  /// Checkpoint-descriptor bookkeeping must agree with any retained event
  /// body that remains locally materialized in clock.
  /// @default_steps 20
  val inv_checkpoint_descriptor_root_consistency = PEERS.forall(p => {
    val st = nodes.get(p)
    st.finalized_checkpoint_events.keys().forall(finalized_commit =>
      st.finalized_checkpoint_events.get(finalized_commit).keys().forall(event_id =>
        not(st.clock.has(event_id)) or
          st.clock.get(event_id).prolly_root_hash
            == st.finalized_checkpoint_events.get(finalized_commit).get(event_id)
      )
    )
  })

  /// Finalized-boundary invariants in the full swarm model are kept here as
  /// bounded regression checks and executable documentation. The canonical
  /// Apalache proofs for these action-local frame properties live in the
  /// dedicated harness modules under `./proofs/finalized_boundary/`.

  /// LOCAL_WRITE's pre-reconcile state update is tentative-only; it must not
  /// directly mutate finalized state.
  /// Bounded regression check only; canonical proof:
  /// `./proofs/finalized_boundary/write.qnt:inv_write_preserves_finalized_state_inductive`.
  /// @default_steps 6
  val inv_write_base_preserves_finalized_state = PEERS.forall(p => {
    val st = nodes.get(p)
    writeEnabled(st) implies CONTENTS.forall(content => {
      val postWriteState = writeBaseResult(st, p, content)
      sameFinalizedState(postWriteState, st)
    })
  })

  /// Reconcile over LOCAL_WRITE's pre-reconcile state remains tentative-only;
  /// it must not mutate finalized state relative to that intermediate state.
  /// Bounded regression check only; canonical proof:
  /// `./proofs/finalized_boundary/write.qnt:inv_write_preserves_finalized_state_inductive`.
  /// @default_steps 6
  val inv_write_reconcile_preserves_finalized_state = PEERS.forall(p => {
    val st = nodes.get(p)
    writeEnabled(st) implies CONTENTS.forall(content => {
      val postWriteState = writeBaseResult(st, p, content)
      val post = reconcileResult(postWriteState)
      sameFinalizedState(post, postWriteState)
    })
  })

  /// LOCAL_WRITE is the composition of a tentative-only write-base update and
  /// a tentative-only reconcile pass, so finalized state is preserved across
  /// the whole helper transition as well.
  /// Bounded regression check only; canonical proof:
  /// `./proofs/finalized_boundary/write.qnt:inv_write_preserves_finalized_state_inductive`.
  /// @default_steps 3
  val inv_write_preserves_finalized_state = all {
    inv_write_base_preserves_finalized_state,
    inv_write_reconcile_preserves_finalized_state,
  }

  /// RECEIVE_EVENT's pre-reconcile local ingestion step is tentative-only; it
  /// must not directly mutate finalized state.
  /// Bounded regression check only; canonical proof:
  /// `./proofs/finalized_boundary/receive.qnt:inv_receive_preserves_finalized_state_inductive`.
  /// @default_steps 20
  val inv_receive_base_preserves_finalized_state = PEERS.forall(p => {
    val st = nodes.get(p)
    val processable = inbox.get(p).filter(e =>
      validEvent(e) and
      not(st.clock.keys().contains(e.event_id)) and
      not(st.finalized_events.has(e.event_id))
    )
    processable.forall(e => {
      val postReceiveState = receiveBaseResult(st, p, e)
      sameFinalizedState(postReceiveState, st)
    })
  })

  /// Reconcile over RECEIVE_EVENT's pre-reconcile state remains tentative-only;
  /// it must not mutate finalized state relative to that intermediate state.
  /// Bounded regression check only; canonical proof:
  /// `./proofs/finalized_boundary/receive.qnt:inv_receive_preserves_finalized_state_inductive`.
  /// @default_steps 20
  val inv_receive_reconcile_preserves_finalized_state = PEERS.forall(p => {
    val st = nodes.get(p)
    val processable = inbox.get(p).filter(e =>
      validEvent(e) and
      not(st.clock.keys().contains(e.event_id)) and
      not(st.finalized_events.has(e.event_id))
    )
    processable.forall(e => {
      val postReceiveState = receiveBaseResult(st, p, e)
      val post = reconcileResult(postReceiveState)
      sameFinalizedState(post, postReceiveState)
    })
  })

  /// RECEIVE_EVENT is the composition of a tentative-only base ingestion and a
  /// tentative-only reconcile pass, so finalized state is preserved across the
  /// whole helper transition as well.
  /// Bounded regression check only; canonical proof:
  /// `./proofs/finalized_boundary/receive.qnt:inv_receive_preserves_finalized_state_inductive`.
  /// @default_steps 20
  val inv_receive_preserves_finalized_state = all {
    inv_receive_base_preserves_finalized_state,
    inv_receive_reconcile_preserves_finalized_state,
  }

  /// RECONCILE is a tentative-layer projection only; it must never mutate the
  /// finalized catalog, lineage, or finalized conflict state.
  /// Bounded regression check only; canonical proof:
  /// `./proofs/finalized_boundary/reconcile.qnt:inv_reconcile_preserves_finalized_state_inductive`.
  /// @default_steps 20
  val inv_reconcile_preserves_finalized_state = PEERS.forall(p => {
    val st = nodes.get(p)
    val post = reconcileResult(st)
    not(reconcileEnabled(st)) or sameFinalizedState(post, st)
  })

  /// GC_CLOCK's pre-reconcile pruning step is local resource management only;
  /// it must not directly mutate finalized history or finalized-layer conflict
  /// metadata.
  /// Bounded regression check only; canonical proof:
  /// `./proofs/finalized_boundary/gc.qnt:inv_gc_preserves_finalized_state_inductive`.
  /// @default_steps 20
  val inv_gc_base_preserves_finalized_state = PEERS.forall(p => {
    val st = nodes.get(p)
    gcPruneable(st).forall(event_id => {
      val postGcState = gcBaseResult(st, p, event_id)
      sameFinalizedState(postGcState, st)
    })
  })

  /// Reconcile over GC_CLOCK's pre-reconcile state remains tentative-only; it
  /// must not mutate finalized state relative to that intermediate state.
  /// Bounded regression check only; canonical proof:
  /// `./proofs/finalized_boundary/gc.qnt:inv_gc_preserves_finalized_state_inductive`.
  /// @default_steps 20
  val inv_gc_reconcile_preserves_finalized_state = PEERS.forall(p => {
    val st = nodes.get(p)
    gcPruneable(st).forall(event_id => {
      val postGcState = gcBaseResult(st, p, event_id)
      val post = reconcileResult(postGcState)
      sameFinalizedState(post, postGcState)
    })
  })

  /// GC_CLOCK is the composition of a tentative-only prune step and a
  /// tentative-only reconcile pass, so finalized state is preserved across the
  /// whole helper transition as well.
  /// Bounded regression check only; canonical proof:
  /// `./proofs/finalized_boundary/gc.qnt:inv_gc_preserves_finalized_state_inductive`.
  /// @default_steps 20
  val inv_gc_preserves_finalized_state = all {
    inv_gc_base_preserves_finalized_state,
    inv_gc_reconcile_preserves_finalized_state,
  }

  /// EVICT_PEER changes only witness membership; it must not directly mutate
  /// finalized history or finalized-layer conflict metadata.
  /// Bounded regression check only; canonical proof:
  /// `./proofs/finalized_boundary/evict.qnt:inv_evict_preserves_finalized_state_inductive`.
  /// @default_steps 20
  val inv_evict_preserves_finalized_state = PEERS.forall(p => {
    val st = nodes.get(p)
    PEERS.filter(q =>
      q != p and st.active_peers.contains(q) and not(isConnected(connected, p, q))
    ).forall(q => {
      val post = evictResult(st, q)
      sameFinalizedState(post, st)
    })
  })

  /// Normal finalization extends finalized lineage with at most one new edge:
  /// from the new finalized_commit_id back to the prior finalized_commit_id.
  /// This is the model-level form of "merge commits only when finalized
  /// lineages meet" — do_finalize itself must not synthesize multi-parent joins.
  /// @default_steps 19
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
  /// @default_steps 19
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

  /// Replay-base reconstruction must stay defined for non-subsumed eligible
  /// events: their finalized parents must determine one deterministic parent
  /// basis root.
  /// @default_steps 20
  val inv_finalization_replay_base_is_defined = PEERS.forall(p => {
    val st = nodes.get(p)
    finalizationEligible(st).exclude(subsumedEvents(st.clock)).forall(event_id =>
      finalizationReplayBase(st, event_id) != CONFLICT_HASH
    )
  })

  /// An immediately eligible head that is replay-unsafe against the current
  /// finalized accumulator must remain parked and out of projection until a
  /// human resolution event carries it forward.
  /// @default_steps 20
  val inv_replay_unsafe_eligible_heads_are_parked = PEERS.forall(p => {
    val st = nodes.get(p)
    val replayUnsafe = replayUnsafeEligibleHeads(st, mergeableHeadEventIDs(st))
    all {
      replayUnsafe.subseteq(st.parked_conflicts),
      replayUnsafe.intersect(st.projection_basis).empty(),
    }
  })

  /// Direct finalization-safety check: every non-subsumed eligible event must
  /// merge conflict-free against the current finalized accumulator once replayed
  /// against its reconstructed parent-basis base.
  /// @default_steps 20
  val inv_finalization_eligible_events_are_conflict_free = PEERS.forall(p => {
    val st = nodes.get(p)
    finalizationEligible(st).exclude(subsumedEvents(st.clock)).forall(event_id => {
      val event = st.clock.get(event_id)
      val replayBase = finalizationReplayBase(st, event_id)
      replayBase != CONFLICT_HASH and
        mergeRoots(
          st.finalized_prolly_root_hash,
          event.prolly_root_hash,
          replayBase
        ) != CONFLICT_HASH
    })
  })

  /// ADVANCE_STABILITY may harden finalized-conflict resolution claims and thus
  /// shrink finalized_conflicts, but it must never create a fresh conflict id.
  /// @default_steps 20
  val inv_finalize_does_not_create_finalized_conflicts = PEERS.forall(p => {
    val st = nodes.get(p)
    val post = finalizeResult(st)
    finalizeEnabled(st) implies all {
      post.finalized_conflicts.keys().subseteq(st.finalized_conflicts.keys()),
      post.finalized_conflicts.keys().forall(conflictID =>
        st.finalized_conflicts.has(conflictID) and
          post.finalized_conflicts.get(conflictID) == st.finalized_conflicts.get(conflictID)
      ),
    }
  })

  /// Tentative conflict resolution emits a normal anchored write that
  /// references the resolved parked head and leaves finalized state untouched.
  /// @default_steps 20
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

  /// Resolving one parked head may legitimately release another parked head if
  /// full deterministic recomputation makes that other head part of the live
  /// projection. But an unrelated parked head must not disappear while staying
  /// tentative, non-subsumed, and outside the new projection basis.
  /// @default_steps 20
  val inv_resolve_conflict_preserves_other_parked_heads = PEERS.forall(p => {
    val st = nodes.get(p)
    (st.parked_conflicts.size() >= 2) implies
      st.parked_conflicts.forall(parkedEventID => {
        resolveConflictEnabled(st, parkedEventID) implies CONTENTS.forall(resolvedContent => {
          val post = resolveConflictResult(st, p, parkedEventID, resolvedContent)
          st.parked_conflicts.exclude(Set(parkedEventID)).forall(otherParked =>
            (tentativeHeadEventIDs(post).contains(otherParked) and
              not(subsumedEvents(post.clock).contains(otherParked)) and
              not(post.parked_conflicts.contains(otherParked))) implies
                post.projection_basis.contains(otherParked)
          )
        })
      })
  })

  /// All retained events in clocks satisfy the abstract swarm-wide admission
  /// predicate.
  /// @default_steps 20
  val inv_clock_events_valid = PEERS.forall(p => {
    val st = nodes.get(p)
    st.clock.keys().forall(event_id => validEvent(st.clock.get(event_id)))
  })

  /// Any event still waiting in an inbox is also admissible under the same
  /// uniform predicate.
  /// @default_steps 20
  val inv_inbox_events_valid = PEERS.forall(p =>
    inbox.get(p).forall(e => validEvent(e))
  )

  /// Event roots in clocks must never use the conflict sentinel.
  /// @default_steps 20
  val inv_event_root_not_conflict = PEERS.forall(p => {
    val st = nodes.get(p)
    st.clock.keys().forall(event_id => st.clock.get(event_id).prolly_root_hash != CONFLICT_HASH)
  })

  /// Disjoint concurrent writes must auto-merge and be commutative.
  /// Scenario checks over the abstract 2D state space.
  /// @default_steps 16
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
  /// @default_steps 20
  val inv_no_conflict_tentative_root = PEERS.forall(p => {
    val st = nodes.get(p)
    st.tentative_prolly_root_hash != CONFLICT_HASH
  })

  /// No unhandled finalized conflict (INV7b): finalized_prolly_root_hash is never
  /// CONFLICT_HASH. SYNC_FINALIZED divergence merges handle conflicts by using the
  /// ours-side root (deterministic) — no conflict sentinel leaks into
  /// the finalized state.
  /// @default_steps 20
  val inv_no_conflict_finalized_prolly_root = PEERS.forall(p => {
    val st = nodes.get(p)
    st.finalized_prolly_root_hash != CONFLICT_HASH
  })

  /// Parked event integrity (INV7c): every parked conflict references
  /// a known event in the clock. No conflict is silently discarded.
  /// @default_steps 20
  val inv_conflict_visibility = PEERS.forall(p => {
    val st = nodes.get(p)
    st.parked_conflicts.forall(event_id => st.clock.has(event_id))
  })

  /// Finalized conflict integrity (INV7e): every tracked finalized conflict
  /// is well-formed and references known lineage finalized commits.
  /// @default_steps 20
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
  /// @default_steps 20
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
  /// @default_steps 20
  val inv_finalized_conflict_no_resurrection = PEERS.forall(p => {
    val st = nodes.get(p)
    st.finalized_conflicts.keys().intersect(st.resolved_finalized_conflict_ids).empty()
  })

  /// Chunk-readiness integrity: only known clock events can be chunk-ready.
  /// @default_steps 20
  val inv_chunks_ready_known = PEERS.forall(p => {
    val st = nodes.get(p)
    st.chunks_ready.subseteq(st.clock.keys())
  })

  /// Self must always remain in the current witness set.
  /// @default_steps 20
  val inv_self_in_active_peers = PEERS.forall(p => {
    val st = nodes.get(p)
    st.active_peers.contains(p)
  })

  /// Parked integrity: parked conflicts always reference current heads.
  /// @default_steps 20
  val inv_parked_are_heads = PEERS.forall(p => {
    val st = nodes.get(p)
    st.parked_conflicts.subseteq(st.head_event_ids)
  })

  /// Closure-ready parking safety: once all tentative heads are mergeable,
  /// parked events are a subset of mergeable tentative heads.
  /// @default_steps 20
  val inv_parked_mergeable_when_closure_ready = PEERS.forall(p => {
    val st = nodes.get(p)
    (mergeableHeadEventIDs(st) == tentativeHeadEventIDs(st)) implies
      st.parked_conflicts.subseteq(mergeableHeadEventIDs(st))
  })

  /// Retained finalized subgraph closure: any finalized event body retained in
  /// clock keeps its required retained ancestry.
  /// @default_steps 20
  val inv_retained_finalized_subgraph_parent_closed = PEERS.forall(p => {
    val st = nodes.get(p)
    retainedFinalizedEventIDs(st).forall(event_id =>
      isParentClosed(st.clock, event_id)
    )
  })

  /// Finalization safety: every retained finalized event in clock is
  /// parent-closed and chunk-ready.
  /// @default_steps 20
  val inv_finalized_parent_closed = PEERS.forall(p => {
    val st = nodes.get(p)
    retainedFinalizedEventIDs(st).forall(event_id =>
      isParentClosed(st.clock, event_id) and st.chunks_ready.contains(event_id)
    )
  })

  /// Reconcile fixed-point safety: tentative_prolly_root_hash is always equal to the
  /// deterministic merged-root projection from current state.
  /// @default_steps 20
  val inv_tentative_root_is_compute_projection = PEERS.forall(p => {
    val st = nodes.get(p)
    st.tentative_prolly_root_hash == computeTentativeProjection(st)._1
  })

  /// Reconcile fixed-point safety: parked_conflicts is always equal to the
  /// deterministic parked-set projection from current state.
  /// @default_steps 20
  val inv_parked_conflicts_is_compute_projection = PEERS.forall(p => {
    val st = nodes.get(p)
    st.parked_conflicts == computeTentativeProjection(st)._2
  })

  /// Projection-basis safety: emitted-parent basis is always the deterministic
  /// basis associated with the current tentative root.
  /// @default_steps 20
  val inv_projection_basis_is_compute_projection = PEERS.forall(p => {
    val st = nodes.get(p)
    st.projection_basis == computeTentativeProjection(st)._3
  })

  /// Bounded anti-entropy scenario check: if p can heartbeat q and q's head has
  /// full ancestry in q's local clock, then one closure pull plus enough
  /// do_receive steps yields parent-closed remote heads in p's projected clock.
  /// @default_steps 15
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
  /// anchor and mergeable tentative heads are still missing, unresolved parked
  /// tentative heads must not disappear from parked_conflicts.
  /// @default_steps 20
  val inv_reanchor_provisional_parked_retained = PEERS.forall(p => PEERS.forall(q => {
    val st_p = nodes.get(p)
    val st_q = nodes.get(q)
    val synced = applySyncFinalized(st_p, st_q)
    val unresolvedHeadIDs = st_p.parked_conflicts.intersect(tentativeHeadEventIDs(synced))
    (syncEnabled(connected, p, q, st_p, st_q) and
      synced.finalized_commit_id != st_p.finalized_commit_id and
      mergeableHeadEventIDs(synced).empty()) implies
      unresolvedHeadIDs.subseteq(synced.parked_conflicts)
  }))

  /// Re-anchoring under closure lag: after SYNC_FINALIZED changes
  /// finalized_prolly_root_hash, projection may remain provisional while
  /// mergeable tentative heads are missing. Once required closure and chunks are
  /// available, recomputation must use the current reconcile rule: exclude
  /// replay-unsafe eligible heads from projection, anchored-fold the remaining
  /// ready heads, and retain only unresolved unready parked heads.
  /// @default_steps 20
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
    val readyHeads = mergeableHeadEventIDs(caughtState)
    val replayUnsafeHeads = replayUnsafeEligibleHeads(caughtState, readyHeads)
    val projectionReadyHeads = readyHeads.exclude(replayUnsafeHeads)
    val folded = anchoredFold(
      projectionReadyHeads,
      caughtState,
      caughtState.finalized_prolly_root_hash
    )
    val retainedUnreadyParked = caughtState.parked_conflicts.intersect(
      tentativeHeadEventIDs(caughtState).exclude(readyHeads)
    )
    val expectedParked = folded._2.union(retainedUnreadyParked).union(replayUnsafeHeads)
    val expectedBasis = projectionReadyHeads.exclude(folded._2)
    val recomputed = computeTentativeProjection(caughtState)
    (syncEnabled(connected, p, q, st_p, st_q) and
      synced.finalized_commit_id != st_p.finalized_commit_id and
      not(tentativeHeadEventIDs(synced).empty()) and
      mergeableHeadEventIDs(synced).empty() and
      not(readyHeads.empty()) and
      (
        folded._1 != synced.tentative_prolly_root_hash or
        expectedParked != synced.parked_conflicts or
        expectedBasis != synced.projection_basis
      )
    ) implies
      all {
        recomputed._1 == folded._1,
        recomputed._2 == expectedParked,
        recomputed._3 == expectedBasis,
        recomputed._1 != synced.tentative_prolly_root_hash or
          recomputed._2 != synced.parked_conflicts or
          recomputed._3 != synced.projection_basis,
      }
  }))

  /// Heartbeats should not keep requesting closure once the remote frontier
  /// and head ancestry are already present locally.
  /// @default_steps 20
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
  /// @default_steps 20
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
  /// @default_steps 20
  val inv_self_seen_frontier_exact = PEERS.forall(p => {
    val st = nodes.get(p)
    st.peer_seen_frontier.get(p) == observedFrontier(st.clock)
  })

  /// The local seen frontier is an antichain: no frontier element is an
  /// ancestor of any other frontier element.
  /// @default_steps 20
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
  /// @default_steps 19
  val inv_self_seen_frontier_covers_parent_closed_clock = PEERS.forall(p => {
    val st = nodes.get(p)
    st.clock.keys().forall(event_id =>
      isParentClosed(st.clock, event_id) implies
        frontierCovers(st.clock, st.peer_seen_frontier.get(p), event_id)
    )
  })

  /// The model treats bootstrap/join as external to the core protocol, so all
  /// reachable states must already satisfy the bootstrap boundary contract.
  /// @default_steps 16
  val inv_bootstrap_boundary_ready = PEERS.forall(p => {
    val st = nodes.get(p)
    bootstrapBoundaryReady(p, st)
  })

  /// Seeded bootstrap scenario: a compacted checkpoint state is a valid
  /// bootstrap boundary even with conservative membership and no remote
  /// frontier knowledge.
  /// @default_steps 12
  val inv_bootstrap_seed_compacted_ready = PEERS.forall(p => {
    val seeded = seedBootstrappedCheckpointState(p, false, false)
    bootstrapBoundaryReady(p, seeded)
  })

  /// Seeded bootstrap scenario: retaining the current finalized event body in
  /// clock is also a valid bootstrap boundary.
  /// @default_steps 12
  val inv_bootstrap_seed_retained_ready = PEERS.forall(p => {
    val seeded = seedBootstrappedCheckpointState(p, true, false)
    bootstrapBoundaryReady(p, seeded)
  })

  /// Seeded bootstrap scenario: equal-tip metadata divergence can be repaired
  /// via SYNC_NOOP without changing the finalized tip or violating the
  /// bootstrap boundary contract.
  /// @default_steps 12
  val inv_bootstrap_seed_equal_tip_metadata_repair = PEERS.forall(p => PEERS.forall(q => {
    val st_p = seedBootstrappedCheckpointState(p, false, false)
    val st_q = seedBootstrappedCheckpointState(q, true, true)
    val synced = applySyncFinalized(st_p, st_q)
    (p != q) implies all {
      syncEnabled(allPairs(PEERS), p, q, st_p, st_q),
      syncMode(st_p, st_q) == SYNC_NOOP,
      synced.finalized_commit_id == st_p.finalized_commit_id,
      synced.finalized_prolly_root_hash == st_p.finalized_prolly_root_hash,
      synced.finalized_checkpoint_events == mergeFinalizedCheckpointMap(
        st_p.finalized_checkpoint_events,
        st_q.finalized_checkpoint_events
      ),
      bootstrapBoundaryReady(p, synced),
    }
  }))

  pure def localReadyUnfinalized(st: NodeState): Set[EventID] =
    st.clock.keys().filter(event_id =>
      not(st.finalized_events.has(event_id)) and
      not(parkedBranchEvents(st).contains(event_id)) and
      isParentClosed(st.clock, event_id) and
      st.chunks_ready.contains(event_id)
    )

  /// @default_steps 20
  val inv_singleton_membership_self_coverage = PEERS.forall(p => {
    val st = nodes.get(p)
    (st.active_peers == Set(p)) implies
      finalizationCandidates(st) == localReadyUnfinalized(st)
  })

  /// Conflicts do not block LOCAL_WRITE when model resource bounds permit it.
  /// @default_steps 20
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
  /// @default_steps 20
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
  /// peers with the same tentative-projection inputs deterministically agree on
  /// parked events once all tentative heads are mergeable.
  /// @default_steps 20
  val inv_parking_agreement = PEERS.forall(p1 => PEERS.forall(p2 => {
    val s1 = nodes.get(p1)
    val s2 = nodes.get(p2)
    (sameTentativeProjectionInputs(s1, s2) and
     mergeableHeadEventIDs(s1) == tentativeHeadEventIDs(s1) and
     mergeableHeadEventIDs(s2) == tentativeHeadEventIDs(s2)) implies
      (s1.parked_conflicts == s2.parked_conflicts and
       s1.projection_basis == s2.projection_basis)
  }))

  /// Heads are always derived from clock structure (no hidden divergence).
  /// @default_steps 20
  val inv_heads_consistent = PEERS.forall(p => {
    val st = nodes.get(p)
    st.head_event_ids == computeHeadEventIDs(st.clock)
  })

  /// INV9a: Parked and subsumed events are disjoint.
  /// A subsumed event has been resolved; it must not remain parked.
  /// @default_steps 20
  val inv_parked_subsumed_disjoint = PEERS.forall(p => {
    val st = nodes.get(p)
    st.parked_conflicts.intersect(subsumedEvents(st.clock)).empty()
  })

  /// INV9b: Every subsumed event has a parent link from its resolver.
  /// The event that carries resolved_parked_event_id must include the
  /// subsumed event in its parent_event_ids.
  /// @default_steps 20
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

  /// INV9c: Subsumed next-finalization steps are bookkeeping-only.
  /// If ADVANCE_STABILITY selects a subsumed event, the finalized tip/root and
  /// finalized lineage must stay fixed. The step may only add bookkeeping for
  /// exactly that event under the current checkpoint descriptor and apply the
  /// corresponding durable finalized-conflict cleanup.
  /// @default_steps 20
  val inv_subsumed_finalize_bookkeeping_only = PEERS.forall(p => {
    val st = nodes.get(p)
    val post = finalizeResult(st)
    val nextEventID = nextFinalizationEventID(st)
    val nextResolvedClaims =
      if (nextEventID == EMPTY_EVENT_ID) Set()
      else match st.clock.get(nextEventID).resolved_finalized_conflict_id {
        | Some(cid) => Set(cid)
        | None => Set()
      }
    val expectedCheckpoint =
      if (nextEventID == EMPTY_EVENT_ID) Map()
      else if (st.finalized_checkpoint_events.has(st.finalized_commit_id))
        st.finalized_checkpoint_events.get(st.finalized_commit_id)
          .put(nextEventID, st.clock.get(nextEventID).prolly_root_hash)
      else
        Map().put(nextEventID, st.clock.get(nextEventID).prolly_root_hash)
    (finalizeEnabled(st) and nextFinalizationIsSubsumed(st)) implies all {
      post.finalized_commit_id == st.finalized_commit_id,
      post.finalized_prolly_root_hash == st.finalized_prolly_root_hash,
      post.finalized_commit_parents == st.finalized_commit_parents,
      post.finalized_events == st.finalized_events.put(
        nextEventID,
        st.clock.get(nextEventID).prolly_root_hash
      ),
      post.finalized_checkpoint_events == st.finalized_checkpoint_events.put(
        st.finalized_commit_id,
        expectedCheckpoint
      ),
      post.resolved_finalized_conflict_ids ==
        st.resolved_finalized_conflict_ids.union(nextResolvedClaims),
      post.finalized_conflicts == dropFinalizedConflicts(
        st.finalized_conflicts,
        st.resolved_finalized_conflict_ids.union(nextResolvedClaims)
      ),
    }
  })

  /// Finalization and parking are disjoint: parked events are never finalized.
  /// @default_steps 20
  val inv_finalized_parked_disjoint = PEERS.forall(p => {
    val st = nodes.get(p)
    st.finalized_events.keys().intersect(st.parked_conflicts).empty()
  })

  /// Finalized retained bodies are below the tentative boundary: they may stay
  /// in clock for bookkeeping, but never participate in the live projection basis.
  /// @default_steps 20
  val inv_finalized_projection_basis_disjoint = PEERS.forall(p => {
    val st = nodes.get(p)
    st.finalized_events.keys().intersect(st.projection_basis).empty()
  })

  /// Three-peer partition-heal regression:
  /// if two different merge-first choices produce different intermediate heal
  /// tips, one later merge-of-merges step must reconcile them symmetrically
  /// while preserving all original finalized histories as ancestors.
  /// @default_steps 20
  val inv_three_peer_partition_heal_reconcilable = PEERS.forall(p => PEERS.forall(q => PEERS.forall(r => {
    val st_p = nodes.get(p)
    val st_q = nodes.get(q)
    val st_r = nodes.get(r)
    val left = applySyncFinalized(applySyncFinalized(st_p, st_q), st_r)
    val right = applySyncFinalized(applySyncFinalized(st_p, st_r), st_q)
    val reconciled = applySyncFinalized(left, right)
    val reconciledRev = applySyncFinalized(right, left)
    val frontierAfter = finalizedTipFrontier(
      Set(
        st_p.finalized_commit_id,
        st_q.finalized_commit_id,
        st_r.finalized_commit_id,
        reconciled.finalized_commit_id
      ),
      reconciled.finalized_commit_parents
    )
    (p != q and q != r and p != r and
      syncEnabled(connected, p, q, st_p, st_q) and
      syncEnabled(connected, p, r, st_p, st_r) and
      syncEnabled(connected, q, r, st_q, st_r) and
      syncMode(st_p, st_q) == SYNC_MERGE and
      syncMode(st_p, st_r) == SYNC_MERGE and
      syncMode(st_q, st_r) == SYNC_MERGE) implies all {
        sameFinalizedState(reconciled, reconciledRev),
        frontierAfter == Set(reconciled.finalized_commit_id),
        finalizedDescendsFromAll(
          reconciled.finalized_commit_id,
          Set(st_p.finalized_commit_id, st_q.finalized_commit_id, st_r.finalized_commit_id),
          reconciled.finalized_commit_parents
        ),
      }
  })))


}
