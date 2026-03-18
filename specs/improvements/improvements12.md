# DoltSwarm Verification Improvements (Current Uncommitted State)

This note summarizes the current uncommitted work around Apalache, Quint, and
the finalized-vs-tentative proof boundary.

It reflects the current working tree state in:

- `specs/model/doltswarm_model.qnt`
- `specs/doltswarm_verify.qnt`
- `specs/proofs/finalized_boundary/write.qnt`
- `specs/proofs/finalized_boundary/receive.qnt`
- `specs/proofs/finalized_boundary/gc.qnt`
- `specs/proofs/finalized_boundary/evict.qnt`
- `specs/proofs/finalized_boundary/reconcile.qnt`
- `specs/tooling/quint_cloud_runner.py`
- `specs/tooling/quint_invariants.py`
- `specs/tooling/Dockerfile.quint`

It also references local experimental output in:

- `specs/cloud-results/`

This is not a committed protocol revision. It is a record of the current proof
engineering state and what was learned from it.

## Executive Summary

The result is now substantially better than where this effort started:

- the original main-spec invariant `inv_write_preserves_finalized_state` in
  `specs/doltswarm_verify.qnt` is still impractical under Apalache,
- but the same write/finalized-state protocol claim is now proved inductively
  in `specs/proofs/finalized_boundary/write.qnt`,
- the reconcile/finalized-state frame property is also now proved inductively
  in `specs/proofs/finalized_boundary/reconcile.qnt`,
- and `RECEIVE_EVENT`, `GC_CLOCK`, and `EVICT_PEER` now also have cloud-validated
  inductive harness proofs.

Cloud runs also show that the remaining bottleneck in the old full-spec check
is overwhelmingly JVM/Apalache-side symbolic state growth, not Docker limits
and not visible `z3` memory pressure.

So the current direction is no longer “make the old monolithic invariant pass
in the full spec at all costs.” The practical direction is:

1. keep the full spec for simulation and bounded regressions,
2. keep using Apalache on sliced inductive harnesses for finalized-boundary
   properties,
3. treat the harnesses as the primary proof artifacts for these action-local
   frame properties.

## Current Uncommitted Files

### `specs/model/doltswarm_model.qnt`

Current uncommitted changes include:

- `writeBaseResult(...)` split out from `writeResult(...)`
- `receiveBaseResult(...)` split out from `receiveResult(...)`
- `gcBaseResult(...)` split out from `gcResult(...)`
- `applyTentativeProjection(...)` factored out from `reconcileResult(...)`
- `sameFinalizedTipState(...)`
- `sameFinalizedMetadataState(...)`
- `sameFinalizedBookkeepingState(...)`
- `inv_write_base_preserves_finalized_state`
- `inv_write_reconcile_preserves_finalized_state`
- `inv_write_preserves_finalized_state` rewritten as their conjunction
- `inv_receive_base_preserves_finalized_state`
- `inv_receive_reconcile_preserves_finalized_state`
- `inv_receive_preserves_finalized_state` rewritten as their conjunction
- `inv_gc_base_preserves_finalized_state`
- `inv_gc_reconcile_preserves_finalized_state`
- `inv_gc_preserves_finalized_state` rewritten as their conjunction
- `do_reconcile(...)`, `do_gc(...)`, and `do_evict(...)` rewritten to use the
  pure helper transitions directly

This file is now the source of truth for protocol semantics, helpers, seed
states, and the full transition system.

### `specs/doltswarm_verify.qnt`

This file is now the bounded whole-model regression wrapper over
`specs/model/doltswarm_model.qnt`.

Current uncommitted changes include:

- direct imports of `basicSpells`, `hlc`, and `doltswarm_model`
- the full invariant suite moved here unchanged from the old monolithic file
- finalized-boundary invariants annotated as bounded regression checks with
  canonical proof references under `specs/proofs/finalized_boundary/`

This keeps the bounded regression entrypoint stable while separating protocol
semantics from proof/check organization.

### `specs/proofs/finalized_boundary/write.qnt`

This file is entirely new and uncommitted.

It contains a dedicated inductive proof harness for the write/finalized-state
frame property:

- small live state:
  - `target_peer`
  - `target_content`
  - `phase`
- seeded base states:
  - empty
  - bootstrapped checkpoint states
  - already-written states
- explicit phases:
  - before write
  - after `writeBaseResult`
  - after `reconcileResult`
- split finalized views:
  - tip
  - metadata
  - bookkeeping
- top-level inductive theorem:
  - `inv_write_preserves_finalized_state_inductive`

This is now the practical proof artifact for the write-preserves-finalized
property.

### `specs/proofs/finalized_boundary/receive.qnt`

This file is new and uncommitted.

It mirrors the write harness for `RECEIVE_EVENT`:

- seeded base families:
  - empty
  - bootstrapped compacted
  - bootstrapped retained
  - bootstrapped metadata
  - empty written
  - boot written
- explicit phases:
  - before receive
  - after `receiveBaseResult`
  - after `reconcileResult`
- split finalized views:
  - tip
  - metadata
  - bookkeeping
- top-level inductive theorem:
  - `inv_receive_preserves_finalized_state_inductive`

### `specs/proofs/finalized_boundary/gc.qnt`

This file is new and uncommitted.

It provides the same proof pattern for `GC_CLOCK`:

- bootstrapped retained and metadata seed states
- explicit phases:
  - before GC
  - after `gcBaseResult`
  - after `reconcileResult`
- split finalized views
- top-level inductive theorem:
  - `inv_gc_preserves_finalized_state_inductive`

### `specs/proofs/finalized_boundary/evict.qnt`

This file is new and uncommitted.

It provides a smaller harness for `EVICT_PEER`:

- empty and empty-written seed states
- explicit phases:
  - before evict
  - after evict
- split finalized views
- top-level inductive theorem:
  - `inv_evict_preserves_finalized_state_inductive`

### `specs/proofs/finalized_boundary/reconcile.qnt`

This file is new and uncommitted.

It now isolates the finalized-state frame part of standalone `RECONCILE`:

- pending multi-head seed states built from `writeBaseResult(...)`
- empty, bootstrapped compacted, bootstrapped retained, and bootstrapped
  metadata families
- explicit phases:
  - before reconcile
  - after applying a tentative overlay
- symbolic tentative overlay payload:
  - representative root value
  - parked-head subset
  - projection-basis subset
- split finalized views
- top-level inductive theorems by family and view

This harness no longer expands `computeTentativeProjection(...)` inside the
proof target. It proves the exact frame property that finalized state is
unchanged by the tentative overlay update used by `reconcileResult(...)`.

### `specs/tooling/quint_cloud_runner.py`

This file now has uncommitted cloud-runner changes to support the large-VM
verification experiments:

- support for `--verify-heap-ratio`
- verify-mode heap sizing based on a configurable fraction of container memory
  instead of a fixed hard-coded ratio
- updated config printing to show the effective heap ratio

This change was added after the first UpCloud runs showed that the old
full-spec check was hitting the JVM ceiling while the VM still had large
amounts of free RAM.

### `specs/tooling/quint_invariants.py` and `specs/tooling/Dockerfile.quint`

These files were moved under `specs/tooling/` as part of the repo
reorganization. They are unchanged in role:

- `quint_invariants.py` remains the local simulator/benchmark helper used by
  `task quint:run` and `task quint:benchmark`
- `Dockerfile.quint` remains the build definition for the cloud/local
  `quint-runner` image

The important structural change is that tooling now lives separately from the
model and proof files.

## Environment Repair

One local environment repair was required outside the repo:

- Homebrew `node@24` had to be upgraded from `24.14.0` to `24.14.0_1`

Reason:

- Quint is pinned to `node@24`,
- the old bottle linked against `libsimdjson.30.dylib`,
- Homebrew had upgraded `simdjson` to the `libsimdjson.31.dylib` bottle.

After upgrading `node@24`, Quint worked again.

## What Was Implemented

### 1. Split `LOCAL_WRITE` into base-update and reconcile pieces

The first successful cleanup was to make the write helper align with the
protocol boundary:

- `writeBaseResult(...)` performs the local tentative write-base update only
- `writeResult(...)` is now:
  - `writeBaseResult(...)`
  - then `computeTentativeProjection(...)`

This made it possible to phrase:

- “write base preserves finalized state”
- “reconcile over the write-base state preserves finalized state”

as separate proof obligations.

### 2. Split finalized equality into smaller views

The original monolithic finalized comparator was factored into:

- `sameFinalizedTipState(...)`
- `sameFinalizedMetadataState(...)`
- `sameFinalizedBookkeepingState(...)`
- `sameFinalizedState(...) = all { ... }`

This was the first proof-shape change that materially helped.

### 3. Built a dedicated write inductive harness

Instead of verifying the write property inside the full swarm transition
system, the harness in `specs/proofs/finalized_boundary/write.qnt` proves it over a
small synthetic transition system that only models the write path.

The proof shape is:

- tiny harness state
- fixed seeded base states
- explicit write phases
- separate finalized-view obligations
- top-level conjunction

That turned out to be the first tractable inductive encoding.

### 4. Cloned the harness pattern to receive, GC, evict, and reconcile

The same proof shape has now been implemented for:

- `RECEIVE_EVENT`
- `GC_CLOCK`
- `EVICT_PEER`
- standalone `RECONCILE`

That required:

- splitting `receiveResult(...)` into base + reconcile
- splitting `gcResult(...)` into base + reconcile
- using the pure transition helpers directly from the action layer
- building separate proof modules instead of trying to prove everything inside
  the full swarm transition system

### 5. Refactored reconcile to prove the frame update directly

The key reconcile-specific improvement was:

- factor `applyTentativeProjection(...)` out of `reconcileResult(...)`

and then change the reconcile proof target from:

- “symbolically run `computeTentativeProjection(...)` inside the theorem”

to:

- “prove that applying any head-subset tentative overlay leaves finalized state
  unchanged”

This keeps `reconcileResult(...)` semantically exact while removing the part of
the proof that was causing Apalache to blow up before it even reached the
checker phase.

## What Was Tried Under Apalache

### Main-spec bounded checks

The original main-spec invariant family in `specs/doltswarm_verify.qnt` was
tested repeatedly:

- `inv_write_base_preserves_finalized_state`
- `inv_write_reconcile_preserves_finalized_state`
- `inv_write_preserves_finalized_state`

Observed result:

- the split is logically cleaner,
- but even the split helpers remain too expensive in the full spec,
- and the old full-spec check still blows up at `State 2: Checking 2 state invariants`.

### Harness-based inductive checks

The write harness went through several reductions:

1. symbolic seeded-state harness
2. tag-based harness
3. concrete seeded families
4. view-split obligations
5. phase-split obligations

That sequence eventually produced a real inductive proof that passes.

### Additional harness results

Current results are:

- `specs/proofs/finalized_boundary/write.qnt`
  - top-level theorem passes locally
  - top-level theorem also passes on cloud
- `specs/proofs/finalized_boundary/receive.qnt`
  - parse/typecheck pass
  - empty-family theorem passes locally
  - top-level theorem now also passes on cloud
- `specs/proofs/finalized_boundary/gc.qnt`
  - top-level theorem passes locally at `JVM_ARGS='-Xmx8g'`
  - top-level theorem also passes on cloud
- `specs/proofs/finalized_boundary/evict.qnt`
  - top-level theorem passes locally at `JVM_ARGS='-Xmx8g'`
  - top-level theorem also passes on cloud
- `specs/proofs/finalized_boundary/reconcile.qnt`
  - parse/typecheck pass
  - empty tip lemma passes locally at `JVM_ARGS='-Xmx8g'`
  - top-level theorem now passes on UpCloud after the
    `applyTentativeProjection(...)` refactor

## Cloud Verification Results

### 1. Write harness pass on cloud

The write harness theorem passed in the cloud:

- spec: `specs/proofs/finalized_boundary/write.qnt`
- invariant: `inv_write_preserves_finalized_state_inductive`
- result: `PASS`

Cloud result directory:

- `specs/cloud-results/inductive-cloud-20260317/verify-20260317-132040/`

Relevant files:

- `summary.txt`
- `inv_write_preserves_finalized_state_inductive.pass.log`
- `quint-runner-1_monitor.csv`

This is the first successful inductive proof of the write/finalized-state
property in the current working tree.

### 2. Old full-spec write invariant still fails on UpCloud

The original full-spec invariant was retried in several UpCloud runs:

- `specs/cloud-results/upcloud-old-write-20260317/...`
- `specs/cloud-results/upcloud-old-write-256g-20260317/...`
- `specs/cloud-results/upcloud-old-write-256g-75pct-20260317/...`
- `specs/cloud-results/upcloud-old-write-256g-80pct-20260317/...`

All of these were runs of:

- spec: `specs/doltswarm_verify.qnt`
- invariant: `inv_write_preserves_finalized_state`
- steps: `2`

Observed pattern:

- 128 GB VM:
  - reaches `State 2: Checking 2 state invariants`
  - then OOMs / crashes
- 256 GB VM at default heap ratio:
  - gets much farther
  - still crashes and restarts
- 256 GB VM at `--verify-heap-ratio 0.75`:
  - still fails at `State 2`
- 256 GB VM at `--verify-heap-ratio 0.80`:
  - runs for about 2448s on the first attempt
  - still crashes at `State 2`

Important memory observation from the 80% run:

- JVM heap: about `195850m`
- peak VM usage seen in `/root/monitor.csv`: about `170290 MB`
- free RAM still remaining on the VM at failure: roughly `75-90 GB`
- no separate `z3` process was visible during sampled checks

Conclusion:

- Docker memory is not the current limiting factor.
- Even at 80% heap, the old full-spec check is still dominated by JVM-side
  symbolic state growth.

### 3. Reconcile harness cloud experiments

Standalone `RECONCILE` went through two distinct phases.

Initial failed shape:

- aggregated theorem on `HIMEM-12xCPU-256GB`
  - spec: `specs/proofs/finalized_boundary/reconcile.qnt`
  - invariant: `inv_reconcile_preserves_finalized_state_inductive`
  - behavior:
    - remained stuck after `PASS #4: InlinePass`
    - no visible `z3` process appeared
    - Java RSS climbed to roughly `70 GB` before the run was stopped cleanly
  - result category in logs:
    - `infra_grpc` because the run was interrupted for cleanup

- family-level run on `HIMEM-6xCPU-128GB`
  - first family still remained stuck after `InlinePass`
  - Java RSS climbed to roughly `60 GB`

- empty-view run on `HIMEM-6xCPU-128GB`
  - even the tip-only lemma remained stuck after `InlinePass`
  - Java RSS reached roughly `36-37 GB` after about nine minutes

Reworked passing shape:

- `reconcileResult(...)` was refactored to route its record update through
  `applyTentativeProjection(...)`
- the harness theorem was changed from “symbolically run
  `computeTentativeProjection(...)`” to “prove the tentative overlay record
  update preserves finalized state”
- after that change:
  - `quint parse` and `quint typecheck` pass
  - `JVM_ARGS='-Xmx8g' quint verify ... --inductive-invariant=inv_reconcile_empty_preserves_finalized_tip_inductive`
    passes locally in about 53s
  - the top-level theorem passes on UpCloud

Passing UpCloud run:

- spec: `specs/proofs/finalized_boundary/reconcile.qnt`
- invariant: `inv_reconcile_preserves_finalized_state_inductive`
- provider: `upcloud`
- VM: `HIMEM-12xCPU-256GB`
- heap ratio: `0.75`
- result: `PASS`
- wall time: `1664.5s`
- check CPU time: `1529.6s`
- peak VM memory: `55495MB / 257696MB` (22%)

Cloud result directory:

- `specs/cloud-results/reconcile-frame-upcloud-20260317/verify-20260317-183535/`

Relevant files:

- `summary.txt`
- `inv_reconcile_preserves_finalized_state_inductive.pass.log`
- `quint-runner-1_monitor.csv`

### 4. Receive harness pass on cloud

The full receive harness theorem also passed on UpCloud:

- spec: `specs/proofs/finalized_boundary/receive.qnt`
- invariant: `inv_receive_preserves_finalized_state_inductive`
- provider: `upcloud`
- VM: `HIMEM-12xCPU-256GB`
- heap ratio: `0.75`
- result: `PASS`
- wall time: `1657.9s`
- check CPU time: `1511.6s`
- peak VM memory: `57714MB / 257696MB` (22%)

Cloud result directory:

- `specs/cloud-results/receive-frame-upcloud-20260318/verify-20260318-160506/`

Relevant files:

- `summary.txt`
- `inv_receive_preserves_finalized_state_inductive.pass.log`
- `quint-runner-1_monitor.csv`

### 5. GC harness pass on cloud

The GC harness theorem also passed on UpCloud:

- spec: `specs/proofs/finalized_boundary/gc.qnt`
- invariant: `inv_gc_preserves_finalized_state_inductive`
- provider: `upcloud`
- VM: `HIMEM-12xCPU-256GB`
- heap ratio: `0.75`
- result: `PASS`
- wall time: `444.9s`
- check CPU time: `252.5s`
- peak VM memory: `15516MB / 257697MB` (6%)

Cloud result directory:

- `specs/cloud-results/gc-frame-upcloud-20260318/verify-20260318-163417/`

Relevant files:

- `summary.txt`
- `inv_gc_preserves_finalized_state_inductive.pass.log`
- `quint-runner-1_monitor.csv`

### 6. Evict harness pass on cloud

The evict harness theorem also passed on UpCloud:

- spec: `specs/proofs/finalized_boundary/evict.qnt`
- invariant: `inv_evict_preserves_finalized_state_inductive`
- provider: `upcloud`
- VM: `HIMEM-12xCPU-256GB`
- heap ratio: `0.75`
- result: `PASS`
- wall time: `240.9s`
- check CPU time: `67.1s`
- peak VM memory: `8035MB / 257696MB` (3%)

Cloud result directory:

- `specs/cloud-results/evict-frame-upcloud-20260318/verify-20260318-164152/`

Relevant files:

- `summary.txt`
- `inv_evict_preserves_finalized_state_inductive.pass.log`
- `quint-runner-1_monitor.csv`

## What The Current State Means

### What is now proved

The finalized-boundary proof state is now:

- `specs/proofs/finalized_boundary/write.qnt`
  - top-level inductive theorem passes on cloud
- `specs/proofs/finalized_boundary/receive.qnt`
  - top-level inductive theorem passes on cloud
- `specs/proofs/finalized_boundary/reconcile.qnt`
  - top-level inductive theorem passes on cloud
- `specs/proofs/finalized_boundary/gc.qnt`
  - top-level inductive theorem passes on cloud
- `specs/proofs/finalized_boundary/evict.qnt`
  - top-level inductive theorem passes on cloud

So the write, receive, reconcile, GC, and evict finalized-boundary claims now
all have practical cloud-validated inductive proof artifacts.

### What is not solved

The following are still not in their final desired state:

- monolithic Apalache verification of
  `specs/doltswarm_verify.qnt: inv_write_preserves_finalized_state`
- deciding whether the old full-spec bounded invariants are still worth further
  engineering effort now that the action-sliced proofs are in place

The old whole-spec write check is still useful as a stress test and as evidence
about tool limits, but it is no longer the right primary target.

## Current Limitations

### 1. The old full-spec write invariant is still not a viable Apalache target

The full model still forces Apalache to symbolically reason about too much
state and too many helper expansions at once.

### 2. The successful proof is action-sliced, not whole-spec

The harness proves the intended write boundary property, but in a dedicated
proof module rather than inside the entire swarm transition system.

### 3. The same pattern is now cloned, but proof maturity still varies

This part is now much improved:

- `RECEIVE_EVENT`, `GC_CLOCK`, `EVICT_PEER`, and standalone `RECONCILE` all
  have dedicated inductive harness modules.

The remaining limitation is narrower:

- the current proof suite is complete for these action slices, but it lives in
  separate proof harness modules rather than one whole-spec theorem.

### 4. The main bottleneck still looks JVM-side

The cloud runs strongly suggest:

- the biggest remaining optimization target is Apalache/JVM symbolic state
  growth,
- not `z3` native memory,
- and not Docker container limits.

### 5. Reconcile is no longer the blocking outlier

The reconcile frame proof is now structurally viable and cloud-proven. The
remaining work is now broader proof packaging:

- decide how much more effort should still go into the old full-spec bounded
  invariants,
- and decide whether to codify the harness-based cloud checks as the canonical
  proof suite.

## Recommended Next Step

The next step should shift away from proof discovery and toward proof
productization.

The most likely productive directions are:

1. codify the cloud-validated harness runs as the canonical finalized-boundary
   proof suite
2. keep using `specs/doltswarm_verify.qnt` for simulation and bounded
   regressions, but stop treating the old whole-spec finalized-preservation
   invariants as the primary Apalache targets
3. only return to the old monolithic invariant if there is a concrete need for
   a full-spec bounded regression despite the better harness-based proofs

That is now a better use of effort than continuing to brute-force the old
whole-spec write invariant in `specs/doltswarm_verify.qnt`.
