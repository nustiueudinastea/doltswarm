// -*- mode: Bluespec; -*-

module doltswarm_write_inductive {
  import doltswarm_model.* from "../../model/doltswarm_model"

  pure val PHASE_BEFORE: int = 0
  pure val PHASE_AFTER_BASE: int = 1
  pure val PHASE_AFTER_RECONCILE: int = 2
  pure val ALL_PHASES: Set[int] = Set(PHASE_BEFORE, PHASE_AFTER_BASE, PHASE_AFTER_RECONCILE)
  pure val BASE_PHASES: Set[int] = Set(PHASE_BEFORE, PHASE_AFTER_BASE)
  pure val RECONCILE_PHASES: Set[int] = Set(PHASE_AFTER_BASE, PHASE_AFTER_RECONCILE)

  var target_peer: int
  var target_content: int
  var phase: int

  def commonTypeOK(allowedPhases: Set[int]): bool = all {
    target_peer.in(PEERS),
    target_content.in(CONTENTS),
    phase.in(allowedPhases),
  }

  def basePhaseTypeOK: bool =
    commonTypeOK(BASE_PHASES)

  def reconcilePhaseTypeOK: bool =
    commonTypeOK(RECONCILE_PHASES)

  def phasedTypeOK: bool =
    commonTypeOK(ALL_PHASES)

  pure def emptyBaseState(target: int): NodeState =
    emptyNodeState(target)

  pure def bootCompactedBaseState(target: int): NodeState =
    seedBootstrappedCheckpointState(target, false, false)

  pure def bootRetainedBaseState(target: int): NodeState =
    seedBootstrappedCheckpointState(target, true, false)

  pure def bootMetadataBaseState(target: int): NodeState =
    seedBootstrappedCheckpointState(target, true, true)

  pure def emptyWrittenBaseState(target: int): NodeState =
    writeResult(emptyNodeState(target), target, 11)

  pure def bootWrittenBaseState(target: int): NodeState =
    writeResult(seedBootstrappedCheckpointState(target, false, false), target, 12)

  pure def writeBasePhaseState(base: NodeState, target: int, content: int,
      currentPhase: int): NodeState =
    if (currentPhase == PHASE_BEFORE)
      base
    else
      writeBaseResult(base, target, content)

  pure def reconcilePhaseState(baseWriteState: NodeState, currentPhase: int): NodeState =
    if (currentPhase == PHASE_AFTER_BASE)
      baseWriteState
    else
      reconcileResult(baseWriteState)

  pure def writeBasePreservesFinalizedTip(base: NodeState, target: int,
      content: int, currentPhase: int): bool =
    sameFinalizedTipState(
      writeBasePhaseState(base, target, content, currentPhase),
      base,
    )

  pure def writeBasePreservesFinalizedMetadata(base: NodeState, target: int,
      content: int, currentPhase: int): bool =
    sameFinalizedMetadataState(
      writeBasePhaseState(base, target, content, currentPhase),
      base,
    )

  pure def writeBasePreservesFinalizedBookkeeping(base: NodeState, target: int,
      content: int, currentPhase: int): bool =
    sameFinalizedBookkeepingState(
      writeBasePhaseState(base, target, content, currentPhase),
      base,
    )

  pure def reconcilePreservesFinalizedTip(base: NodeState, target: int,
      content: int, currentPhase: int): bool = {
    val postWrite = writeBaseResult(base, target, content)
    sameFinalizedTipState(
      reconcilePhaseState(postWrite, currentPhase),
      postWrite,
    )
  }

  pure def reconcilePreservesFinalizedMetadata(base: NodeState, target: int,
      content: int, currentPhase: int): bool = {
    val postWrite = writeBaseResult(base, target, content)
    sameFinalizedMetadataState(
      reconcilePhaseState(postWrite, currentPhase),
      postWrite,
    )
  }

  pure def reconcilePreservesFinalizedBookkeeping(base: NodeState, target: int,
      content: int, currentPhase: int): bool = {
    val postWrite = writeBaseResult(base, target, content)
    sameFinalizedBookkeepingState(
      reconcilePhaseState(postWrite, currentPhase),
      postWrite,
    )
  }

  def baseInvariant(base: NodeState, preservation: bool): bool = all {
    phasedTypeOK,
    writeEnabled(base),
    not(BASE_PHASES.contains(phase)) or preservation,
  }

  def reconcileInvariant(base: NodeState, preservation: bool): bool = all {
    phasedTypeOK,
    writeEnabled(base),
    not(RECONCILE_PHASES.contains(phase)) or preservation,
  }

  action base_harness_init = {
    nondet peer = PEERS.oneOf()
    nondet content = CONTENTS.oneOf()
    all {
      target_peer' = peer,
      target_content' = content,
      phase' = PHASE_BEFORE,
    }
  }

  action base_harness_step =
    if (phase == PHASE_BEFORE)
      all {
        target_peer' = target_peer,
        target_content' = target_content,
        phase' = PHASE_AFTER_BASE,
      }
    else
      all {
        target_peer' = target_peer,
        target_content' = target_content,
        phase' = phase,
      }

  action reconcile_harness_init = {
    nondet peer = PEERS.oneOf()
    nondet content = CONTENTS.oneOf()
    all {
      target_peer' = peer,
      target_content' = content,
      phase' = PHASE_AFTER_BASE,
    }
  }

  action reconcile_harness_step =
    if (phase == PHASE_AFTER_BASE)
      all {
        target_peer' = target_peer,
        target_content' = target_content,
        phase' = PHASE_AFTER_RECONCILE,
      }
    else
      all {
        target_peer' = target_peer,
        target_content' = target_content,
        phase' = phase,
      }

  action phased_harness_init = {
    nondet peer = PEERS.oneOf()
    nondet content = CONTENTS.oneOf()
    all {
      target_peer' = peer,
      target_content' = content,
      phase' = PHASE_BEFORE,
    }
  }

  action phased_harness_step =
    if (phase == PHASE_BEFORE)
      all {
        target_peer' = target_peer,
        target_content' = target_content,
        phase' = PHASE_AFTER_BASE,
      }
    else if (phase == PHASE_AFTER_BASE)
      all {
        target_peer' = target_peer,
        target_content' = target_content,
        phase' = PHASE_AFTER_RECONCILE,
      }
    else
      all {
        target_peer' = target_peer,
        target_content' = target_content,
        phase' = phase,
      }

  val inv_write_empty_base_preserves_finalized_tip_inductive =
    baseInvariant(
      emptyBaseState(target_peer),
      writeBasePreservesFinalizedTip(emptyBaseState(target_peer), target_peer, target_content, phase),
    )

  val inv_write_empty_base_preserves_finalized_metadata_inductive =
    baseInvariant(
      emptyBaseState(target_peer),
      writeBasePreservesFinalizedMetadata(emptyBaseState(target_peer), target_peer, target_content, phase),
    )

  val inv_write_empty_base_preserves_finalized_bookkeeping_inductive =
    baseInvariant(
      emptyBaseState(target_peer),
      writeBasePreservesFinalizedBookkeeping(emptyBaseState(target_peer), target_peer, target_content, phase),
    )

  val inv_write_empty_reconcile_preserves_finalized_tip_inductive =
    reconcileInvariant(
      emptyBaseState(target_peer),
      reconcilePreservesFinalizedTip(emptyBaseState(target_peer), target_peer, target_content, phase),
    )

  val inv_write_empty_reconcile_preserves_finalized_metadata_inductive =
    reconcileInvariant(
      emptyBaseState(target_peer),
      reconcilePreservesFinalizedMetadata(emptyBaseState(target_peer), target_peer, target_content, phase),
    )

  val inv_write_empty_reconcile_preserves_finalized_bookkeeping_inductive =
    reconcileInvariant(
      emptyBaseState(target_peer),
      reconcilePreservesFinalizedBookkeeping(emptyBaseState(target_peer), target_peer, target_content, phase),
    )

  val inv_write_empty_preserves_finalized_state_inductive = all {
    inv_write_empty_base_preserves_finalized_tip_inductive,
    inv_write_empty_base_preserves_finalized_metadata_inductive,
    inv_write_empty_base_preserves_finalized_bookkeeping_inductive,
    inv_write_empty_reconcile_preserves_finalized_tip_inductive,
    inv_write_empty_reconcile_preserves_finalized_metadata_inductive,
    inv_write_empty_reconcile_preserves_finalized_bookkeeping_inductive,
  }

  val inv_write_boot_compacted_base_preserves_finalized_tip_inductive =
    baseInvariant(
      bootCompactedBaseState(target_peer),
      writeBasePreservesFinalizedTip(bootCompactedBaseState(target_peer), target_peer, target_content, phase),
    )

  val inv_write_boot_compacted_base_preserves_finalized_metadata_inductive =
    baseInvariant(
      bootCompactedBaseState(target_peer),
      writeBasePreservesFinalizedMetadata(bootCompactedBaseState(target_peer), target_peer, target_content, phase),
    )

  val inv_write_boot_compacted_base_preserves_finalized_bookkeeping_inductive =
    baseInvariant(
      bootCompactedBaseState(target_peer),
      writeBasePreservesFinalizedBookkeeping(bootCompactedBaseState(target_peer), target_peer, target_content, phase),
    )

  val inv_write_boot_compacted_reconcile_preserves_finalized_tip_inductive =
    reconcileInvariant(
      bootCompactedBaseState(target_peer),
      reconcilePreservesFinalizedTip(bootCompactedBaseState(target_peer), target_peer, target_content, phase),
    )

  val inv_write_boot_compacted_reconcile_preserves_finalized_metadata_inductive =
    reconcileInvariant(
      bootCompactedBaseState(target_peer),
      reconcilePreservesFinalizedMetadata(bootCompactedBaseState(target_peer), target_peer, target_content, phase),
    )

  val inv_write_boot_compacted_reconcile_preserves_finalized_bookkeeping_inductive =
    reconcileInvariant(
      bootCompactedBaseState(target_peer),
      reconcilePreservesFinalizedBookkeeping(bootCompactedBaseState(target_peer), target_peer, target_content, phase),
    )

  val inv_write_boot_compacted_preserves_finalized_state_inductive = all {
    inv_write_boot_compacted_base_preserves_finalized_tip_inductive,
    inv_write_boot_compacted_base_preserves_finalized_metadata_inductive,
    inv_write_boot_compacted_base_preserves_finalized_bookkeeping_inductive,
    inv_write_boot_compacted_reconcile_preserves_finalized_tip_inductive,
    inv_write_boot_compacted_reconcile_preserves_finalized_metadata_inductive,
    inv_write_boot_compacted_reconcile_preserves_finalized_bookkeeping_inductive,
  }

  val inv_write_boot_retained_base_preserves_finalized_tip_inductive =
    baseInvariant(
      bootRetainedBaseState(target_peer),
      writeBasePreservesFinalizedTip(bootRetainedBaseState(target_peer), target_peer, target_content, phase),
    )

  val inv_write_boot_retained_base_preserves_finalized_metadata_inductive =
    baseInvariant(
      bootRetainedBaseState(target_peer),
      writeBasePreservesFinalizedMetadata(bootRetainedBaseState(target_peer), target_peer, target_content, phase),
    )

  val inv_write_boot_retained_base_preserves_finalized_bookkeeping_inductive =
    baseInvariant(
      bootRetainedBaseState(target_peer),
      writeBasePreservesFinalizedBookkeeping(bootRetainedBaseState(target_peer), target_peer, target_content, phase),
    )

  val inv_write_boot_retained_reconcile_preserves_finalized_tip_inductive =
    reconcileInvariant(
      bootRetainedBaseState(target_peer),
      reconcilePreservesFinalizedTip(bootRetainedBaseState(target_peer), target_peer, target_content, phase),
    )

  val inv_write_boot_retained_reconcile_preserves_finalized_metadata_inductive =
    reconcileInvariant(
      bootRetainedBaseState(target_peer),
      reconcilePreservesFinalizedMetadata(bootRetainedBaseState(target_peer), target_peer, target_content, phase),
    )

  val inv_write_boot_retained_reconcile_preserves_finalized_bookkeeping_inductive =
    reconcileInvariant(
      bootRetainedBaseState(target_peer),
      reconcilePreservesFinalizedBookkeeping(bootRetainedBaseState(target_peer), target_peer, target_content, phase),
    )

  val inv_write_boot_retained_preserves_finalized_state_inductive = all {
    inv_write_boot_retained_base_preserves_finalized_tip_inductive,
    inv_write_boot_retained_base_preserves_finalized_metadata_inductive,
    inv_write_boot_retained_base_preserves_finalized_bookkeeping_inductive,
    inv_write_boot_retained_reconcile_preserves_finalized_tip_inductive,
    inv_write_boot_retained_reconcile_preserves_finalized_metadata_inductive,
    inv_write_boot_retained_reconcile_preserves_finalized_bookkeeping_inductive,
  }

  val inv_write_boot_metadata_base_preserves_finalized_tip_inductive =
    baseInvariant(
      bootMetadataBaseState(target_peer),
      writeBasePreservesFinalizedTip(bootMetadataBaseState(target_peer), target_peer, target_content, phase),
    )

  val inv_write_boot_metadata_base_preserves_finalized_metadata_inductive =
    baseInvariant(
      bootMetadataBaseState(target_peer),
      writeBasePreservesFinalizedMetadata(bootMetadataBaseState(target_peer), target_peer, target_content, phase),
    )

  val inv_write_boot_metadata_base_preserves_finalized_bookkeeping_inductive =
    baseInvariant(
      bootMetadataBaseState(target_peer),
      writeBasePreservesFinalizedBookkeeping(bootMetadataBaseState(target_peer), target_peer, target_content, phase),
    )

  val inv_write_boot_metadata_reconcile_preserves_finalized_tip_inductive =
    reconcileInvariant(
      bootMetadataBaseState(target_peer),
      reconcilePreservesFinalizedTip(bootMetadataBaseState(target_peer), target_peer, target_content, phase),
    )

  val inv_write_boot_metadata_reconcile_preserves_finalized_metadata_inductive =
    reconcileInvariant(
      bootMetadataBaseState(target_peer),
      reconcilePreservesFinalizedMetadata(bootMetadataBaseState(target_peer), target_peer, target_content, phase),
    )

  val inv_write_boot_metadata_reconcile_preserves_finalized_bookkeeping_inductive =
    reconcileInvariant(
      bootMetadataBaseState(target_peer),
      reconcilePreservesFinalizedBookkeeping(bootMetadataBaseState(target_peer), target_peer, target_content, phase),
    )

  val inv_write_boot_metadata_preserves_finalized_state_inductive = all {
    inv_write_boot_metadata_base_preserves_finalized_tip_inductive,
    inv_write_boot_metadata_base_preserves_finalized_metadata_inductive,
    inv_write_boot_metadata_base_preserves_finalized_bookkeeping_inductive,
    inv_write_boot_metadata_reconcile_preserves_finalized_tip_inductive,
    inv_write_boot_metadata_reconcile_preserves_finalized_metadata_inductive,
    inv_write_boot_metadata_reconcile_preserves_finalized_bookkeeping_inductive,
  }

  val inv_write_empty_written_base_preserves_finalized_tip_inductive =
    baseInvariant(
      emptyWrittenBaseState(target_peer),
      writeBasePreservesFinalizedTip(emptyWrittenBaseState(target_peer), target_peer, target_content, phase),
    )

  val inv_write_empty_written_base_preserves_finalized_metadata_inductive =
    baseInvariant(
      emptyWrittenBaseState(target_peer),
      writeBasePreservesFinalizedMetadata(emptyWrittenBaseState(target_peer), target_peer, target_content, phase),
    )

  val inv_write_empty_written_base_preserves_finalized_bookkeeping_inductive =
    baseInvariant(
      emptyWrittenBaseState(target_peer),
      writeBasePreservesFinalizedBookkeeping(emptyWrittenBaseState(target_peer), target_peer, target_content, phase),
    )

  val inv_write_empty_written_reconcile_preserves_finalized_tip_inductive =
    reconcileInvariant(
      emptyWrittenBaseState(target_peer),
      reconcilePreservesFinalizedTip(emptyWrittenBaseState(target_peer), target_peer, target_content, phase),
    )

  val inv_write_empty_written_reconcile_preserves_finalized_metadata_inductive =
    reconcileInvariant(
      emptyWrittenBaseState(target_peer),
      reconcilePreservesFinalizedMetadata(emptyWrittenBaseState(target_peer), target_peer, target_content, phase),
    )

  val inv_write_empty_written_reconcile_preserves_finalized_bookkeeping_inductive =
    reconcileInvariant(
      emptyWrittenBaseState(target_peer),
      reconcilePreservesFinalizedBookkeeping(emptyWrittenBaseState(target_peer), target_peer, target_content, phase),
    )

  val inv_write_empty_written_preserves_finalized_state_inductive = all {
    inv_write_empty_written_base_preserves_finalized_tip_inductive,
    inv_write_empty_written_base_preserves_finalized_metadata_inductive,
    inv_write_empty_written_base_preserves_finalized_bookkeeping_inductive,
    inv_write_empty_written_reconcile_preserves_finalized_tip_inductive,
    inv_write_empty_written_reconcile_preserves_finalized_metadata_inductive,
    inv_write_empty_written_reconcile_preserves_finalized_bookkeeping_inductive,
  }

  val inv_write_boot_written_base_preserves_finalized_tip_inductive =
    baseInvariant(
      bootWrittenBaseState(target_peer),
      writeBasePreservesFinalizedTip(bootWrittenBaseState(target_peer), target_peer, target_content, phase),
    )

  val inv_write_boot_written_base_preserves_finalized_metadata_inductive =
    baseInvariant(
      bootWrittenBaseState(target_peer),
      writeBasePreservesFinalizedMetadata(bootWrittenBaseState(target_peer), target_peer, target_content, phase),
    )

  val inv_write_boot_written_base_preserves_finalized_bookkeeping_inductive =
    baseInvariant(
      bootWrittenBaseState(target_peer),
      writeBasePreservesFinalizedBookkeeping(bootWrittenBaseState(target_peer), target_peer, target_content, phase),
    )

  val inv_write_boot_written_reconcile_preserves_finalized_tip_inductive =
    reconcileInvariant(
      bootWrittenBaseState(target_peer),
      reconcilePreservesFinalizedTip(bootWrittenBaseState(target_peer), target_peer, target_content, phase),
    )

  val inv_write_boot_written_reconcile_preserves_finalized_metadata_inductive =
    reconcileInvariant(
      bootWrittenBaseState(target_peer),
      reconcilePreservesFinalizedMetadata(bootWrittenBaseState(target_peer), target_peer, target_content, phase),
    )

  val inv_write_boot_written_reconcile_preserves_finalized_bookkeeping_inductive =
    reconcileInvariant(
      bootWrittenBaseState(target_peer),
      reconcilePreservesFinalizedBookkeeping(bootWrittenBaseState(target_peer), target_peer, target_content, phase),
    )

  val inv_write_boot_written_preserves_finalized_state_inductive = all {
    inv_write_boot_written_base_preserves_finalized_tip_inductive,
    inv_write_boot_written_base_preserves_finalized_metadata_inductive,
    inv_write_boot_written_base_preserves_finalized_bookkeeping_inductive,
    inv_write_boot_written_reconcile_preserves_finalized_tip_inductive,
    inv_write_boot_written_reconcile_preserves_finalized_metadata_inductive,
    inv_write_boot_written_reconcile_preserves_finalized_bookkeeping_inductive,
  }

  val inv_write_preserves_finalized_state_inductive = all {
    inv_write_empty_preserves_finalized_state_inductive,
    inv_write_boot_compacted_preserves_finalized_state_inductive,
    inv_write_boot_retained_preserves_finalized_state_inductive,
    inv_write_boot_metadata_preserves_finalized_state_inductive,
    inv_write_empty_written_preserves_finalized_state_inductive,
    inv_write_boot_written_preserves_finalized_state_inductive,
  }
}
