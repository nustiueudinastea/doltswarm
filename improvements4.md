# DoltSwarm Protocol & Spec Improvements (Round 4, Consolidated)

Consolidated findings after reviewing the current `doltswarm-protocol.md`, `specs/`, and prior rounds.

Decisions applied in this round:
- Heartbeat/local activity should explicitly progress local clock contribution to stability.
- `SYNC_FINALIZED` should use the simpler full-map `finalized_events` exchange contract.

All Round 4 items are now resolved and documented below in protocol/spec terms.

---

## Resolved Items (Correctness-First)

### 1. ~~Reconcile/Closure-Lag Hardening: avoid incorrect parking resets and close single-head re-anchor gaps~~ FIXED

**Refs:** protocol `RECONCILE` + `SYNC_FINALIZED` re-anchor contract; spec `computeMergedRoot`, re-anchor invariants.

**Status.** Fixed by making closure-lag behavior explicit and conservative, and by using one fixed-anchor merge semantic for ready heads.

**Implemented protocol behavior.**
- `RECONCILE` now has an explicit closure-lag branch:
  - if `heads != ∅` and `mergeable_heads == ∅`, projection is provisional,
  - `latest_root` is kept,
  - parked IDs are conservatively retained as `parked_conflicts <- parked_conflicts ∩ heads`.
- `RECONCILE` explicitly states single-head and multi-head both use the same `FixedAnchorFold(..., finalized_root)` semantics for mergeable heads (no semantic bypass in fold semantics).
- `SYNC_FINALIZED` re-anchor step now explicitly requires recomputation and explicitly forbids silently clearing unresolved parked IDs while closure is incomplete.

**Implemented spec behavior.**
- `computeMergedRoot` now mirrors closure-lag semantics:
  - if no ready heads, keep `latest_root` and keep retained parked IDs on current heads,
  - if some heads are ready, fold only ready heads and conservatively retain parked unresolved unready heads.
- Added/updated invariants:
  - `inv_reanchor_provisional_parked_retained`,
  - `inv_reanchor_after_closure_lag`,
  - fixed-point checks `inv_latest_root_is_compute_merged_root` and `inv_parked_conflicts_is_compute_merged_root`.

**Why this closes the issue.**
- Prevents accidental parked-state erasure under closure lag.
- Makes re-anchor behavior explicit, conservative, and testable.

---

### 2. ~~Self-clock contribution to `stable_hlc` should be explicit and uniform~~ FIXED

**Refs:** protocol `peer_hlc`/`stable_hlc` semantics; spec local-HLC-advancing actions.

**Status.** Fixed by making self contribution a normative requirement and mirroring it in HLC-advancing transitions.

**Implemented protocol behavior.**
- State definition now explicitly requires `peer_hlc[self]` to be updated whenever local HLC advances.
- `stable_hlc` is explicitly defined as `min(peer_hlc[p] for p in active_peers)` with self included.
- Explicit self-update + recompute is present in:
  - `LOCAL_WRITE`,
  - `RECEIVE_EVENT`,
  - heartbeat send/receive paths,
  - finalized conflict resolution flow.

**Implemented spec behavior.**
- Self-contribution updates are mirrored in HLC-advancing actions (`do_write`, `do_receive`, conflict-resolution writes, heartbeat receive handling).
- Added reduced-membership progress scenario check:
  - `inv_singleton_membership_self_stability`.

**Why this closes the issue.**
- Removes ambiguity in stability progression.
- Preserves progress behavior in reduced-membership states without heartbeat-order artifacts.

---

### 3. ~~Make full-map `SYNC_FINALIZED` catalog exchange explicit and remove suffix ambiguity~~ FIXED

**Refs:** protocol `SYNC_FINALIZED` exchange contract; spec `syncFinalizedCore`/catalog merge comments.

**Status.** Fixed by making full-map exchange normative and demoting delta/suffix language to optional optimization.

**Implemented protocol behavior.**
- `SYNC_FINALIZED` now normatively exchanges the full canonical `finalized_events` map.
- ADOPT branch remains explicit overwrite semantics (unambiguous under full-map contract).
- Delta/suffix language is retained only as non-normative optimization.

**Implemented spec behavior.**
- No semantic change needed (spec already modeled full-map union/merge behavior).
- Comments/contract wording aligned with normative full-map semantics.

**Why this closes the issue.**
- Eliminates full-map vs suffix ambiguity.
- Keeps baseline protocol semantics simple and deterministic.

---

### 4. ~~INV1 should be split to avoid retained-state vs logical-state conflation under compaction~~ FIXED

**Refs:** protocol invariants section; spec structural convergence invariant naming.

**Status.** Fixed by splitting INV1 into logical identity convergence and retained-structure convergence.

**Implemented protocol behavior.**
- Added:
  - `INV1a` Logical Event-Identity Convergence (`clock.keys ∪ finalized_events.keys`),
  - `INV1b` Retained-Structure Convergence conditioned on equal retained `clock.keys`.

**Implemented spec behavior.**
- Structural invariant naming/comments aligned via `inv_retained_structure_convergence`.

**Why this closes the issue.**
- Removes compaction-related ambiguity.
- Keeps correctness claims precise under retained-state differences.

---

### 5. ~~Add an explicit conceptual section: Tentative State vs Finalized State~~ FIXED

**Refs:** protocol state model/invariants framing; spec module-level model comments.

**Status.** Fixed by introducing an explicit conceptual split section and aligning model commentary.

**Implemented protocol behavior.**
- Added `2.2.1 Tentative State vs. Finalized State` with explicit contracts:
  - Finalized state is monotone shared checkpoint state.
  - Tentative state is deterministic but revisable local projection.
  - Tentative vs finalized conflict surfaces are explicitly distinguished.

**Implemented spec behavior.**
- Module documentation now mirrors the same split.

**Why this closes the issue.**
- Improves readability and auditability of re-anchoring/conflict behavior.
- Reduces conceptual drift between protocol prose and spec.

---

### 6. ~~Eventual-consistency claim should be tied to explicit fairness assumptions~~ FIXED

**Refs:** protocol convergence claims (Goal/INV2/INV3); spec bounded liveness-oriented scenario checks.

**Status.** Fixed by adding explicit assumptions section in the normative protocol text.

**Implemented protocol behavior.**
- Added `2.6 Eventual Consistency Assumptions` covering:
  - heartbeat fairness/retries,
  - ancestor-closure pull completion,
  - chunk availability,
  - delivery/retry fairness for accepted events/resolution events.

**Implemented spec behavior.**
- Liveness-oriented bounded scenarios/invariants retained and expanded around closure/re-anchor/stability behavior (without claiming full temporal proof).

**Why this closes the issue.**
- Makes convergence claims explicit and falsifiable.
- Documents operational prerequisites for eventual consistency.

---

### 7. ~~Spec enhancement: add minimal chunk-readiness abstraction~~ FIXED

**Refs:** protocol lifecycle `ANNOUNCED -> PARENTS_READY -> CHUNKS_READY -> MERGEABLE`; spec transition model.

**Status.** Fixed by adding explicit `chunks_ready` state and async chunk-completion action.

**Implemented protocol behavior.**
- Lifecycle text explicitly models parent-closure and chunk-readiness separately.
- `RECONCILE` and `ADVANCE_STABILITY` gates now require both readiness conditions.

**Implemented spec behavior.**
- Added `chunks_ready: Set[EventCID]` to `NodeState`.
- Added `do_chunks_ready(p, cid)` action.
- `do_write` immediately marks local event chunk-ready.
- `do_receive` models announced-not-ready receive path.
- `mergeableHeads`, `computeMergedRoot`, and `finalizationCandidates` require both parent-closure and chunk-ready.
- `do_gc` compaction now intersects `chunks_ready` with retained clock keys.
- Added `inv_chunks_ready_known`.

**Why this closes the issue.**
- Models real lifecycle states that were previously skipped.
- Prevents regressions where events are merged/finalized before data readiness.

---

### 8. ~~Further separate normative protocol from implementation-specific mechanics~~ FIXED

**Refs:** protocol vs implementation boundaries, especially former implementation-heavy sections.

**Status.** Fixed by tightening protocol text to normative contracts and moving concrete mechanics to implementation doc.

**Implemented protocol behavior.**
- `doltswarm-protocol.md` now keeps normative state-machine contracts and explicitly points implementation mechanics to `doltswarm-implementation.md`.
- Section content previously focused on implementation detail is now minimized/referenced rather than embedded.

**Implemented spec behavior.**
- No semantic changes required.

**Why this closes the issue.**
- Reduces protocol drift risk.
- Improves portability across implementations.

---

### 9. ~~Minor clarity: explicitly state `parents_sorted` is canonicalization of `parents`~~ FIXED

**Refs:** protocol event/CID payload definitions.

**Status.** Fixed with explicit normative clarification in data-type section.

**Implemented protocol behavior.**
- Added explicit sentence: `parents_sorted` is deterministic canonical ordering of `Event.parents` used only for CID/signature canonicalization; it carries no extra causal semantics.

**Implemented spec behavior.**
- Comments aligned (no semantic model changes needed).

**Why this closes the issue.**
- Prevents set/list semantic confusion and CID mismatch risks.

---

## Round 4 Remaining Open Items

None.
