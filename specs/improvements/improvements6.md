# DoltSwarm Protocol & Spec Improvements (Round 6, Consolidated)

Consolidated findings after clarifying the three protocol identity layers:

- event-layer facts (`EventID`)
- content-layer facts (prolly-tree root hashes)
- finalized-history facts (`FinalizedCommitID`)

Decisions applied in this round:
- Finalized history is now identified by `FinalizedCommitID`, not by content root alone.
- Tentative emits now parent from an authoritative `projection_basis`, not raw heads.
- Finalized-conflict resolution is monotone and only hardens through finalized state.
- Stable-frontier finalization conflicts are treated as impossible by construction.
- Timeout-based false suspicion is treated semantically as temporary partition.
- Unreplicated local writes are explicitly crash-volatile.
- Naming cleanup now uses layer-explicit terms across event, content, tentative-commit, and finalized-commit state.

All Round 6 items are now either fixed, materially improved, or explicitly decided and documented below.

---

## Resolved Items (Correctness / Semantics)

### 1. ~~Finalized lineage must use an authoritative finalized-commit identity, not content-root identity~~ FIXED

**Refs:** protocol `FinalizedCommitID`, finalized state model, `ADVANCE_STABILITY`, `SYNC_FINALIZED`; spec `FinalizedCommitID`, lineage helpers, `finalizeResult`, `syncFinalizedCore`.

- Protocol: `doltswarm-protocol.md` lines 207, 247-250, 440-442, 470-526, 888-890.
- Spec: `specs/doltswarm_verify.qnt` lines 77, 108-111, 522-617, 768-799, 900-989, 1541-1645.

**Status.** Fixed by splitting finalized-history identity from finalized content identity and making finalized-commit lineage authoritative.

**Implemented protocol behavior.**
- Added `FinalizedCommitID` as the finalized-history identity.
- Split former finalized-root semantics into:
  - `finalized_commit_id`
  - `finalized_prolly_root_hash`
- Replaced root-based finalized lineage with `finalized_commit_parents`.
- `ADVANCE_STABILITY` now derives a new deterministic finalized commit from finalized content plus finalized event catalog.
- `SYNC_FINALIZED` now classifies `ADOPT` / `NOOP` / `MERGE` by finalized-commit ancestry and computes LCA on finalized-commit lineage.

**Implemented spec behavior.**
- Added `type FinalizedCommitID = (ProllyRootHash, int)`.
- Added `finalized_commit_id` and `finalized_commit_parents` to `NodeState`.
- Replaced root-lineage helpers with finalized-commit-lineage helpers:
  - `finalizedCommitLess`
  - `lineageAncestors`
  - `lineageLcaFinalizedCommit`
  - `finalizedCommitIsAncestor`
- `finalizeResult` and `syncFinalizedCore` now operate on finalized commits, not raw finalized roots.
- Added finalized-commit correctness checks, especially `inv_finalized_commit_id_matches_state`.

**Why this closes the issue.**
- Multiple finalized commits may legitimately point at the same content root.
- Content-root recurrence no longer collapses distinct history nodes.
- Finalized sync, LCA lookup, and nested partition recovery now operate on actual history identity instead of overloading content identity.

---

### 2. ~~The tentative commit zone should be explicitly demoted to local materialization~~ FIXED

**Refs:** protocol state-model split and conceptual boundary notes; spec module commentary and finalized-preservation invariants.

- Protocol: `doltswarm-protocol.md` lines 236-263, 639-646, 832-850.
- Spec: `specs/doltswarm_verify.qnt` lines 30-32, 2001-2046.

**Status.** Fixed by making the tentative/finalized split explicit in the protocol model and keeping correctness claims attached only to event state, content state, and finalized commit state.

**Implemented protocol behavior.**
- Added `2.2.1 Tentative State vs. Finalized State`.
- Explicitly defined:
  - finalized state as monotone shared finalized-commit state
  - tentative state as deterministic but revisable local projection
- Explicitly named the local tentative materialization as `tentative_commit_id`, distinct from shared `finalized_commit_id`.
- Clarified that local tentative materialization is not part of shared identity, sync mode selection, or finalized lineage semantics.

**Implemented spec behavior.**
- Updated module comments to mirror the same boundary.
- Kept finalized-state mutations confined to finalization/sync actions.
- Retained explicit invariants showing tentative-layer actions do not directly mutate finalized history.

**Why this closes the issue.**
- The protocol boundary is now crisp: the event DAG and finalized commit DAG are shared semantics; the tentative commit zone is local implementation state derived from them.

---

### 3. ~~Local emits need an authoritative projection basis so event parents match the content root they claim~~ FIXED

**Refs:** protocol state model, `LOCAL_WRITE`, `RECONCILE`, conflict-resolution flows; spec `computeTentativeProjection`, local emit helpers, projection invariants.

- Protocol: `doltswarm-protocol.md` lines 244, 304-319, 354-367, 378, 387, 776, 798, 832-835.
- Spec: `specs/doltswarm_verify.qnt` lines 105, 394-447, 667-743, 814-838, 1065-1102, 2235-2237.

**Status.** Fixed by adding `projection_basis` as authoritative tentative parent basis and making all local emits use it.

**Implemented protocol behavior.**
- Added `projection_basis` to peer state.
- `RECONCILE` now returns `(tentative_prolly_root_hash, parked_conflicts, projection_basis)`.
- `LOCAL_WRITE` parents from `projection_basis`.
- `RESOLVE_CONFLICT` parents from `projection_basis ∪ {event_id}`.
- `RESOLVE_FINALIZED_CONFLICT` parents from `projection_basis`.

**Implemented spec behavior.**
- Added `projection_basis` to `NodeState`.
- Replaced the old merge-root helper contract with `computeTentativeProjection`, which returns:
  - tentative root
  - parked set
  - projection basis
- Updated all local emit paths to use the computed basis.
- Added fixed-point invariant `inv_projection_basis_is_compute_projection`.

**Why this closes the issue.**
- An emitted event's causal claim and content claim now line up.
- Closure lag no longer lets a peer announce parents that were not actually embodied in the root it wrote against.

---

### 4. ~~Stability advancement must run after any transition that enlarges the finalizable set~~ FIXED

**Refs:** protocol `LOCAL_WRITE`, `RECEIVE_EVENT`, heartbeat/finalization trigger text, `ADVANCE_STABILITY`; spec `do_finalize`, frontier refreshes, closure/chunk readiness actions.

- Protocol: `doltswarm-protocol.md` lines 318-319, 346, 415-427, 456, 462-468, 524.
- Spec: `specs/doltswarm_verify.qnt` lines 689, 725, 765, 1164-1178, 1233-1262, 1354-1403.

**Status.** Fixed by making candidate-enlarging triggers explicit in the protocol and treating standalone `do_finalize` in the spec as the scheduling abstraction for those triggers.

**Implemented protocol behavior.**
- The protocol now normatively says `ADVANCE_STABILITY` must run after any change that can enlarge the candidate set.
- This explicitly includes changes to:
  - local frontier / local clock
  - `peer_seen_frontier`
  - `active_peers`
  - `chunks_ready`
  - parked-state changes
  - finalized commit/content re-anchors
- Local clock mutations now refresh `peer_seen_frontier[self]` as required post-state.

**Implemented spec behavior.**
- The spec keeps `do_finalize` decoupled from the action that caused enablement.
- The enablement conditions now depend on the same readiness, witness, parked, and finalized-anchor inputs as the protocol.
- Local write, receive, chunk-ready, sync, and GC paths all recompute the tentative projection/frontier state needed for later finalization.

**Why this closes the issue.**
- Finalization progress is no longer accidentally gated on only a subset of candidate-enabling transitions.
- The prose and model now say the same thing, even though the model factors triggering into a standalone action.

---

### 5. ~~Finalized-conflict resolution must be monotone at the finalized-history layer~~ FIXED

**Refs:** protocol finalized-conflict state, `RESOLVE_FINALIZED_CONFLICT`, `ADVANCE_STABILITY`, `SYNC_FINALIZED`; spec `resolveFinalizedConflictResult`, `finalizeResult`, `syncFinalizedCore`, finalized-conflict invariants.

- Protocol: `doltswarm-protocol.md` lines 217-219, 252-253, 381-390, 437-443, 479-519, 886.
- Spec: `specs/doltswarm_verify.qnt` lines 95-97, 114, 667-691, 768-799, 900-989, 1888-1905, 2141-2177.

**Status.** Fixed by separating tentative resolution claims from durable finalized resolution state.

**Implemented protocol behavior.**
- Added `resolved_finalized_conflict_ids` as durable monotone finalized state.
- Reframed `resolved_finalized_conflict_id` on events as a claim, not immediate conflict deletion.
- `RESOLVE_FINALIZED_CONFLICT` no longer directly clears finalized conflicts.
- `ADVANCE_STABILITY` now hardens claim IDs carried by newly finalized events into `resolved_finalized_conflict_ids`.
- `SYNC_FINALIZED` now merges open conflicts as:
  - `(our_conflicts ∪ their_conflicts) \ (our_resolved ∪ their_resolved)`

**Implemented spec behavior.**
- Added `resolved_finalized_conflict_ids` to `NodeState`.
- Changed finalized-conflict IDs to be finalized-commit-based triples.
- `resolveFinalizedConflictResult` now emits a claim-carrying event only.
- `do_receive` no longer clears finalized conflicts on mere event observation.
- `finalizeResult` hardens newly finalized claims into durable resolved IDs and removes only those open conflicts.
- `syncFinalizedCore` merges open conflicts against the unioned durable resolved set.

**Why this closes the issue.**
- A history-layer conflict fact can no longer disappear while the supposed resolution is still only tentative.
- Anti-resurrection no longer depends on retaining specific event bodies forever.

---

### 6. ~~Stable-frontier finalization conflicts need one explicit, layer-consistent rule~~ FIXED

**Refs:** protocol `ADVANCE_STABILITY` anchored frontier fold and conflict note; spec `stableFrontierConflicts`, `finalizeEnabled`, `finalizeBatch`.

- Protocol: `doltswarm-protocol.md` lines 431-443, 808-814.
- Spec: `specs/doltswarm_verify.qnt` lines 423-447, 765-799.

**Status.** Fixed by choosing the stronger rule: stable non-parked frontier heads must be finalization-safe by construction.

**Implemented protocol behavior.**
- `ADVANCE_STABILITY` now treats stable-frontier merge conflicts as protocol violations, not as a third recoverable conflict mode.
- The prose now states that tentative parking and finalized-layer conflict tracking already cover the intended conflict surfaces.

**Implemented spec behavior.**
- Added `stableFrontierConflicts(st)`.
- `finalizeEnabled(st)` now requires `stableFrontierConflicts(st).empty()`.
- `finalizeBatch` no longer models "skip conflicting stable frontier heads"; such a state is disabled instead.

**Why this closes the issue.**
- This removes the ambiguous "hardened for bookkeeping but not applied to finalized state" path.
- The finalized boundary now has only two conflict modes:
  - tentative parking below the boundary
  - explicit finalized-layer conflict tracking during finalized-sync divergence merge

---

### 7. ~~Anchor-root readiness must be uniform across all event materialization paths~~ FIXED

**Refs:** protocol event lifecycle, receive path, re-anchor text, join snapshot contract; spec `chunks_ready`, `do_chunks_ready`, reconcile/finalization gates.

- Protocol: `doltswarm-protocol.md` lines 242, 280, 325-345, 431, 524, 536-547, 650.
- Spec: `specs/doltswarm_verify.qnt` lines 103, 302, 419, 481, 1024, 1164-1178, 2177-2179.

**Status.** Fixed by making chunk-readiness uniformly mean readiness of both the event root and its recorded anchor root.

**Implemented protocol behavior.**
- `CHUNKS_READY` now explicitly means both `prolly_root_hash` and `anchor_prolly_root_hash` are available locally.
- Receive-side chunk pull requests both roots as needed.
- Re-anchor and heartbeat closure text now preserve the same requirement.
- Join/bootstrap text now carries finalized commit/content state plus retained clock state consistent with later anchored replay.

**Implemented spec behavior.**
- `chunks_ready` remains an explicit state component.
- Reconciliation and finalization both require parent closure and chunk readiness.
- Local emit paths immediately mark the local event ready; remote receive requires later `do_chunks_ready`.
- Added/retained readiness invariant `inv_chunks_ready_known`.

**Why this closes the issue.**
- Anchored replay is only valid if both the branch root and the recorded base root are present.
- The readiness rule is now uniform instead of being implied in some paths and omitted in others.

---

### 8. ~~Finalized sync should be finalized-commit-lineage-centric, with finalized events as secondary bookkeeping~~ FIXED

**Refs:** protocol `SYNC_FINALIZED`, finalized metadata exchange, finalized-layer conflict description; spec `syncMode`, `syncEnabled`, `syncFinalizedCore`.

- Protocol: `doltswarm-protocol.md` lines 470-526, 770, 886-890.
- Spec: `specs/doltswarm_verify.qnt` lines 853-989, 1633-1952.

**Status.** Fixed by making finalized-commit lineage the semantic center of finalized sync and demoting finalized-event catalogs to supporting bookkeeping/bootstrap state.

**Implemented protocol behavior.**
- `SYNC_FINALIZED` now case-splits by finalized-commit ancestry, not by finalized-event subset relations.
- Lineage-LCA common ancestor lookup is now authoritative.
- Finalized conflicts are keyed by `(ours_finalized_commit_id, theirs_finalized_commit_id, common_finalized_commit_id)`.
- `finalized_events` is retained for bookkeeping, repair, bootstrap, and deterministic finalized-commit-id derivation, but it no longer defines finalized lineage.

**Implemented spec behavior.**
- `syncMode`, `syncAdoptionReady`, and `syncEnabled` now operate on finalized-commit lineage.
- `syncFinalizedCore` merges finalized-commit metadata, durable resolved IDs, and finalized-event catalogs, but lineage is authoritative.
- Deterministic ours/theirs assignment is now based on `finalizedCommitLess(...)`.
- Merge results add lineage edges from the resulting finalized commit back to the two input finalized commits.

**Why this closes the issue.**
- Finalized sync semantics now scale around actual history identity.
- Nested partition recovery no longer depends on replaying unioned finalized-event sets as if they were one linear history.

---

### 9. ~~The normative protocol should separate core semantics from deployment/implementation detail~~ FIXED

**Refs:** protocol validation and implementation notes, explicit implementation-doc references; spec modeling notes.

- Protocol: `doltswarm-protocol.md` lines 298, 850, 874, 895.
- Spec: `specs/doltswarm_verify.qnt` lines 20, 1233-1238.

**Status.** Fixed by tightening the protocol around state-machine semantics and pushing concrete implementation mechanics into the implementation document.

**Implemented protocol behavior.**
- `ValidEvent(e)` is defined semantically, without tying the protocol to one auth envelope.
- Finalized-commit representation is described semantically first; using a deterministic Dolt commit hash is framed as one realization.
- Implementation-specific package/API structure is now explicitly moved to `doltswarm-implementation.md`.

**Implemented spec behavior.**
- The model continues to use abstract admission (`validEvent(e)`) and direct-state reads for heartbeat/sync abstraction.
- Commentary now calls out those choices as modeling abstractions rather than protocol rules.

**Why this closes the issue.**
- The protocol document is easier to audit as a protocol.
- Portability improves because deployment choices no longer masquerade as semantic requirements.

---

### 10. ~~The naming scheme should become layer-explicit~~ FIXED

**Refs:** protocol finalized-state field names and conflict IDs; spec type aliases and state names.

- Protocol: `doltswarm-protocol.md` lines 196, 207-219, 244, 247-253, 470-526.
- Spec: `specs/doltswarm_verify.qnt` lines 74-80, 95-97, 105, 108-114, 142-157.

**Status.** Fixed by applying the layer-explicit naming scheme across the protocol and Quint spec, including the tentative-vs-finalized commit split.

**Implemented protocol behavior.**
- Introduced layer-explicit finalized names:
  - `FinalizedCommitID`
  - `finalized_commit_id`
  - `finalized_prolly_root_hash`
  - `finalized_commit_parents`
  - `ours_finalized_commit_id` / `theirs_finalized_commit_id` / `common_finalized_commit_id`
- Introduced layer-explicit tentative/local naming:
  - `TentativeCommitID`
  - `tentative_commit_id`
- Renamed event/content fields and related helpers to stay layer-explicit:
  - `event_id`
  - `parent_event_ids`
  - `head_event_ids`
  - `prolly_root_hash`
  - `anchor_prolly_root_hash`
  - `head_event_ids_digest`
- Added `projection_basis` as an explicit tentative-layer concept.

**Implemented spec behavior.**
- Added `ProllyRootHash` and `FinalizedCommitID` type aliases.
- Mirrored the same layer-explicit naming split in `NodeState`, lineage helpers, event identifiers, and finalized-conflict helpers.

**Why this was chosen.**
- The highest-value rule is now explicit everywhere: `tentative_commit_id` is local and rewritable, while `finalized_commit_id` is shared stable history.
- Matching event/content names then make each layer legible without overloading one word like `commit`, `root`, or `id`.

---

## Explicit Semantic Choices (Documented, Not Additional Mechanisms)

### 11. ~~Timeout-based witness eviction should be treated as a protocol-semantic choice, not just an operational note~~ CLARIFIED

**Refs:** protocol `active_peers`, timeout handling, partition semantics; spec timeout/heartbeat membership commentary.

- Protocol: `doltswarm-protocol.md` lines 254, 462-468, 754-776.
- Spec: `specs/doltswarm_verify.qnt` lines 1354-1403, 2340-2366.

**Status.** Clarified by making the semantic tradeoff explicit: false suspicion is treated the same as temporary partition.

**Implemented protocol behavior.**
- The timeout rule now says independent eviction is allowed and changes the current witness set.
- The prose explicitly states that timeout-based false suspicion is semantically equivalent to temporary partition.
- Partition recovery is defined through the same finalized-commit-lineage `SYNC_FINALIZED` path as real disconnection.

**Implemented spec behavior.**
- No stronger failure detector was added.
- The model continues to use connection/time-out style membership changes, which matches the chosen semantics.

**Why this was chosen.**
- It preserves the protocol's non-consensus design.
- Introducing agreement over witness membership would add a new coordination problem that the protocol is explicitly trying to avoid.

---

### 12. ~~Crash recovery and tentative local-write durability need an explicit event-layer boundary~~ CLARIFIED

**Refs:** protocol Layer 3 state description and crash notes.

- Protocol: `doltswarm-protocol.md` lines 236, 536-547, 856.

**Status.** Clarified by choosing the simple boundary: unreplicated tentative local writes are crash-volatile.

**Implemented protocol behavior.**
- The protocol now states that Layer 3 is in-memory.
- Crash recovery is defined via `PEER_JOIN` from a live peer, not by replaying a local durable emit journal.
- A local write that has not yet reached another live peer is explicitly outside the protocol's durability guarantee.

**Implemented spec behavior.**
- No extra crash-journal state was added to the model.
- The existing state machine remains consistent with the documented boundary.

**Why this was chosen.**
- It keeps the core protocol focused on shared-state correctness.
- A durable local emit journal is a plausible future product feature, but it is an implementation extension, not required to define protocol convergence.

---

### 13. ~~HLC tie-breaking fairness bias should be either fixed or explicitly accepted~~ DECIDED

**Refs:** protocol HLC ordering note.

- Protocol: `doltswarm-protocol.md` lines 170-173.

**Status.** Decided as an explicit simplicity tradeoff, with the bias now documented.

**Implemented protocol behavior.**
- The protocol now notes that `PeerID` tie-breaking creates a static priority bias.
- It also records the obvious alternative: hash-based tie-breaking if fairness is later prioritized.

**Implemented spec behavior.**
- No semantic change was made to the ordering model in this round.

**Why this was chosen.**
- The existing rule is deterministic, simple, and not a safety issue.
- The fairness concern is real but lower priority than the finalized-lineage and projection-basis correctness fixes completed in this round.

---

## Round 6 Remaining Open Items

None for protocol correctness.

No further naming cleanup is required for protocol semantics.
