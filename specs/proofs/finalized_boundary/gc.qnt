// -*- mode: Bluespec; -*-

module doltswarm_gc_inductive {
  import doltswarm_model.* from "../../model/doltswarm_model"

  pure val PHASE_BEFORE: int = 0
  pure val PHASE_AFTER_BASE: int = 1
  pure val PHASE_AFTER_RECONCILE: int = 2
  pure val ALL_PHASES: Set[int] = Set(PHASE_BEFORE, PHASE_AFTER_BASE, PHASE_AFTER_RECONCILE)
  pure val BASE_PHASES: Set[int] = Set(PHASE_BEFORE, PHASE_AFTER_BASE)
  pure val RECONCILE_PHASES: Set[int] = Set(PHASE_AFTER_BASE, PHASE_AFTER_RECONCILE)

  var target_peer: int
  var phase: int

  def typeOK: bool = all {
    target_peer.in(PEERS),
    phase.in(ALL_PHASES),
  }

  pure def bootRetainedBaseState(target: int): NodeState =
    seedBootstrappedCheckpointState(target, true, false)

  pure def bootMetadataBaseState(target: int): NodeState =
    seedBootstrappedCheckpointState(target, true, true)

  pure def gcTargetEventID: EventID =
    seedEvent(1, 1, 0, 11, EMPTY_ROOT, Set()).event_id

  pure def gcBasePhaseState(base: NodeState, target: int,
      currentPhase: int): NodeState =
    if (currentPhase == PHASE_BEFORE)
      base
    else
      gcBaseResult(base, target, gcTargetEventID)

  pure def reconcilePhaseState(baseGcState: NodeState,
      currentPhase: int): NodeState =
    if (currentPhase == PHASE_AFTER_BASE)
      baseGcState
    else
      reconcileResult(baseGcState)

  pure def gcBasePreservesFinalizedTip(base: NodeState, target: int,
      currentPhase: int): bool =
    sameFinalizedTipState(
      gcBasePhaseState(base, target, currentPhase),
      base,
    )

  pure def gcBasePreservesFinalizedMetadata(base: NodeState, target: int,
      currentPhase: int): bool =
    sameFinalizedMetadataState(
      gcBasePhaseState(base, target, currentPhase),
      base,
    )

  pure def gcBasePreservesFinalizedBookkeeping(base: NodeState, target: int,
      currentPhase: int): bool =
    sameFinalizedBookkeepingState(
      gcBasePhaseState(base, target, currentPhase),
      base,
    )

  pure def reconcilePreservesFinalizedTip(base: NodeState, target: int,
      currentPhase: int): bool = {
    val postGc = gcBaseResult(base, target, gcTargetEventID)
    sameFinalizedTipState(
      reconcilePhaseState(postGc, currentPhase),
      postGc,
    )
  }

  pure def reconcilePreservesFinalizedMetadata(base: NodeState, target: int,
      currentPhase: int): bool = {
    val postGc = gcBaseResult(base, target, gcTargetEventID)
    sameFinalizedMetadataState(
      reconcilePhaseState(postGc, currentPhase),
      postGc,
    )
  }

  pure def reconcilePreservesFinalizedBookkeeping(base: NodeState, target: int,
      currentPhase: int): bool = {
    val postGc = gcBaseResult(base, target, gcTargetEventID)
    sameFinalizedBookkeepingState(
      reconcilePhaseState(postGc, currentPhase),
      postGc,
    )
  }

  def gcBaseInvariant(base: NodeState, preservation: bool): bool = all {
    typeOK,
    gcPruneable(base).contains(gcTargetEventID),
    not(BASE_PHASES.contains(phase)) or preservation,
  }

  def gcReconcileInvariant(base: NodeState, preservation: bool): bool = all {
    typeOK,
    gcPruneable(base).contains(gcTargetEventID),
    not(RECONCILE_PHASES.contains(phase)) or preservation,
  }

  action phased_harness_init = {
    nondet peer = PEERS.oneOf()
    all {
      target_peer' = peer,
      phase' = PHASE_BEFORE,
    }
  }

  action phased_harness_step =
    if (phase == PHASE_BEFORE)
      all {
        target_peer' = target_peer,
        phase' = PHASE_AFTER_BASE,
      }
    else if (phase == PHASE_AFTER_BASE)
      all {
        target_peer' = target_peer,
        phase' = PHASE_AFTER_RECONCILE,
      }
    else
      all {
        target_peer' = target_peer,
        phase' = phase,
      }

  val inv_gc_boot_retained_base_preserves_finalized_tip_inductive =
    gcBaseInvariant(
      bootRetainedBaseState(target_peer),
      gcBasePreservesFinalizedTip(bootRetainedBaseState(target_peer), target_peer, phase),
    )

  val inv_gc_boot_retained_base_preserves_finalized_metadata_inductive =
    gcBaseInvariant(
      bootRetainedBaseState(target_peer),
      gcBasePreservesFinalizedMetadata(bootRetainedBaseState(target_peer), target_peer, phase),
    )

  val inv_gc_boot_retained_base_preserves_finalized_bookkeeping_inductive =
    gcBaseInvariant(
      bootRetainedBaseState(target_peer),
      gcBasePreservesFinalizedBookkeeping(bootRetainedBaseState(target_peer), target_peer, phase),
    )

  val inv_gc_boot_retained_reconcile_preserves_finalized_tip_inductive =
    gcReconcileInvariant(
      bootRetainedBaseState(target_peer),
      reconcilePreservesFinalizedTip(bootRetainedBaseState(target_peer), target_peer, phase),
    )

  val inv_gc_boot_retained_reconcile_preserves_finalized_metadata_inductive =
    gcReconcileInvariant(
      bootRetainedBaseState(target_peer),
      reconcilePreservesFinalizedMetadata(bootRetainedBaseState(target_peer), target_peer, phase),
    )

  val inv_gc_boot_retained_reconcile_preserves_finalized_bookkeeping_inductive =
    gcReconcileInvariant(
      bootRetainedBaseState(target_peer),
      reconcilePreservesFinalizedBookkeeping(bootRetainedBaseState(target_peer), target_peer, phase),
    )

  val inv_gc_boot_retained_preserves_finalized_state_inductive = all {
    inv_gc_boot_retained_base_preserves_finalized_tip_inductive,
    inv_gc_boot_retained_base_preserves_finalized_metadata_inductive,
    inv_gc_boot_retained_base_preserves_finalized_bookkeeping_inductive,
    inv_gc_boot_retained_reconcile_preserves_finalized_tip_inductive,
    inv_gc_boot_retained_reconcile_preserves_finalized_metadata_inductive,
    inv_gc_boot_retained_reconcile_preserves_finalized_bookkeeping_inductive,
  }

  val inv_gc_boot_metadata_base_preserves_finalized_tip_inductive =
    gcBaseInvariant(
      bootMetadataBaseState(target_peer),
      gcBasePreservesFinalizedTip(bootMetadataBaseState(target_peer), target_peer, phase),
    )

  val inv_gc_boot_metadata_base_preserves_finalized_metadata_inductive =
    gcBaseInvariant(
      bootMetadataBaseState(target_peer),
      gcBasePreservesFinalizedMetadata(bootMetadataBaseState(target_peer), target_peer, phase),
    )

  val inv_gc_boot_metadata_base_preserves_finalized_bookkeeping_inductive =
    gcBaseInvariant(
      bootMetadataBaseState(target_peer),
      gcBasePreservesFinalizedBookkeeping(bootMetadataBaseState(target_peer), target_peer, phase),
    )

  val inv_gc_boot_metadata_reconcile_preserves_finalized_tip_inductive =
    gcReconcileInvariant(
      bootMetadataBaseState(target_peer),
      reconcilePreservesFinalizedTip(bootMetadataBaseState(target_peer), target_peer, phase),
    )

  val inv_gc_boot_metadata_reconcile_preserves_finalized_metadata_inductive =
    gcReconcileInvariant(
      bootMetadataBaseState(target_peer),
      reconcilePreservesFinalizedMetadata(bootMetadataBaseState(target_peer), target_peer, phase),
    )

  val inv_gc_boot_metadata_reconcile_preserves_finalized_bookkeeping_inductive =
    gcReconcileInvariant(
      bootMetadataBaseState(target_peer),
      reconcilePreservesFinalizedBookkeeping(bootMetadataBaseState(target_peer), target_peer, phase),
    )

  val inv_gc_boot_metadata_preserves_finalized_state_inductive = all {
    inv_gc_boot_metadata_base_preserves_finalized_tip_inductive,
    inv_gc_boot_metadata_base_preserves_finalized_metadata_inductive,
    inv_gc_boot_metadata_base_preserves_finalized_bookkeeping_inductive,
    inv_gc_boot_metadata_reconcile_preserves_finalized_tip_inductive,
    inv_gc_boot_metadata_reconcile_preserves_finalized_metadata_inductive,
    inv_gc_boot_metadata_reconcile_preserves_finalized_bookkeeping_inductive,
  }

  val inv_gc_preserves_finalized_state_inductive = all {
    inv_gc_boot_retained_preserves_finalized_state_inductive,
    inv_gc_boot_metadata_preserves_finalized_state_inductive,
  }
}
