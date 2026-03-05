# DoltSwarm Protocol & Spec Improvements

Combined findings from protocol review, ordered by severity.

---

## Critical (affects correctness in normal operation)

### 1. Finalization silently drops concurrent non-conflicting writes

**Refs:** protocol §2.4 ADVANCE_STABILITY step 3 (line 267), spec `do_finalize` (line 476), protocol §2.4 RECONCILE step 3a (line 224).

**Problem.** ADVANCE_STABILITY uses `mergeRoots(finalized_root, e.root_hash, finalized_root)` — the ancestor is the *accumulator*, making every step a fast-forward to `e.root_hash`. RECONCILE correctly uses a *fixed* ancestor (`finalized_root` at entry). The two merge patterns are inconsistent:

- RECONCILE: `mergeRoots(acc, e.root, fixed_ancestor)` — correct three-way merge
- ADVANCE_STABILITY: `mergeRoots(acc, e.root, acc)` — always fast-forwards

Concrete scenario: events e1 (writes row A) and e2 (writes row B) are concurrent, both stable. Finalizing e1 fast-forwards to R1. Finalizing e2: `mergeRoots(R1, R2, R1)` — ours == ancestor → fast-forward to R2 = F + row B only. Row A is silently lost.

The spec cannot catch this bug because its `mergeRoots` model always returns `CONFLICT_HASH` for two distinct raw content values diverging from ancestor — it cannot represent non-conflicting concurrent writes to different tables/rows.

**Protocol fix.** ADVANCE_STABILITY should use the same fixed-ancestor pattern as RECONCILE. Save `prev_finalized_root` before the batch, use it as the merge ancestor for all events in the batch:

```
prev_fin = finalized_root
for e in sorted(newly_stable, hlc):
    result = MergeRoots(finalized_root, e.root_hash, prev_fin)
    if conflict: skip
    else: finalized_root = result
```

**Spec fix.** Either (a) change `do_finalize` to batch-process all candidates with a fixed ancestor, or (b) add a `finalization_anchor: Hash` state variable that tracks the pre-batch `finalized_root` and is used as the ancestor in `do_finalize`. Also enhance the `mergeRoots` model to produce clean merges for some concurrent-write scenarios (e.g., add distinguishable content pairs that merge non-conflictingly) so the model checker can exercise this code path. Add an invariant: "when all events are stable and unparked, `finalized_root` equals the deterministic reconciled merge result."

---

### 2. ~~Partition recovery: subset peer cannot converge (no ADOPT_FINALIZED mechanism)~~ FIXED

**Refs:** protocol §2.4 `SYNC_FINALIZED` + HEARTBEAT step 6 trigger, protocol §6.5 partition recovery, spec `do_sync_finalized`.

**Status.** Fixed by replacing merge-only finalized reconciliation with subset-aware synchronization.

**Implemented protocol behavior.**

```
SYNC_FINALIZED(peer, remote):
  if my.fin_events ⊂ remote.fin_events:
    → adopt remote (finalized_root, finalized_events)
    → re-anchor tentative events only if finalized_root changed
  if remote.fin_events ⊂ my.fin_events:
    → no-op (remote will adopt from us)
  if neither is subset:
    → deterministic three-way merge (ours/theirs/common)
    → on conflict, keep deterministic ours-side root
```

**Implemented spec behavior.**
- Replaced `do_merge_finalized` with `do_sync_finalized`.
- Removed the old strict subset-exclusion precondition.
- Added explicit subset case split (adopt / no-op / merge) in one action.
- Kept deterministic conflict handling in the merge branch.
- Wired `do_sync_finalized` into `step`.

**Why this closes the issue.** A behind peer is no longer blocked by a subset precondition; it can always converge by adoption, while true divergence still uses deterministic three-way merge.

---

### 3. ~~`computeFinalizedRoot` is unsound for nested partition recoveries~~ FIXED

**Refs:** protocol §2.4 `SYNC_FINALIZED` divergence merge path, protocol §6.5 nested partitions, spec `do_sync_finalized` lineage-LCA helpers.

**Status.** Fixed by replacing replay-based common-ancestor reconstruction with finalized checkpoint lineage metadata + LCA lookup.

**Implemented protocol behavior.**

```
SYNC_FINALIZED(peer, remote):
  exchange finalized metadata:
    - finalized_events (suffix in practice)
    - finalized_parents / finalized_ancestors / finalized_depth
  merge lineage metadata maps
  if merge branch:
    common_root = lineageLCA(our_finalized_root, their_finalized_root,
                             finalized_ancestors, finalized_depth)
    merged = MergeRoots(ours, theirs, common_root)
    update finalized_root/events
    update checkpoint lineage for resulting root
```

`ADVANCE_STABILITY` now updates finalized checkpoint lineage metadata whenever `finalized_root` advances.

**Implemented spec behavior.**
- Added lineage metadata fields to `NodeState`:
  - `finalized_parents: Hash -> Set[Hash]`
  - `finalized_ancestors: Hash -> Set[Hash]`
  - `finalized_depth: Hash -> int`
- Added lineage helpers:
  - map merge helpers
  - `lineageAddCheckpoint(...)`
  - `lineageLcaRoot(...)`
- `do_finalize` now updates lineage metadata when `finalized_root` changes.
- `do_sync_finalized` now computes `common_root` via `lineageLcaRoot(...)` over merged local+remote lineage metadata.
- Removed `computeFinalizedRoot` from the spec.

**Why this closes the issue.** Common ancestor recovery is no longer derived from replaying unioned finalized events. Nested partition recoveries use explicit finalized lineage metadata, so prior fork-and-join merge outputs are preserved.

---

## High (affects correctness in edge cases or breaks protocol guarantees)

### 4. ~~PEER_JOIN snapshot contract is internally inconsistent~~ FIXED

**Refs:** protocol §2.4 PEER_JOIN (line 308 — "all non-finalized events"), protocol §2.4 PEER_JOIN step 6 (line 325 — verify all finalized_events in clock), spec `inv_no_silent_data_loss` (line 767).

**Problem.** The STATE_SNAPSHOT says `clock` contains "all non-finalized events." But step 6 says "verify all `finalized_events` are in `clock`" — impossible if finalized events were excluded. Additionally, `MERGE_FINALIZED` common-root computation needs full Event data (root_hash) from finalized events. The invariant `inv_no_silent_data_loss` requires `finalized_events ⊂ clock.keys()`.

**Protocol fix.** Choose one model and be explicit: either (a) the clock always contains all events (match the invariant, cost: unbounded memory), or (b) define a split model: `clock_tentative` for live events, `finalized_root_log` (from fix #3) for finalized state. Update the snapshot contract and invariant to match. If using (b), the snapshot transfers `finalized_root_log` instead of expecting finalized events in the clock.

**Spec fix.** Model the chosen approach. If (b), relax `inv_no_silent_data_loss` to check log entries instead of clock membership for finalized events.

**Why this is now closed.** The protocol now explicitly defines `STATE_SNAPSHOT.clock` as the complete event clock (finalized + non-finalized) and requires `finalized_events ⊆ clock.keys()`. This matches the current Quint model (`inv_no_silent_data_loss` and `syncAdoptionReady`) and removes the PEER_JOIN contract contradiction. Unbounded memory remains a separate future GC concern (#12).

---

### 5. ~~Unilateral admission window drops violate G-Set convergence~~ FIXED

**Refs:** protocol §2.4 `RECEIVE_EVENT` strict admission rule, protocol §6.1 strict convergence requirement, protocol INV1, spec `do_receive`.

**Status.** Fixed by removing unilateral wall-time discard behavior and making clock admission strict: valid, non-duplicate events must enter the clock.

**Implemented protocol behavior.**

```
RECEIVE_EVENT(peer, e):
  if e.cid already in clock: discard (idempotent)
  else if signature invalid: discard
  else:
    MUST accept into clock
    MUST NOT discard by local wall-time checks (e.g., |now() - e.hlc.wall|)
```

§6.1 now explicitly forbids unilateral admission windows that drop otherwise-valid events. Skew handling is operational (log/flag, rate-limit, disconnect, key revocation), but valid events still enter `clock`.

**Implemented spec behavior.**
- `do_receive` continues to insert every valid non-duplicate event into `clock`.
- Added explicit spec commentary that no local wall-time admission filter is modeled or permitted.

**Why this closes the issue.** Admission is now independent of local wall clock skew, so peers no longer diverge on clock membership due to unilateral age checks. The G-Set model and INV1 convergence assumptions are preserved without adding protocol messages or coordination.

---

### 6. ~~Anti-entropy pull-on-demand does not specify ancestor-closure fetch~~ FIXED

**Refs:** protocol §2.4 HEARTBEAT step 5, protocol §2.4 RECEIVE_EVENT lifecycle/reconcile/finalize gates, spec `ancestorIdClosure`/`pullClosureCIDs`/`do_heartbeat`/`computeMergedRoot`/`finalizationCandidates`.

**Status.** Fixed by defining pull-on-demand as explicit remote-head ancestor closure fetch and enforcing parent-closure gating in both protocol and Quint state machine.

**Implemented protocol behavior.**
- HEARTBEAT mismatch path now requests full remote head set, then recursively fetches missing ancestors for each remote head (head-to-ancestor closure), and pulls chunks for fetched events.
- RECEIVE_EVENT lifecycle now includes `PARENTS_READY` (`ANNOUNCED → PARENTS_READY → CHUNKS_READY → MERGEABLE`).
- RECONCILE now operates only on heads that are both parent-closed and chunk-ready.
- ADVANCE_STABILITY finalization candidates now require parent-closure (`e is PARENTS_READY`).
- Partition-recovery clock-merge text and heartbeat design notes now explicitly reference closure-based fetch.

**Implemented spec behavior.**
- Added `ancestorIdClosure(...)`, `isParentClosed(...)`, `mergeableHeads(...)`, and `pullClosureCIDs(...)`.
- `do_heartbeat` now enqueues only closure-based pull targets (remote heads + recursive ancestors), not all missing remote-clock events.
- `computeMergedRoot` now merges/parks only parent-closed heads.
- `do_receive` now uses closure-gated `computeMergedRoot` directly (orphan heads remain announced until closure arrives).
- `finalizationCandidates` now requires parent-closure.
- Added guard invariants:
  - `inv_parked_parent_closed`
  - `inv_finalized_parent_closed`

**Why this closes the issue.** The protocol no longer leaves pull scope ambiguous, and reconciliation/finalization cannot process orphan heads. The Quint model now exercises the same closure contract instead of masking gaps via pull-all over-approximation.

---

### 7. ~~Finalized conflict path has no explicit resolution state machine~~ FIXED

**Refs:** protocol §2.2 per-peer state (`finalized_conflicts`), §2.4 `SYNC_FINALIZED` conflict branch, §2.4 `RESOLVE_FINALIZED_CONFLICT`, §2.5 INV7e/INV7h; spec `NodeState.finalized_conflicts`, `syncFinalizedCore`, `do_resolve_finalized_conflict`, `inv_sync_conflict_recorded`, `inv_finalized_conflict_visibility`.

**Status.** Fixed by adding explicit finalized-conflict tracking and an explicit resolution action while preserving non-blocking finalization.

**Implemented protocol behavior.**
- Added `finalized_conflicts: Map[FinalizedConflictID, FinalizedConflictInfo]` to per-peer state.
- In `SYNC_FINALIZED` adopt branch (`our_finalized_events ⊂ their_finalized_events`):
  - Adopt remote `finalized_conflicts` together with `finalized_root/finalized_events`.
- In `SYNC_FINALIZED` divergence merge conflict path:
  - Start from key-union of local+remote `finalized_conflicts` (preserve existing open conflicts from both peers).
  - Keep deterministic ours-side root as `finalized_root` (non-blocking).
  - Compute deterministic `conflict_id = hash(serialize(ours_root, theirs_root, common_root))`.
  - Insert/update `finalized_conflicts[conflict_id]` with conflict metadata.
- Added `RESOLVE_FINALIZED_CONFLICT(peer, conflict_id, resolution_ops)`:
  - Requires `conflict_id ∈ finalized_conflicts`.
  - Emits a normal write event via `LOCAL_WRITE` mechanics with `resolved_finalized_conflict_id = conflict_id`.
  - Removes tracked conflict entry explicitly after successful write creation/publication.
- Updated `RECEIVE_EVENT`:
  - If an event carries `resolved_finalized_conflict_id`, remove the referenced entry from `finalized_conflicts` (idempotent).
- Updated `SYNC_FINALIZED` conflict-map handling:
  - Track/merge known resolution references and filter conflict maps to exclude already-resolved ids (prevents resurrection on later merge unions).
- Added finalized-conflict persistence to `PEER_JOIN` snapshot transfer.
- Added INV7e/INV7h framing: finalized conflicts are tracked until resolved and cannot be resurrected after a known resolution reference.

**Implemented spec behavior.**
- Added `FinalizedConflictID`/`FinalizedConflict` types.
- Extended `Event` with `resolved_finalized_conflicts` (set-model of optional conflict reference).
- Added `finalized_conflicts` map to `NodeState` (initialized in `init`).
- Updated `syncFinalizedCore` adopt branch:
  - Copies `st_remote.finalized_conflicts` during finalized-state adoption, filtered by known resolved refs.
- Updated `syncFinalizedCore` merge branch:
  - Unions local+remote `finalized_conflicts` as the merge-branch base.
  - Filters merged conflicts by known resolved refs.
  - On `CONFLICT_HASH`, records deterministic finalized conflict entry.
  - Skips reinserting conflict ids already known as resolved.
  - Keeps deterministic ours-side root as effective finalized root.
- Added `do_resolve_finalized_conflict` action:
  - Removes one conflict entry explicitly.
  - Creates/broadcasts a normal write event carrying the resolved conflict reference and recomputes reconcile projection.
- Updated `do_receive`:
  - Clears tracked finalized conflicts referenced by received events.
- Wired `do_resolve_finalized_conflict` into `step`.
- Added regression checks:
  - `inv_sync_contract_adopt` (adopt branch now also requires conflict-map adoption)
  - `inv_sync_contract_merge` (merge branch must preserve both sides' pre-existing conflict IDs)
  - `inv_sync_conflict_recorded` (conflict branch must record entry)
  - `inv_sync_conflict_id_symmetric` (both sync directions derive the same conflict ID)
  - `inv_resolve_finalized_conflict_contract` (resolution removes exactly one conflict ID, does not directly mutate finalized root/events, and emits matching conflict reference)
  - `inv_finalized_conflict_visibility` (tracked entries are well-formed/lineage-rooted)
  - `inv_finalized_conflict_authenticity` (tracked entries correspond to actual merge conflicts)
  - `inv_finalized_conflict_no_resurrection` (ids referenced as resolved in observed events cannot remain in tracked conflict map)

**Why this closes the issue.** Finalized conflicts are no longer ephemeral notifications. They are first-class tracked state with explicit resolution flow, and the Quint model now checks both recording and structural integrity.

---

## Medium (protocol/spec inconsistency or incomplete specification)

### 8. ~~CID/signature canonicalization is under-specified and coupled to optional metadata~~ FIXED

**Refs:** protocol §2.1 canonical payload definitions, §2.3.1 signing/verification, §7.2 event signing notes; spec EventCID modeling comment.

**Status.** Fixed by introducing an explicit canonical payload contract that is codec-agnostic and decouples CID identity from optional advisory metadata.

**Implemented protocol behavior.**
- Replaced `UnsignedEvent` with:
  - `CIDPayload = (root_hash, sorted parents, hlc, peer, resolved_finalized_conflict_id)`
  - `SignedPayload = (CIDPayload, op_summary)`
- Defined `CanonicalEncode(v)` by required properties (deterministic, injective, canonical ordering, canonical Optional encoding), without mandating a concrete wire codec.
- Defined:
  - `EventCID = hash(CanonicalEncode(CIDPayload))`
  - `Signature = Sign(private_key, CanonicalEncode(SignedPayload))`
- Updated `LOCAL_WRITE` and signing sections accordingly.

**Implemented spec behavior.**
- Updated spec comments to match the protocol abstraction: protocol CID is canonical-hash based, while the model uses tuple CIDs as an abstraction.

**Why this closes the issue.** Parent ordering and Optional encoding are now explicitly canonicalized, cross-implementation CID/signature mismatches are avoidable, and `op_summary` no longer affects event identity.

---

### 9. ~~Deterministic ours/theirs rule differs between protocol and spec~~ FIXED

**Refs:** protocol §2.4 `SYNC_FINALIZED` divergence merge path, modeling notes; spec `syncFinalizedCore`.

**Status.** Fixed by standardizing both protocol and spec on finalized-root hash comparison.

**Implemented protocol behavior.**
- Replaced ambiguous "associated tip event by HLC" wording with:
  - compare `our_finalized_root` vs `their_finalized_root` lexicographically by hash bytes
  - lower hash = "ours", higher hash = "theirs"
- Updated corresponding design notes/examples to the same rule.

**Implemented spec behavior.**
- `syncFinalizedCore` already used root-hash ordering; comments were aligned to the same deterministic rule.

**Why this closes the issue.** Ours/theirs assignment is now unambiguous after partition merges and consistent across document + model.

---

### 10. ~~Total order "causal → HLC → CID" is redundant; simplify to HLC~~ FIXED

**Refs:** protocol ordering text in architecture/operations sections, INV4 note; spec `sortCIDsByHLC`.

**Status.** Fixed by simplifying protocol wording to HLC total order and removing stale CID tiebreak references.

**Implemented protocol behavior.**
- Reworded ordering descriptions to "HLC total order".
- Added/kept explicit causality note that parent links imply strictly lower HLC (INV4), so causal ordering is preserved by construction.
- Removed redundant "causal → HLC → CID" phrasing.

**Implemented spec behavior.**
- No algorithmic change required; spec already orders by HLC.

**Why this closes the issue.** Protocol and spec now describe the same ordering model without redundant or misleading tie-break language.

---

### 11. ~~HLC PeerID tiebreaker creates systematic parking bias~~ FIXED

**Refs:** protocol §2.1 HLC definition/notes; spec HLC ordering (`less` semantics).

**Status.** Fixed as an explicit design decision: keep raw PeerID tie-breaker for now and document fairness tradeoff as non-blocking.

**Implemented protocol behavior.**
- Retained `(wall, logical, peer)` ordering.
- Added explicit note that hashed tie-break randomization is possible but currently not used (complexity vs. benefit tradeoff).

**Implemented spec behavior.**
- No change needed; model already matches `(wall, logical, peer)` ordering.

**Why this closes the issue.** The protocol/spec mismatch and ambiguity are resolved. Remaining bias is documented as an intentional tradeoff rather than an accidental inconsistency.

---

## Low (spec improvements, documentation, long-term concerns)

### 12. ~~Unbounded clock and finalized_events growth; no GC strategy~~ FIXED

**Refs:** protocol §2.2 state table, §2.4 `GC_CLOCK`, `RECEIVE_EVENT`, `SYNC_FINALIZED`, `PEER_JOIN`, §7.5 memory notes; spec `NodeState.finalized_event_roots`, `do_gc`.

**Status.** Fixed by adding explicit finalized-history compaction plus finalized-event metadata retention.

**Implemented protocol behavior.**
- Added `finalized_event_index: Map[EventCID, Hash]` to retain finalized CID/root metadata even when finalized event bodies are pruned from `clock`.
- Added `GC_CLOCK(peer)`:
  - prune finalized events only when no non-finalized event depends on them
  - persist pruned metadata into `finalized_event_index`
  - recompute `heads` and reconcile projection.
- Updated:
  - `RECEIVE_EVENT` dedup to include `finalized_event_index`
  - `SYNC_FINALIZED` metadata exchange/adopt/merge to include finalized-event catalog semantics
  - `PEER_JOIN` snapshot and verification to include finalized metadata representation.

**Implemented spec behavior.**
- Added `finalized_event_roots` to `NodeState`.
- Added `do_gc` + helper functions (`gcPruneable`, catalog merge/retain/index helpers).
- Updated `do_receive` and `do_finalize` to maintain catalog correctness.
- Updated sync contract checks and data-loss invariant to use finalized-event catalog semantics.

**Why this closes the issue.** Finalized event bodies can now be compacted safely, and finalized CID accounting remains explicit for sync/join/invariant safety.

---

### 13. ~~Spec `inv_data_convergence` is weaker than protocol INV2~~ FIXED

**Refs:** protocol INV2 convergence intent; spec `inv_data_convergence`.

**Status.** Fixed by strengthening the invariant from the trivial single-head case to full reconcile-projection agreement for equivalent retained state.

**Implemented spec behavior.**
- Replaced old single-head-only condition with:
  - same retained `clock`
  - same `finalized_root`
  - non-empty `mergeableHeads`
  - implies same `latest_root` and same `parked_conflicts`.

**Why this closes the issue.** The invariant now directly checks deterministic derived-state convergence for the meaningful multi-head reconcile regime, not just the trivial case.

---

### 14. ~~`computeHeads` not formally defined in §2.1~~ FIXED

**Refs:** protocol §2.1 definitions, protocol actions using `computeHeads`, spec `computeHeads`.

**Status.** Fixed by adding an explicit formal definition in the core protocol data model.

**Implemented protocol behavior.**
- Added:

```
computeHeads(clock) = { cid ∈ clock.keys() | ¬∃ e ∈ clock.values(): cid ∈ e.parents }
```

**Implemented spec behavior.**
- No change required; spec already had a formal `computeHeads`.

**Why this closes the issue.** `computeHeads` is now defined once in §2.1 and referenced consistently across protocol actions.

---

### 15. §7.9 "check no pending conflicts" contradicts "writes are never blocked"

**Refs:** protocol §7.9 (line 721), protocol §2.4 LOCAL_WRITE (line 179).

**Problem.** §7.9 says `Commit`/`ExecAndCommit`: "check no pending conflicts." This reads as a precondition that blocks writes, directly contradicting §2.4 LOCAL_WRITE: "No conflict precondition — writes are never blocked."

**Fix.** Reword §7.9 to: "exclude parked conflicts from parents (active_heads = heads \\ parked_conflicts), create Event, add to clock, publish." The intent is parent selection, not a blocking check.

---

### 16. Spec `do_reconcile` purpose undocumented

**Refs:** spec `do_reconcile` (line 365).

**Problem.** `do_receive` and `do_write` both inline reconciliation already. `do_reconcile` as a standalone action is only useful when `finalized_root` changes between events (via `do_finalize` or `do_merge_finalized`), causing parking decisions to change. This role is not documented.

**Fix.** Add a comment to `do_reconcile`: "Primarily exercises re-anchoring after `finalized_root` advances (via finalization or partition merge). Steady-state reconciliation is inlined in `do_receive` and `do_write`."
