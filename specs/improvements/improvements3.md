# DoltSwarm Protocol & Spec Improvements (Round 3, Consolidated)

Consolidated findings after integrating:
- prior review findings in this round, and
- the follow-up assessment you provided.

Items below are intentionally **open** (not marked fixed). They are ordered by protocol correctness importance (highest first).

---

## Correctness-Prioritized Open Items

### 1. SYNC_FINALIZED NOOP branch can drop unresolved finalized conflicts

**Refs:** protocol NOOP branch behavior in `SYNC_FINALIZED`; spec `syncFinalizedCore` NOOP branch and `inv_sync_contract_noop`.

**Problem.** In the ahead/no-op branch, conflict maps are intentionally not merged. This can remove unresolved finalized conflicts that exist only on the behind peer, violating the “tracked until resolved” intent unless resolution references have actually been observed.

**Protocol fix.** In all sync branches, reconcile conflict maps as:

```
merged_conflicts = (our_conflicts ∪ their_conflicts) \ known_resolved_ids
```

Optionally add deterministic lineage relevance filtering for stale conflicts.

**Spec fix.**
- Update NOOP branch logic in `syncFinalizedCore`.
- Relax `inv_sync_contract_noop` to preserve finalized-root/events no-op semantics while allowing monotone unresolved conflict propagation.
- Add invariant: unresolved conflict IDs cannot disappear without resolution reference evidence.

**Why this should be done.**
- Prevents silent loss of unresolved finalized-layer conflict visibility.
- Maintains non-blocking behavior without sacrificing auditability.

---
### 2. SYNC_FINALIZED mode selection is still event-set-driven instead of lineage-root-driven

**Refs:** protocol `SYNC_FINALIZED` case split (subset/no-op/merge), finalized metadata model (`finalized_parents/ancestors/depth`); spec `syncMode`, `syncAdoptionReady`.

**Problem.** Branch selection currently depends on `finalized_events` subset relations, even though finalized-root lineage metadata is already present and is the structurally correct source of truth for ancestor/descendant decisions. This keeps sync behavior coupled to unbounded event catalogs and increases metadata pressure.

**Protocol fix.** Rebase mode classification on finalized-root ancestry:
- `ADOPT` if local finalized root is an ancestor of remote finalized root.
- `NOOP` if remote finalized root is an ancestor of local finalized root.
- `MERGE` otherwise.

Keep finalized event catalogs for bookkeeping and compaction, but not as primary sync branch selectors.

**Spec fix.**
- Rewrite `syncMode` and adoption gating to use lineage ancestry.
- Update subset-oriented sync regression invariants to ancestry-oriented variants.

**Why this should be done.**
- Better aligns branch semantics with actual finalized-root merge logic.
- Reduces control-plane dependence on growing finalized-event sets.
- Improves elegance: one root-lineage model for both ancestor discovery and mode selection.

---
### 3. RESOLVE_CONFLICT semantics with multiple parked heads are implicit and potentially unsafe

**Refs:** protocol `RESOLVE_CONFLICT` parents = all heads; spec `do_resolve_conflict`.

**Problem.** Resolving one parked conflict currently creates an event with parents = all heads, which can implicitly subsume other parked heads. This behavior is deterministic but under-specified and can lead to user surprise/data omission if the resolution SQL only accounts for one conflict.

**Protocol fix (recommended).** Resolve one parked event explicitly:

```
resolve_parents = active_heads ∪ {event_cid}
```

instead of `parents = all heads`.

**Spec fix.**
- Update `do_resolve_conflict` parent selection accordingly.
- Add invariant/scenario check that unresolved parked heads remain parked unless explicitly resolved/subsumed.

**Why this should be done.**
- More principled conflict lifecycle: one explicit conflict resolution per action.
- Better matches “parked events persist until explicitly resolved.”

---
### 4. Re-anchoring correctness after finalized-root changes needs stronger guarantees for single-head/closure-lag states

**Refs:** protocol `SYNC_FINALIZED` re-anchor step; spec `computeMergedRoot` path when `readyHeads.empty()`.

**Problem.** There are states where `heads` is non-empty but no parent-closed mergeable head exists yet; `computeMergedRoot` returns existing `latest_root`. After finalized-root changes, this can temporarily preserve stale tentative projection until closure arrives. This is operationally acceptable only if explicitly specified and tested.

**Protocol fix.**
- Clarify re-anchoring contract: after finalized-root change, tentative projection is provisional until parent/chunk closure conditions are met.
- Add explicit trigger semantics for re-run on closure completion.

**Spec fix.**
- Add a scenario check that stale projection is eventually eliminated once required closure is available.

**Why this should be done.**
- Avoids false confidence that re-anchoring is instantaneous in all states.
- Tightens eventual-consistency argument under closure lag.

---
### 5. Normalize `parked_conflicts` semantics in `doltswarm-protocol.md` (authoritative set + optional local detail cache)

**Refs:** protocol §2.2 state table, §2.4 actions, §2.5 INV9, §3 modeling notes, PEER_JOIN snapshot shape; spec uses set only.

**Problem.** The protocol document currently mixes two concerns:
- authoritative replicated state needed for correctness (`which events are parked`), and
- optional presentation/debug payload (`tables/details`).

This creates drift with spec/snapshot set semantics and blurs what is actually part of replicated protocol state.

**Proposed change.**
- Protocol-level `parked_conflicts` should be `Set[EventCID]`.
- Conflict detail payload (tables/details) should be optional local diagnostic metadata, explicitly non-protocol.

**Required `doltswarm-protocol.md` edits (next step).**
- In **§2.2 Per-Peer State**, change `parked_conflicts` type from map-to-details to `Set[EventCID]`.
- Add one explicit note in §2.2 that implementations MAY keep a local cache (e.g. `parked_conflict_details: Map[EventCID, MergeResult]`) for UX/debugging; this cache is non-authoritative and non-replicated.
- In **§2.4 actions**, replace map-style usage with set usage:
  - `heads \\ parked_conflicts.keys()` -> `heads \\ parked_conflicts` (LOCAL_WRITE / RESOLVE_FINALIZED_CONFLICT).
  - RECONCILE conflict step should say “insert CID into parked set”; if conflict details are mentioned, mark them as optional local metadata.
- In **§2.5 INV9**, replace set-vs-map expression with pure set intersection (`finalized_events ∩ parked_conflicts == ∅`).
- In **§3 modeling notes**, remove wording that treats protocol map vs spec set as intentional divergence; state that authoritative protocol semantics are set-based and optional detail maps are implementation-local.
- Ensure **PEER_JOIN snapshot** remains set-based for `parked_conflicts` and does not imply replication of diagnostic detail payload.

**Why this should be done.**
- Eliminates schema drift across protocol/spec/join snapshots.
- Keeps replicated state minimal and deterministic.

---
### 6. Merge `finalized_events` + `finalized_event_index` into a single canonical finalized-event map

**Refs:** protocol state currently tracks both set and map; spec tracks set plus `finalized_event_roots`.

**Problem.** Two overlapping structures represent finalized event identity and root mapping, increasing state complexity and requiring reconciliation helpers (`catalog` unions) across actions.

**Proposed change.**

```
finalized_events: Map[EventCID, Hash]   // canonical finalized catalog
```

Set semantics become `finalized_events.keys()`.

**Protocol updates.**
- Replace all set membership checks with key checks.
- Remove separate finalized-event index.
- Simplify `RECEIVE_EVENT`, `ADVANCE_STABILITY`, `GC_CLOCK`, `SYNC_FINALIZED`, `PEER_JOIN` snapshot.

**Spec updates.**
- Replace `finalized_events` + `finalized_event_roots` with one map.
- Remove/inline `finalizedEventCatalog` helper.

**Why this should be done.**
- Clearer model, fewer invariants, fewer synchronization edge-cases.
- Strong alignment between protocol and Quint state representation.

---
### 7. Add explicit spec invariant: no event root hash may equal conflict sentinel

**Refs:** spec event creation actions and `CONFLICT_HASH` semantics.

**Problem.** No invariant currently guards against regressions that might accidentally emit conflict sentinel values as event roots.

**Spec fix.**

```
inv_event_root_not_conflict:
  for all events in all clocks, root_hash != CONFLICT_HASH
```

**Why this should be done.**
- Cheap and catches a real class of model regressions early.

---
### 8. Quint merge abstraction still under-models non-conflicting concurrent SQL merges

**Refs:** spec `mergeRoots` abstraction over scalar `CONTENTS`.

**Problem.** Current abstraction over-produces conflicts and cannot represent disjoint-row/table non-conflicting merges well enough. This weakens confidence that formal checks validate the protocol’s core objective.

**Spec fix.**
- Upgrade abstract state to at least two independent writable dimensions.
- Conflict only on overlapping writes.
- Add convergence/commutativity checks for disjoint concurrent writes.

**Protocol fix.** None required, but document model abstraction limits explicitly.

**Why this should be done.**
- Formal model better matches “concurrent writes, auto-merge non-conflicts” goal from Motivation.

---
### 9. Lineage state can likely be simplified by making `finalized_parents` authoritative and deriving caches

**Refs:** protocol tracks `finalized_parents`, `finalized_ancestors`, `finalized_depth`; spec mirrors this.

**Problem.** Maintaining three synchronized lineage maps increases mutation surface and invariant burden.

**Proposed change.**
- Keep `finalized_parents` as authoritative lineage DAG.
- Derive ancestors/depth on demand for LCA, or keep them as optional local caches with explicit non-authoritative status.

**Protocol updates.**
- If fully derived-on-demand: exchange only parents metadata in `SYNC_FINALIZED` and snapshots.
- If cached: define cache invalidation/rebuild rules and avoid treating caches as authoritative replicated state.

**Spec updates.**
- Either remove cached maps from `NodeState`, or model them as derived values.
- Simplify lineage consistency invariants accordingly.

**Why this should be done.**
- Meaningful state reduction and less cache-coherence complexity.
- Cleaner formal contract around lineage correctness.

---
### 10. Make the shared `FixedAnchorFold` pattern explicit across RECONCILE and ADVANCE_STABILITY

**Refs:** protocol RECONCILE and ADVANCE_STABILITY fold structures; spec `computeMergedRoot` and `finalizeBatch`.

**Problem.** Both flows implement the same fixed-anchor ordered merge pattern but are documented independently, obscuring the common proof idea.

**Proposed change.**
- Define `FixedAnchorFold(heads, anchor) -> (root, conflict_set)` once.
- Reuse it normatively in both actions.

**Why this should be done.**
- Reduces conceptual surface area.
- Makes proof obligations and future modifications safer.

---
### 11. Keep `do_reconcile` in spec but gate it to non-noop transitions

**Refs:** spec standalone `do_reconcile` action.

**Problem.** Unconditional reconcile action increases branching with many no-op explorations.

**Proposed change.**
- Keep action for idempotence/re-anchor coverage.
- Add guard so it only fires when it changes `(latest_root, parked_conflicts)`.

**Why this should be done.**
- Reduces state-space cost while preserving useful coverage.

---
### 12. Heartbeat policy remains fixed-rate O(n²) under idle load

**Refs:** periodic 2s all-peer heartbeat rule.

**Problem.** Background traffic remains high regardless of activity.

**Proposed change.**
- Adaptive strategy: event-driven heartbeats + low-rate idle keepalive + jitter + optional direct probes.
- Preserve liveness with fairness assumptions rather than strict periodicity.

**Why this should be done.**
- Better communication minimization without changing safety semantics.

---
### 13. Separate normative protocol from implementation-specific sections

**Refs:** current protocol doc includes detailed Go package/file/API/test planning.

**Problem.** Normative protocol semantics are mixed with implementation plan details, which reduces clarity and transport/encoding agnosticism.

**Proposed change.**
- Split into:
  - `doltswarm-protocol.md` (normative state machine, invariants, design decisions),
  - `doltswarm-implementation.md` (Go-specific architecture/migration/testing details).

**Why this should be done.**
- Easier correctness review.
- Lower spec drift risk.
- Better alignment with “protocol should not care about implementation details.”

---
### 14. Remove `OpSummary` from protocol Event (or keep as signed optional extension, not core state-machine data)

**Refs:** protocol Event payload/signing model; spec already omits `OpSummary`.

**Problem.** `OpSummary` does not influence protocol actions but increases event size and payload complexity.

**Proposed change (preferred).**
- Remove `OpSummary` from core Event and signing/cid payloads entirely.
- Treat operation summary as local or derived metadata.

**Alternative.**
- Keep as optional signed extension payload for operational observability, but explicitly outside core protocol state-machine semantics.

**Why this should be done.**
- Smaller wire/control-plane footprint.
- Simpler canonicalization/signing model.
- Better protocol minimalism.

---

## Likely Misjudgements / Cautions (detailed)

These are points from the follow-up assessment that are likely incorrect, incomplete, or risky given the progression in `improvements.md` and `improvements2.md`.

### M1. “Finalized lineage DAG is tiny, bounded by partition recoveries”

**Why likely wrong.** In the current protocol/spec evolution, finalized checkpoint lineage is also updated during normal finalized-root advancement, not only partition merges. That means lineage size can grow with finalized-root transitions generally, so complexity assumptions based purely on partition count are optimistic.

**Risk if uncorrected.**
- Underestimates cost of fully on-demand closure/depth recomputation.
- Could lead to removing useful caches without sizing/complexity guardrails.

**Better framing.**
- Simplify lineage state, but treat DAG size as workload-dependent.
- If moving to on-demand derivation, define complexity expectations and optional bounded caches.

---

### M2. “If kept, OpSummary should be unsigned advisory metadata”

**Why likely wrong.** This weakens integrity guarantees introduced in earlier rounds. Unsigned advisory fields can be rewritten in transit and create trust ambiguity in operator-facing diagnostics. That is a regression relative to signing hardening in `improvements2.md`.

**Risk if uncorrected.**
- Potentially misleading operational metadata.
- Inconsistent trust model: core event authenticated, attached explanation not authenticated.

**Better framing.**
- Either remove `OpSummary` entirely from protocol Event, or keep it signed as an optional extension field.

---

### M3. “Single-head computeMergedRoot path is always correct after finalized-root advances”

**Why likely wrong.** This conclusion ignores closure-lag states and the `readyHeads.empty()` branch behavior where `latest_root` is preserved. After finalized-root changes, a single logical head may still be not-mergeable/partially-known locally; projection can remain stale until closure arrives and reconcile reruns.

**Risk if uncorrected.**
- Overconfidence in immediate re-anchor correctness.
- Gaps in liveness-oriented scenario coverage.

**Better framing.**
- Treat this as eventually-correct with explicit closure-triggered recomputation guarantees and tests.

---

### M4. “Remove `do_reconcile` from step; no unique coverage benefit”

**Why likely incomplete.** Standalone reconcile still provides useful idempotence and re-anchor transition coverage across nondeterministic action interleavings. Fully removing it can hide regressions in explicit reconcile-trigger logic.

**Risk if uncorrected.**
- Reduced behavioral exploration in model checking.

**Better framing.**
- Keep it, but gate to state-changing executions (item #11).

---

### M5. “Unify SYNC_FINALIZED to one merge path now”

**Why risky at this stage.** Mathematically attractive, but conflict-map propagation semantics (especially unresolved conflict retention and anti-resurrection behavior) are already subtle. Branch unification before conflict-map rules are fully normalized can reintroduce bugs that were explicitly addressed in `improvements2.md`.

**Risk if uncorrected.**
- Silent regressions in finalized-conflict visibility/accounting.

**Better framing.**
- Keep branch split until conflict-map monotonicity and lineage-based mode selection are stabilized; revisit unification later as a targeted simplification.

---

## Priority order (correctness-first)

1. #1 SYNC_FINALIZED NOOP branch can drop unresolved finalized conflicts.
2. #2 SYNC_FINALIZED mode selection should be lineage-root-driven.
3. #3 RESOLVE_CONFLICT parent semantics with multiple parked heads.
4. #4 Re-anchoring guarantees under closure lag.
5. #5 Normalize authoritative parked-conflicts semantics in the protocol doc.
6. #6 Unify finalized-event structures into one canonical map.
7. #7 Add `inv_event_root_not_conflict` in Quint.
8. #8 Improve Quint merge model fidelity for non-conflicting concurrency.
9. #9 Simplify lineage state authority/cache strategy.
10. #10 Make shared `FixedAnchorFold` explicit.
11. #11 Gate standalone `do_reconcile` to non-noop transitions.
12. #12 Improve heartbeat policy under idle load.
13. #13 Split normative protocol vs implementation sections.
14. #14 Remove (or externalize) `OpSummary` from core Event state-machine payload.
