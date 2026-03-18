# Quint Specs Layout

This directory now has three distinct roles:

- `model/doltswarm_model.qnt`
  The main protocol model. Keep full-state helpers, actions, seed states, and
  shared predicates here.
- `doltswarm_verify.qnt`
  Bounded whole-model regression wrapper over the model.
- `proofs/finalized_boundary/`
  Canonical action-sliced Apalache inductive proof artifacts for
  finalized-vs-tentative boundary frame properties.

## Current Status

The finalized-boundary harness suite is now the canonical proof path for these
action-local frame properties:

- `LOCAL_WRITE`: cloud-validated inductive proof
- `RECEIVE_EVENT`: cloud-validated inductive proof
- `RECONCILE`: cloud-validated inductive proof
- `GC_CLOCK`: cloud-validated inductive proof
- `EVICT_PEER`: cloud-validated inductive proof

The corresponding invariants in `doltswarm_verify.qnt` are still useful, but as
bounded regression checks and executable documentation rather than as the
primary Apalache targets.

## Canonical Finalized-Boundary Proof Artifacts

| Property | Canonical spec | Theorem |
| --- | --- | --- |
| `LOCAL_WRITE` preserves finalized state | `proofs/finalized_boundary/write.qnt` | `inv_write_preserves_finalized_state_inductive` |
| `RECEIVE_EVENT` preserves finalized state | `proofs/finalized_boundary/receive.qnt` | `inv_receive_preserves_finalized_state_inductive` |
| `RECONCILE` preserves finalized state | `proofs/finalized_boundary/reconcile.qnt` | `inv_reconcile_preserves_finalized_state_inductive` |
| `GC_CLOCK` preserves finalized state | `proofs/finalized_boundary/gc.qnt` | `inv_gc_preserves_finalized_state_inductive` |
| `EVICT_PEER` preserves finalized state | `proofs/finalized_boundary/evict.qnt` | `inv_evict_preserves_finalized_state_inductive` |

The corresponding invariants still exist in `doltswarm_verify.qnt`, but they
should be treated as bounded regression checks and executable documentation, not
as the primary proof artifacts for these action-local frame properties.

## Typical Commands

Local parse/typecheck:

```sh
quint parse specs/proofs/finalized_boundary/write.qnt
quint typecheck specs/proofs/finalized_boundary/write.qnt
```

Local inductive verify:

```sh
JVM_ARGS='-Xmx8g' quint verify specs/proofs/finalized_boundary/write.qnt \
  --init=phased_harness_init \
  --step=phased_harness_step \
  --inductive-invariant=inv_write_preserves_finalized_state_inductive
```

Cloud inductive verify:

```sh
python3 specs/tooling/quint_cloud_runner.py \
  --vms 1 \
  --provider upcloud \
  --mode verify \
  --spec proofs/finalized_boundary/write.qnt \
  --init phased_harness_init \
  --step phased_harness_step \
  --inductive \
  --steps 2 \
  --vm-type HIMEM-12xCPU-256GB \
  --verify-heap-ratio 0.75 \
  --invariant inv_write_preserves_finalized_state_inductive
```

## Current Organization

- `model/doltswarm_model.qnt`
  Main swarm semantics and transition system.
- `doltswarm_verify.qnt`
  Bounded full-model regressions over the imported model.
- `proofs/finalized_boundary/`
  One action-sliced harness per finalized-boundary frame property.
- `tooling/quint_cloud_runner.py`, `tooling/quint_invariants.py`,
  `tooling/Dockerfile.quint`
  Supporting tooling.

## Remaining Optional Work

Nothing below is required for the current finalized-boundary harness proof suite
to be useful. These are only follow-on improvements if a new proof target or
tooling need justifies more work.

1. Keep `model/doltswarm_model.qnt` as the source of truth for semantics, and
   decide whether the bounded regressions in `doltswarm_verify.qnt` should be
   slimmed down further.
2. If a future Apalache proof gets expensive, first prefer the current pattern:
   a small action-sliced harness plus smaller finalized projections.
3. If that is still not enough, consider making frame preservation more
   syntactic by splitting `NodeState` into nested finalized and tentative
   sub-records.
4. If a proof is blocked by `computeTentativeProjection`, abstract that helper
   in a proof-oriented harness and validate the abstraction with bounded checks
   and simulation.
5. For closure-heavy or structurally solver-hostile obligations, prefer a
   different tool rather than forcing more Apalache refactoring:
   TLC for tiny explicit-state runs, Alloy for transitive-closure-style checks,
   or TLAPS for manual decomposed proofs.

The default strategy should remain:

1. use `doltswarm_verify.qnt` for simulation and bounded regressions
2. use `proofs/finalized_boundary/` for canonical action-sliced Apalache proof
   artifacts for finalized-boundary frame properties
3. only revisit larger model refactors if a new property demands them
