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

### 7. Finalized conflict path has no explicit resolution state machine

**Refs:** protocol §2.4 MERGE_FINALIZED step 5 (line 291), §6.5 (line 499), §7.10 (line 783).

**Problem.** When `MERGE_FINALIZED` encounters a conflict, the ours-side root is chosen and the conflict is "surfaced to the user." But unlike tentative conflicts (which have `parked_conflicts`, `RESOLVE_CONFLICT`, and INV7 invariants), finalized conflicts have no state variable, no resolution action, and no invariant ensuring they are tracked. A finalized conflict can be silently lost if the node restarts or the user misses the notification.

**Protocol fix.** Add:
- `finalized_conflicts: Map[MergeID, ConflictInfo]` to per-peer state (§2.2)
- `RESOLVE_FINALIZED_CONFLICT` action (async, non-blocking, similar to `RESOLVE_CONFLICT`)
- Invariant INV7e: every finalized conflict is tracked until explicitly resolved; none is silently cleared

The resolution creates a new event whose root incorporates the conflict-losing side's data, flowing through normal LOCAL_WRITE mechanics.

**Spec fix.** Add `finalized_conflicts` to NodeState, add `do_resolve_finalized_conflict` action, add invariant.

---

## Medium (protocol/spec inconsistency or incomplete specification)

### 8. CID/signature canonicalization is under-specified and coupled to optional metadata

**Refs:** protocol §2.1 (line 105–109), §2.3.1 (line 170), §7.2 (line 543).

**Problem.** `UnsignedEvent = serialize(root_hash, parents, hlc, peer, op_summary)` — but `parents` is a `Set` (unordered) and `op_summary` is optional with a sentinel. Without a canonical serialization spec (sorted parents, field order, encoding format), cross-implementation CID/signature mismatches are inevitable. Additionally, hashing optional advisory data (`op_summary`) into the event's identity creates unnecessary coupling — events with identical data but different summaries produce different CIDs.

**Protocol fix.** Either:
- (a) Specify exact canonical serialization: parents sorted by CID bytes, fixed field order, specific encoding (e.g., protobuf with sorted repeated fields, or a custom canonical form).
- (b) Simplify by excluding `op_summary` from `UnsignedEvent` / CID computation. If tamper-proofing is needed, include it in the signature payload but not the CID:

```
UnsignedEvent = serialize(root_hash, sorted(parents), hlc, peer)  // CID input
EventCID = hash(UnsignedEvent)
SignedPayload = serialize(root_hash, sorted(parents), hlc, peer, op_summary)
Signature = Sign(private_key, SignedPayload)
```

**Spec fix.** Align spec comments with the chosen canonicalization rule.

---

### 9. Deterministic ours/theirs rule differs between protocol and spec

**Refs:** protocol §2.4 MERGE_FINALIZED step 3 (line 287), modeling notes (line 362), spec `do_merge_finalized` (line 585).

**Problem.** Protocol uses "HLC total order of the associated tip events" for ours/theirs. Spec uses `st_p.finalized_root < st_q.finalized_root` (root hash comparison). These are different orderings. Additionally, "associated tip event" is ambiguous after a MERGE_FINALIZED — the finalized_root no longer corresponds to any single event's root_hash.

**Fix.** Pick one deterministic rule and use it in both protocol and spec. Root-hash lexicographic comparison is simpler, fully local (no "tip event" lookup), and unambiguous after partition merges. Update the protocol to match the spec's approach, or vice versa.

---

### 10. Total order "causal → HLC → CID" is redundant; simplify to HLC

**Refs:** protocol §2.4 ADVANCE_STABILITY step 2 (line 266), spec `sortCIDsByHLC` (line 158).

**Problem.** The protocol specifies the total order as "causal → HLC → CID." All three levels are unnecessary:
- Causal order is subsumed by HLC: INV4 guarantees `ancestor(e1, e2) ⟹ e1.hlc < e2.hlc`.
- CID tiebreaker is never reached: HLC includes `peer`, and `tick()` ensures strict monotonicity per peer. Two distinct events always have different HLCs.

The spec already uses only HLC ordering (`sortCIDsByHLC`), which is correct. The protocol's specification is redundant and potentially confusing.

**Fix.** Replace "total order: causal → HLC → CID" with "HLC total order" throughout the protocol. Remove the CID tiebreaker mention. Add a brief note explaining that HLC subsumes causal ordering by construction (INV4).

---

### 11. HLC PeerID tiebreaker creates systematic parking bias

**Refs:** protocol §2.1 HLC definition (line 98–103), spec `hlc.qnt` `less` function (line 31).

**Problem.** The `(wall, logical, peer)` lexicographic comparison means lower-PeerID peers' events always sort earlier when wall and logical match. In RECONCILE, the first head by total order fast-forwards (never parked); later heads are candidates for parking. This creates a systematic bias: the lower-PeerID peer's writes are less likely to be parked during conflicts.

**Fix.** Replace the raw PeerID tiebreaker with `hash(wall ⊕ logical ⊕ peer)` for the final comparison. This distributes priority randomly across peers per-event while keeping the comparison cheap. Wall/logical ties are rare in practice (nanosecond precision), so this is primarily a fairness improvement.

---

## Low (spec improvements, documentation, long-term concerns)

### 12. Unbounded clock and finalized_events growth; no GC strategy

**Refs:** protocol §7.5 (line 660), §2.2 state table (line 138–149).

**Problem.** The `clock` map and `finalized_events` set grow without bound. The protocol defines no pruning strategy. For a long-running system, this is an unbounded memory leak. After finalization, an event's full body (parents, signature, op_summary) is no longer needed for any protocol action — only the CID and root_hash matter (for MERGE_FINALIZED intersection and log lookup).

**Fix.** Add an explicit GC rule:

```
GC_CLOCK(peer):
  pruneable = { cid ∈ finalized_events |
    all events that reference cid as parent are also finalized }
  clock ← clock \ pruneable
```

Retain CID set in `finalized_events` and `finalized_root_log` entries (from fix #3). Discard full Event bodies. This bounds active memory to O(tentative events) + O(finalized CIDs × 20 bytes). Update `PEER_JOIN` snapshot to align: transfer `finalized_root_log` instead of expecting finalized events in the clock.

---

### 13. Spec `inv_data_convergence` is weaker than protocol INV2

**Refs:** protocol INV2 (line 336), spec `inv_data_convergence` (line 733).

**Problem.** The spec checks: same clock + both single-head + same heads → same `latest_root`. This is the trivial case. The interesting case — same events + same `finalized_root` with *multiple* heads — is only partially covered by `inv_parking_agreement`. The protocol's INV2 is stronger: same events processed in same total order → same `latest_root`.

**Spec fix.** Strengthen to:

```quint
val inv_data_convergence = PEERS.forall(p1 => PEERS.forall(p2 => {
  val s1 = nodes.get(p1)
  val s2 = nodes.get(p2)
  (s1.clock.keys() == s2.clock.keys() and
   s1.finalized_root == s2.finalized_root) implies
    (s1.latest_root == s2.latest_root and
     s1.parked_conflicts == s2.parked_conflicts)
}))
```

This directly captures: same inputs → same derived state. It subsumes the current check and `inv_parking_agreement`.

---

### 14. `computeHeads` not formally defined in §2.1

**Refs:** protocol §2.4 RECEIVE_EVENT step 5 (line 211), spec `computeHeads` (line 150).

**Problem.** `computeHeads(clock)` is used throughout the protocol (LOCAL_WRITE, RECEIVE_EVENT, RECONCILE, RESOLVE_CONFLICT) but only defined inline in RECEIVE_EVENT step 5. It deserves a formal definition in §2.1 alongside the other data types.

**Fix.** Add to §2.1:

```
computeHeads(clock) = { cid ∈ clock.keys() | ∀ e ∈ clock.values(): cid ∉ e.parents }
```

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
