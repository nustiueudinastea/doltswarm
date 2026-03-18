# DoltSwarm Protocol & Spec Improvements (Round 9, Consolidated)

Consolidated findings after the deep `SYNC_NOOP` regression failure around retained finalized event bodies.

This file reflects the implemented state in:
- `doltswarm-protocol.md`
- `specs/doltswarm_verify.qnt`

It supersedes the earlier assumption that equal-tip finalized metadata repair was already safe once the finalized tip itself stayed unchanged.

## Outcome Summary

Round 9 closes a retained-state bug in equal-tip `SYNC_FINALIZED`.

The important result is not that the finalized tip logic changed. The finalized tip/root behavior in `SYNC_NOOP` was already correct. The real correction is that the protocol and the spec now agree on a stronger local safety rule:
- equal-tip metadata repair may still expose new retained finalized event bodies,
- those retained finalized bodies must be hydrated and marked ready locally before they remain retained,
- if they still cannot remain parent-closed and ready, they must be compacted immediately,
- and the tentative side must then remain provisional until later anti-entropy repairs closure.

This keeps finalized metadata, retained finalized bodies, and local readiness in sync.

## Decisions Applied In This Round

- `SYNC_NOOP` remains a finalized tip/root no-op.
- Equal-tip metadata repair is now treated as a retained-finalized hydration case, not only as bookkeeping merge.
- Retained finalized cleanup is now stricter:
  - invalid retained finalized bodies are compacted even if non-finalized retained events still point at them.
- The protocol now explicitly says that this can leave the tentative side provisional until later anti-entropy repairs closure.
- The regression was validated with a deep single-invariant run rather than only bounded default-suite settings.

## Resolved Items

### 1. ~~Equal-tip `SYNC_NOOP` metadata repair could retain finalized event bodies without hydrating them locally~~ FIXED

**Refs:** retained-finalized sync rule, `SYNC_FINALIZED` post-branch cleanup, `applySyncFinalized`, and the NOOP retained-finalized regression invariant.

- Protocol: `doltswarm-protocol.md` lines 635-636.
- Spec: `specs/doltswarm_verify.qnt` lines 1377-1449 and 2331-2338.

**Status.** Fixed by making `SYNC_NOOP` hydrate retained finalized closure exposed by equal-tip metadata repair before tentative projection recomputation.

**Problem in the earlier design.**

The old `SYNC_NOOP` path did the following:
- merged finalized lineage/checkpoint/conflict metadata,
- rebuilt `finalized_events`,
- compacted retained finalized state if bookkeeping changed,
- recomputed tentative projection.

What it did **not** do was the hydration/readiness step that the `ADOPT` and `MERGE` paths already performed.

That meant a peer could learn:
- "this remote event is part of finalized history for our shared finalized tip"

without also ensuring:
- "I have the local retained event body and closure in a ready state"

This created a locally inconsistent retained state:
- an event could be both finalized and retained in `clock`,
- but not `chunks_ready`,
- or retained with missing retained ancestry after the metadata repair reshaped the finalized bookkeeping view.

**Implemented protocol behavior.**

- The retained-finalized rule now explicitly states that equal-tip `NOOP` metadata repair may reveal retained finalized bodies that must be hydrated and marked ready before proceeding.
- The rule now also states that if this cannot be maintained, those finalized event bodies must be compacted out even if tentative retained events still reference them.
- The protocol now explicitly allows the tentative side to remain provisional in that case until later anti-entropy restores closure.

**Implemented spec behavior.**

- `applySyncFinalized` now treats `bookkeepingChanged` in the `NOOP` branch as a retained-finalized hydration trigger.
- The `NOOP` branch now:
  - pulls missing closure for retained finalized event ids from the remote clock,
  - applies those remote events locally,
  - marks retained finalized event ids in the hydrated clock as `chunks_ready`,
  - compacts again,
  - then recomputes tentative projection.

**Why this closes the issue.**

Equal-tip finalized metadata repair is no longer allowed to leave behind:
- finalized-but-unready retained event bodies, or
- retained finalized event bodies whose required retained ancestry has not actually been materialized locally.

The finalized-layer bookkeeping and the local retained-body readiness model now move together.

### 2. ~~Retained-finalized compaction was too weak once invalid retained finalized bodies were still referenced by non-finalized retained events~~ FIXED

**Refs:** retained-finalized helper definitions and `compactRetainedFinalized`.

- Spec: `specs/doltswarm_verify.qnt` lines 563-589.
- Protocol: `doltswarm-protocol.md` line 635.

**Status.** Fixed by distinguishing "safe to compact" from "must not remain retained".

**Problem in the earlier design.**

The original compaction helper only removed finalized event bodies that were pruneable in the narrow GC sense:
- no other retained clock event depended on them.

That was not strong enough for the retained-finalized safety rule.

In the failing counterexample, a retained finalized event body could still be:
- not parent-closed, or
- not chunk-ready,

yet remain protected from compaction simply because a non-finalized retained event still pointed at it.

That preserved the very bad state the invariant was trying to forbid.

**Implemented spec behavior.**

- Added `invalidRetainedFinalizedEventIDs(...)`.
- `compactRetainedFinalized(...)` now compacts the union of:
  - normal GC-pruneable retained finalized events, and
  - retained finalized events that are not both parent-closed and `chunks_ready`.

This is intentionally stronger than ordinary retained-GC pruning.

**Implemented protocol behavior.**

- The retained-finalized rule now explicitly says compaction must still happen even if non-finalized retained events point at the invalid finalized body.
- In that situation the tentative side becomes provisional again, which is the safer local state than pretending the retained finalized subgraph is ready.

**Why this closes the issue.**

The protocol’s retained-finalized rule is now actually enforceable:
- if a finalized body remains retained locally, it is valid,
- otherwise it is compacted out and the node waits for later closure repair.

That matches the intended meaning of "retained finalized event bodies must stay ready or be compacted."

## Validation Run

The original failure was found in a deep suite run on:
- `inv_sync_noop_retained_finalized_clean`

After the fix, the following targeted regression passed:
- `task quint:run INVARIANT=inv_sync_noop_retained_finalized_clean SAMPLES=50000 STEPS=60`

Observed result:
- `No violation found`
- runtime about `176190ms`
- Quint seed reported at completion:
  - `0xb7c922698e193100`

This is the important validation for this round because the original bug escaped the lighter bounded runs and only showed up during a deep `SYNC_NOOP` exploration.

## Remaining Follow-Up

Round 9 closes the specific retained-finalized `SYNC_NOOP` bug, but two follow-ups still remain sensible:

- Rerun the broader finalized-sync subset after this change, not only the single failing invariant.
- Consider adding one more explicit regression invariant focused on the provisional aftermath of forced retained-finalized compaction:
  - if invalid retained finalized bodies are compacted, the resulting local projection may stay provisional, but finalized bookkeeping must remain self-consistent and no invalid retained finalized body may remain in `clock`.
