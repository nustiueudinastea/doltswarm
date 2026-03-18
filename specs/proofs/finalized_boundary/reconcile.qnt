// -*- mode: Bluespec; -*-

module doltswarm_reconcile_inductive {
  import doltswarm_model.* from "../../model/doltswarm_model"

  pure val PHASE_BEFORE: int = 0
  pure val PHASE_AFTER_RECONCILE: int = 1
  pure val ALL_PHASES: Set[int] = Set(PHASE_BEFORE, PHASE_AFTER_RECONCILE)
  pure val BOOLEANS: Set[bool] = Set(true, false)
  pure val ALL_PROJECTION_ROOTS: Set[ProllyRootHash] =
    CONTENTS.union(Set(EMPTY_ROOT, CONFLICT_HASH))

  var target_peer: int
  var phase: int
  var projection_root: ProllyRootHash
  var parked_seed_event: bool
  var parked_wall1_event1: bool
  var parked_wall1_event2: bool
  var parked_wall2_event1: bool
  var parked_wall2_event2: bool
  var basis_seed_event: bool
  var basis_wall1_event1: bool
  var basis_wall1_event2: bool
  var basis_wall2_event1: bool
  var basis_wall2_event2: bool

  def typeOK: bool = all {
    target_peer.in(PEERS),
    phase.in(ALL_PHASES),
    projection_root.in(ALL_PROJECTION_ROOTS),
    parked_seed_event.in(BOOLEANS),
    parked_wall1_event1.in(BOOLEANS),
    parked_wall1_event2.in(BOOLEANS),
    parked_wall2_event1.in(BOOLEANS),
    parked_wall2_event2.in(BOOLEANS),
    basis_seed_event.in(BOOLEANS),
    basis_wall1_event1.in(BOOLEANS),
    basis_wall1_event2.in(BOOLEANS),
    basis_wall2_event1.in(BOOLEANS),
    basis_wall2_event2.in(BOOLEANS),
  }

  pure def pendingDoubleWriteState(base: NodeState, target: int,
      firstContent: int, secondContent: int): NodeState = {
    val once = writeBaseResult(base, target, firstContent)
    writeBaseResult(once, target, secondContent)
  }

  pure def emptyPendingReconcileState(target: int): NodeState =
    pendingDoubleWriteState(emptyNodeState(target), target, 11, 12)

  pure def bootCompactedPendingReconcileState(target: int): NodeState =
    pendingDoubleWriteState(seedBootstrappedCheckpointState(target, false, false), target, 12, 14)

  pure def bootRetainedPendingReconcileState(target: int): NodeState =
    pendingDoubleWriteState(seedBootstrappedCheckpointState(target, true, false), target, 12, 14)

  pure def bootMetadataPendingReconcileState(target: int): NodeState =
    pendingDoubleWriteState(seedBootstrappedCheckpointState(target, true, true), target, 12, 14)

  pure def retainedSeedEventID: EventID =
    seedEvent(1, 1, 0, 11, EMPTY_ROOT, Set()).event_id

  pure def pendingWall1Event1(target: int): EventID = makeEventID(target, 1, 1)
  pure def pendingWall1Event2(target: int): EventID = makeEventID(target, 1, 2)
  pure def pendingWall2Event1(target: int): EventID = makeEventID(target, 2, 1)
  pure def pendingWall2Event2(target: int): EventID = makeEventID(target, 2, 2)

  pure def selectedEvent(flag: bool, event_id: EventID): Set[EventID] =
    if (flag) Set(event_id) else Set()

  pure def projectionSet(target: int, includeSeedEvent: bool,
      includeWall1Event1: bool, includeWall1Event2: bool,
      includeWall2Event1: bool, includeWall2Event2: bool): Set[EventID] =
    selectedEvent(includeSeedEvent, retainedSeedEventID)
      .union(selectedEvent(includeWall1Event1, pendingWall1Event1(target)))
      .union(selectedEvent(includeWall1Event2, pendingWall1Event2(target)))
      .union(selectedEvent(includeWall2Event1, pendingWall2Event1(target)))
      .union(selectedEvent(includeWall2Event2, pendingWall2Event2(target)))

  def projectionParkedSet(target: int): Set[EventID] =
    projectionSet(
      target,
      parked_seed_event,
      parked_wall1_event1,
      parked_wall1_event2,
      parked_wall2_event1,
      parked_wall2_event2,
    )

  def projectionBasisSet(target: int): Set[EventID] =
    projectionSet(
      target,
      basis_seed_event,
      basis_wall1_event1,
      basis_wall1_event2,
      basis_wall2_event1,
      basis_wall2_event2,
    )

  def reconcileFrameState(base: NodeState, currentPhase: int,
      target: int): NodeState =
    if (currentPhase == PHASE_BEFORE)
      base
    else
      applyTentativeProjection(
        base,
        projection_root,
        projectionParkedSet(target),
        projectionBasisSet(target),
      )

  def reconcilePreservesFinalizedTip(base: NodeState,
      currentPhase: int, target: int): bool =
    sameFinalizedTipState(
      reconcileFrameState(base, currentPhase, target),
      base,
    )

  def reconcilePreservesFinalizedMetadata(base: NodeState,
      currentPhase: int, target: int): bool =
    sameFinalizedMetadataState(
      reconcileFrameState(base, currentPhase, target),
      base,
    )

  def reconcilePreservesFinalizedBookkeeping(base: NodeState,
      currentPhase: int, target: int): bool =
    sameFinalizedBookkeepingState(
      reconcileFrameState(base, currentPhase, target),
      base,
    )

  def reconcileInvariant(base: NodeState, preservation: bool): bool = all {
    typeOK,
    base.head_event_ids.size() > 1,
    all {
      projectionParkedSet(target_peer).subseteq(base.head_event_ids),
      projectionBasisSet(target_peer).subseteq(base.head_event_ids),
    } implies (phase == PHASE_BEFORE or preservation),
  }

  action harness_init = {
    nondet peer = PEERS.oneOf()
    nondet root = ALL_PROJECTION_ROOTS.oneOf()
    nondet parkedSeed = BOOLEANS.oneOf()
    nondet parkedW1E1 = BOOLEANS.oneOf()
    nondet parkedW1E2 = BOOLEANS.oneOf()
    nondet parkedW2E1 = BOOLEANS.oneOf()
    nondet parkedW2E2 = BOOLEANS.oneOf()
    nondet basisSeed = BOOLEANS.oneOf()
    nondet basisW1E1 = BOOLEANS.oneOf()
    nondet basisW1E2 = BOOLEANS.oneOf()
    nondet basisW2E1 = BOOLEANS.oneOf()
    nondet basisW2E2 = BOOLEANS.oneOf()
    all {
      target_peer' = peer,
      phase' = PHASE_BEFORE,
      projection_root' = root,
      parked_seed_event' = parkedSeed,
      parked_wall1_event1' = parkedW1E1,
      parked_wall1_event2' = parkedW1E2,
      parked_wall2_event1' = parkedW2E1,
      parked_wall2_event2' = parkedW2E2,
      basis_seed_event' = basisSeed,
      basis_wall1_event1' = basisW1E1,
      basis_wall1_event2' = basisW1E2,
      basis_wall2_event1' = basisW2E1,
      basis_wall2_event2' = basisW2E2,
    }
  }

  action harness_step =
    if (phase == PHASE_BEFORE)
      all {
        target_peer' = target_peer,
        phase' = PHASE_AFTER_RECONCILE,
        projection_root' = projection_root,
        parked_seed_event' = parked_seed_event,
        parked_wall1_event1' = parked_wall1_event1,
        parked_wall1_event2' = parked_wall1_event2,
        parked_wall2_event1' = parked_wall2_event1,
        parked_wall2_event2' = parked_wall2_event2,
        basis_seed_event' = basis_seed_event,
        basis_wall1_event1' = basis_wall1_event1,
        basis_wall1_event2' = basis_wall1_event2,
        basis_wall2_event1' = basis_wall2_event1,
        basis_wall2_event2' = basis_wall2_event2,
      }
    else
      all {
        target_peer' = target_peer,
        phase' = phase,
        projection_root' = projection_root,
        parked_seed_event' = parked_seed_event,
        parked_wall1_event1' = parked_wall1_event1,
        parked_wall1_event2' = parked_wall1_event2,
        parked_wall2_event1' = parked_wall2_event1,
        parked_wall2_event2' = parked_wall2_event2,
        basis_seed_event' = basis_seed_event,
        basis_wall1_event1' = basis_wall1_event1,
        basis_wall1_event2' = basis_wall1_event2,
        basis_wall2_event1' = basis_wall2_event1,
        basis_wall2_event2' = basis_wall2_event2,
      }

  val inv_reconcile_empty_preserves_finalized_tip_inductive =
    reconcileInvariant(
      emptyPendingReconcileState(target_peer),
      reconcilePreservesFinalizedTip(emptyPendingReconcileState(target_peer), phase, target_peer),
    )

  val inv_reconcile_empty_preserves_finalized_metadata_inductive =
    reconcileInvariant(
      emptyPendingReconcileState(target_peer),
      reconcilePreservesFinalizedMetadata(emptyPendingReconcileState(target_peer), phase, target_peer),
    )

  val inv_reconcile_empty_preserves_finalized_bookkeeping_inductive =
    reconcileInvariant(
      emptyPendingReconcileState(target_peer),
      reconcilePreservesFinalizedBookkeeping(emptyPendingReconcileState(target_peer), phase, target_peer),
    )

  val inv_reconcile_empty_preserves_finalized_state_inductive = all {
    inv_reconcile_empty_preserves_finalized_tip_inductive,
    inv_reconcile_empty_preserves_finalized_metadata_inductive,
    inv_reconcile_empty_preserves_finalized_bookkeeping_inductive,
  }

  val inv_reconcile_boot_compacted_preserves_finalized_tip_inductive =
    reconcileInvariant(
      bootCompactedPendingReconcileState(target_peer),
      reconcilePreservesFinalizedTip(bootCompactedPendingReconcileState(target_peer), phase, target_peer),
    )

  val inv_reconcile_boot_compacted_preserves_finalized_metadata_inductive =
    reconcileInvariant(
      bootCompactedPendingReconcileState(target_peer),
      reconcilePreservesFinalizedMetadata(bootCompactedPendingReconcileState(target_peer), phase, target_peer),
    )

  val inv_reconcile_boot_compacted_preserves_finalized_bookkeeping_inductive =
    reconcileInvariant(
      bootCompactedPendingReconcileState(target_peer),
      reconcilePreservesFinalizedBookkeeping(bootCompactedPendingReconcileState(target_peer), phase, target_peer),
    )

  val inv_reconcile_boot_compacted_preserves_finalized_state_inductive = all {
    inv_reconcile_boot_compacted_preserves_finalized_tip_inductive,
    inv_reconcile_boot_compacted_preserves_finalized_metadata_inductive,
    inv_reconcile_boot_compacted_preserves_finalized_bookkeeping_inductive,
  }

  val inv_reconcile_boot_retained_preserves_finalized_tip_inductive =
    reconcileInvariant(
      bootRetainedPendingReconcileState(target_peer),
      reconcilePreservesFinalizedTip(bootRetainedPendingReconcileState(target_peer), phase, target_peer),
    )

  val inv_reconcile_boot_retained_preserves_finalized_metadata_inductive =
    reconcileInvariant(
      bootRetainedPendingReconcileState(target_peer),
      reconcilePreservesFinalizedMetadata(bootRetainedPendingReconcileState(target_peer), phase, target_peer),
    )

  val inv_reconcile_boot_retained_preserves_finalized_bookkeeping_inductive =
    reconcileInvariant(
      bootRetainedPendingReconcileState(target_peer),
      reconcilePreservesFinalizedBookkeeping(bootRetainedPendingReconcileState(target_peer), phase, target_peer),
    )

  val inv_reconcile_boot_retained_preserves_finalized_state_inductive = all {
    inv_reconcile_boot_retained_preserves_finalized_tip_inductive,
    inv_reconcile_boot_retained_preserves_finalized_metadata_inductive,
    inv_reconcile_boot_retained_preserves_finalized_bookkeeping_inductive,
  }

  val inv_reconcile_boot_metadata_preserves_finalized_tip_inductive =
    reconcileInvariant(
      bootMetadataPendingReconcileState(target_peer),
      reconcilePreservesFinalizedTip(bootMetadataPendingReconcileState(target_peer), phase, target_peer),
    )

  val inv_reconcile_boot_metadata_preserves_finalized_metadata_inductive =
    reconcileInvariant(
      bootMetadataPendingReconcileState(target_peer),
      reconcilePreservesFinalizedMetadata(bootMetadataPendingReconcileState(target_peer), phase, target_peer),
    )

  val inv_reconcile_boot_metadata_preserves_finalized_bookkeeping_inductive =
    reconcileInvariant(
      bootMetadataPendingReconcileState(target_peer),
      reconcilePreservesFinalizedBookkeeping(bootMetadataPendingReconcileState(target_peer), phase, target_peer),
    )

  val inv_reconcile_boot_metadata_preserves_finalized_state_inductive = all {
    inv_reconcile_boot_metadata_preserves_finalized_tip_inductive,
    inv_reconcile_boot_metadata_preserves_finalized_metadata_inductive,
    inv_reconcile_boot_metadata_preserves_finalized_bookkeeping_inductive,
  }

  val inv_reconcile_preserves_finalized_state_inductive = all {
    inv_reconcile_empty_preserves_finalized_state_inductive,
    inv_reconcile_boot_compacted_preserves_finalized_state_inductive,
    inv_reconcile_boot_retained_preserves_finalized_state_inductive,
    inv_reconcile_boot_metadata_preserves_finalized_state_inductive,
  }
}
