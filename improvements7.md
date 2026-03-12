# DoltSwarm Protocol & Spec Improvements (Round 7, Consolidated)

Consolidated findings after the final Round 7 protocol/spec rewrite.

This file now reflects the implemented end state in:
- `doltswarm-protocol.md`
- `specs/doltswarm_verify.qnt`

It supersedes the earlier Round 7 writeup that still assumed:
- one checkpoint descriptor per finalized event,
- finalized sync triggered only by finalized tip divergence,
- a less explicit protocol boundary between core semantics, p2p transport, and Dolt integration.

## Outcome Summary

Round 7 is now closed from a protocol/spec perspective.

The important result is not just that `FinalizedCommitID` became a real commit-layer identity again. The larger cleanup is that the finalized layer is now split cleanly into:
- authoritative finalized history,
- finalized sync metadata,
- local finalized bookkeeping.

That split preserves the requirement that all peers see the same finalized `dolt log`, while avoiding the incorrect assumption that a stock Dolt commit hash must also uniquely encode every hardened checkpoint-event provenance detail.

## Decisions Applied In This Round

- Finalized history is modeled as real deterministic finalized commits, not content-plus-catalog fingerprints.
- `FinalizedCommitID` is derived from finalized commit payload after the exact finalized parent set is known.
- Finalized checkpoint commits may subsume multiple hardened event ids without changing the finalized commit DAG.
- `SYNC_FINALIZED` now repairs equal-tip finalized metadata divergence, not just differing finalized tips.
- `finalized_events` is now explicitly a local derived bookkeeping view, not the primary synchronized finalized payload.
- Finalized sync is now commit-centric: finalized tip/root, lineage metadata, checkpoint descriptors, finalized conflicts, and durable resolved conflict ids.
- Finalized checkpointing is canonical single-event compilation, not scheduler-dependent stable-batch materialization.
- The protocol boundary is simplified around three abstract adapter capabilities:
  - control plane,
  - content plane,
  - bootstrap/join plane.
- Tentative Layer 2 state is now an optional local materialization handle, not a required protocol identity.
- Bootstrap/join is now explicitly out of model and specified only by postconditions.
- Local witness/readiness semantics are tightened:
  - `peer_seen_frontier[self]` is locally derived,
  - `chunks_ready` is strictly local,
  - retained finalized event bodies must remain parent-closed and chunk-ready or be compacted.
- Finalized-layer terminology is now explicit and consistent across protocol prose and spec commentary.
- The singular/plural finalized-conflict claim mismatch was fixed in the protocol prose.

## Resolved Items

### 1. `FinalizedCommitID` is now real commit-layer identity, but checkpoint-event provenance is intentionally separate

**Status:** Fixed, with a different final shape than the earlier Round 7 draft implied.

**Problem in the earlier design.**

The earlier protocol/spec pair claimed `FinalizedCommitID` could represent real finalized history, but in practice it was still too entangled with finalized-event bookkeeping. Worse, if two peers finalized different events that compiled to the same checkpoint content/parent payload, the old model had no clean way to preserve both hardened event ids while still keeping the same finalized commit DAG.

**Implemented fix.**

- `FinalizedCommitID` is now commit-layer identity derived from:
  - finalized prolly-root hash,
  - exact finalized parent list,
  - finalized commit kind.
- Checkpoint-event provenance is no longer forced into the finalized commit id.
- Instead, each checkpoint finalized commit carries a descriptor set:
  - `finalized_checkpoint_events[finalized_commit_id] = { event_id -> prolly_root_hash }`
- Multiple hardened events may therefore map to the same deterministic checkpoint commit without changing finalized history identity.

**Why this is the right fix.**

This preserves the user-facing requirement:
- the finalized Dolt DAG is identical across peers,
- finalized `dolt log` stays the same across peers.

At the same time, it avoids requiring stock Dolt commit hashes to encode more semantic detail than Dolt actually controls.

**Important correction to the earlier critique.**

The original Round 7 concern was that same-tip divergence in checkpoint-event provenance would be invisible to sync. That is no longer true, because finalized metadata repair is now explicit:
- heartbeats carry a `finalized_metadata_digest`,
- `SYNC_FINALIZED` triggers when either the finalized tip differs or the finalized metadata digest differs,
- equal-tip peers can therefore still reconcile per-checkpoint event provenance and finalized conflict metadata.

### 2. Canonical checkpoint batching is now deterministic single-event compilation

**Status:** Fixed.

**Problem in the earlier design.**

Once finalized commit identity became parent-sensitive, stable-batch checkpointing stopped being harmless. Two peers with the same stable event history could produce different finalized commit chains if one peer finalized a batch in one step and another finalized the same events across multiple steps.

**Implemented fix.**

`ADVANCE_STABILITY` is now a deterministic compiler repeated to fixpoint:
- compute the currently stable candidates,
- restrict to events whose parents are already finalized,
- choose the single canonical next event by deterministic HLC/EventID order,
- merge only that event against the current finalized root via its recorded anchor,
- materialize one checkpoint finalized commit,
- repeat.

**Why this closes the issue.**

Checkpoint history is now canonical rather than scheduler-dependent. Real finalized merge commits remain reserved for finalized-lineage divergence in `SYNC_FINALIZED`, which is the actual history-level merge boundary.

### 3. Local witness/readiness semantics and bootstrap/join boundaries are now explicit

**Status:** Fixed.

**Problem in the earlier design.**

The old prose still blurred core protocol transitions with transport/bootstrap behavior, and it was not explicit enough about which state is truly local:
- `peer_seen_frontier[self]`,
- `chunks_ready`,
- retained finalized event bodies.

**Implemented fix.**

- Added uniform local-derived-state rules.
- `chunks_ready` is now explicitly local-only.
- `peer_seen_frontier[self]` must be re-derived after local `clock` mutation.
- Retained finalized event bodies must remain parent-closed and chunk-ready or be compacted.
- Replaced `PEER_JOIN`-style core semantics with a bootstrap/join boundary contract outside the core model.

**Why this closes the issue.**

The protocol now has a cleaner separation:
- the core state machine defines replicated semantics,
- transport/bootstrap code is responsible only for producing a valid local starting state that satisfies explicit postconditions.

### 4. Finalized sync is now commit-centric and no longer treats `finalized_events` as the primary replicated payload

**Status:** Fixed.

**Problem in the earlier design.**

Even after commit identity was repaired, finalized sync still treated the full finalized-event catalog too much like the primary replicated object.

**Implemented fix.**

`SYNC_FINALIZED` now exchanges and merges:
- finalized tip commit/root,
- finalized commit lineage metadata,
- per-checkpoint event-set descriptors,
- finalized conflict metadata,
- durable resolved finalized conflict ids.

`finalized_events` is then reconstructed locally from:
- the resulting finalized tip,
- reachable finalized commit lineage,
- reachable checkpoint descriptors.

**Why this closes the issue.**

The primary replicated finalized object is now the finalized commit DAG plus its auxiliary finalized-layer metadata. `finalized_events` remains available where the local state machine needs it, but it is no longer the authoritative sync payload.

## Additional Simplifications Made After The Initial Round 7 Review

These were not just fixes; they are meaningful reductions in protocol surface area.

### A. Cleaner adapter boundary

The protocol now assumes only three abstract capabilities around the core state machine:
- control-plane dissemination of small protocol messages,
- content-addressed fetch of required roots/chunks,
- bootstrap/join state transfer before core transitions begin.

This removes unnecessary normative dependence on:
- direct mesh wording,
- specific RPC style,
- provider selection details,
- transport codec details,
- implementation-specific Dolt wiring tricks.

### B. Tentative Layer 2 state is no longer over-specified

`tentative_commit_id` is now an optional local materialization handle rather than an identity the protocol cares about. A peer may realize tentative state as:
- a temporary Dolt commit,
- a ref,
- a working-set projection,
- or no explicit Layer 2 object at all.

This better matches both the old Go implementation boundary and the intended architecture.

### C. Finalized-layer terminology is now explicit

The protocol and spec now use the same three-way naming split:
- authoritative finalized history,
- finalized sync metadata,
- local finalized bookkeeping.

This removes the earlier ambiguity where `finalized_events` sometimes read like shared canonical history and sometimes like local derived state.

### D. The finalized-conflict claim wording mismatch is fixed

The protocol event field remains singular:
- `resolved_finalized_conflict_id`

The spec still models this as a singleton set for convenience:
- `resolved_finalized_conflicts`

The normative protocol prose now explicitly performs singleton union at finalization time instead of mixing singular and plural wording.

### E. Post-review fix: the Quint `FinalizedCommitID` abstraction is now injective and overflow-safe

One correctness issue remained after the main Round 7 rewrite, but it was in the
**spec abstraction**, not in the protocol direction itself.

**Problem.**

The Quint model originally encoded the parent-set portion of
`FinalizedCommitID` with a fixed-base accumulation:
- `encodeFinalizedCommitIDSet(...)`
- `finalizedCommitKey(...)`

That construction was not actually injective once recursive parent fingerprints
grew beyond the chosen radix. Distinct finalized parent sets could therefore
alias to the same abstract `FinalizedCommitID`, weakening the model's claim that
commit identity was faithfully represented.

A later spec implementation of that repair switched to a quadratic pairing
encoding (`natPair(a, b)`). That removed the aliasing problem, but it still
depended on unbounded integer arithmetic and became evaluator-fragile once the
Rust backend moved to checked `i64` arithmetic.

**Implemented fix.**

- Replaced arithmetic parent fingerprints with a pure canonical finalized-commit encoding.
- The Quint spec now represents `FinalizedCommitID` as:
  - finalized prolly-root hash,
  - a canonical token list,
  - finalized commit kind.
- That token list is built deterministically from:
  - the finalized prolly-root hash,
  - the finalized commit kind,
  - the exact sorted parent finalized-commit ids.
- Finalized checkpoint and merge commit creation are pure functions again:
  - the same finalized payload always yields the same abstract finalized commit id,
  - identity no longer depends on arithmetic growth or mutable allocation order.
- The spec now checks the resulting abstraction via:
  - `inv_finalized_commit_id_matches_state`,
  - `inv_finalized_events_derived_from_checkpoints`.
- Tightened the protocol text for `CanonicalEncode(...)` so it now explicitly
  requires canonical **map** ordering in addition to set/list ordering and
  Optional encoding.

**Why this is the right fix.**

The real protocol semantics already rely on:
- `FinalizedCommitID = hash(CanonicalEncode(FinalizedCommitPayload))`

So the model should use an abstraction that preserves structural uniqueness of
commit payloads, not one that can silently alias distinct parent sets or depend
on overflow-sensitive arithmetic. This fix restores alignment between:
- the protocol's intended finalized commit identity,
- the spec's abstract finalized commit identity,
- the Round 7 claim that finalized history is commit-centric and parent-sensitive.

## Clarification On Communication Cost

One earlier Round 7 claim was too strong: finalized communication should not be described as simply "minimal" without qualification.

The current design does minimize steady-state control traffic:
- heartbeats carry digests,
- equal-tip repair is triggered only when finalized metadata digests differ.

But finalized repair and recovery still require history-derived metadata exchange:
- lineage parents,
- checkpoint descriptors,
- finalized conflicts,
- resolved finalized conflict ids.

So the accurate statement is:
- steady-state control traffic is compact,
- finalized repair traffic is still proportional to the finalized metadata that must be reconciled.

That is not a correctness problem. It is the right tradeoff for preserving an identical finalized commit DAG while keeping checkpoint-event provenance outside the commit hash.

## Open Items

- After the post-review injective-encoding fix, no blocking Round 7 correctness issues remain in the protocol/spec.
- The remaining caution is descriptive, not semantic: future writeups should avoid implying that finalized commit hashes alone carry all finalized checkpoint provenance, or that finalized repair is constant-size.

## Verification

Focused Quint checks covering the changed behavior passed:
- `inv_sync_equal_tip_metadata_repair`
- `inv_finalized_events_derived_from_checkpoints`
- `inv_finalized_commit_id_matches_state`
- `inv_sync_contract_noop`
- `inv_sync_contract_adopt`
- `inv_sync_contract_merge`

Additional post-review verification:
- A bounded brute-force spot check of the new abstract `FinalizedCommitID`
  encoding found no collisions in the searched space.

The current protocol/spec wording also now aligns on:
- equal-tip finalized metadata repair,
- checkpoint event-set descriptors,
- commit-centric finalized sync,
- canonical single-event checkpointing,
- out-of-model bootstrap/join,
- explicit finalized-layer terminology,
- injective abstract finalized commit identity.
