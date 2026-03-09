# Doltswarm: P2P Sync Protocol over Dolt

## 0. Motivation and Research Background

### Goal

Design a peer-to-peer synchronization protocol for Dolt databases that allows multiple peers to concurrently read and write SQL data, automatically merge non-conflicting changes, surface conflicts to users for resolution, and converge on a shared finalized history — all without a central coordinator, with minimal inter-peer communication, and with no consensus protocol.

### Three layers, three identities, three responsibilities

Doltswarm works only if three different things are kept conceptually separate:

- **Event layer (Merkle clock DAG):** identifies **what happened** and **what is concurrent**. Events are immutable protocol facts. `EventID` is event identity. This layer is the authoritative shared structure while concurrency is still unsettled.
- **Data layer (prolly trees):** identifies **what database state results** from a write or merge. `prolly_root_hash` is content identity. This layer stores SQL state and is where Dolt's three-way merge operates.
- **Commit layer (Dolt commit DAG):** materializes database states as Dolt commits. This layer itself has two zones:
  - a **finalized zone**, whose commit ids are shared `finalized_commit_id` values in immutable history
  - a **tentative zone**, whose local `tentative_commit_id` values are convenience state and may be rewritten as finalization advances

These identities are intentionally different:

- two different events may point to the same `prolly_root_hash`
- two different finalized commits may point to the same `prolly_root_hash`
- the same content root may legitimately recur later in history

That is not a bug. It is the core architectural split. Doltswarm is not trying to make one identifier do every job. It is trying to combine:

- an event DAG for real-time causal settlement
- prolly-tree roots for deterministic content merge
- a Dolt commit DAG whose finalized zone is the user-visible immutable history product

This means the Dolt commit DAG is not monolithic from the protocol's perspective. Its finalized zone is authoritative shared history. Its tentative zone is a local projection surface over still-unsettled event/content state. `tentative_commit_id` values may be rewritten as finalization advances. `finalized_commit_id` values, by contrast, are durable shared history products.

### The real boundary problem: when does local reconciliation harden into shared history?

The hard problem in doltswarm is not "when do we merge data?" Dolt already provides a three-way merge over `RootValue`s at the data layer. The harder problem is: **when does a reversible local reconciliation become irreversible shared history?**

That boundary is what separates two different kinds of merge:

- **Tentative reconciliation:** multiple Merkle-clock heads are allowed, local `RECONCILE` computes a current projection over prolly-tree roots, and local `tentative_commit_id` materializations are treated as rewritable convenience state.
- **History-level merge:** two already-finalized lineages later meet after a partition or long disconnect, so the shared finalized Dolt DAG must record a real merge commit instead of pretending one side can be rewritten away.

This leads to the design rule that motivates the whole protocol:

**merge roots continuously, merge commits only when finalized lineages meet.**

For connected, still-settling concurrency, we do not want to emit a Dolt merge commit for every concurrent write. That would collapse back to "just let every peer make commits and merge the commit DAG all the time", which is exactly what causes merge storms, repeated re-parenting, and unstable commit identity. The Merkle clock exists to keep that churn below the history boundary.

For partition-grade divergence, the story changes. Once each side has already hardened some history for its current witness set, rewinding finalized history would violate monotonicity. At that point a real merge in the finalized Dolt DAG is the correct representation: two independently hardened histories met and had to be joined.

### The problem with Dolt's commit model in a P2P setting

Dolt stores data in content-addressed prolly trees (deterministic B-trees where the same dataset always produces the same root hash). This is excellent for sync — identical data produces identical chunks regardless of which peer produced it. However, Dolt layers a Git-style commit DAG on top of this data layer. Each commit's hash is computed from its serialized flatbuffer, which includes `parent_addrs` — the 20-byte hashes of parent commits. Commit identity therefore depends on position in history, not just on table state.

In a P2P setting, that creates the core tension. If peers create commits independently and later try to stitch them into one shared history, every rebase or parent reassignment changes commit hashes and cascades through descendants. Any external reference to the old commit hash becomes stale. Git solves this by centralizing ordering at a server or maintainer. Doltswarm cannot rely on that.

So the protocol must do two things at once:

- avoid treating every concurrent write as a history-layer commit-DAG merge problem
- still provide a common, meaningful finalized Dolt history once some set of content changes is no longer considered rewritable

### What DefraDB taught us: a Merkle DAG can be the truth

DefraDB (by Source Network) takes the CRDT route. Each document is stored as a Merkle CRDT update DAG. Concurrent edits naturally produce multiple heads, and peers converge by exchanging immutable DAG nodes and applying the embedded CRDT semantics. There is no extra obligation to compile those heads into a single shared commit DAG for users. The Merkle DAG itself is the truth.

The key lesson from DefraDB is that **a CRDT-shaped shared structure removes the need to coordinate on linear history while the system is still settling.** Peers can simply union immutable nodes and compute the same current state from the same causal graph. The tradeoff is that DefraDB is operating in a document/CRDT world, not in a SQL database with rich three-way merge semantics and a user-visible commit history.

### What OrbitDB taught us: exchanging heads is enough if the log is the product

OrbitDB pushes the same lesson from a different angle. OrbitDB peers synchronize by exchanging log heads and then pulling missing log entries until they converge on the same log state. The system promises eventual consistency of the replicated log, not a separate finalized history layer with stable, user-meaningful commit identities.

That contrast matters. OrbitDB's sync problem stops at "do peers eventually observe the same append-only log?" Doltswarm has an extra obligation: the replicated causal structure is not the final user-facing history. It must eventually be compiled into a common finalized Dolt DAG. That makes doltswarm's stability boundary stricter than OrbitDB's head-exchange story.

### What Fireproof taught us: separate the clock from the data

Fireproof provides the architectural blueprint. It cleanly separates two layers:

**Merkle Clock (Pail):** a causal event log where concurrent writes produce multiple heads and clocks can be union-merged cheaply.

**Prolly Trees:** deterministic trees that store the actual state, so applying the same accepted event set yields the same root.

The lesson is not only "use a Merkle clock." The deeper lesson is: **keep causal structure and data state separate.** The clock answers *what happened and what is concurrent*; the data layer answers *what state results when those changes are merged*. Fireproof can stop there because its clock/data model is already the system of record. Doltswarm cannot stop there, because it also owes users a third layer: a common, stable Dolt commit DAG for finalized history.

### The non-obvious finding in Dolt: MergeRoots is DAG-independent

The critical discovery from examining Dolt's Go codebase is that Dolt's three-way merge algorithm operates on `RootValue` objects, not commits:

```go
func MergeRoots(ctx, ourRoot, theirRoot, ancRoot *doltdb.RootValue) (*doltdb.RootValue, ...)
```

The merge algorithm computes `Diff(ancestor, ours)` and `Diff(ancestor, theirs)` using prolly tree diffing, streams both diffs in primary key order, and performs cell-level conflict detection. None of this touches the commit DAG. The DAG is only used *upstream* to find the common ancestor via `FindCommonAncestor`, which walks parent pointers.

This means: **if we can supply a common ancestor `RootValue` through any external mechanism, the entire merge engine works without modification.** We do not need to change Dolt's merge code, its prolly tree implementation, or its chunk storage. We only need to replace the ancestor discovery layer.

Additionally, Dolt's chunk-level sync (`Puller`) operates on opaque content-addressed chunks — it follows hash references without distinguishing between commit objects and prolly tree nodes. We can sync prolly tree data between peers without any commit wrapping.

### The synthesis: a Merkle clock over Dolt's prolly trees

Combining these findings:

From **DefraDB** we take the principle that unsettled concurrency should live in a CRDT-shaped shared structure, not in a globally-agreed commit chain.

From **OrbitDB** we take the lesson that exchanging heads and missing causal history is enough while the replicated causal structure is still the product.

From **Fireproof** we take the architectural split: keep the Merkle clock as the causal/witness layer and the prolly trees as the state/merge layer.

From **Dolt** we keep everything valuable: the SQL interface, the prolly tree storage engine, the three-way merge with cell-level conflict detection, the chunk-level sync protocol, and the local commit DAG as the final human-facing history representation.

The protocol does not modify Dolt's merge engine. Instead, it composes the three layers into one pipeline:

1. **Events settle first:** the Merkle clock DAG records immutable writes and causal relationships, and remains the authoritative shared structure while concurrency is unsettled.
2. **Content is derived continuously:** peers use those events plus recorded anchors to compute prolly-tree root projections with `MergeRoots`, silently and deterministically.
3. **History hardens later:** once the witness condition holds, some portion of that settled event/content state is compiled into finalized Dolt commits. Those finalized commits are the immutable shared history product.

Viewed this way, the protocol sits between prolly-tree data and the Dolt commit layer, while allowing the commit DAG to serve two different roles:

- while history is still tentative, the Merkle clock is the authoritative shared structure
- `RECONCILE` is local and silent: peers continuously merge content roots without emitting history-layer merge artifacts
- the tentative zone of the Dolt commit DAG is a local projection surface, not globally stable history
- once the witness condition for finalization holds, some portion of that settled state hardens into the finalized immutable zone of the Dolt commit DAG
- only if two already-finalized lineages later meet do we emit a real Dolt merge commit

This is what makes doltswarm different from the other systems above. DefraDB, OrbitDB, and Fireproof can stop at the converged clock/log/DAG level. Doltswarm cannot, because it must periodically compile that causal structure into a common, meaningful, user-visible Dolt history.

The result is a protocol with a sharper boundary:

- **below the boundary:** live concurrency in the event layer, deterministic content reconciliation in the data layer, no finalized-history merges
- **at the boundary:** a witness condition decides which settled event/content state hardens into shared history
- **above the boundary:** finalized lineages are monotone, and when they later diverge and meet, the Dolt DAG records a real merge commit

That boundary — not data merge alone — is the central design problem of doltswarm.

---

## 1. Architecture Overview

Three layers, two provided by Dolt, one new:

```
┌──────────────────────────────────────────────────────┐
│  Layer 3: Reconciliation (NEW — doltswarm Go)        │
│  Merkle clock (in-memory), causal stability,         │
│  merge orchestration, pluggable transport             │
├──────────────────────────────────────────────────────┤
│  Layer 2: Commit DAG (EXISTING — Dolt, local-only)   │
│  Local branch refs, working sets, dolt log           │
├──────────────────────────────────────────────────────┤
│  Layer 1: Prolly Trees + ChunkStore (EXISTING)       │
│  Content-addressed NBS, RootValue, MergeRoots        │
└──────────────────────────────────────────────────────┘
```

**Layer 1 (Dolt, unmodified):** Prolly trees store table data. `ChunkStore.{HasMany,GetMany,PutMany}` is the I/O surface. `MergeRoots(ours, theirs, ancestor)` performs three-way merge given any three `RootValue` objects. `Puller` syncs chunks by walking hash references. All content-addressed via SHA-512/20.

**Layer 2 (Dolt, local-only):** Each peer creates Dolt commits locally after reconciliation. Tentative local commits are named by local `tentative_commit_id` values and may differ across peers (parents, timestamps differ). Finalized shared-history commits are named by deterministic `finalized_commit_id` values. The commit DAG is therefore split into a **finalized zone** (deterministic, identical across peers) and a **tentative zone** (local convenience, rewritten on finalization).

**Layer 3 (NEW — this protocol):** A Merkle clock DAG of events held in-memory and disseminated via broadcast over a direct peer mesh. Causal stability determines finalization (no voting). Merge orchestration calls into Layer 1. All conflicts — data and schema — are rejected and pushed back to the user.

---

## 2. Abstract Protocol

### 2.1 Data Types

```
Hash        = byte[20]                    -- SHA-512/20
PeerID      = opaque peer identifier
HLC         = { wall: uint64, logical: uint16, peer: PeerID }
                -- Total order: (wall, logical, peer) lexicographic.
                -- NOTE: PeerID tiebreaker creates a static priority bias
                -- (lower PeerID always wins ties). In practice, wall times
                -- are nanosecond-precision and ties are rare. A hash-based
                -- tiebreaker (hash(wall || logical || peer)) distributes
                -- bias randomly but adds comparison cost. The current
                -- raw comparison is kept for simplicity.

EventIDPayload = (
  prolly_root_hash: Hash,
  anchor_prolly_root_hash: Hash,
  parent_event_ids_sorted: List[EventID],         -- parents sorted by EventID bytes (lexicographic)
  hlc: HLC,
  peer: PeerID,
  resolved_finalized_conflict_id: Optional[FinalizedConflictID]
)

CanonicalEncode(v) = deterministic, injective byte encoding for protocol values.
                     -- This protocol does NOT mandate a concrete wire codec.
                     -- It mandates properties: fixed field order, canonical set/list ordering,
                     -- and canonical Optional encoding (present/absent is unambiguous).

EventID   = hash(CanonicalEncode(EventIDPayload))

Event = {
  event_id:            EventID,               -- redundant on wire (receivers recompute), useful for quick dedup
  prolly_root_hash:     Hash,                  -- RootValue hash after this peer's local write
  anchor_prolly_root_hash: Hash,               -- finalized prolly-root hash against which this event's prolly_root_hash was produced
  resolved_finalized_conflict_id: Optional[FinalizedConflictID], -- null unless this event claims to resolve a finalized conflict
  parent_event_ids:    Set[EventID],         -- causal parents in the Merkle clock
  hlc:            HLC,
  peer:           PeerID,
}

MergeResult =
  | Ok     { merged_root: Hash }
  | Conflict { tables: Set[string], details: string }

FinalizedCommitID = deterministic finalized shared-history commit identifier
             -- semantic role: finalized shared-history identity
             -- one implementation may realize this as the deterministic
             -- finalized Dolt commit hash

TentativeCommitID = local-only tentative commit identifier
                  -- semantic role: current rewritable local commit materialization
                  -- one implementation may realize this as a local Dolt commit hash

FinalizedConflictID = Hash
                    -- hash(serialize(ours_finalized_commit_id, theirs_finalized_commit_id, common_finalized_commit_id))

FinalizedConflictInfo = {
  id:         FinalizedConflictID,
  ours_finalized_commit_id:  FinalizedCommitID,
  theirs_finalized_commit_id: FinalizedCommitID,
  common_finalized_commit_id: FinalizedCommitID,
  tables:     Set[string],
  details:    string,
}

computeHeadEventIDs(clock) = {
  event_id ∈ clock.keys() | ¬∃ e ∈ clock.values(): event_id ∈ e.parent_event_ids
}

```

`parent_event_ids_sorted` is the canonical deterministic ordering of `Event.parent_event_ids` used only for `EventID` canonicalization. It carries no additional causal semantics.

The core protocol state machine is defined over `Event`. Any transport/authentication envelope around `Event` is outside the core protocol and influences admission only through `ValidEvent(e)`.

### 2.2 Per-Peer State

All Layer 3 state is in-memory. Persistence comes from Dolt's Layer 1/2. On crash recovery, a peer rejoins from a live peer via `PEER_JOIN` (explicit state transfer of finalized state + retained clock events), not from message replay. Unreplicated tentative local writes are therefore **crash-volatile**: if an event has not yet left Layer 3 and reached another live peer, that write is outside the protocol's durability guarantee.

| Variable | Type | Description |
|---|---|---|
| `clock` | `Map[EventID, Event]` | Locally retained Merkle clock events: all non-finalized events plus any retained finalized events not yet compacted by `GC_CLOCK`. The retained finalized subset `clock.keys() ∩ finalized_events.keys()` MUST stay parent-closed. |
| `head_event_ids` | `Set[EventID]` | Clock head event IDs (events with no children locally) |
| `chunks_ready` | `Set[EventID]` | Announced events whose `prolly_root_hash` and `anchor_prolly_root_hash` chunks are locally available (`CHUNKS_READY`). |
| `tentative_prolly_root_hash` | `Hash` | Current merged RootValue |
| `projection_basis` | `Set[EventID]` | The exact event-id basis embodied in `tentative_prolly_root_hash`: the head-event-id set whose anchored fold produced the current tentative content projection. Under closure lag this basis may lag behind raw `head_event_ids`; `LOCAL_WRITE` and `RESOLVE_*` parent from `projection_basis`, not from raw head event IDs. |
| `hlc` | `HLC` | Local hybrid logical clock used only for deterministic event ordering and tie-breaking |
| `peer_seen_frontier` | `Map[PeerID, Set[EventID]]` | Last known seen frontier per peer. A seen frontier is the maximal set of locally parent-closed events that peer has observed. `peer_seen_frontier[self]` MUST equal the locally derived seen frontier. |
| `finalized_commit_id` | `FinalizedCommitID` | Authoritative finalized-history identity at the stability boundary |
| `finalized_prolly_root_hash` | `Hash` | RootValue carried by `finalized_commit_id` |
| `finalized_events` | `Map[EventID, Hash]` | Canonical finalized event catalog (`event_id -> prolly_root_hash`). Set semantics are `finalized_events.keys()` |
| `finalized_commit_parents` | `Map[FinalizedCommitID, Set[FinalizedCommitID]]` | Authoritative finalized commit DAG parent links (`child_commit_id -> parent_commit_ids`) used for lineage ancestry/LCA |
| `parked_conflicts` | `Set[EventID]` | Authoritative set of parked events (later in total order, conflicted at merge). Excluded from LOCAL_WRITE parents and finalization. During closure lag (some heads not mergeable yet), unresolved parked head IDs are retained conservatively until closure completes and reconciliation recomputes against a complete mergeable set. |
| `finalized_conflicts` | `Map[FinalizedConflictID, FinalizedConflictInfo]` | Open finalized-layer conflicts detected in `SYNC_FINALIZED` divergence merges. These remain open until the corresponding resolution claim hardens into finalized state. |
| `resolved_finalized_conflict_ids` | `Set[FinalizedConflictID]` | Durable monotone set of finalized-layer conflict ids that are already resolved in finalized state. This is the authoritative anti-resurrection state; event-carried resolution refs are claims, not the source of truth. |
| `active_peers` | `Set[PeerID]` | Peers seen in the last heartbeat window; this is the current witness set for finalization |

Retained finalized event bodies are a special case inside the mixed `clock` map: if retained, they MUST form a parent-closed subgraph. This is preserved by `GC_CLOCK` and by the retained-finalized sync rules in `SYNC_FINALIZED`.

### 2.2.1 Tentative State vs. Finalized State

The protocol has two explicit state zones:

- **Finalized state (monotone shared ground truth):** `finalized_commit_id`, `finalized_prolly_root_hash`, `finalized_events`, `finalized_commit_parents`, and `resolved_finalized_conflict_ids`. This state advances monotonically and is the shared finalized-commit layer reconciled by `ADVANCE_STABILITY` and `SYNC_FINALIZED`.
- **Tentative state (revisable local projection):** `clock`, `head_event_ids`, `chunks_ready`, `tentative_prolly_root_hash`, `parked_conflicts`, and `projection_basis`. This is a deterministic projection from the currently-known frontier relative to the current finalized anchor. It is recomputed when new events, ancestors, chunks, or a new finalized anchor arrive.

Conflict handling is also split:
- **Tentative-layer conflicts:** represented by `parked_conflicts`; non-blocking and resolved via `RESOLVE_CONFLICT`.
- **Finalized-layer conflicts:** represented by `finalized_conflicts`; non-blocking and resolved via `RESOLVE_FINALIZED_CONFLICT`, then durably cleared only once the corresponding claim hardens into finalized state.

`tentative_commit_id` is intentionally outside this Layer 3 state machine. It is a local Layer 2 materialization derived from `tentative_prolly_root_hash`. `finalized_commit_id`, by contrast, is part of the shared protocol state because it identifies the stable finalized-history boundary.

### 2.3 Communication Model

The protocol assumes a **direct mesh** network: every peer can send messages to every other peer. Communication is via a **pluggable transport** abstraction. The protocol is transport-agnostic — the transport must provide broadcast delivery and point-to-point streams, but the protocol does not depend on any specific networking library.

**Control plane (broadcast)** — two message types, broadcast to all peers:

| Message Type | Payload | Rate |
|---|---|---|
| `Event` | See §2.1 | On local write |
| `Heartbeat` | `{ peer: PeerID, head_event_ids_digest: Hash, seen_frontier: Set[EventID], finalized_commit_id: FinalizedCommitID, finalized_prolly_root_hash: Hash }` | Adaptive (event-driven + idle keepalive) |

**Data plane (chunk sync)** — prolly tree data is synced via **point-to-point streams** between specific peers, triggered by event receipt. When a peer receives an event, it requests the prolly tree chunks needed to evaluate that event: the chunks for `event.prolly_root_hash`, and the chunks for `event.anchor_prolly_root_hash` if they are not already local. The sync walks each prolly tree from its root hash, requesting only chunks not already present in the local `ChunkStore`. Point-to-point, not broadcast — only the peer that needs chunks requests them. Content-addressed deduplication via Dolt's SHA-512/20 hashing ensures structural sharing across events.

**No voting, no consensus, no coordinator.** There is no agreement mechanism in the protocol. Convergence follows from set union (the Merkle clock is a G-Set) and deterministic merge (same inputs → same output on every peer).

### 2.3.1 Event Identity and Admission

Core event identity is:
- `event_id = hash(CanonicalEncode(EventIDPayload))`, where `EventIDPayload` includes `prolly_root_hash`, `anchor_prolly_root_hash`, canonical parent list, `hlc`, `peer`, and the optional finalized-conflict resolution **claim** `resolved_finalized_conflict_id`.
- `CanonicalEncode(...)` MUST be deterministic, injective, and canonical over field order, set/list ordering, and Optional encoding.
- On the wire, `event_id` is redundant (receivers recompute it) but useful for quick deduplication before full deserialization.

The normative admission rule is a swarm-wide predicate `ValidEvent(e)`.

- Every peer in one swarm MUST apply the same `ValidEvent(e)` predicate to all received events.
- `ValidEvent(e)` MUST be deterministic from the event payload plus shared swarm configuration.
- `ValidEvent(e)` MUST reject malformed events, `EventID`/payload mismatches, and any event that fails the swarm's configured authenticity/integrity checks.
- Events for which `ValidEvent(e) = false` are **silently discarded** (not added to the clock, not forwarded).

How a deployment implements `ValidEvent(e)` is outside the core state machine. Authentication material, if any, is carried outside the core `Event` and MUST NOT change the logical event fields or `EventID`.

What is forbidden is per-peer discretion. One peer MUST NOT accept an event that another compliant peer in the same swarm would reject under the shared `ValidEvent(e)` definition.

### 2.4 Protocol Actions

#### LOCAL_WRITE(peer, sql_ops) → Event

No conflict precondition — writes are never blocked.

1. Apply `sql_ops` to local Dolt instance → new `RootValue` with hash `r`.
2. `hlc ← tick(hlc)`.
3. `projection_basis` is the authoritative local parent basis for the current tentative content projection.
4. Build `event_id_payload = (r, finalized_prolly_root_hash, sort(projection_basis), hlc, peer, null)`.
5. `event_id = hash(CanonicalEncode(event_id_payload))`.
6. `e = Event { event_id, prolly_root_hash: r, anchor_prolly_root_hash: finalized_prolly_root_hash, resolved_finalized_conflict_id: null, parent_event_ids: projection_basis, hlc, peer }`.
7. `clock[e.event_id] ← e`.
8. `chunks_ready ← chunks_ready ∪ {e.event_id}` (local writes are immediately chunk-ready).
9. Recompute `head_event_ids` from clock. `computeHeadEventIDs` still returns parked event IDs as head event IDs when no event has them as parents. That is correct: they persist as forks until resolved.
10. Recompute local seen frontier: `peer_seen_frontier[self] ← SeenFrontier(clock)`.
11. `RECONCILE(peer)` recomputes `(tentative_prolly_root_hash, parked_conflicts, projection_basis)` from the new clock/head state.
12. After any change that can enlarge the finalization candidate set, call `ADVANCE_STABILITY(peer)`. This includes at minimum the local frontier update in step 10, any chunk-readiness change, any parked-state change, any finalized-anchor change, and any witness-set change.
13. Create or replace the local tentative Dolt commit, yielding a local `tentative_commit_id`. This is Layer 2 convenience state, not shared protocol identity.
14. Publish `e` to all peers.

#### RECEIVE_EVENT(peer, e) → Accept | Discard

Events follow a per-event lifecycle: **ANNOUNCED → PARENTS_READY → CHUNKS_READY → MERGEABLE**.

- **ANNOUNCED**: Event is in the clock and `head_event_ids` are recomputed, but parent closure and chunk availability are not yet established locally. The event participates in causal ordering and head computation, but NOT in `RECONCILE` or finalization.
- **PARENTS_READY**: All ancestor event IDs reachable from `e.parent_event_ids` are present in the local clock.
- **CHUNKS_READY**: Chunk sync for both `e.prolly_root_hash` and `e.anchor_prolly_root_hash` is complete.
- **MERGEABLE**: Event is both **PARENTS_READY** and **CHUNKS_READY**, and `RECONCILE` has processed it (either merged or parked).

Steps:

1. If `e.event_id ∈ clock ∨ e.event_id ∈ finalized_events.keys()`: discard (idempotent).
2. **Admission check:** Evaluate the swarm-wide predicate `ValidEvent(e)`. If `ValidEvent(e) = false`, discard silently.
3. **Strict admission rule:** If an event is valid and new, it MUST be accepted into `clock`. Peers MUST NOT discard based on local wall-time checks (e.g., `|now() - e.hlc.wall|`).
4. `hlc ← merge(hlc, e.hlc)`.
5. `clock[e.event_id] ← e`. Event is now **ANNOUNCED**.
6. If `e.resolved_finalized_conflict_id` is present, treat it as a **resolution claim**, not as immediate finalized-layer truth. Receiving the event MUST NOT directly remove the conflict from `finalized_conflicts`.
7. Recompute `head_event_ids` from clock: `head_event_ids = { event_id ∈ clock | event_id is not a parent of any event in clock }`.
8. Recompute local seen frontier: `peer_seen_frontier[self] ← SeenFrontier(clock)`. This frontier contains only locally **PARENTS_READY** events.
10. Request prolly tree chunks for `e.prolly_root_hash` and, if needed, for `e.anchor_prolly_root_hash` from the originating peer (or any peer that already has them) via point-to-point stream.
11. On completion of all required chunk sync for step 10: `chunks_ready ← chunks_ready ∪ {e.event_id}` (event becomes **CHUNKS_READY**).
12. Parent-closure readiness is tracked independently: an event becomes **PARENTS_READY** only when all ancestor event IDs reachable from its parent links are present in the local `clock` (typically via heartbeat pull-on-demand).
13. Call `RECONCILE(peer)`. `RECONCILE` operates only on `head_event_ids` that are **PARENTS_READY** and **CHUNKS_READY** (or already **MERGEABLE**). Head event IDs missing ancestors or chunks are skipped until a later reconciliation pass.
14. After any change that can enlarge the finalization candidate set, call `ADVANCE_STABILITY(peer)`. For `RECEIVE_EVENT`, that includes at minimum the local frontier refresh in step 8, later chunk completion, later parent-closure completion, and any parked-state change caused by reconciliation.

#### RECONCILE(peer) → unit

**Merges are deterministic local computations, not new information.** If peers A and B both have events `{e1, e2}` with two head event IDs, they each independently call `MergeRoots` and arrive at the same `tentative_prolly_root_hash`. There is nothing to communicate — the merge result is fully determined by the inputs that all peers already have. No event is created, nothing is broadcast.

**Reconcile has an explicit closure-lag contract.** Reconcile operates on ALL `head_event_ids` (not pre-filtered by existing `parked_conflicts`). When all head event IDs are mergeable, parking is recomputed deterministically from anchored fold semantics. When closure is incomplete (some head event IDs are not yet mergeable), projection is provisional and unresolved parked head IDs are retained conservatively until closure completion.

`AnchoredFold(head_event_ids, start_prolly_root_hash) -> (root, conflict_set)` is the shared merge primitive used by both `RECONCILE` and `ADVANCE_STABILITY`: sort head event IDs by HLC, start `acc = start_prolly_root_hash`, then fold `MergeRoots(acc, head.prolly_root_hash, head.anchor_prolly_root_hash)` so each event is replayed against the finalized anchor it was originally written on.

1. If `head_event_ids = ∅`: set `tentative_prolly_root_hash ← finalized_prolly_root_hash`, set `parked_conflicts ← ∅`, set `projection_basis ← ∅`, return.
2. `mergeable_head_event_ids = { h ∈ head_event_ids | h is PARENTS_READY ∧ h is CHUNKS_READY }`.
3. `unready_head_event_ids = head_event_ids \ mergeable_head_event_ids`.
4. If `mergeable_head_event_ids = ∅`: no merge is possible yet. Keep `tentative_prolly_root_hash` unchanged, conservatively retain unresolved parked head event IDs (`parked_conflicts ← parked_conflicts ∩ head_event_ids`), and preserve the previous `projection_basis`. Return.
5. Sort `mergeable_head_event_ids` by HLC total order (deterministic).
6. Apply `AnchoredFold(mergeable_head_event_ids, finalized_prolly_root_hash)` to compute `(merged_root, parked_ready)`:
   - This single fold contract is used for both single-head and multi-head reconciliation (no semantic single-head bypass).
   - On each conflicting fold step, the later head in total order is parked.
7. `retained_unready_parked = parked_conflicts ∩ unready_head_event_ids`.
8. `parked_conflicts ← parked_ready ∪ retained_unready_parked`.
9. `tentative_prolly_root_hash ← merged_root`.
10. `projection_basis ← mergeable_head_event_ids \ parked_ready`. This is the exact event-id basis embodied in `tentative_prolly_root_hash`.
11. **`head_event_ids` are unchanged.** Head event IDs are always derived from `computeHeadEventIDs(clock)` — since reconcile does not add events to the clock, the head-event-id set cannot change. Parked head event IDs remain as persistent DAG forks until `RESOLVE_CONFLICT`.

> **Why no merge events?** Broadcasting merges would be pure redundancy — every peer holding the same events computes the same merged state. Worse, if merge events were broadcast, concurrent merges by different peers would create new DAG forks requiring further reconciliation (an infinite merge loop). Only three things produce network traffic: `LOCAL_WRITE` (new data), conflict-resolution actions (`RESOLVE_CONFLICT` and `RESOLVE_FINALIZED_CONFLICT`, both flowing through `LOCAL_WRITE`), and `HEARTBEAT` (which naturally reflects the reduced head set after merge).

#### RESOLVE_CONFLICT(peer, event_id, resolution_ops) → unit

User (typically the author of the parked event) provides `resolution_ops` (SQL that fixes conflicting rows/schema). Resolution is async — no peer is blocked waiting for it.

1. Apply `resolution_ops` to local Dolt → `resolved_root`.
2. Remove `event_id` from `parked_conflicts`.
3. Create new `Event` with `resolved_root`, `resolve_parent_event_ids = projection_basis ∪ {event_id}`, and `resolved_finalized_conflict_id = null`.
4. Normal `LOCAL_WRITE` flow from step 4 (same `EventID` construction, with `resolved_finalized_conflict_id = null`).

#### RESOLVE_FINALIZED_CONFLICT(peer, conflict_id, resolution_ops) → unit

User resolves a tracked finalized-layer conflict (created by `SYNC_FINALIZED` conflict handling) without blocking finalization.

1. Precondition: `conflict_id ∈ finalized_conflicts`.
2. Apply `resolution_ops` to local Dolt → `resolved_root`.
3. Create new `Event` with `resolved_root`, `parent_event_ids = projection_basis`, and `resolved_finalized_conflict_id = conflict_id`.
4. Normal `LOCAL_WRITE` flow from step 4 (same `EventID` construction, but with `resolved_finalized_conflict_id = conflict_id` from step 3).
5. Event publication is only a **claim** that conflict `conflict_id` is resolved. The id moves to durable finalized-layer resolution state only when that event later finalizes, or when the peer learns the durable resolved-id set through finalized-state sync.
6. `RESOLVE_FINALIZED_CONFLICT` MUST NOT directly mutate `finalized_commit_id`, `finalized_prolly_root_hash`, `finalized_events`, `finalized_conflicts`, or `resolved_finalized_conflict_ids`; those evolve only via existing finalization/sync paths.

#### HEARTBEAT(peer) → unit

Adaptive policy:
- Event-driven heartbeat on local frontier/finalized changes (new event, head-event-id digest change, finalized commit/prolly-root change).
- Idle keepalive heartbeat at low rate (default `HEARTBEAT_IDLE_INTERVAL = 10s`) with bounded jitter.
- Optional direct probe heartbeat on suspected peer lag.

On heartbeat send:

1. `head_event_ids_digest ← hash(sort(head_event_ids))`.
2. `seen_frontier ← peer_seen_frontier[self]`.
3. Broadcast `{ peer: self, head_event_ids_digest, seen_frontier, finalized_commit_id, finalized_prolly_root_hash }` to all peers.

On receive from peer `q`:

1. `active_peers ← active_peers ∪ {q}`.
2. **Head digest comparison:** Compute `own_head_event_ids_digest ← hash(sort(own head_event_ids))`. If `own_head_event_ids_digest ≠ received.head_event_ids_digest`, request the sender's full head-event-id set.
3. **Seen-frontier refresh:** `peer_seen_frontier[q] ← received.seen_frontier`. The received frontier is authoritative witness metadata for peer `q`; no separate digest/request round is used for it.
4. **Ancestor-closure pull-on-demand:** Pull missing ancestor closure for the union of the remote head-event-id set and the remote seen frontier:
   1. Let `remote_targets = remote_head_event_ids ∪ received.seen_frontier`.
   2. For each `t ∈ remote_targets`, recursively request every ancestor event ID reachable from `t` that is missing locally.
   3. For each fetched event, pull chunks for its `prolly_root_hash` and, if needed, `anchor_prolly_root_hash`.
   Continue until each remote target is parent-closed locally (or no additional ancestors are available from the sender's clock view).
5. **Finalized-state synchronization trigger:** If `received.finalized_commit_id ≠ finalized_commit_id`, trigger `SYNC_FINALIZED(peer, received.finalized_commit_id, received.finalized_prolly_root_hash)`. `SYNC_FINALIZED` handles lineage-based ADOPT/NOOP/MERGE, with `EMPTY_ROOT`/the empty finalized commit id treated as the root of finalized lineage. See §6.5.
6. If step 1 or step 3 changed the witness inputs, call `ADVANCE_STABILITY(peer)`.

#### ADVANCE_STABILITY(peer) → unit

Triggered after **any transition that can enlarge the finalization candidate set**. This includes at minimum:

- changes to any `peer_seen_frontier[...]`
- changes to `active_peers`
- changes to `chunks_ready`
- completion of parent closure
- changes to `parked_conflicts`
- changes to `finalized_commit_id` / `finalized_prolly_root_hash`

Let `CoveredBy(frontier, e)` mean: some `f ∈ frontier` has `e` in its ancestor-or-self closure.

1. `newly_stable = { e ∈ clock | e.event_id ∉ finalized_events.keys() ∧ e.event_id ∉ parked_conflicts ∧ e is PARENTS_READY ∧ e is CHUNKS_READY ∧ ∀ q ∈ active_peers: CoveredBy(peer_seen_frontier[q], e) }`. Parked events cannot be finalized until resolved, and non-ready events (missing ancestors or chunks) cannot be finalized.
2. `stable_frontier = { e ∈ newly_stable | no child of e is in newly_stable }` (the maximal stable events by causality).
3. Sort `stable_frontier` by HLC total order (deterministic).
4. Snapshot previous finalized commit/prolly-root once: `prev_finalized_commit_id = finalized_commit_id`, `prev_finalized_prolly_root_hash = finalized_prolly_root_hash`, `acc = finalized_prolly_root_hash`.
5. Apply `AnchoredFold(stable_frontier, prev_finalized_prolly_root_hash)` (equivalently: for each frontier event `e` in order: `merged = MergeRoots(acc, e.prolly_root_hash, e.anchor_prolly_root_hash)`).
   - **Normative requirement:** this fold MUST be conflict-free. A non-parked stable frontier head is required to be finalization-safe by construction. Encountering a conflict here is a protocol violation, not a third conflict mode.
6. Bookkeeping finalization: for each `e ∈ newly_stable`, set `finalized_events[e.event_id] ← e.prolly_root_hash`. Also union all `resolved_finalized_conflict_id` claims carried by `newly_stable` events into durable `resolved_finalized_conflict_ids`.
7. Materialize deterministic Dolt commits (all fields from event metadata, HLC timestamp, deterministic parents).
8. Replace tentative commits. `finalized_prolly_root_hash ← acc`.
9. `finalized_commit_id ← DeterministicFinalizedCommitID(finalized_prolly_root_hash, finalized_events.keys())`.
10. If `finalized_commit_id ≠ prev_finalized_commit_id`, update finalized commit metadata:
   - `finalized_commit_parents[finalized_commit_id] ← finalized_commit_parents[finalized_commit_id] ∪ {prev_finalized_commit_id}`, excluding self-edges and excluding any proposed parent already in `finalized_commit_id`'s descendant closure under the current `finalized_commit_parents` graph. This preserves acyclic finalized lineage even if a previously-seen content root recurs later.
11. Remove any id now in `resolved_finalized_conflict_ids` from `finalized_conflicts`.

> **Why frontier-only?** Replaying every stable event root can re-apply causal chains in the same batch and create false conflicts. Merging only stable frontier heads applies each stable branch exactly once, while `finalized_events` still records all stabilized events for accounting.

#### GC_CLOCK(peer) → unit

Optional local compaction for finalized history retention.

1. `pruneable = { event_id ∈ finalized_events.keys() ∩ clock.keys() | event_id is not an ancestor of any other retained event in clock }`.
2. For each `event_id ∈ pruneable`:
   - Remove `event_id` from `clock`.
   - Remove `event_id` from `chunks_ready`.
3. Recompute `head_event_ids = computeHeadEventIDs(clock)`.
4. Recompute reconcile projection (`tentative_prolly_root_hash`, `parked_conflicts`, `projection_basis`) from current reconcile inputs (`head_event_ids`, `clock`, `chunks_ready`, `finalized_prolly_root_hash`, `parked_conflicts`, `projection_basis`).

`GC_CLOCK` must preserve correctness: compacting finalized event bodies cannot remove ancestry needed by any retained event. The retained `clock` subgraph must stay parent-closed.

#### EVICT_PEER(peer) → unit

After `HEARTBEAT_TIMEOUT` (10s) with no heartbeat from peer `q`:

1. `active_peers ← active_peers \ {q}`.
2. Keep `peer_seen_frontier[q]` as cached metadata, but it no longer participates in the witness set.
3. Call `ADVANCE_STABILITY` — finalization resumes under the reduced witness set.

No coordination needed. In the modeled semantics, timeout-based false suspicion is treated as semantically equivalent to a temporary partition: history may harden under the reduced witness set, and later recovery is handled by the same fork-accepting `SYNC_FINALIZED` machinery.

#### SYNC_FINALIZED(peer, their_finalized_commit_id, their_finalized_prolly_root_hash) → unit

Triggered when a peer detects a different `finalized_commit_id` from a reconnecting peer (via heartbeat, step 6). `EMPTY_ROOT` / the empty finalized commit id are treated as normal finalized-lineage roots, so `SYNC_FINALIZED` also covers empty-peer adoption/recovery. Every peer independently computes the same deterministic outcome from local+remote finalized metadata — no coordination, no consensus.

1. Exchange finalized metadata with the remote peer:
   - current finalized commit/prolly-root pair (`finalized_commit_id`, `finalized_prolly_root_hash`)
   - full canonical finalized event catalog (`finalized_events: Map[EventID, Hash]`)
   - finalized commit metadata (`finalized_commit_parents`)
   - open finalized-conflict metadata (`finalized_conflicts`)
   - durable finalized conflict resolutions (`resolved_finalized_conflict_ids`)
   Compute merged metadata:
   - `merged_finalized_events = our_finalized_events ∪ their_finalized_events` (map key union)
   - `merged_finalized_commit_parents = our_finalized_commit_parents ∪ their_finalized_commit_parents`
   - `merged_resolved_ids = our_resolved_finalized_conflict_ids ∪ their_resolved_finalized_conflict_ids`
   - `merged_conflicts = (our_finalized_conflicts ∪ their_finalized_conflicts) \ merged_resolved_ids`
2. Classify sync mode from finalized-commit lineage (not event-set subset):
   - **ADOPT** if `our_finalized_commit_id` is an ancestor of `their_finalized_commit_id` in `merged_finalized_commit_parents`.
   - **NOOP** if `their_finalized_commit_id` is an ancestor of `our_finalized_commit_id` in `merged_finalized_commit_parents`.
   - **MERGE** otherwise.
3. Apply branch:
   - **ADOPT**:
     - Fetch chunks for `their_finalized_prolly_root_hash`.
     - `finalized_commit_id ← their_finalized_commit_id`.
     - `finalized_prolly_root_hash ← their_finalized_prolly_root_hash`.
     - `finalized_events ← their_finalized_events`.
     - `finalized_commit_parents ← merged_finalized_commit_parents`.
     - `finalized_conflicts ← merged_conflicts`.
     - `resolved_finalized_conflict_ids ← merged_resolved_ids`.
   - **NOOP**:
     - `finalized_commit_id`, `finalized_prolly_root_hash`, and `finalized_events` unchanged.
     - `finalized_commit_parents ← merged_finalized_commit_parents`.
     - `finalized_conflicts ← merged_conflicts` (monotone unresolved propagation; no silent drops).
     - `resolved_finalized_conflict_ids ← merged_resolved_ids`.
   - **MERGE**:
   1. `common_finalized_commit_id = lineageLCA(our_finalized_commit_id, their_finalized_commit_id, merged_finalized_commit_parents)`.
   2. Fetch chunks for `their_finalized_prolly_root_hash` via SwarmChunkStore/Puller.
   3. Deterministic ours/theirs: compare `our_finalized_commit_id` and `their_finalized_commit_id` lexicographically by finalized-commit identity. Lower finalized commit id = "ours", higher finalized commit id = "theirs".
   4. `result = MergeRoots(content(our_finalized_commit_id), content(their_finalized_commit_id), content(common_finalized_commit_id))`.
   5. Match:
      - `Ok(merged)`: `finalized_prolly_root_hash ← merged`
      - `Conflict(tables, details)`:
        - `finalized_prolly_root_hash ← content(our_finalized_commit_id)` (deterministic)
        - `conflict_id = hash(serialize(our_finalized_commit_id, their_finalized_commit_id, common_finalized_commit_id))`
        - Start from `base_conflicts = merged_conflicts`.
        - If `conflict_id ∉ merged_resolved_ids`, set `finalized_conflicts[conflict_id] ← { id: conflict_id, ours_finalized_commit_id: our_finalized_commit_id, theirs_finalized_commit_id: their_finalized_commit_id, common_finalized_commit_id, tables, details }` on top of `base_conflicts`; otherwise keep `base_conflicts` unchanged.
        - Surface conflict to user (which tables, which rows, `conflict_id`). No blocking — finalization continues immediately.
   6. Merge finalized bookkeeping:
      - `finalized_events ← merged_finalized_events`
      - `resolved_finalized_conflict_ids ← merged_resolved_ids`
   7. `finalized_commit_id ← DeterministicFinalizedCommitID(finalized_prolly_root_hash, finalized_events.keys())`.
   8. Update finalized commit metadata for the resulting finalized commit:
      - `finalized_commit_parents[finalized_commit_id] ← finalized_commit_parents[finalized_commit_id] ∪ ({our_finalized_commit_id, their_finalized_commit_id} \ {finalized_commit_id})`, again excluding any proposed parent already in `finalized_commit_id`'s descendant closure. This preserves acyclic finalized lineage when a merge result lands on a content root already seen in history.
   9. Create a deterministic Dolt merge commit with both finalized tips as parents. All metadata fields are derived deterministically from finalized states with deterministic parent ordering and standard merge metadata.
4. **Retained finalized event bodies must stay ready or be compacted:** After metadata exchange and branch application, if some `event_id ∈ clock.keys()` is now also in `finalized_events.keys()`, the peer MUST ensure that retained finalized event body remains both parent-closed in the retained `clock` subgraph and chunk-ready locally (fetch missing chunks if needed). If either condition cannot be maintained, that finalized event body MUST be compacted out of `clock` immediately. A retained finalized clock event must not remain finalized-but-unready or finalized-but-orphaned locally.
5. **Re-anchor tentative events when finalized commit/prolly-root changes:** Every tentative event carries the `anchor_prolly_root_hash` of the finalized prolly-root hash it was originally written on. If `finalized_commit_id` or `finalized_prolly_root_hash` changed (adopt or merge), replay all non-finalized events over the new accumulator via `RECONCILE`, using each event's recorded `anchor_prolly_root_hash` in `AnchoredFold`. This is CPU-only for already-mergeable head event IDs. If any head event ID is missing ancestors or root/anchor chunks, the projection remains provisional; implementations MUST NOT silently clear unresolved parked head IDs in this state, and MUST recompute on closure completion (ancestor pull completion, chunk completion, or subsequent `RECEIVE_EVENT`/heartbeat-triggered reconcile).

> **Why lineage-LCA common ancestor lookup?** Replaying finalized events to reconstruct `common_root` is unsound after fork-and-join recoveries: finalized event sets contain divergent lineages, and from-scratch replay can overwrite previous merge results. Using finalized commit lineage metadata (`finalized_commit_parents`) gives a deterministic common-ancestor commit/prolly-root pair for arbitrarily nested partition recoveries.

#### PEER_JOIN(new_peer) → unit

State-transfer join: the new peer requests a consistent snapshot from any active peer, then bootstraps local state from it.

1. Join the peer mesh (begin receiving broadcast messages).
2. Request `STATE_SNAPSHOT` from any active peer via point-to-point RPC:
   ```
   STATE_SNAPSHOT = {
     finalized_commit_id: FinalizedCommitID,   -- opaque finalized-history identity
     finalized_prolly_root_hash: Hash,         -- opaque value, NOT recomputed from events
     finalized_events:  Map[EventID, Hash], -- canonical finalized catalog (event_id -> prolly_root_hash)
     finalized_commit_parents: Map[FinalizedCommitID, Set[FinalizedCommitID]],
     clock:             Map[EventID, Event],  -- retained event clock (all non-finalized + unpruned finalized)
     chunks_ready:      Set[EventID],     -- retained CHUNKS_READY flags for announced events
     parked_conflicts:  Set[EventID],     -- currently parked events
     finalized_conflicts: Map[FinalizedConflictID, FinalizedConflictInfo], -- open finalized-layer conflicts
     resolved_finalized_conflict_ids: Set[FinalizedConflictID],
   }
   ```
   `finalized_commit_id` / `finalized_prolly_root_hash` must be transferred as opaque finalized state. `finalized_commit_parents` is transferred with them so `SYNC_FINALIZED` can recover common ancestors via lineage LCA without event replay. `finalized_events` carries finalized event IDs even if their full Event bodies were compacted from `clock`.
3. Fetch chunks for `finalized_prolly_root_hash` and retained event `prolly_root_hash` / `anchor_prolly_root_hash` values via Puller (shallow clone — only chunks not already local). An event is not `CHUNKS_READY` after join until both its content root and its recorded anchor root are locally available.
4. Initialize local state from snapshot:
   - `clock ← snapshot.clock` (union with any events already received via broadcast during join).
   - `head_event_ids ← computeHeadEventIDs(clock)`.
   - `chunks_ready ← snapshot.chunks_ready ∩ clock.keys()`.
   - `finalized_commit_id ← snapshot.finalized_commit_id`.
   - `finalized_prolly_root_hash ← snapshot.finalized_prolly_root_hash`.
   - `finalized_events ← snapshot.finalized_events`.
   - `finalized_commit_parents ← snapshot.finalized_commit_parents`.
   - `parked_conflicts ← snapshot.parked_conflicts`.
   - `finalized_conflicts ← snapshot.finalized_conflicts`.
   - `resolved_finalized_conflict_ids ← snapshot.resolved_finalized_conflict_ids`.
   - `peer_seen_frontier ← { p: ∅ for all p in PEERS }`.
   - `active_peers ← { self }` — grows from heartbeats.
   - `(tentative_prolly_root_hash, parked_conflicts, projection_basis) ← computeReconcileProjection(...)` from heads and `finalized_prolly_root_hash`.
5. Begin heartbeating. First full heartbeat round establishes `peer_seen_frontier` entries for all active peers.
6. Verification: recompute `head_event_ids` from `clock`, verify `parked_conflicts ⊂ clock.keys()`, verify `chunks_ready ⊂ clock.keys()`, verify retained finalized events are parent-closed if kept, and verify lineage metadata contains both the empty finalized commit id and `finalized_commit_id`.

#### PEER_LEAVE(peer) → unit

Graceful: publish leave, removed from `active_peers` immediately. Ungraceful: stops heartbeating, evicted after `HEARTBEAT_TIMEOUT`.

### 2.5 Key Invariants

**INV1a — Logical Event-Identity Convergence:** For any two peers `p, q` that have received the same events, canonical logical identity agrees over `clock.keys() ∪ finalized_events.keys()`.

**INV1b — Retained-Structure Convergence:** For any two peers `p, q`, if retained `clock.keys()` are equal, then retained derived structure agrees (`p.head_event_ids == q.head_event_ids`). This invariant is conditioned on retained state because compacted peers can keep different in-memory event bodies while remaining logically equivalent via `finalized_events`.

**INV1c — Retained Finalized Subgraph Closure:** For any peer `p`, the retained finalized subset `p.clock.keys() ∩ p.finalized_events.keys()` is parent-closed. `GC_CLOCK` can compact finalized event bodies, but any finalized event body that remains in `clock` must retain the finalized ancestry needed by other retained events.

**INV2 — Data Convergence:** For any two peers that have processed the same set of events in the same total order, `p.tentative_prolly_root_hash == q.tentative_prolly_root_hash` and `p.projection_basis == q.projection_basis`.

**INV3 — Finalized History Agreement:** For any two peers with the same canonical `finalized_events` map, the finalized Dolt commit DAG is byte-identical: same hashes, same parents, same roots. During a network partition, peers in different connected components finalize independently under their reduced `active_peers`; their finalized catalogs therefore differ whenever those components finalize different event sets. When the partition heals, `SYNC_FINALIZED` reconciles states (lineage ADOPT/NOOP/MERGE) into a shared fork-and-join DAG that is eventually identical across all peers. This invariant is unconditional — it also covers partition-merge convergence (same inputs → same synced result) because divergence merges handle conflicts deterministically by using the ours-side root.

**INV4 — Causal Consistency:** If `e1` is an ancestor of `e2` in the clock DAG, then `e1` precedes `e2` in the total order.

**INV5 — No Silent Data Loss:** Every event either enters the finalized set or is parked (awaiting human resolution). No event disappears silently. Formerly-parked events whose content is subsumed by a later resolution event are still accounted for in `finalized_events`; finalized-root advancement is driven by stable frontier roots, so subsumed intermediate roots are not re-applied. If finalized event bodies are compacted from `clock`, finalized event IDs remain represented in canonical `finalized_events`.

**INV6 — Witness Frontier Soundness:** `peer_seen_frontier[q]` is always interpreted as an observed causal frontier, not a prediction. Finalization depends only on ancestor coverage by the currently active witness set, never on wall-clock progress. `finalized_commit_id` never moves backward in finalized-commit lineage. When a peer is re-added to `active_peers` after partition recovery with a frontier behind the current witness boundary, further finalization is delayed, but already-finalized history is unchanged.

**INV7 — Conflict Visibility:** All merge conflicts are captured and surfaced — none leak into working or finalized state, none are silently discarded. Nine sub-properties:

- **INV7a — No Unhandled Tentative Conflict:** `tentative_prolly_root_hash` is never a conflict sentinel. Every conflict detected by `MergeRoots` during reconciliation is captured in `parked_conflicts` (the later event by total order is parked). No conflict result propagates into the working state.
- **INV7b — No Unhandled Finalized Conflict:** `finalized_prolly_root_hash` is never a conflict sentinel. `ADVANCE_STABILITY` requires stable non-parked frontier heads to be finalization-safe by construction, and `SYNC_FINALIZED` divergence merges handle conflicts by using the ours-side root (deterministic). No conflict result propagates into the finalized state.
- **INV7c — Parked Event Integrity:** Every parked event references a known event in the clock. No conflict is silently discarded — parked events persist until explicitly resolved via `RESOLVE_CONFLICT`.
- **INV7d — Parking Agreement (closure-ready):** For any two peers with the same clock and the same `finalized_prolly_root_hash`, parking decisions and `projection_basis` agree once relevant heads are mergeable. Under closure lag, parked tracking is conservative: unresolved parked head IDs are retained provisionally until closure completes.
- **INV7e — Finalized Conflict Tracking:** Every finalized-layer conflict detected by `SYNC_FINALIZED` conflict handling is inserted into `finalized_conflicts` and remains present until its id enters durable `resolved_finalized_conflict_ids`.
- **INV7f — Finalized Conflict Authenticity:** Every tracked `finalized_conflicts` entry corresponds to an actual merge conflict for its tuple `(ours_finalized_commit_id, theirs_finalized_commit_id, common_finalized_commit_id)` under `MergeRoots`.
- **INV7g — Finalized Conflict ID Symmetry:** For the same finalized-merge conflict inputs, all peers derive the same deterministic `conflict_id`, regardless of which side initiates `SYNC_FINALIZED`.
- **INV7h — Finalized Conflict No-Resurrection:** Once a finalized conflict id is present in durable `resolved_finalized_conflict_ids`, that id must not remain or reappear in `finalized_conflicts`.
- **INV7i — Unresolved Conflict Monotonicity in Sync:** Across all `SYNC_FINALIZED` branches (ADOPT/NOOP/MERGE), unresolved finalized conflict ids propagate monotonically: `merged_conflicts = (our_conflicts ∪ their_conflicts) \ merged_resolved_ids`.

**INV8 — Head-Event-ID Derivation Consistency:** For every peer `p`, `p.head_event_ids == computeHeadEventIDs(p.clock)`. Head event IDs are a derived view of clock structure, never an independently-authoritative data source.

**INV9 — Finalized/Parked Disjointness:** For every peer `p`, `p.finalized_events.keys() ∩ p.parked_conflicts == ∅`. An event cannot be both parked (unresolved conflict) and finalized.

**INV10 — Reconcile Fixed-Point Consistency:** For every peer `p`, `p.tentative_prolly_root_hash`, `p.parked_conflicts`, and `p.projection_basis` must equal the deterministic reconcile projection of current state (including provisional closure-lag behavior). No action leaves stale reconcile outputs after changing `heads`, `clock`, `chunks_ready`, or finalized commit/content state.

**INV11 — Bounded Heartbeat Closure Catchup (spec regression check):** If peers `p` and `q` are connected and `q`'s head ancestry exists in `q.clock`, then one heartbeat closure pull from `q` plus enough `RECEIVE_EVENT` steps at `p` is sufficient to make those remote heads parent-closed in `p`'s clock view.

### 2.6 Eventual Consistency Assumptions

The eventual-convergence claims in Goal/INV2/INV3 require the following fairness/availability assumptions:

1. Connected peers eventually exchange heartbeats (with retries).
2. Ancestor-closure pull-on-demand eventually completes for reachable heads.
3. Referenced prolly-tree chunks eventually become available from at least one reachable peer.
4. Accepted events and resolution events are eventually delivered/retried across connected peers (no permanent starvation of one sender/receiver pair).

---

## 3. Quint Formal Specification

The formal Quint specification lives in `specs/`. See `specs/doltswarm_verify.qnt` for the model-checkable specification with invariants. The spec is the source of truth for the **core state machine**: event write, receive, reconcile, heartbeat, finalize, finalized-event compaction (`do_gc`), **tentative conflict resolution** (resolve_conflict), **finalized conflict resolution** (resolve_finalized_conflict), **network partitions** (connectivity model, peer eviction), and **partition recovery** (sync_finalized). Partition conflicts from divergence merges in `SYNC_FINALIZED` are handled inline using the ours-side root (no blocking), while conflict metadata is persisted in `finalized_conflicts` until the corresponding conflict id reaches durable finalized-state resolution (`resolved_finalized_conflict_ids`). The spec models chunk availability with an explicit `chunks_ready` abstraction (`do_chunks_ready`). The swarm-wide admission predicate is modeled abstractly as `validEvent(e)`; concrete transport/auth envelopes remain outside the core model. The spec models eviction and reconnection via heartbeat re-adding to `active_peers`. It does not model `PEER_JOIN` state transfer or graceful leave. The spec starts all peers with identical empty state, which subsumes the post-join steady state.

**Modeling notes (spec vs. this document):**

- **Finalization ordering:** `ADVANCE_STABILITY` (§2.4) processes a full batch each time it runs: compute `newly_stable` from active-peer seen-frontier coverage, derive the stable frontier (maximal stable events), sort that frontier by HLC total order, and fold from the current `finalized_prolly_root_hash` accumulator while merging each event against its recorded `anchor_prolly_root_hash`. Stable non-parked frontier heads are required to be finalization-safe by construction. The Quint spec models the same batch semantics via `finalizeBatch`.
- **Finalized-commit lineage metadata:** The protocol treats `finalized_commit_parents` as authoritative lineage state over finalized commits, while `finalized_prolly_root_hash` remains content payload. `do_finalize` computes a new deterministic `finalized_commit_id` from `(finalized_prolly_root_hash, finalized_events.keys())`, updates parents when the finalized commit advances, and `do_sync_finalized` uses lineage-LCA (`lineageLcaFinalizedCommit`) to compute the common finalized commit for divergence merges. This avoids unsound from-scratch replay of finalized events and preserves nested partition recovery correctness (INV3).
- **`mergeRoots` asymmetry:** In real Dolt, `MergeRoots(ours, theirs, ancestor)` is asymmetric — conflict markers reference "ours" vs "theirs". The protocol and Quint spec align on deterministic ours/theirs assignment in `do_sync_finalized`: lower finalized-commit id = "ours". This is fully local and unambiguous after fork-and-join recoveries.
- **EventID representation:** The protocol defines `EventID = hash(CanonicalEncode(EventIDPayload))`, where `EventIDPayload = (prolly_root_hash, anchor_prolly_root_hash, sorted parent_event_ids, hlc, peer, resolved_finalized_conflict_id)`. The core protocol is codec-agnostic: implementations need canonical bytes, not a mandated wire format. The spec uses `EventID = (peer, wall, logical)` — a tuple derived from HLC — to avoid modeling hashing/byte encodings while preserving uniqueness.
- **Event fields and admission:** The spec's `Event` includes `anchor_prolly_root_hash` and `resolved_finalized_conflicts` (a set-model encoding of protocol field `resolved_finalized_conflict_id`) and matches the core protocol event shape. The swarm-wide admission predicate is abstracted as `validEvent(e)`, so the model captures uniform admission semantics without fixing one concrete auth envelope.
- **`parked_conflicts` type:** Authoritative semantics are set-based in both protocol and spec (`Set[EventID]`).
- **`finalized_conflicts` type:** The protocol defines `finalized_conflicts: Map[FinalizedConflictID, FinalizedConflictInfo]` (includes table/detail presentation metadata). The spec models `finalized_conflicts` as a map keyed by the same deterministic conflict ID, but tracks only the structural finalized-commit tuple (`ours_finalized_commit_id`, `theirs_finalized_commit_id`, `common_finalized_commit_id`) needed for state-machine checks.
- **Heartbeat frontier exchange:** The protocol heartbeat carries `head_event_ids_digest`, the full `seen_frontier`, and the finalized commit/prolly-root pair. The spec's `do_heartbeat` reads the sender's node state directly, updates the stored `peer_seen_frontier`, and pulls the same closure target without modeling message serialization.
- **Heartbeat as direct state read:** The protocol broadcasts heartbeat messages carrying `head_event_ids_digest` plus the full seen frontier. Receivers compare only the head-event-id digest; the frontier itself is refreshed directly from the heartbeat. The spec's `do_heartbeat` reads the sender's node state directly (no heartbeat inbox, no digest compare for head event IDs) as a standard model-checking simplification; it does not model heartbeat loss or reordering.
- **Pull-on-demand scope:** The protocol (§2.4 HEARTBEAT steps 2-4) compares `head_event_ids_digest`, requests the full remote head-event-id set only on mismatch, and always uses the received full seen frontier when recursively pulling missing ancestor closure for their union. The spec models the same closure pull target while still treating heartbeat as direct state read and skipping the digest message mechanics.
- **Finalized sync trigger and case split:** The protocol (§2.4 HEARTBEAT step 6) triggers `SYNC_FINALIZED` whenever finalized commit ids differ. The empty finalized commit id is treated as the root of finalized lineage, so an empty peer can ADOPT a non-empty finalized state through the normal sync path. `SYNC_FINALIZED` case-splits by finalized-commit lineage ancestry (ADOPT/NOOP/MERGE), not finalized-event subsets. Conflict maps are merged as `(ours ∪ theirs) \ (our_resolved ∪ their_resolved)` in every branch. The spec mirrors this contract via `syncMode`, `syncAdoptionReady`, `syncEnabled`, and `applySyncFinalized`.
- **SYNC_FINALIZED regression checks in spec:** The Quint model includes branch-specific contract invariants (`inv_sync_contract_adopt`, `inv_sync_contract_noop`, `inv_sync_contract_merge`, `inv_sync_unresolved_conflicts_monotone`, `inv_sync_conflict_recorded`, `inv_sync_conflict_id_symmetric`) and pairwise convergence scenario checks to catch lineage-mode and conflict-propagation regressions early.
- **Finalized-conflict hardening checks in spec:** The Quint model checks finalized-conflict structural integrity (`inv_finalized_conflict_visibility`), authenticity against merge semantics (`inv_finalized_conflict_authenticity`), explicit resolve-action contract behavior (`inv_resolve_finalized_conflict_contract`: emit a finalized-conflict resolution claim without directly mutating finalized history), and anti-resurrection (`inv_finalized_conflict_no_resurrection`).
- **Lineage/LCA regression checks in spec:** The Quint model additionally checks finalized-lineage integrity (`inv_lineage_well_formed`, `inv_lineage_closure`), LCA soundness (`inv_lineage_lca_sound`), sync metadata-union correctness (`inv_sync_metadata_union`), merge-parent edge recording (`inv_sync_merge_parent_edges`), and nested merge composition determinism (`inv_sync_nested_merge_determinism`).
- **Tentative/finalized boundary checks in spec:** The Quint model now checks that tentative-layer actions do not silently mutate finalized history: `LOCAL_WRITE` preserves finalized state (`inv_write_preserves_finalized_state`), `RECONCILE` preserves finalized state (`inv_reconcile_preserves_finalized_state`), and normal `ADVANCE_STABILITY` adds at most the single lineage edge back to the previous `finalized_commit_id` (`inv_finalize_linear_extension_only`). This is the executable version of the protocol rule "merge roots continuously, merge commits only when finalized lineages meet."
- **Tentative conflict-resolution checks in spec:** The Quint model checks the explicit `RESOLVE_CONFLICT` contract (`inv_resolve_conflict_contract`): the emitted event is anchored to the current `finalized_prolly_root_hash`, parents from `projection_basis ∪ {resolved_parked_event}`, removes that parked head from the head/parked set after recomputation, and does not directly mutate finalized state.
- **Witness/frontier hardening checks in spec:** In addition to `inv_self_seen_frontier_exact` and singleton self-coverage, the Quint model now checks witness-set sanity (`inv_self_in_active_peers`), local seen-frontier antichain shape (`inv_self_seen_frontier_antichain`), and coverage of every locally parent-closed event by the self frontier (`inv_self_seen_frontier_covers_parent_closed_clock`). These checks keep the seen-frontier finalization boundary aligned with the protocol's causal witness semantics.
- **Closure/reconcile hardening checks in spec:** The Quint model enforces reconcile fixed-point consistency (`inv_tentative_root_is_compute_projection`, `inv_parked_conflicts_is_compute_projection`, `inv_projection_basis_is_compute_projection`), bounded heartbeat closure catchup (`inv_heartbeat_closure_catchup_bounded`), no redundant closure pull once remote ancestry is already present (`inv_heartbeat_no_redundant_pull_when_closed`), and exact remote-frontier refresh on heartbeat (`inv_heartbeat_refreshes_remote_witness_exactly`). Together these catch regressions where orphan heads, stale witness metadata, or unnecessary anti-entropy drift from the deterministic projection.
- **Non-blocking-progress and rejoin checks in spec:** The Quint model now includes bounded regressions for the protocol's "no peer is blocked" goal: writes remain enabled in the presence of either tentative or finalized conflicts when resource bounds allow (`inv_write_enabled_despite_conflicts`), and re-adding a lagging peer to `active_peers` via heartbeat cannot rewind already-finalized history (`inv_rejoin_does_not_rewind_finalized`).
- **Multi-peer partition-heal regression checks in spec:** Beyond the pairwise sync convergence checks, the Quint model now includes a 3-peer partition-heal regression (`inv_three_peer_partition_heal_converges`) to catch order-dependent finalized-merge behavior when independently hardened histories meet after asymmetric healing.
- **Finalized metadata exchange:** The protocol's `SYNC_FINALIZED` exchanges canonical finalized-event catalogs (`finalized_events` map), authoritative lineage parents (`finalized_commit_parents`), open finalized conflicts (`finalized_conflicts`), and durable resolved conflict ids (`resolved_finalized_conflict_ids`). The spec reads full remote state directly (over-approximation) and then applies the same merge/filter contract before case-split execution.
- **Action decoupling from heartbeat:** The protocol (§2.4 HEARTBEAT steps 5-6) triggers `SYNC_FINALIZED` inline whenever finalized roots differ, and implicitly triggers `ADVANCE_STABILITY` when witness inputs change (`peer_seen_frontier` or `active_peers`). The spec decouples both into standalone nondeterministic actions (`do_sync_finalized`, `do_finalize`) that can fire independently in the `step` relation. The preconditions are equivalent, so the reachable state space is the same — the spec just does not model the causal triggering chain.
- **`localWall` state variable:** The spec adds `localWall: int` to `NodeState`, not present in the protocol's §2.2 per-peer state table. This separates wall-clock progression into explicit `do_tick` actions, giving the model checker control over time advancement. In the protocol, wall time is implicit (read from the system clock).
- **`mergeRoots` abstraction:** The protocol delegates to Dolt's `MergeRoots`, which performs cell-level three-way merge over prolly trees. The spec now models two independent writable dimensions so disjoint concurrent writes auto-merge and only overlapping writes conflict. `CONFLICT_HASH` remains the model sentinel.
- **Event-only inbox:** The spec's `inbox: PeerID → Set[Event]` carries only `Event` messages. Heartbeats use direct state reads (`do_heartbeat`). This follows from the "heartbeat as direct state read" simplification above.
- **Chunk and parent-closure gates:** The protocol defines a per-event lifecycle (ANNOUNCED → PARENTS_READY → CHUNKS_READY → MERGEABLE). `CHUNKS_READY` means both `prolly_root_hash` and `anchor_prolly_root_hash` are locally available for that event. The spec models this with explicit `chunks_ready` state and a bounded `do_chunks_ready` action; reconciliation and finalization both require parent-closure and chunk-readiness for events they fold.
To run the spec: `task quint:run`

---

## 4. What Dolt Provides vs. What We Build

### 4.1 Dolt Provides (No Modification Required)

| Component | API | Used For |
|---|---|---|
| Prolly tree storage | `ChunkStore.{HasMany,GetMany,PutMany}` | Content-addressed data I/O |
| Three-way merge | `MergeRoots(ours, theirs, ancestor)` | DAG-free merge of any 3 RootValues |
| Chunk store I/O | `ChunkStore.{HasMany,GetMany}` | Determine missing chunks, store received chunks |
| Dangling commits | `CommitDanglingWithParentCommits` | Creating commits with arbitrary parents |
| Ghost hashes | `PersistGhostHashes` | References to absent history |
| Schema merge | `SchemaMerge` | Detecting schema conflicts |
| SQL engine | `sqle.Server` | Local read/write via SQL |
| Diff engine | Prolly tree differ | `Diff(ancestor, current)` for op summaries |

### 4.2 What We Build (Layer 3 — doltswarm)

| Component | Storage | Transport |
|---|---|---|
| **Merkle Clock** | In-memory `map[EventID]Event` | Broadcast (Event messages) |
| **HLC Module** | In-memory per peer | Embedded in events |
| **Heartbeat** | In-memory peer tracking | Broadcast (Heartbeat messages) |
| **Total Order** | Pure computation | N/A |
| **Event Admission** | Shared `ValidEvent` contract | Transport/auth envelope outside core `Event` |
| **Chunk Sync** | Calls ChunkStore APIs | Point-to-point streams for chunk requests |
| **Reconciliation Loop** | Calls Dolt Go APIs | N/A (local after chunk sync) |
| **Witness Frontier Tracker** | In-memory `peer_seen_frontier` | Derived from heartbeats |
| **Finalization Engine** | Writes to Dolt Layer 2 | Local only |
| **Conflict Surface** | Dolt conflict tables + metadata | Local SQL |
| **Peer Manager** | In-memory `active_peers` | Derived from heartbeats |

### 4.3 Boundary

```
doltswarm (Go)                              Dolt (Go, unmodified)
──────────────                              ─────────────────────
RECEIVE_EVENT(e)
  │
  ├─► ValidEvent(e) ───────────────► discard if false
  │
  ├─► ChunkSync(e.prolly_root_hash, e.peer) ► walk prolly tree from root
  │     p2p stream to e.peer               ChunkStore.HasMany (missing?)
  │                                        ChunkStore.PutMany (store)
  │
  ├─► MergeRoots(ours, theirs, anc) ──► merge/merge.go
  │     Ok  → tentative_prolly_root_hash = merged
  │     Conflict → parked_conflicts
  │
  ├─► [tentative] CommitDangling ──────► local Dolt commit
  │
  └─► [finalized] CreateDeterministic ─► shared Dolt commit
        deterministic fields from HLC      byte-identical across peers
```

---

## 5. Required Dolt Modifications

### 5.1 Necessary

1. **Public `MergeRootValues` wrapper.** Currently `MergeRoots` is internal to `merge` package:
   ```go
   func (db *DoltDB) MergeRootValues(ctx context.Context,
       ours, theirs, ancestor *RootValue) (*RootValue, map[string]*MergeStats, error)
   ```

2. **Unrelated histories support.** `MergeRoots` with `EMPTY_ROOT` as ancestor must surface same-table-different-schema as a conflict, not an error.

3. **Deterministic commit helper:**
   ```go
   func (db *DoltDB) CreateDeterministicCommit(ctx context.Context,
       root Hash, parents []Hash, meta CommitMeta) (Hash, error)
   ```
   All fields caller-supplied. Identical hash on every peer.

### 5.2 Desirable

4. **Scoped prolly tree walk** — sync specific tables only.
5. **Conflict table event tagging** — annotate conflict rows with `EventID`.
6. **`dolt_finalization_status` system table** — expose stability boundary via SQL.

---

## 6. Edge Cases

### 6.1 Old Events

Old events (e.g., from a peer reconnecting after a partition) are accepted into the clock like any other event. There is no protocol-level rejection mechanism — the Merkle clock is a G-Set (append-only, union-merge). Old events are placed in the correct position by the HLC total order, reconciled via `MergeRoots`, and eventually finalized. If they conflict with newer events, they are parked like any other conflict (§6.2).

**Strict convergence requirement:** Peers MUST NOT apply unilateral wall-time admission windows that discard otherwise-valid events. Local wall clocks can differ; dropping by `|now() - e.hlc.wall|` would violate the Merkle clock G-Set model and break INV1a (logical event-identity convergence). Skew handling is operational: peers log/flag suspicious timestamps and apply transport/identity controls (rate-limit, disconnect, key revocation), but valid events still enter `clock`.

### 6.2 Conflicts (Data or Schema)

All conflicts are resolved deterministically using the HLC total order. When `MergeRoots` detects a conflict between two events, the earlier event (by total order) wins and its root becomes the working state. The later event is "parked": it stays in the clock (it happened), its data is preserved, but it is excluded from the active lineage (`tentative_prolly_root_hash`, `projection_basis`, finalization).

All peers independently compute the same parked set from the same closure-ready view — no coordination needed. Under closure lag, parked tracking is conservative until required ancestors/chunks arrive. No peer is blocked from writing. Parked events surface as conflicts for human resolution via `RESOLVE_CONFLICT`. Finalized-layer conflicts (from partition recovery) are tracked separately in `finalized_conflicts` and resolved via `RESOLVE_FINALIZED_CONFLICT`.

### 6.3 Peer Goes Offline

Heartbeats stop. After `HEARTBEAT_TIMEOUT` (10s), each remaining peer independently evicts. The witness set shrinks, finalization resumes. No coordination. In the protocol semantics, false suspicion is treated the same as temporary partition: finalized history may harden under the reduced witness set, and later repair happens through the same `SYNC_FINALIZED` fork-accept path. When peer returns: `PEER_JOIN` — shallow clone from finalized state, catch up clock, re-enter via heartbeat.

### 6.4 Concurrent Schema Changes

Same pipeline as data conflicts. `SchemaMerge` auto-resolves where possible (e.g., both add different columns). Non-resolvable cases → conflict → user.

### 6.5 Network Partition

Peers in each partition continue operating independently. Each side evicts unreachable peers after `HEARTBEAT_TIMEOUT`, reduces `active_peers`, and finalizes independently under its reduced membership. Both sides' finalized histories diverge.

**During partition:** Each partition operates as a fully functional cluster. Events flow within the partition, seen-frontier coverage advances based on the reduced `active_peers`, and events finalize. Each side's `finalized_commit_id` / `finalized_prolly_root_hash` advances along its own lineage. No data loss — all writes are preserved.

**When partition heals:** Heartbeats resume between previously-separated peers. Two things happen:

1. **Clock merge:** Events from both sides flow via broadcast and heartbeat pull-on-demand with head-to-ancestor closure fetch. Each peer recursively fetches missing ancestors for remote heads until those heads are parent-closed locally, then reconciles when chunk-ready. Each peer's clock grows to include all events from both partitions. Old events from the reconnecting peer are accepted unconditionally — the Merkle clock is a G-Set. Multiple heads emerge (at least one per partition lineage). Tentative `RECONCILE` runs normally against these heads once closure gates are satisfied.

2. **Finalized-state synchronization:** Heartbeats carry the finalized commit/content pair. When a peer receives a heartbeat with a different `finalized_commit_id`, it triggers `SYNC_FINALIZED`. The empty finalized commit id is treated as ordinary lineage, so empty peers recover by the same ADOPT/NOOP/MERGE machinery. Divergence uses a single three-way `MergeRoots(ours, theirs, common_ancestor)` with lineage-LCA common ancestor recovery.

**Fork-and-join topology:** The finalized Dolt commit DAG is no longer strictly linear after partition recovery. It has a fork at the point of partition and a join at the merge commit. Both sides' finalized commits are preserved as history — `dolt log` shows the full picture: pre-split linear chain, two parallel branches during partition, merge commit at reconnection.

**Partition conflicts:** If both sides modified the same rows in independently finalized events (the divergence-merge branch of `SYNC_FINALIZED`), `MergeRoots` returns a `Conflict`. Rather than blocking finalization, the protocol uses the ours-side finalized commit's prolly-root hash (lower finalized-commit id, deterministic) as `finalized_prolly_root_hash`, unions both sides' finalized events, records an entry in `finalized_conflicts`, and continues finalizing immediately. This preserves the "no peer is blocked" design principle while ensuring conflict tracking survives notifications/restarts (via state transfer).

**Re-anchoring:** After `SYNC_FINALIZED` changes `finalized_commit_id` / `finalized_prolly_root_hash` (adopt or merge), all tentative events are re-evaluated against the new finalized content root. Parking decisions and `projection_basis` are recomputed because the ancestor for merge computations changed. This is a CPU-only replay once required chunks are already local.

**Nested partitions:** If a partition splits further (e.g., all three peers isolated), the same logic applies recursively. Reconnection uses lineage-based `SYNC_FINALIZED` modes; merge cases produce deterministic merge commits in the Dolt DAG. The topology becomes more complex but remains well-defined and deterministic. Pairwise divergence merges are computed in deterministic order (by finalized-root hash comparison).

**Determinism:** All peers independently compute the same partition merge result. The "ours" vs "theirs" assignment is deterministic (lower finalized-commit id = "ours"). The merge commit metadata is deterministic (derived from the two finalized states). INV3 holds after convergence.

---

## 7. Design Decisions

These decisions refine the abstract protocol.

### 7.1 Conflict handling: park later event, block nobody

Conflicts detected by `MergeRoots` are handled using the HLC total order to deterministically select which event is "parked." The earlier event in total order wins; the later event is parked. All peers independently compute the same parking decision.

Parked events:
- Stay in the clock (the event happened, it's preserved)
- Are excluded from `LOCAL_WRITE` parents and finalization (reconciliation recomputes parking when closure is ready; during closure lag unresolved parked heads are retained conservatively)
- Are surfaced to the user for resolution via `RESOLVE_CONFLICT`
- Do not block any peer from writing

Resolution is async: any peer (typically the parked event's author) issues `RESOLVE_CONFLICT` with SQL that merges the parked data into the current state. The resolution event's `parent_event_ids` are `projection_basis ∪ {event_id}` so one parked conflict is explicitly resolved per action.

Earlier designs blocked the conflicting peer from `LOCAL_WRITE` until resolution. This was too restrictive — a single conflict could stall a peer indefinitely. An even earlier design used First-Write-Wins (FWW), which auto-resolved by dropping the later commit. FWW causes silent data loss.

### 7.1.1 Finalization of formerly-parked events

When a parked event is resolved via `RESOLVE_CONFLICT`, it leaves `parked_conflicts` (the resolution event references it as a parent, making it no longer a head). The event is then in the clock, not finalized, not parked — eligible for finalization.

A formerly-parked event's `prolly_root_hash` branches from an earlier state — it does not incorporate concurrent changes that caused the parking. Replaying every stable event root can therefore re-introduce the same conflict at finalized layer.

**Fix:** `ADVANCE_STABILITY` merges only stable frontier heads, starting from the current `finalized_prolly_root_hash` accumulator and using each frontier event's recorded `anchor_prolly_root_hash`. Stable non-parked frontier heads are required to be finalization-safe by construction, so a frontier merge conflict here is a protocol violation rather than a recoverable third conflict mode. This is safe because:

1. Events still in `parked_conflicts` are excluded from finalization candidates by step 1.
2. Resolved histories are represented by later frontier roots that subsume earlier conflicting branches.
3. Causal ancestors inside the stable set are accounted for in `finalized_events` but not re-applied as roots, avoiding duplicate application and false conflicts.

This approach is receipt-order independent: frontier selection and HLC ordering are pure functions of `(clock, chunks_ready, peer_seen_frontier, active_peers, finalized_events, parked_conflicts, finalized_commit_id, finalized_prolly_root_hash)`.

### 7.2 Event identity and admission: swarm-uniform

Every core event identity uses the canonical abstract payload:
- `EventIDPayload = (prolly_root_hash, anchor_prolly_root_hash, sorted parent_event_ids, hlc, peer, resolved_finalized_conflict_id)`

`EventID = hash(CanonicalEncode(EventIDPayload))`.
`CanonicalEncode` is codec-agnostic and must be deterministic/injective with canonical ordering and Optional encoding.

Admission is defined by the swarm-wide predicate `ValidEvent(e)`, not by per-peer optional checks. Every peer in a swarm MUST apply the same predicate. `ValidEvent(e)` MUST reject malformed payloads, `EventID`/payload mismatches, and any failure of the swarm's chosen authenticity/integrity checks. Any authentication mechanism lives outside the core `Event` and feeds only into `ValidEvent(e)`. What matters normatively is that admission is uniform within one swarm.

### 7.3 No epoch grouping

Events are processed individually via `MergeRoots`. There is no batching into wall-time epochs. Each event is replayed against its recorded `anchor_prolly_root_hash` over the current accumulator. This is simpler than epoch-based batching and avoids epoch boundary edge cases.

### 7.3.1 Merges are silent local state

Merges (RECONCILE) are deterministic local computations derived entirely from the Merkle clock and `finalized_prolly_root_hash`. They produce no events, no network traffic, and no DAG entries. Every peer holding the same event set independently computes the same `tentative_prolly_root_hash` and `projection_basis`.

Only three protocol actions produce broadcast messages:
- **LOCAL_WRITE** — new data (the event's `parent_event_ids = projection_basis` naturally reflects the tentative projection it was written on).
- **RESOLVE_CONFLICT** / **RESOLVE_FINALIZED_CONFLICT** — user resolution SQL, both flow through LOCAL_WRITE.
- **HEARTBEAT** — adaptive event-driven/keepalive, carries `head_event_ids_digest` + full `seen_frontier` + finalized commit/content metadata.

Head event IDs (`computeHeadEventIDs(clock)`) are always derived from the clock structure, never stored independently. After reconciliation, `head_event_ids` remain multi-valued until the next LOCAL_WRITE collapses them.

### 7.4 Data plane chunk-sync contract

Normative contract:

1. Chunk transfer is point-to-point and content-addressed.
2. Event receipt triggers chunk sync for the announced `prolly_root_hash`.
3. Reconcile/finalization operations must only use events whose required chunks are locally available.
4. Missing chunks are fetched from any reachable peer. The protocol does not distinguish the origin peer from any other reachable provider.

Any implementation that satisfies this contract is protocol-compliant. Concrete API wiring is outside this document and is maintained in [`doltswarm-implementation.md`](./doltswarm-implementation.md).

### 7.5 In-memory Layer 3 state

All Merkle clock state (events, heads, peer tracking) is held in-memory. There is no local persistence for Layer 3. On crash, a peer rejoins from a live peer via `PEER_JOIN` (shallow clone of finalized state + retained clock events). This requires at least one live peer for recovery.

Because Layer 3 is in-memory, a locally-created event that has not yet been replicated is crash-volatile. Dolt may still contain local tentative commits or prolly chunks, but without the Layer 3 event fact the protocol does not treat that write as durable shared state.

`GC_CLOCK` bounds memory: finalized event bodies can be pruned from `clock` once no retained event depends on them, while finalized event-id accounting remains in canonical `finalized_events`.

### 7.6 Seen-frontier heartbeats

Anti-entropy is handled by adaptive heartbeats (event-driven + low-rate idle keepalive, carrying `{ peer, head_event_ids_digest, seen_frontier, finalized_commit_id, finalized_prolly_root_hash }`). Heartbeats keep one compact digest for head event IDs: `head_event_ids_digest = hash(sort(head_event_ids))`. They carry the full local seen frontier on every send: `seen_frontier = peer_seen_frontier[self]`. On receive, the receiver updates `peer_seen_frontier[q]` directly from the heartbeat, compares only `head_event_ids_digest`, requests the sender's full head-event-id set on mismatch, and then recursively pulls missing ancestor closure for `remote_head_event_ids ∪ received.seen_frontier` plus required root and anchor chunks. This removes the stale-frontier ambiguity from the digest-only design while keeping full-head-event-id transfer on-demand. The cost is slightly larger heartbeat control traffic; the benefit is simpler witness and anti-entropy semantics.

### 7.7 No pull-first optimization

The `LOCAL_WRITE` path does not include a pre-write sync pass. Peers write immediately and let `RECONCILE` handle convergence. This simplifies the write path at the cost of potentially more merge operations.

### 7.8 Pluggable transport

The protocol assumes a direct mesh (every peer can reach every other peer) but does not depend on any specific networking library. The transport provides broadcast delivery for the control plane and point-to-point streams for the data plane.

### 7.9 Core package adaptation plan

Moved to [`doltswarm-implementation.md`](./doltswarm-implementation.md) to keep this document focused on normative protocol semantics.

### 7.10 Partition recovery: accept the fork

When a network partition separates peers into independent groups, each group evicts unreachable peers, reduces `active_peers`, and finalizes independently. The two sides' `finalized_commit_id` / `finalized_prolly_root_hash` values diverge. On reconnection, the protocol must reconcile these divergent finalized histories.

**Decision: accept the fork.** Both sides' finalized histories are treated as valid. The divergent finalized roots are merged via a single `MergeRoots` call to produce a new shared finalized base. The Dolt commit DAG gets a fork-and-join topology — a merge commit with both finalized tips as parents. No finalization is rolled back. No data is lost.

**Why not "re-tentativize"?** The alternative — treating one side's post-split finalized events as tentative again — violates INV6 (Stability Monotonicity) and requires unwinding finalization, which is complex and breaks the user expectation that finalized data is settled. Accept-the-fork is simpler and preserves all invariants with minor scoping adjustments.

**Why not "last-write-wins"?** Auto-resolving the partition merge by discarding one side's finalized data causes silent data loss — unacceptable for finalized content.

**Finalized-layer conflicts use the ours-side finalized commit's prolly-root hash and are explicitly tracked.** During normal operation, conflicts only occur at the tentative layer (parked events). Partition recovery introduces finalized-layer conflicts when `SYNC_FINALIZED` takes its divergence-merge path over incompatible finalized writes. Rather than blocking finalization, the protocol handles these conflicts inline: the ours-side finalized commit's prolly-root hash (lower finalized-commit id, deterministic) becomes `finalized_prolly_root_hash`, both sides' finalized events are unioned, and a deterministic `finalized_conflicts` entry is recorded. Users resolve these entries via `RESOLVE_FINALIZED_CONFLICT`, which emits a normal write event carrying `resolved_finalized_conflict_id`. The conflict is cleared durably only once that claim hardens into finalized state (or is learned through finalized-state sync via `resolved_finalized_conflict_ids`). This preserves the "no peer is blocked" design principle while preventing finalized conflicts from being silently forgotten.

**Finalized common-ancestor lookup uses finalized-commit lineage, not event replay.** `SYNC_FINALIZED` computes `common_finalized_commit_id` via lineage LCA over authoritative finalized commit metadata (`finalized_commit_parents`), not by replaying `finalized_events`. This preserves correctness under nested partition recoveries, where replay over unioned finalized events is unsound. General finalized-root advancement still uses anchored frontier folding in `ADVANCE_STABILITY`: start from the current `finalized_prolly_root_hash`, merge stable frontier heads via `mergeRoots(acc, head.prolly_root_hash, head.anchor_prolly_root_hash)`, and mark all newly stable events finalized for bookkeeping.

**Deterministic merge commit.** The partition merge commit must be byte-identical across all peers for INV3. This requires: (1) deterministic ours/theirs assignment (lower finalized-commit id = ours), (2) deterministic metadata (derived from finalized states), (3) deterministic parent ordering. Dolt's `CreateDeterministicCommit` provides this.


## 8. Implementation Notes

Repository/package structure, public Go API, and integration test planning are maintained in [`doltswarm-implementation.md`](./doltswarm-implementation.md).
