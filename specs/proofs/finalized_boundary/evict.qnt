// -*- mode: Bluespec; -*-

module doltswarm_evict_inductive {
  import doltswarm_model.* from "../../model/doltswarm_model"

  pure val PHASE_BEFORE: int = 0
  pure val PHASE_AFTER_EVICT: int = 1
  pure val ALL_PHASES: Set[int] = Set(PHASE_BEFORE, PHASE_AFTER_EVICT)

  var target_peer: int
  var phase: int

  def typeOK: bool = all {
    target_peer.in(PEERS),
    phase.in(ALL_PHASES),
  }

  pure def emptyBaseState(target: int): NodeState =
    emptyNodeState(target)

  pure def emptyWrittenBaseState(target: int): NodeState =
    writeResult(emptyNodeState(target), target, 11)

  pure def evictedPeer(target: int): int =
    if (target == 1) 2 else 1

  pure def evictPhaseState(base: NodeState, target: int, currentPhase: int): NodeState =
    if (currentPhase == PHASE_BEFORE)
      base
    else
      evictResult(base, evictedPeer(target))

  pure def evictPreservesFinalizedTip(base: NodeState, target: int,
      currentPhase: int): bool =
    sameFinalizedTipState(
      evictPhaseState(base, target, currentPhase),
      base,
    )

  pure def evictPreservesFinalizedMetadata(base: NodeState, target: int,
      currentPhase: int): bool =
    sameFinalizedMetadataState(
      evictPhaseState(base, target, currentPhase),
      base,
    )

  pure def evictPreservesFinalizedBookkeeping(base: NodeState, target: int,
      currentPhase: int): bool =
    sameFinalizedBookkeepingState(
      evictPhaseState(base, target, currentPhase),
      base,
    )

  def evictInvariant(base: NodeState, preservation: bool): bool = all {
    typeOK,
    evictedPeer(target_peer) != target_peer,
    base.active_peers.contains(evictedPeer(target_peer)),
    phase == PHASE_BEFORE or preservation,
  }

  action harness_init = {
    nondet peer = PEERS.oneOf()
    all {
      target_peer' = peer,
      phase' = PHASE_BEFORE,
    }
  }

  action harness_step =
    if (phase == PHASE_BEFORE)
      all {
        target_peer' = target_peer,
        phase' = PHASE_AFTER_EVICT,
      }
    else
      all {
        target_peer' = target_peer,
        phase' = phase,
      }

  val inv_evict_empty_preserves_finalized_tip_inductive =
    evictInvariant(
      emptyBaseState(target_peer),
      evictPreservesFinalizedTip(emptyBaseState(target_peer), target_peer, phase),
    )

  val inv_evict_empty_preserves_finalized_metadata_inductive =
    evictInvariant(
      emptyBaseState(target_peer),
      evictPreservesFinalizedMetadata(emptyBaseState(target_peer), target_peer, phase),
    )

  val inv_evict_empty_preserves_finalized_bookkeeping_inductive =
    evictInvariant(
      emptyBaseState(target_peer),
      evictPreservesFinalizedBookkeeping(emptyBaseState(target_peer), target_peer, phase),
    )

  val inv_evict_empty_preserves_finalized_state_inductive = all {
    inv_evict_empty_preserves_finalized_tip_inductive,
    inv_evict_empty_preserves_finalized_metadata_inductive,
    inv_evict_empty_preserves_finalized_bookkeeping_inductive,
  }

  val inv_evict_empty_written_preserves_finalized_tip_inductive =
    evictInvariant(
      emptyWrittenBaseState(target_peer),
      evictPreservesFinalizedTip(emptyWrittenBaseState(target_peer), target_peer, phase),
    )

  val inv_evict_empty_written_preserves_finalized_metadata_inductive =
    evictInvariant(
      emptyWrittenBaseState(target_peer),
      evictPreservesFinalizedMetadata(emptyWrittenBaseState(target_peer), target_peer, phase),
    )

  val inv_evict_empty_written_preserves_finalized_bookkeeping_inductive =
    evictInvariant(
      emptyWrittenBaseState(target_peer),
      evictPreservesFinalizedBookkeeping(emptyWrittenBaseState(target_peer), target_peer, phase),
    )

  val inv_evict_empty_written_preserves_finalized_state_inductive = all {
    inv_evict_empty_written_preserves_finalized_tip_inductive,
    inv_evict_empty_written_preserves_finalized_metadata_inductive,
    inv_evict_empty_written_preserves_finalized_bookkeeping_inductive,
  }

  val inv_evict_preserves_finalized_state_inductive = all {
    inv_evict_empty_preserves_finalized_state_inductive,
    inv_evict_empty_written_preserves_finalized_state_inductive,
  }
}
