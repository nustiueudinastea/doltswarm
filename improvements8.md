# DoltSwarm Protocol & Spec Improvements (Round 8, Consolidated)

Consolidated findings after investigating nested finalized-history merges during partition healing.

This file reflects the implemented state in:
- `doltswarm-protocol.md`
- `specs/doltswarm_verify.qnt`

It supersedes the earlier assumption that nested pairwise `SYNC_FINALIZED` composition should be one-shot associative for more than two incomparable finalized tips.

## Outcome Summary

Round 8 closes the semantic mismatch that caused the old nested-merge invariant failure.

The important result is not that binary finalized merge became "more deterministic." Binary `SYNC_FINALIZED` was already deterministic for a fixed pair of finalized tips. The real correction is that the protocol and the spec now agree on the right convergence model:
- one `SYNC_FINALIZED` merge is one deterministic binary frontier-reduction step,
- healing a partition with more than two incomparable finalized tips may require repeated rounds,
- earlier merge commits remain finalized history and are later joined by merge-of-merge commits,
- eventual consistency at the history layer is therefore monotone and append-only, not one-shot associative.

## Decisions Applied In This Round

- `SYNC_FINALIZED` remains a deterministic binary operation for the current local/remote finalized pair.
- Partition healing with more than two incomparable finalized tips is now explicitly iterative.
- Previously materialized finalized merge commits are preserved as ancestors; there is no parent flattening and no finalized-history rewrite.
- The protocol goal/motivation now states eventual convergence of finalized history itself, not just eventual data convergence.
- The Quint regression strategy now checks frontier reduction and bounded merge-reconcilability instead of exact equality of different nested merge parenthesizations.

## Resolved Items

### 1. ~~Nested finalized partition healing was specified as if pairwise merge composition were one-shot associative~~ FIXED

**Refs:** protocol eventual-consistency goal, coordinator/convergence note, `SYNC_FINALIZED`, INV3, partition recovery; spec finalized-frontier helpers, `syncFinalizedCore`, and partition-heal regressions.

- Protocol: `doltswarm-protocol.md` lines 7-9, 50-52, 363, 574-630, 677, 961-973.
- Spec: `specs/doltswarm_verify.qnt` lines 736-748, 1155-1244, 1975-2028, 2989-3020.

**Status.** Fixed by changing the protocol/spec meaning of healed finalized convergence from "nested pairwise merge results must already be identical" to "different intermediate merge histories must remain merge-reconcilable by later deterministic sync rounds."

**Problem in the earlier design.**

The old spec expected:
- `sync(sync(A, B), C) == sync(A, sync(B, C))`

for pairwise-divergent finalized tips. That is too strong for a Git-like append-only finalized DAG, because:
- each binary merge commit is deterministic for its immediate parent pair,
- but different heal orders can legitimately produce different intermediate merge commits,
- and finalized commit identity depends on those immediate parents.

So the real failure was not nondeterminism of a single merge step. The failure was expecting one-shot associativity from repeated binary history merges.

**Implemented protocol behavior.**

- The Goal/Motivation section now states that finalized-history convergence is monotone and append-only under repeated deterministic sync rounds.
- The general convergence note now says that more than two incomparable finalized tips converge by repeated deterministic `SYNC_FINALIZED` rounds, not by one-shot associativity.
- `SYNC_FINALIZED` now explicitly says that one merge is one binary frontier-reduction step only.
- The merge branch now allows later rounds to merge earlier merge commits while preserving them as ancestors.
- The partition-recovery section now says that independently hardened histories are accepted as valid and may require later merge-of-merge healing steps.

**Implemented spec behavior.**

- Added `finalizedTipFrontier(...)` to reason about the maximal incomparable finalized tips in a candidate set.
- Added `finalizedDescendsFromAll(...)` to state that healed tips preserve earlier finalized histories as ancestors.
- Added `sameFinalizedState(...)` for symmetric reconciliation checks after later merge-of-merges.
- Updated `syncFinalizedCore` commentary to match the new protocol meaning: deterministic binary step, not one-shot associativity.

**Why this closes the issue.**

The protocol and the model now agree on the real history-level guarantee:
- binary finalized merges are deterministic,
- healing may take multiple rounds,
- convergence is judged by whether the incomparable frontier eventually collapses without losing history, not by whether every nested parenthesization already yields the same immediate tip.

### 2. ~~The protocol goal and motivation did not state the right eventual-consistency target for finalized history~~ FIXED

**Refs:** protocol Goal/Motivation and partition-heal framing.

- Protocol: `doltswarm-protocol.md` lines 7-9, 44-52.

**Status.** Fixed by making finalized-history convergence an explicit goal rather than leaving it implicit behind data convergence language.

**Problem in the earlier design.**

The older motivation text correctly distinguished tentative reconciliation from finalized history, but it did not say clearly enough what eventual consistency should mean once independently hardened finalized histories later meet again.

That omission made it too easy to read the protocol as requiring one-shot canonicalization, when the intended Git-like behavior is:
- preserve finalized history,
- keep healing until the remaining incomparable frontier collapses.

**Implemented protocol behavior.**

- The top-level Goal now says peers converge on a shared finalized history through monotone append-only reconciliation, even after partitions harden different finalized lineages.
- The Motivation section now says eventual consistency is not only about shared data state; it also requires eventual convergence of the finalized Dolt history itself without rollback or history rewriting.
- The boundary discussion now states the sharper target: partition healing may take multiple steps, but it must remain monotone and append-only.

**Why this closes the issue.**

The protocol now states the design target that the spec is actually checking:
- finalized-history convergence after partitions,
- with no rollback,
- no flattening,
- no rewriting of already-finalized merges.

### 3. ~~The spec regression checks were testing the wrong property for multi-peer partition healing~~ FIXED

**Refs:** spec bounded liveness/regression notes and new merge-of-merges checks; protocol spec-modeling notes.

- Protocol: `doltswarm-protocol.md` lines 743-751.
- Spec: `specs/doltswarm_verify.qnt` lines 1975-2028, 2349-2388, 2989-3020.

**Status.** Fixed by replacing the old associativity-style invariants with frontier-reduction and reconcilability checks that match the revised protocol.

**Problem in the earlier design.**

The old regressions effectively encoded:
- different 3-way heal orders must immediately yield the same finalized tip

That is not the right safety target for an append-only merge-of-merges design. The right questions are:
- does one merge step reduce the pairwise frontier correctly?
- if different heal orders create different intermediate merge commits, can those intermediates still be reconciled later?
- does later reconciliation preserve all original finalized histories as ancestors?

**Implemented spec behavior.**

- Added `inv_sync_merge_frontier_reduction`:
  - a pairwise `SYNC_MERGE` reduces the local two-tip frontier to one descendant tip.
- Added `inv_sync_nested_merge_reconcilable`:
  - different 3-way heal orders may differ in the middle, but one later merge-of-merges step must reconcile them symmetrically.
- Added `inv_three_peer_partition_heal_reconcilable`:
  - two different merge-first choices across three pairwise-divergent peers must remain joinable later while preserving the original finalized histories as ancestors.
- Kept the existing bounded pairwise convergence checks in the liveness-oriented regression section, since they are still useful as lower-level guards.

**Validation run.**

The following checks were run successfully after the protocol/spec change:
- targeted `quint run` checks for:
  - `inv_sync_merge_frontier_reduction`
  - `inv_sync_nested_merge_reconcilable`
  - `inv_three_peer_partition_heal_reconcilable`
- full bounded suite:
  - `task quint:run SAMPLES=300 STEPS=20`

**Why this closes the issue.**

The spec now tests the actual protocol claim for partition healing:
- monotone binary frontier reduction,
- bounded merge-of-merges reconcilability,
- preservation of append-only finalized history.

## Remaining Follow-Up

Round 8 fixes the protocol/spec mismatch and the immediate regression coverage, but one larger proof obligation remains open:

- The current Quint checks are still bounded safety/regression checks.
- They do not yet constitute a full liveness proof that, after quiescence and under the fairness assumptions in `doltswarm-protocol.md`, repeated healing rounds must eventually collapse any connected component's finalized frontier to one common tip.

That follow-up should likely be done by adding:
- stronger frontier-monotonicity invariants,
- a component-level rank/progress measure,
- and possibly a dedicated heal-round harness or temporal property for `quint verify`.
