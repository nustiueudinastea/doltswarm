# DoltSwarm Protocol & Spec — Improvement Findings

Two independent reviews of `doltswarm-protocol.md` and `specs/doltswarm_verify.qnt`. Findings are ordered by severity. Each entry describes the issue, the proposed change, and its implications for the protocol and spec.

---

## 1. ~~HIGH — Remove the stale-vote/rejection mechanism from the core protocol~~ SOLVED

**The problem.** The staleness vote system (`StaleVote` broadcast, 30% threshold rejection, rollback-after-late-rejection, lineage exemption) is the most coordination-heavy part of the protocol. It reintroduces consensus-like behaviour that directly contradicts the motivating design principles of §0 ("no consensus protocol", "minimal inter-peer communication"). Specific concerns:

- It is a **distributed voting protocol**: peers broadcast `StaleVote` messages, collect them, and make a threshold decision (`≥ ceil(0.3 × |active_peers|)`). This is a quorum check — a form of consensus.
- It requires a **third broadcast message type** (`StaleVote`) that exists solely for this mechanism.
- It requires **rollback logic** (§6.7): if an event was already merged by some peers before rejection, those peers must replay all non-rejected events from `finalized_root`. This is the only place the protocol requires undoing work.
- The **lineage exemption** (§6.1) exists solely to patch the staleness mechanism's interaction with partition recovery. It requires walking DAG ancestry per-peer (`e.cid ∈ ancestors(e')`) on every stale-candidate event — expensive and complex.
- Rejecting an old event that a user genuinely wrote is **silent data loss** — the thing INV5 is supposed to prevent. The protocol accepts the event into the clock, merges it, and then retroactively rips it out. From the originating peer's perspective, their committed write vanished.
- The mechanism can **conflict with accept-the-fork partition recovery**: events rejected in one partition propagate rejection across partition boundaries (§6.8 "Cross-partition rejections"), but the other side may have built causal chains on top of that event. Rejection then leaves dangling parent references.

Refs: `doltswarm-protocol.md:176,209,224,502-506,554`, `doltswarm_verify.qnt:391-401,440-517`.

**Proposed change.** Remove `StaleVote`, `stale_votes`, `rejected_events`, `RECEIVE_STALE_VOTE`, lineage exemption logic, and rollback-after-late-rejection from the protocol entirely. If a safety valve against unbounded old events is desired, replace with a **local admission policy**: each peer independently discards events with `|now() - e.hlc.wall| > ADMISSION_WINDOW` (configurable, e.g. 60s). No broadcast, no votes, no rollback. The peer simply does not add the event to its clock. This is a unilateral decision — no coordination.

**Implications.**
- The protocol drops from three broadcast message types to two (Event, Heartbeat).
- INV7 (Rejection Consistency) and INV9 (Finalization/Rejection Exclusivity) are removed — they have no subject matter.
- `rejected_events` state variable is removed. Every event in the clock either gets finalized or stays parked until resolved. No event disappears.
- Lineage exemption logic is removed. Partition recovery simplifies: a reconnecting peer's event chain is just accepted.
- §6.7 (Rollback After Late Rejection) is removed entirely.
- The spec loses `do_stale_vote`, `rejected_events`, `stale_votes`, the staleness check in `do_receive`, the rejection branch in `do_stale_vote`, vote pruning in `do_finalize`, and `inv_rejection_threshold`. Significant complexity reduction.
- A malicious peer flooding garbage events should be handled at the identity/transport layer (revoke peer, deny connections), not at the protocol layer.

---

## 2. ~~HIGH — Finalization of formerly-parked events is unhandled (protocol bug)~~ SOLVED

**The problem.** There is an unhandled interaction between `RESOLVE_CONFLICT` and `ADVANCE_STABILITY` that can produce a conflict during finalization replay — a path that has no error handling.

Scenario:
1. Event `e_parked` conflicts during `RECONCILE` and is parked.
2. User resolves via `RESOLVE_CONFLICT` → creates `e_resolution` with `parents = ALL heads` (including `e_parked`).
3. `e_parked` is removed from `parked_conflicts` (step 2). After recomputation (RECONCILE step 4), `e_parked` is no longer a head (it's a parent of `e_resolution`), so the recomputed parked set doesn't include it.
4. Now `e_parked` is: in the clock, not finalized, not rejected, not parked. It is eligible for finalization.
5. When `stable_hlc` advances past `e_parked.hlc`, `ADVANCE_STABILITY` step 1 includes it in `newly_stable`.
6. The replay does `mergeRoots(acc, e_parked.root_hash, finalized_root)`. Since `e_parked.root_hash` was a branch that diverged from the main lineage (that's why it conflicted), this produces the **same conflict** that caused parking originally.
7. `ADVANCE_STABILITY` has no conflict-handling path. The protocol is stuck.

The `computeFinalizedRoot` fold assumes each event's `root_hash` incorporates all previous state (a linear causal chain). A formerly-parked event's root branches off from an earlier state — it doesn't incorporate the main lineage.

Refs: `doltswarm-protocol.md:250-257,275-284`, `doltswarm_verify.qnt:623-660`.

**Proposed change.** Introduce a `subsumed_events: Set[EventCID]` state variable. When `RESOLVE_CONFLICT` resolves a parked event `e_parked`:
- Add `e_parked` to `subsumed_events`.
- In `ADVANCE_STABILITY` step 1, filter: `newly_stable = { ... ∧ e ∉ subsumed_events }`.
- When the resolution event is finalized, add `e_parked` to `finalized_events` for bookkeeping (it did happen, it is accounted for) but do NOT replay it in the fold.
- The spec should add `subsumed_events: Set[EventCID]` to `NodeState` and update `do_resolve_conflict` and `do_finalize` accordingly.

**Implications.**
- One new small state variable. No new message types, no new network traffic.
- Fixes a latent protocol bug that would manifest whenever a conflict is resolved and the parked event subsequently crosses the stability boundary.
- INV5 (No Silent Data Loss) is preserved: subsumed events are tracked, not dropped.
- `computeFinalizedRoot` continues to work as a simple fold over non-subsumed events.

---

## 3. ~~HIGH — Finalization blocks indefinitely on partition conflicts~~ SOLVED

**The problem.** When `MERGE_FINALIZED` returns a conflict, `ADVANCE_STABILITY` is blocked entirely (§2.4 step 0: "If `partition_conflict` is set: block"). This means no new events can be finalized until a human resolves the partition conflict. In a system where the motivating goal is "no peer is blocked", this is a protocol-layer stall:

- If both sides of a partition modified the same rows and then reconnect, finalization halts globally until a human provides resolution SQL.
- Meanwhile, tentative events accumulate without being finalized. The gap between `stable_hlc` and the finalization frontier grows indefinitely.
- There is no timeout, no fallback, no automatic resolution. The protocol simply waits.

This creates a fundamentally different (worse) conflict surface than tentative conflicts: tentative conflicts park the later event but finalization continues for all other events. Partition conflicts halt finalization for everything.

Refs: `doltswarm-protocol.md:279,317-321,550`, `doltswarm_verify.qnt:627`.

**Proposed change.** Unify partition conflicts with the existing parked-conflict mechanism. Instead of blocking `ADVANCE_STABILITY`:

- When `MERGE_FINALIZED` returns a conflict, record a **conflict-marker finalized commit** (a deterministic merge commit whose root is a sentinel or the "ours" side's root — the earlier by HLC, matching the tentative parking rule). Both sides' finalized events are unioned into `finalized_events`. `finalized_root` advances to the conflict-marker commit's root.
- The conflict is surfaced to the user via the same mechanism as tentative conflicts (Dolt conflict tables, etc.).
- `ADVANCE_STABILITY` is **not blocked**. It continues finalizing new events on top of the conflict-marker root.
- Resolution is still via user SQL, but it flows through the normal `RESOLVE_CONFLICT` / `LOCAL_WRITE` path rather than a special `RESOLVE_PARTITION_CONFLICT` action.

This eliminates: `PartitionConflict` as a separate type, `partition_conflict` as a separate state variable, `RESOLVE_PARTITION_CONFLICT` as a separate action, the "finalization blocked" check in `ADVANCE_STABILITY`, and INV11.

**Implications.**
- Finalization never stalls. The protocol's non-blocking property is fully preserved.
- The conflict-marker approach mirrors what Dolt itself does for unresolved merge conflicts (conflict tables). The user sees the conflict in SQL and resolves it at their pace.
- One less protocol action, one less state variable, one less invariant.
- Trade-off: the "ours" side wins by default (the earlier-by-HLC finalized root becomes the working state), so the "theirs" side's finalized changes are temporarily invisible until resolution. This matches tentative conflict semantics (earlier wins, later is parked).
- The spec's `do_merge_finalized`, `do_resolve_partition_conflict`, and `hasPartitionConflict` guards simplify substantially.

---

## 4. ~~HIGH — `computeFinalizedRoot` is unsound after partition merges~~ SOLVED

**The problem.** `computeFinalizedRoot` sorts all finalized events by HLC and folds `mergeRoots(acc, x.root_hash, acc)`. Since `mergeRoots(acc, x, acc)` is always `x` (fast-forward when ours == ancestor), the fold returns the HLC-max event's `root_hash`. The protocol document itself identifies this (§7.10 "Why a single three-way merge?"):

> Interleaving them in HLC order and fast-forward folding would silently lose one side's changes.

Yet after `MERGE_FINALIZED` sets `finalized_root` to the merged result and `finalized_events` to the union of both sides, any subsequent call to `computeFinalizedRoot` (e.g., `do_finalize` in the spec at line 639, or state reconstruction during `PEER_JOIN`) recomputes from scratch and **overwrites** the carefully-computed merged root with the HLC-max event's `root_hash`. This silently discards one partition's changes.

In the spec, `do_finalize` calls `computeFinalizedRoot(newFinalizedEvents, st.clock)` every time it finalizes an event. After a partition merge, this immediately corrupts `finalized_root`.

Refs: `doltswarm-protocol.md:307,323`, `doltswarm_verify.qnt:215-228,639`.

**Proposed change.** Stop recomputing `finalized_root` from scratch. Instead, advance it incrementally:

- `ADVANCE_STABILITY` replays only `newly_stable` events by folding: `mergeRoots(finalized_root, e.root_hash, finalized_root)` for each newly-stable event in total order.
- `MERGE_FINALIZED` sets `finalized_root` to its merged result. Subsequent finalization steps fold on top of it.
- Remove `computeFinalizedRoot` as a from-scratch recomputation. It is only used as a helper during `MERGE_FINALIZED` to compute the common ancestor (where it is correct, because common events DO form a linear chain).
- In the spec: `do_finalize` should compute `newFinRoot = mergeRoots(st.finalized_root, st.clock.get(cid).root_hash, st.finalized_root)` instead of calling `computeFinalizedRoot(newFinalizedEvents, st.clock)`.

**Implications.**
- Fixes a correctness bug: `finalized_root` is no longer silently corrupted after partition merges.
- `computeFinalizedRoot` remains as a pure function for computing the common ancestor during `MERGE_FINALIZED` (where the input is the pre-split linear chain of events). It is NOT used as the general finalized root computation.
- INV3 (Finalized History Agreement) now holds correctly after partition recovery.
- `PEER_JOIN` must transfer `finalized_root` as an opaque value, not reconstruct it from events. This is already what the protocol implies ("Request from any active peer: `finalized_root` + ...").
- The spec becomes simpler: `do_finalize` does a single `mergeRoots` call instead of sorting and folding the entire finalized set.

---

## 5. ~~MEDIUM-HIGH — Ancestor policy is internally inconsistent~~ SOLVED

**The problem.** The protocol document says two different things about the merge ancestor in `RECONCILE`:

- §2.4 RECONCILE step 3a: "Ideally, find the **LCA** in the Merkle clock DAG... If LCA is not found, fall back to `finalized_root`."
- §7.3: "Each event triggers its own merge operation with the ancestor discovered via LCA in the Merkle clock."
- §2.4 RECONCILE step 3a (continued): "**Current simplification:** both the Quint spec and Go implementation use `finalized_root` as the ancestor for all merges."

The protocol simultaneously specifies LCA as the ideal, documents it as the current approach in some places, and acknowledges it's not implemented in others. The `find_lca` function appears in the boundary diagram (§4.3) as a real component. §6.3 discusses "No Common Ancestor" as an edge case for `find_lca`. The `MerkleClock` interface in §7.9 lists `LCA(h1, h2 EventCID)` as a key method. But neither the spec nor the implementation uses it.

This creates confusion about what the protocol actually is: is LCA part of the protocol or not?

Refs: `doltswarm-protocol.md:239,516,588,734`, `doltswarm_verify.qnt:246`.

**Proposed change.** Pick one rule and state it cleanly. The simplest choice: **`finalized_root` is the merge ancestor, full stop.** Remove all LCA language, `find_lca` references, and "ideally" hedging from the protocol document. The ancestor rule becomes:

> `ancestor_root = finalized_root`

If LCA is added later as an optimization (to reduce spurious conflicts), it can be introduced as a protocol extension. But the base protocol should be unambiguous.

**Implications.**
- The protocol becomes internally consistent — one rule, no conditionals.
- `MerkleClock` interface drops the `LCA(h1, h2)` method. Simpler interface.
- §6.3 (No Common Ancestor) is removed — `finalized_root` is always available.
- §4.3 boundary diagram drops `find_lca`.
- Using `finalized_root` may produce more spurious conflicts than LCA would (since `finalized_root` may be older than the true common ancestor). This is a known trade-off: simpler protocol at the cost of more parking. In practice, with frequent finalization, `finalized_root` is usually close to the true LCA.

---

## 6. ~~MEDIUM~~ SOLVED — Heartbeat traffic is not minimized

**The problem.** Heartbeats carry `{ peer, hlc, heads, finalized_root }` and are broadcast every 2s to all peers. With `n` peers, this is `O(n^2)` messages per heartbeat interval. Each heartbeat carries the full `heads` set, which can be large if many events are unmerged or parked.

More importantly, when all peers are in sync (the common case), the heads are identical across all peers. Broadcasting full head sets every 2s when nothing has changed is wasted bandwidth.

Refs: `doltswarm-protocol.md:169,261,705`.

**Proposed change.** Replace the full `heads` set with a compact **frontier digest** — a single hash over the sorted head set (e.g., `hash(sort(heads))`). The heartbeat becomes:

```
Heartbeat = { peer, hlc, heads_digest: Hash, finalized_root }
```

On receive, if the digest matches the receiver's own `hash(sort(heads))`, no pull is needed. On mismatch, the receiver requests the full head set (or the missing events) via a point-to-point request. This turns the common case (peers in sync) into a cheap digest comparison and only pays the full cost when peers have actually diverged.

**Implications.**
- Heartbeat payload shrinks from variable-size (proportional to head count) to fixed-size (single hash).
- The common case (all peers in sync) requires zero follow-up. Only divergence triggers additional traffic.
- Adds one round-trip on divergence detection (digest mismatch → request full heads). This is acceptable since divergence triggers chunk sync anyway.
- The spec doesn't need to change — it models heartbeats as direct state reads, which is already an over-approximation.

---

## 7. ~~MEDIUM~~ SOLVED — Event identity and signing are under-specified

**The problem.** The protocol defines:

- `EventCID = Hash` — "hash of serialized Event" (§2.1, line 97)
- `Signature = byte[]` — "signs (root_hash, parents, hlc, peer, op_summary)" (§2.1, line 109)
- `Event.cid` — `hash(r, active_heads, hlc, peer)` (§2.4 LOCAL_WRITE step 5, line 197)

These three definitions are inconsistent:
- The CID definition at line 97 says "hash of serialized Event" (the full event including signature?).
- The CID computation at line 197 says `hash(r, active_heads, hlc, peer)` — excludes `op_summary` and `signature`.
- The signature covers `(root_hash, parents, hlc, peer, op_summary)` — includes `op_summary` but the CID doesn't.
- It's unclear whether the CID includes the signature (circular dependency) or not.

The Quint spec sidesteps this entirely by using `EventCID = (peer, wall, logical)` (line 388), derived from the HLC — no hash function involved.

Refs: `doltswarm-protocol.md:97,109,197`, `doltswarm_verify.qnt:44-45`.

**Proposed change.** Define a canonical unsigned event byte representation explicitly:

```
UnsignedEvent = serialize(root_hash, parents, hlc, peer, op_summary)
EventCID      = hash(UnsignedEvent)        -- CID is hash of unsigned bytes
Signature     = Sign(private_key, UnsignedEvent)  -- signs same bytes
```

The signature is excluded from CID computation (no circularity). The CID and signature cover the same fields. The `cid` field in the wire format is redundant (receivers recompute it from the unsigned bytes) but useful for quick dedup before deserialization.

**Implications.**
- Removes ambiguity about what the CID covers.
- Removes the circular dependency between CID and signature.
- `op_summary` is included in the CID (it's part of UnsignedEvent), making events with different summaries distinct even if the data is the same. This is correct — the summary is metadata about the operation, not just the result.
- The spec doesn't need to change — its `(peer, wall, logical)` CID abstraction preserves uniqueness without modeling hash functions.

---

## 8. ~~MEDIUM~~ SOLVED — `RECEIVE_EVENT` has no chunk-materialization gate before reconciliation

**The problem.** `RECEIVE_EVENT` step 8 requests chunks for `e.root_hash`, then step 9 calls `RECONCILE`. But reconciliation requires calling `MergeRoots(current_root, e.root_hash, ancestor)`, which needs the prolly tree data for `e.root_hash` to be locally available. Chunk sync (step 8) is an async network operation — it may not complete before reconciliation (step 9) tries to access the data.

The protocol presents steps 8-9 sequentially, but doesn't define what happens if chunks aren't ready. Does `RECONCILE` block until chunks arrive? Does it skip the event and retry later? The protocol says nothing.

Refs: `doltswarm-protocol.md:217-218`.

**Proposed change.** Define an explicit per-event lifecycle:

```
ANNOUNCED → CHUNKS_READY → MERGEABLE
```

- **ANNOUNCED**: Event is in the clock, heads are recomputed, but chunks are not yet local. The event participates in causal ordering and head computation, but NOT in `RECONCILE`.
- **CHUNKS_READY**: Chunk sync for `e.root_hash` is complete. The event is now eligible for reconciliation.
- **MERGEABLE**: `RECONCILE` has processed this event (either merged or parked).

`RECONCILE` operates only on heads that are `CHUNKS_READY` or `MERGEABLE`. This avoids accessing data that isn't local, without blocking the clock from advancing.

**Implications.**
- Makes the async nature of chunk sync explicit. No implicit assumption that chunks arrive instantly.
- The head set may temporarily include events whose chunks aren't ready. `RECONCILE` filters these out. Once chunks arrive, the next `RECONCILE` pass picks them up.
- This is essentially what any correct implementation must do anyway. Making it explicit in the protocol prevents implementers from assuming sequential execution.
- The spec doesn't model chunk sync (document-only), so no spec changes needed. But the spec's instant-reconciliation model is a sound over-approximation (it assumes chunks are always available).

---

## 9. ~~MEDIUM~~ SOLVED — `MERGE_FINALIZED` requires exchanging full `finalized_events` sets

**The problem.** `MERGE_FINALIZED` step 1: "Exchange `finalized_events` sets with the remote peer. The intersection gives the common pre-split base." This requires:

- A point-to-point state exchange mechanism that is neither broadcast nor chunk stream — not covered by the §2.3 communication model.
- Transferring full `finalized_events` sets, which grow monotonically over the lifetime of the system. For a long-running cluster, this could be thousands of event CIDs.

The heartbeat carries `finalized_root` (compact) but not `finalized_events` (unbounded). Divergence detection is cheap (compare roots), but computing the common ancestor requires the full sets.

The spec sidesteps this by reading remote state directly (`do_merge_finalized` reads `st_q.finalized_events`).

Refs: `doltswarm-protocol.md:307`, `doltswarm_verify.qnt:735-752`.

**Proposed change.** Two options (not mutually exclusive):

**Option A: Compact finalized frontier in heartbeat.** Add a digest of finalized events to the heartbeat (e.g., XOR of all finalized CIDs, or a Bloom filter). On divergence detection, use the digest to quickly determine whether intersection computation is needed.

**Option B: Incremental finalized-event exchange.** Instead of exchanging full sets, exchange only events finalized since the last known common point. Since both sides share a prefix of finalized events (everything before the partition), only the divergent suffix needs to be exchanged. The common root can be computed incrementally:
- Each peer maintains a `finalized_tip_cid` (the HLC-max finalized event CID).
- On divergence, request the remote's finalized events since the common `finalized_root` (which is known from the pre-partition state).

**Implications.**
- Option A adds a small fixed-size field to heartbeats. Negligible bandwidth cost.
- Option B requires a new point-to-point RPC ("give me finalized events since root X"), but avoids transferring the full set.
- Both approaches scale better than full-set exchange.
- The common-ancestor computation for `MERGE_FINALIZED` still needs the intersection, but with only the divergent suffixes, the sets are small (bounded by the partition duration).
- The spec is unaffected (direct state reads are already an over-approximation).

---

## 10. ~~MEDIUM~~ SOLVED — Merge `EVICT_STALE_PEER` and `EPOCH_FORCE_FINALIZE`

**The problem.** The protocol has two timeout-based eviction mechanisms:

- `EVICT_STALE_PEER` (§2.4): fires after `HEARTBEAT_TIMEOUT` (10s) when a specific peer stops heartbeating. Removes peer from `active_peers`, recomputes `stable_hlc`.
- `EPOCH_FORCE_FINALIZE` (§2.4): fires after `EPOCH_TIMEOUT` (60s) when `stable_hlc` hasn't advanced. Evicts "blocking peer(s)", recomputes `stable_hlc`.

These overlap: if a peer stops heartbeating, `EVICT_STALE_PEER` fires at 10s. The only scenario where `EPOCH_FORCE_FINALIZE` adds value is: a peer IS heartbeating but its HLC is stuck. However, each heartbeat updates `peer_hlc[sender]` with the sender's current HLC, so `stable_hlc` advances with each heartbeat. A peer with a frozen wall clock would still advance its logical counter on each heartbeat tick. `EPOCH_FORCE_FINALIZE` is unreachable when `EVICT_STALE_PEER` works correctly.

The spec already doesn't model `EPOCH_FORCE_FINALIZE` (§3: "timeout-based fallback, difficult to express as a model-checkable action").

Refs: `doltswarm-protocol.md:286-301`.

**Proposed change.** Remove `EPOCH_FORCE_FINALIZE`. If a longer safety-net timeout is desired, make `HEARTBEAT_TIMEOUT` configurable (e.g., `HEARTBEAT_TIMEOUT_SHORT = 10s` for normal eviction, `HEARTBEAT_TIMEOUT_LONG = 60s` for byzantine-tolerant environments). One mechanism, configurable parameter.

**Implications.**
- One fewer protocol action. Simpler mental model.
- §6.5 (Forced Epoch Finalization) is removed.
- No spec changes needed (already not modeled).
- If there is a genuine scenario where a heartbeating peer blocks `stable_hlc` (which I don't believe exists given HLC semantics), document it explicitly and design a targeted fix.

---

## 11. ~~MEDIUM~~ SOLVED — Quint spec `mergeRoots` hash combination can collide with sentinels

**The problem.** The spec's `mergeRoots` returns `ours + theirs + ancestor` for the non-conflict case (both diverge from ancestor, but at least one is not a raw `CONTENTS` value). With abstract integers `CONTENTS = {100, 200}`, `CONFLICT_HASH = -1`, `EMPTY_ROOT = 0`, the sum can accidentally produce sentinel values:

- `mergeRoots(-201, 200, 0)` → `-201 + 200 + 0 = -1 = CONFLICT_HASH`. A non-conflicting merge produces the conflict sentinel.
- `mergeRoots(50, 50, 0)` → `50 + 50 + 0 = 100 ∈ CONTENTS`. A merged hash looks like raw content, causing false conflicts in subsequent merges.

If a merged hash equals `CONFLICT_HASH`, `inv_no_conflict_latest_root` would fire incorrectly (false positive) or mask a real bug (false negative, depending on direction). If a merged hash equals a `CONTENTS` value, subsequent merges incorrectly treat it as a divergent raw write and produce spurious `CONFLICT_HASH`.

Refs: `doltswarm_verify.qnt:176-187`.

**Proposed change.** Use a hash combination that cannot collide with sentinels:

```quint
else 1000 + abs(ours) * 31 + abs(theirs) * 17 + abs(ancestor) * 7
```

This keeps the result above the `CONTENTS` range (100, 200) and well away from `CONFLICT_HASH` (-1) and `EMPTY_ROOT` (0). The specific formula doesn't matter as long as: (a) it's deterministic and symmetric in ours/theirs (since the spec's merge is symmetric for non-conflict), (b) it never produces -1, 0, 100, or 200 for any reachable inputs.

**Implications.**
- Eliminates false-positive/false-negative invariant results caused by hash collisions.
- Strengthens confidence in model-checking results.
- Pure spec change — no protocol impact.

---

## 12. ~~MEDIUM~~ SOLVED — `PEER_JOIN` is under-specified

**The problem.** `PEER_JOIN` (§2.4) is four lines of hand-waving in a protocol that carefully specifies every other action:

> 1. Join the peer mesh. 2. Heartbeats provide current heads. 3. Request: finalized_root + commits + clock events + chunks. 4. Reconstruct local state.

Key unspecified details:
- Which state variables are transferred? (`parked_conflicts`? `subsumed_events`? `stale_votes` if staleness is kept?)
- How is `finalized_root` transferred? As an opaque value (required if finding #4 is applied) or reconstructed from events (broken after partition merges)?
- How is `peer_hlc` initialized? (Must wait for a full heartbeat round before `stable_hlc` is meaningful.)
- How is `finalized_events` transferred? (Receiving "finalized Dolt commits" doesn't give event CIDs.)
- Is there integrity verification? (Can the transferring peer lie about finalized state?)
- Is there a state-transfer protocol (RPC? streaming?), or does the joining peer piece it together from broadcast messages?

The spec only partially models join (eviction and reconnection via heartbeat re-adding to `active_peers`).

Refs: `doltswarm-protocol.md:338-343`.

**Proposed change.** Specify `PEER_JOIN` as a state-transfer snapshot:

```
PEER_JOIN(new_peer) → unit

1. Join peer mesh (begin receiving broadcasts).
2. Request STATE_SNAPSHOT from any active peer:
   {
     finalized_root:    Hash,          -- opaque, not recomputed
     finalized_events:  Set[EventCID],
     clock:             Map[EventCID, Event],  -- non-finalized events only
     parked_conflicts:  Set[EventCID],
     subsumed_events:   Set[EventCID],         -- if finding #2 adopted
   }
3. Fetch chunks for finalized_root and latest_root via Puller.
4. Initialize local state from snapshot.
   peer_hlc: initialized to ZERO for all peers.
   stable_hlc: ZERO (advances naturally from heartbeats).
   active_peers: { self } (grows from heartbeats).
5. Begin heartbeating. First full heartbeat round establishes peer_hlc entries.
6. Verify: recompute heads from clock, verify finalized_root is reachable.
```

**Implications.**
- Joining peers get a consistent, complete snapshot instead of piecing state together.
- `finalized_root` is an opaque transfer (required after finding #4), not a recomputation.
- The joining peer starts with `stable_hlc = ZERO`, which is conservative (won't finalize anything until heartbeats establish the actual minimum). This is safe.
- The snapshot can be signed by the transferring peer for integrity (optional, depends on trust model).
- Spec could model this as a `do_join` action that copies state from an active peer.

---

## 13. ~~LOW-MEDIUM~~ SOLVED — Partition-conflict resolution adoption is unmodeled and under-specified

**The problem.** `RESOLVE_PARTITION_CONFLICT` step 8 specifies an adoption optimization: when a peer receives a resolution event from another peer that had the same `partition_conflict`, it adopts the resolved `finalized_root` and clears its own `partition_conflict`. However:

- The spec doesn't model this (§3: "safe under-approximation: each peer resolves independently").
- The detection mechanism is unspecified: how does a receiving peer know an incoming event resolves its partition conflict? The event carries no flag. The receiver would need to check something like: "I have an active `partition_conflict`, and this event's root_hash came from a peer that had the same conflict, and this event subsumes the conflicting lineages."
- If adoption changes `finalized_root` and `finalized_events` on the receiving peer, does it also trigger re-anchoring of tentative events? (Presumably yes, but not stated.)

Refs: `doltswarm-protocol.md:336`, `doltswarm_verify.qnt:404`.

**Proposed change.** If finding #3 (unify partition conflicts with parked conflicts) is adopted, this issue disappears — there is no separate `partition_conflict` to adopt.

If `PartitionConflict` is kept as a separate mechanism: either (a) model the adoption path in the spec and specify the detection criterion explicitly, or (b) remove the adoption optimization from the protocol and have each peer resolve independently. Option (b) is simpler but requires every peer's user to independently resolve the conflict (bad UX). Option (a) needs a detection criterion, e.g.: "An incoming event resolves this peer's `partition_conflict` if `event.root_hash` is the result of `mergeRoots(ours, theirs, ancestor)` for the recorded conflict, or if the event's peer had the same `partition_conflict` (known via heartbeat metadata)."

**Implications.**
- If unified with parked conflicts (finding #3): no separate adoption mechanism needed.
- If kept separate: adding a `resolves_partition: Option[Hash]` field to the event (the merged finalized root) would make detection trivial but adds a field. Alternatively, piggyback on heartbeats: a peer that resolved its partition conflict includes the new `finalized_root` in its next heartbeat, and receivers with the same conflict adopt it.

---

## 14. ~~LOW-MEDIUM~~ SOLVED — PeerID ordering bias in HLC tiebreaker

**The problem.** The HLC total order is `(wall, logical, peer)` lexicographic. When wall time and logical counter are equal (simultaneous writes by different peers in the same clock tick), the peer with the lower PeerID "wins" — its event is ordered earlier and wins the conflict parking decision (earlier event stays, later is parked).

This creates a static priority hierarchy: peer 1 always beats peer 3 in ties. In the spec with `PEERS = {1, 2, 3}`, peer 1 is systematically favored.

In practice, HLC wall times are unlikely to be exactly equal (nanosecond precision), and the logical counter provides further disambiguation. The PeerID tiebreaker fires rarely. But it is a permanent structural bias.

Refs: `hlc.qnt:26-29`, `doltswarm-protocol.md:96`.

**Proposed change.** Document the bias explicitly as a known property. Optionally, use a hash-based tiebreaker for fairness: `hash(wall || logical || peer)` instead of raw PeerID comparison. This distributes the bias randomly across peers. The trade-off is slightly more expensive comparison (one hash per tiebreak) and less human-readable ordering.

**Implications.**
- If documented only: no protocol or spec changes. Users are aware of the bias.
- If changed to hash-based: `hlc.less` in the spec changes to compare `hash(a)` vs `hash(b)` in the final branch. Minor spec change. Protocol document updates the HLC definition.
- The bias is a fairness concern, not a correctness concern. No invariants are affected.

---

## 15. ~~LOW~~ SOLVED — `OpSummary` in the signed event adds weight without affecting protocol logic

**The problem.** `OpSummary` (`tables_modified`, `is_schema_change`, `description`) is a field on every event, included in the signature, but never referenced by any protocol action's logic. No action branches on `tables_modified`. No action checks `is_schema_change`. The `description` is purely informational.

Signing it prevents tampering (good), but it increases event size and signing cost. For a protocol that aims to minimize communication, every byte in the broadcast event matters.

Refs: `doltswarm-protocol.md:102,109,111-115`.

**Proposed change.** Make `OpSummary` optional (nullable). If present, it's included in the signature. If absent, the signature covers a sentinel (empty summary). Peers that want rich metadata include it. Peers optimizing for bandwidth skip it.

Alternatively, move `OpSummary` out of the event entirely and into the Dolt commit metadata (which is local-only). The event becomes: `{ cid, root_hash, parents, hlc, peer, signature }` — strictly the fields needed for protocol logic.

**Implications.**
- If made optional: minor protocol change. Events get smaller on the wire when summary is omitted. Signature computation adjusts.
- If removed: events are minimal. Summary information is still available locally via Dolt commit metadata. Remote peers can reconstruct summaries from the diff if needed (they have the prolly trees).
- No invariant changes. No spec changes (spec already omits `op_summary`).

---

## Summary: Recommended Simplification Direction

These three changes, applied together, would transform the protocol from ~14 protocol actions and ~15 state variables to a significantly simpler core:

1. **Remove stale-vote rejection** (finding #1). Drop from 3 broadcast message types to 2. Remove voting, rollback, lineage exemption. Replace with optional local admission policy.

2. **Unify partition conflicts with parked conflicts** (finding #3). Remove `PartitionConflict` as a separate type, `RESOLVE_PARTITION_CONFLICT` as a separate action. Finalization never blocks. One conflict mechanism for all scenarios.

3. **Standardize on `finalized_root` as the merge ancestor** (finding #5). Remove all LCA references. One rule, no conditionals, no "ideally" hedging. Add LCA later as a documented optimization if spurious conflicts become a real problem.

The resulting protocol:
- **Two broadcast message types:** Event, Heartbeat.
- **Zero voting, zero consensus, zero blocking.**
- **One conflict mechanism:** park the later event, surface to user, resolve via `LOCAL_WRITE`.
- **One merge-ancestor rule:** `finalized_root`.
- **One eviction mechanism:** heartbeat timeout.

This matches the spirit of §0: "coordination limited to broadcast message exchange over a direct mesh, no consensus, and no central coordinator."
