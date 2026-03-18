# DoltSwarm Protocol & Spec Improvements (Round 11, Consolidated)

Consolidated findings after extending the Quint coverage from `spec-tests.md`, fixing the remaining replay-safety bug at the tentative/finalized boundary, and then closing the last two cloud regressions from `specs/cloud-results/20260313-170214`.

This file reflects the implemented state in:
- `doltswarm-protocol.md`
- `specs/doltswarm_verify.qnt`

It supersedes two earlier assumptions:
- that a head becoming the only remaining tentative head after a competing branch hardens is automatically safe to project and later finalize,
- and that some older invariant oracles still described the current `RECONCILE` / `RESOLVE_CONFLICT` semantics after the replay-safety hardening.

## Outcome Summary

Round 11 closes the concrete protocol/spec coverage items that `spec-tests.md` was still calling out, and it also closes all three cloud failures from `specs/cloud-results/20260313-170214`.

The important result is not that the protocol became more eager. The correction is that the protocol and the spec now agree on a stricter and more explicit boundary between tentative projection and finalized replay:
- non-subsumed replay finalization uses a deterministic replay base reconstructed from finalized parents,
- a non-subsumed event is not finalization-eligible unless that replay is already conflict-free,
- a head that is already unsafe to replay into the current finalized accumulator stays parked even if it later becomes the only tentative head,
- `RESOLVE_CONFLICT` still resolves one explicit parked head per action, but the rest of `parked_conflicts` is determined only by the post-resolution deterministic recomputation,
- and the re-anchoring / closure-lag story is now checked against the current reconcile rule rather than an older raw fold over every ready head.

## Decisions Applied In This Round

- Finalization safety is now stated and checked directly, not only as an indirect consequence of other invariants.
- `RECONCILE` now parks immediately replay-unsafe eligible heads instead of letting them re-enter `projection_basis`.
- Post-resolution parked-state evolution is now described as a deterministic recomputation outcome, not as manual preservation of unrelated parked heads.
- Re-anchoring after closure lag is now specified and checked using the current projection rule: `projection_ready = mergeable_head_event_ids \ replay_unsafe_ready`.
- The protocol now states the action-boundary rule that `RECEIVE_EVENT`, `GC_CLOCK`, and `EVICT_PEER` do not silently mutate finalized state.
- Finalized-conflict hardening semantics are now explicit in both the protocol and the spec.
- Bootstrap/join stays transport-specific and out-of-model as a transition, but seeded states now exercise the required boundary postconditions.
- Identity separation, checkpoint-descriptor consistency, and alternate bounded heal schedules are now checked explicitly rather than treated as prose-only assumptions.

## Resolved Items

### 1. ~~Finalization replay safety was under-specified, including the second-order "sole tentative head but still unsafe" case~~ FIXED

**Refs:** `ADVANCE_STABILITY`, `RECONCILE`, replay-base reconstruction, parked-branch/finalization rules, and direct finalization-safety invariants.

- Protocol: `ADVANCE_STABILITY`, `RECONCILE`, `INV7b`, finalization ordering, and `7.1.1 Finalization of formerly-parked events`.
- Spec: `finalizationReplayBase(...)`, `replayFinalizationSafe(...)`, `replayUnsafeEligibleHeads(...)`, `finalizationEligible(...)`, and the direct replay-safety invariants.

**Status.** Fixed by making replay safety part of non-subsumed finalization eligibility and by keeping immediately replay-unsafe eligible heads parked.

**Problem in the earlier design.**

The earlier model already excluded unresolved parked branches and subsumed branches from normal replay finalization, but one deeper counterexample remained:
- after a competing branch hardened,
- another event could become the only remaining tentative head,
- yet replaying it into the new finalized accumulator would still conflict or lack a deterministic replay base.

That meant the old `RECONCILE` rule could expose a head in `projection_basis` that was already inconsistent with the protocol's own finalization boundary.

**Implemented protocol behavior.**

- `ADVANCE_STABILITY` now describes replay against a reconstructed finalized-parent basis.
- `RESOLVE_CONFLICT`'s carried parked parent is explicitly excluded from that replay basis.
- `RECONCILE` now computes replay-unsafe ready heads and keeps them in `parked_conflicts`.
- The parked-branch section now explicitly calls out the second-order case where a head becomes the only tentative head but must still remain parked.

**Implemented spec behavior.**

- Added explicit replay-base helpers for finalization.
- Added `replayUnsafeEligibleHeads(...)`.
- Updated `computeTentativeProjection(...)` so replay-unsafe eligible heads do not re-enter the live projection.
- Tightened `finalizationEligible(...)` so non-subsumed eligibility requires `replayFinalizationSafe(...)`.
- Added direct regressions:
  - `inv_finalization_replay_base_is_defined`
  - `inv_replay_unsafe_eligible_heads_are_parked`
  - `inv_finalization_eligible_events_are_conflict_free`
  - `inv_finalize_does_not_create_finalized_conflicts`

**Why this closes the issue.**

The tentative side is now conservative exactly where the finalized side requires it to be conservative. A head is not treated as live projected state if the protocol already knows it cannot be replayed safely into finalized history.

### 2. ~~The remaining cloud follow-up invariants still encoded pre-fix or over-strong recomputation claims~~ FIXED

**Refs:** `RECONCILE`, `RESOLVE_CONFLICT`, closure-lag / re-anchor rules, and the two cloud-failing invariants.

- Protocol: `RECONCILE`, `RESOLVE_CONFLICT`, and the silent-local-merge explanation.
- Spec: `computeTentativeProjection(...)`, `resolveConflictResult(...)`, `inv_reanchor_after_closure_lag`, and `inv_resolve_conflict_preserves_other_parked_heads`.

**Status.** Fixed by aligning the invariants and the supporting protocol wording with the semantics already implemented in Round 11.

**Problem in the earlier design.**

After the replay-safety fix, two cloud failures remained:
- `inv_reanchor_after_closure_lag` still predicted the old projection rule, effectively folding every ready head instead of first excluding `replayUnsafeEligibleHeads(...)`,
- `inv_resolve_conflict_preserves_other_parked_heads` overclaimed that resolving one parked head could never unpark another unless that other head stopped being a head or became subsumed.

Neither was a new protocol bug. Both were stale statements about what `RECONCILE` does after a finalized re-anchor or after a resolution write.

**Implemented protocol behavior.**

- The protocol now states explicitly that after a finalized re-anchor followed by closure catch-up, recomputation still uses the same `projection_ready = mergeable_head_event_ids \ replay_unsafe_ready` rule.
- The `RESOLVE_CONFLICT` section now states that unrelated parked heads are neither manually preserved nor manually cleared; they remain parked only if the post-resolution deterministic recomputation still parks them, and may otherwise legitimately move into `projection_basis`.

**Implemented spec behavior.**

- `inv_reanchor_after_closure_lag` now reconstructs the expected projection using:
  - `readyHeads`
  - `replayUnsafeEligibleHeads(...)`
  - `projectionReadyHeads = readyHeads \ replayUnsafeHeads`
  - anchored fold over `projectionReadyHeads`
- `inv_resolve_conflict_preserves_other_parked_heads` now checks the real guarantee:
  - if another parked head disappears from `parked_conflicts` while remaining tentative and non-subsumed,
  - it must have become part of the new `projection_basis`.

**Why this closes the issue.**

The cloud follow-up regressions now test the current deterministic recomputation contract instead of an older or stronger one. This preserves the protocol's semantics while removing stale assertions about them.

### 3. ~~Finalized-conflict durability and finalized-state action boundaries were only partially explicit~~ FIXED

**Refs:** conflict-visibility invariants, action-boundary finalized-state discipline, finalized-conflict hardening, and related Quint regressions.

- Protocol: `INV7h*`, `INV10a`, and the Quint modeling notes for finalized-state boundary checks.
- Spec: finalized-state preservation helpers and finalized-conflict lifecycle regressions.

**Status.** Fixed by documenting and checking the lifecycle directly.

**Problem in the earlier design.**

The protocol already intended finalized conflicts to be durable and non-blocking, but two things were not covered directly enough:
- when a `resolved_finalized_conflict_id` claim becomes durable truth,
- and which actions are allowed to mutate authoritative finalized state at all.

That left room for regressions where finalized-conflict state could drift without violating any narrow local property.

**Implemented protocol behavior.**

- `INV7ha` now states that an event-carried finalized-conflict resolution is only a claim until it hardens or arrives through finalized sync.
- `INV10a` now states that `RECEIVE_EVENT`, `GC_CLOCK`, and `EVICT_PEER` do not directly mutate authoritative finalized state or synthesize new finalized conflicts.

**Implemented spec behavior.**

- Added `inv_resolved_finalized_conflict_hardens_and_syncs`.
- Added finalized-state preservation checks for:
  - `inv_receive_preserves_finalized_state`
  - `inv_gc_preserves_finalized_state`
  - `inv_evict_preserves_finalized_state`
- Added `inv_finalize_does_not_create_finalized_conflicts`.

**Why this closes the issue.**

The allowed finalized-state mutation surface is now explicit and regression-checked. Finalized conflicts are preserved durably until a protocol-sanctioned hardening step removes them.

### 4. ~~Bootstrap/join postconditions were documented but not exercised directly~~ FIXED

**Refs:** bootstrap boundary contract, Quint modeling notes, and seeded bootstrap regressions.

- Protocol: bootstrap boundary contract and Quint modeling notes.
- Spec: seeded bootstrapped-state helpers and bootstrap invariants.

**Status.** Fixed by keeping bootstrap out of the transition system but adding seeded-state regressions for the contract it must establish.

**Problem in the earlier design.**

Bootstrap/join was intentionally left out of the core state machine, which is the right scoping choice, but that also meant the spec had no direct regression coverage for the required postconditions after a node rejoins with transferred finalized state and retained clock state.

**Implemented protocol behavior.**

- The bootstrap contract remains transport-specific and out-of-model.
- The protocol now states that the Quint model covers this contract through seeded bootstrapped states rather than through a bootstrap transition.

**Implemented spec behavior.**

- Added:
  - `inv_bootstrap_seed_compacted_ready`
  - `inv_bootstrap_seed_retained_ready`
  - `inv_bootstrap_seed_equal_tip_metadata_repair`

These cover compacted checkpoint bootstrap, retained finalized-body bootstrap, and equal-tip finalized-metadata repair after join.

**Why this closes the issue.**

The model still avoids inventing a fake transport layer, but the bootstrap boundary is no longer untested.

### 5. ~~Identity separation and checkpoint-descriptor consistency were protocol claims without direct regression checks~~ FIXED

**Refs:** protocol motivation around the three identities, checkpoint-descriptor consistency, and the corresponding Quint invariants.

- Protocol: the identity model and `INV3a` / `INV3b`.
- Spec: identity-separation regressions and checkpoint-descriptor consistency checks.

**Status.** Fixed by adding direct regressions.

**Problem in the earlier design.**

The document already relied on two key architectural claims:
- the event layer, content layer, and finalized commit layer use different identities for different jobs,
- and reachable checkpoint descriptors are synchronized finalized metadata, not a best-effort hint.

Those claims were central to the motivation, but they were not being checked directly.

**Implemented spec behavior.**

- Added identity-separation regressions:
  - `inv_same_root_different_parents_distinct_finalized_commit_ids`
  - `inv_same_prolly_root_does_not_collapse_event_history`
- Added retained-body/descriptor consistency:
  - `inv_checkpoint_descriptor_root_consistency`

**Why this closes the issue.**

The Quint suite now enforces architectural claims that the protocol depends on rather than leaving them as prose-only design intent.

### 6. ~~Coverage around bounded partition-heal schedule variation was too narrow~~ FIXED

**Refs:** bounded partition-heal modeling notes and alternate fair-schedule regressions.

- Protocol: the Quint modeling notes for bounded fairness / schedule coverage.
- Spec: alternate scheduled heal-round invariants.

**Status.** Fixed by broadening the bounded regression set.

**Problem in the earlier design.**

Partition-heal convergence checks were still too tied to one deterministic repeated schedule. That was enough for useful bounded evidence, but too narrow for the level of confidence `spec-tests.md` was aiming for.

**Implemented spec behavior.**

- Added alternate bounded heal-schedule checks:
  - `inv_alt_scheduled_heal_round_frontier_nonincreasing`
  - `inv_alt_scheduled_heal_bounded_converges`

**Why this closes the issue.**

The bounded model is still not a full liveness proof for arbitrary schedules, but it no longer depends on one single heal order.

## Validation Run

Round 11 was validated in three stages:

1. The added coverage from the `spec-tests.md` pass was exercised with focused runs of the new invariants at bounded settings.
2. A later reduced broad sweep exposed one real replay-safety bug, which led to the replay-unsafe parking fix above.
3. The last two cloud failures then turned out to be stale / over-strong invariants, and were patched to match the implemented deterministic recomputation semantics.

Post-fix targeted checks that passed include:
- `inv_finalization_eligible_events_are_conflict_free`
  - including the previously failing cloud seed `0x90e9bfd374da0a5e`
- `inv_finalization_replay_base_is_defined`
- `inv_replay_unsafe_eligible_heads_are_parked`
- `inv_finalize_does_not_create_finalized_conflicts`
- `inv_resolved_finalized_conflict_hardens_and_syncs`
- `inv_reanchor_after_closure_lag`
  - including the previously failing cloud seed `0xe409e61cf5be6af6`
- `inv_resolve_conflict_preserves_other_parked_heads`
  - including the previously failing cloud seed `0xf49c085f5a1d6e55`

The focused reruns were checked at bounded settings up to `SAMPLES=5000`, `STEPS=20`, with the cloud regressions also reproduced and rerun at their original `70`-step seeds.

A reduced whole-suite sweep at `task quint:run SAMPLES=2000` progressed into the later sync/fairness checks without surfacing a failure during observation, but it was stopped before completion. Round 11 therefore has strong targeted validation, not a claimed full-suite pass.

## Remaining Follow-Up

Round 11 closes the concrete safety and boundary items that `spec-tests.md` was still flagging. No remaining item from that document currently looks like a missing core protocol/spec rule.

What still remains is narrower:

- A full temporal/liveness proof for arbitrary fair schedules is still not present. The current model only gives bounded progress/convergence evidence under selected schedules.
- Concrete bootstrap transport mechanics are still intentionally outside the core protocol/spec. The seeded states cover postconditions, not snapshot-transfer mechanics.
- Deeper operational stress runs of the full suite are still useful, but that is now a verification-depth concern rather than a missing-spec concern.
