# DoltSwarm Protocol & Spec Improvements (Round 5, Consolidated)

Consolidated findings after moving finalization from `stable_hlc` to an active-peer, causality-based seen frontier (`peer_seen_frontier`).

Decisions applied in this round:
- The finalization witness is now causal: events harden only when covered by every peer in the current `active_peers` witness set.
- HLC remains only a deterministic ordering/tie-break mechanism.
- This round reassesses the original five top-level review findings against that new stability model.

Items below are ordered by remaining protocol-correctness importance. Resolved or materially-improved items are retained here so the review history stays closed over the original five findings.

---

## Resolved in this round

### 1. ~~Event replayability after finalized-root changes is still under-specified~~ FIXED

**Refs:** protocol data types `CIDPayload` / `Event`, local write payload construction, receive lifecycle, anchored replay/finalization, and `SYNC_FINALIZED` re-anchor contract; spec `Event`, `anchoredFold`, `computeMergedRoot`, `finalizeBatch`, and local event constructors.

- Protocol: `doltswarm-protocol.md` lines 147-174, 272-274, 289-291, 305-308, 317, 389-390, 473.
- Spec: `specs/doltswarm_verify.qnt` lines 72-80, 297-307, 323-330, 354-363, 748-755, 1109-1113, 1498-1504.

**Status.** Fixed by choosing **Option A** from this round: add `anchor_root_hash` to every event and replay/finalize events against their recorded anchors.

**Implemented protocol behavior.**
- `CIDPayload`, `SignedPayload`, and `Event` now carry `anchor_root_hash` in addition to `root_hash`.
- `LOCAL_WRITE` stamps `anchor_root_hash = finalized_root` when the event is created.
- `CHUNKS_READY` now means both `root_hash` and `anchor_root_hash` are locally available for that event, and receive-side chunk fetch requests both when needed.
- `RECONCILE` now uses a shared `AnchoredFold(heads, start_root)` contract:
  - sort heads by HLC,
  - start from the current accumulator,
  - fold `MergeRoots(acc, head.root_hash, head.anchor_root_hash)`.
- `ADVANCE_STABILITY` uses the same anchored fold for stable-frontier finalization.
- The singleton-head receive path was tightened to use the same anchored replay semantics instead of blindly fast-forwarding to `head.root_hash`.
- `SYNC_FINALIZED` re-anchor text now explicitly says tentative events replay over the new accumulator using each event's recorded `anchor_root_hash`.

**Implemented spec behavior.**
- `Event` now has an `anchor_root_hash` field.
- `fixedAnchorFold` was replaced with `anchoredFold`, which merges each event against its own recorded anchor.
- `computeMergedRoot` and `finalizeBatch` now use the anchored fold, so the Quint replay/finalization semantics match the protocol text.
- All local event constructors (`do_write`, `do_resolve_conflict`, `resolveFinalizedConflictResult`) stamp `anchor_root_hash = st.finalized_root`.
- The finalized-conflict resolution contract now explicitly checks that emitted resolution events carry the creator's current finalized anchor.

**Why this closes the issue.**
- Events are no longer ambiguous snapshots; they are anchored tentative branch states.
- After `finalized_root` changes, the protocol has enough information to replay retained tentative events coherently.
- The protocol/spec pair now define the same replay rule, including the single-head path and the chunk-readiness requirements for anchor roots.

### 2. ~~`SYNC_FINALIZED` still excludes the `EMPTY_ROOT` recovery path~~ FIXED

**Refs:** protocol heartbeat-triggered sync gate, `SYNC_FINALIZED` precondition, and sync-design notes; spec `syncEnabled` and empty-root ADOPT regression.

- Protocol: `doltswarm-protocol.md` lines 377, 426, 580, 586, 710.
- Spec: `specs/doltswarm_verify.qnt` lines 609-615, 1362-1373.

**Status.** Fixed by choosing **Option A** from this round: allow `SYNC_FINALIZED` whenever finalized roots differ, with `EMPTY_ROOT` treated as the root of finalized lineage.

**Implemented protocol behavior.**
- HEARTBEAT now triggers `SYNC_FINALIZED` whenever `received.finalized_root ≠ finalized_root`; the special non-empty guard was removed.
- `SYNC_FINALIZED` no longer requires both sides to have non-empty finalized roots.
- The protocol now states explicitly that `EMPTY_ROOT` participates in normal lineage classification, so:
  - `EMPTY_ROOT -> non-empty` is an **ADOPT** case,
  - `non-empty -> EMPTY_ROOT` is a **NOOP** case,
  - only incomparable non-empty roots take the **MERGE** branch.
- Modeling/design notes now describe empty-peer recovery as part of the same finalized-sync machinery, not as a special excluded case.

**Implemented spec behavior.**
- Removed the `st_local.finalized_root != EMPTY_ROOT` and `st_remote.finalized_root != EMPTY_ROOT` guards from `syncEnabled`.
- Added `inv_sync_empty_root_adopts`, which checks that a connected peer at `EMPTY_ROOT` takes the normal `SYNC_ADOPT` branch and adopts the remote non-empty finalized root/events.

**Why this closes the issue.**
- `EMPTY_ROOT` is now a normal lineage root instead of a protocol exception.
- Recovery of empty peers no longer depends on retaining finalized event bodies in `clock` or on always using `PEER_JOIN`.
- The protocol/spec pair now define one uniform finalized-recovery story for bootstrap, crash recovery, and reconnect after compaction.

---

### 3. ~~Seen-frontier finalization fixes the witness semantics, but heartbeat anti-entropy still needs one more tightening~~ FIXED

**Refs:** protocol heartbeat payload/action and modeling notes; spec heartbeat note/commentary and pull-target helper.

- Protocol: `doltswarm-protocol.md` lines 242, 363-378, 578-580, 779, 800-802.
- Spec: `specs/doltswarm_verify.qnt` lines 14-17, 257-287, 905-917.

**Status.** Fixed by choosing **Option B** from this round: send the full `seen_frontier` on every heartbeat, while retaining only `heads_digest` as a compact change detector.

**Implemented protocol behavior.**
- `Heartbeat` now carries `{ peer, heads_digest, seen_frontier, finalized_root }`.
- On send, the sender:
  - computes `heads_digest = hash(sort(heads))`,
  - includes the full `peer_seen_frontier[self]` in the heartbeat.
- On receive, the receiver:
  - still compares `heads_digest` and requests the sender's full head set only on mismatch,
  - refreshes `peer_seen_frontier[q]` directly from `received.seen_frontier`,
  - always uses `remote_heads ∪ received.seen_frontier` as the ancestor-pull target.
- The design notes now say explicitly that the frontier itself is carried in full every heartbeat and that only the head set remains digest-gated.

**Implemented spec behavior.**
- No core state-machine change was required: `do_heartbeat` already refreshed `peer_seen_frontier[q]` directly from the sender's state and pulled closure for `remote.heads ∪ remote.peer_seen_frontier[q]`.
- The module notes, pull-target helper comment, and `do_heartbeat` commentary were updated so the spec now matches the protocol exactly on the seen-frontier exchange semantics.
- The only remaining abstraction gap is intentional and narrow: the spec still skips the message-level `heads_digest` compare and reads the sender's head set directly as a modeling simplification.

**Why this closes the issue.**
- The protocol no longer relies on a frontier-digest cache to decide whether witness metadata should be refreshed.
- Anti-entropy and finalization now use the same explicit causal frontier object.
- The spec is no longer stronger than the prose on frontier exchange; both now say the same thing about how witness frontiers propagate.

---

## Resolved in this round (continued)

### 4. ~~Event admission/authentication semantics were deployment-dependent~~ FIXED

**Refs:** protocol event-admission contract, receive-path admission rule, spec modeling note, `validEvent`, `do_receive`, and admission invariants.

- Protocol: `doltswarm-protocol.md` lines 250-269, 305-306, 575, 583, 642, 774.
- Spec: `specs/doltswarm_verify.qnt` lines 19-21, 157-170, 862-875, 1694-1704.

**Status.** Fixed by choosing **Option B** from this round: define a swarm-wide abstract `ValidEvent(e)` predicate and require every peer in one swarm to apply the same predicate.

**Implemented protocol behavior.**
- The event-signing section now defines admission normatively in terms of `ValidEvent(e)`, not in terms of optional per-peer `IdentityResolver` checks.
- The protocol now requires:
  - every peer in one swarm to apply the same `ValidEvent(e)` predicate,
  - `ValidEvent(e)` to be deterministic from event payload plus shared swarm configuration,
  - `ValidEvent(e)` to reject malformed payloads, CID mismatches, and failures of the swarm's chosen authenticity/integrity checks.
- The receive path now uses one explicit admission step: if `ValidEvent(e) = false`, discard; otherwise, if the event is new, it MUST be accepted into `clock`.
- The document now treats concrete auth modes as non-normative deployment profiles above the protocol core (for example `signed` and `trusted`), rather than as peer-local discretion inside the protocol.
- The architecture/boundary and spec-modeling notes were updated so they refer to `ValidEvent(e)` and `validEvent(e)` consistently.

**Implemented spec behavior.**
- Added an explicit abstract predicate `validEvent(e)` to the Quint model.
- `do_receive` now processes only inbox events satisfying `validEvent(e)`, making the admission assumption visible in the state machine instead of implicit in commentary.
- Added explicit invariants:
  - `inv_clock_events_valid`,
  - `inv_inbox_events_valid`,
  so the model checks that retained and in-flight events stay within the abstract admission contract.

**Why this closes the issue.**
- Event admission is now swarm-uniform by definition, not deployment-dependent.
- The protocol keeps auth/transport pluggable without letting different peers in the same swarm diverge on what enters the Merkle clock.
- The protocol prose and the Quint model now say the same thing about admission semantics: the core correctness model assumes one shared event-validity predicate.

---

## Resolved Items

### 5. ~~`clock` retention and GC semantics were inconsistent~~ FIXED

**Refs:** protocol `clock` state definition, invariants, `GC_CLOCK`, and retained-finalized sync rule; spec `retainedFinalizedCIDs`, `gcPruneable`, retained-subgraph invariant, and `do_gc`.

- Protocol: `doltswarm-protocol.md` lines 205-218, 420, 481, 530-534.
- Spec: `specs/doltswarm_verify.qnt` lines 397-405, 1804-1816.

**Status.** Fixed by choosing **Option A** from this round: keep a single mixed `clock` map and make retained-subgraph closure explicit.

**Implemented protocol behavior.**
- The protocol still uses one mixed `clock` map:
  - all non-finalized events,
  - plus retained finalized event bodies not yet compacted.
- The `clock` state definition now says explicitly that the retained finalized subset `clock.keys() ∩ finalized_events.keys()` MUST stay parent-closed.
- The state discussion now calls out retained finalized event bodies as a special cached subset inside the mixed `clock` map and states that `GC_CLOCK` and `SYNC_FINALIZED` preserve that closure rule.
- The invariants section now includes an explicit retained-subgraph invariant (`INV1c`) instead of leaving this property implicit in the `GC_CLOCK` action text alone.

**Implemented spec behavior.**
- Added a first-class helper `retainedFinalizedCIDs(st)` so the retained finalized subset is explicit in the model.
- `gcPruneable` now refers to that helper directly.
- Added an explicit named invariant `inv_retained_finalized_subgraph_parent_closed`.
- Kept the stronger existing readiness invariant (`inv_finalized_parent_closed`) alongside it, so the model now exposes retained-subgraph closure directly instead of only as an implementation consequence of compaction rules.

**Why this closes the issue.**
- The protocol now states the retained-subgraph rule where readers expect it: in state/invariants, not only inside `GC_CLOCK`.
- The spec now has a named retained-subgraph closure property instead of relying on readers to infer it from `gcPruneable`.
- The single-map design stays intact, so the fix improves clarity without increasing protocol state surface.
