# Doltswarm: P2P Sync Protocol over Dolt

## 0. Motivation and Research Background

### Goal

Design a peer-to-peer synchronization protocol for Dolt databases that allows multiple peers to concurrently read and write SQL data, automatically merge non-conflicting changes, surface conflicts to users for resolution, and converge on a shared finalized history — all without a central coordinator, with minimal inter-peer communication, and with no consensus protocol.

### The real boundary problem: when does local reconciliation harden into shared history?

The hard problem in doltswarm is not "when do we merge data?" Dolt already provides a three-way merge over `RootValue`s. The harder problem is: **when does a reversible local reconciliation become irreversible shared history?**

That boundary is what separates two different kinds of merge:

- **Tentative reconciliation:** multiple Merkle-clock heads are allowed, local `RECONCILE` computes a current projection, and tentative Dolt commits are treated as rewritable convenience state.
- **History-level merge:** two already-finalized lineages later meet after a partition or long disconnect, so the shared finalized Dolt DAG must record a real merge commit instead of pretending one side can be rewritten away.

This leads to the design rule that motivates the whole protocol:

**merge roots continuously, merge commits only when finalized lineages meet.**

For connected, still-settling concurrency, we do not want to emit a Dolt merge commit for every concurrent write. That would collapse back to "just let every peer make commits and merge the commit DAG all the time", which is exactly what causes merge storms, repeated re-parenting, and unstable commit identity. The Merkle clock exists to keep that churn below the history boundary.

For partition-grade divergence, the story changes. Once each side has already hardened some history for its current witness set, rewinding finalized history would violate monotonicity. At that point a real merge in the finalized Dolt DAG is the correct representation: two independently hardened histories met and had to be joined.

### The problem with Dolt's commit model in a P2P setting

Dolt stores data in content-addressed prolly trees (deterministic B-trees where the same dataset always produces the same root hash). This is excellent for sync — identical data produces identical chunks regardless of which peer produced it. However, Dolt layers a Git-style commit DAG on top of this data layer. Each commit's hash is computed from its serialized flatbuffer, which includes `parent_addrs` — the 20-byte hashes of parent commits. Commit identity therefore depends on position in history, not just on table state.

In a P2P setting, that creates the core tension. If peers create commits independently and later try to stitch them into one shared history, every rebase or parent reassignment changes commit hashes and cascades through descendants. Any external reference to the old commit hash becomes stale. Git solves this by centralizing ordering at a server or maintainer. Doltswarm cannot rely on that.

So the protocol must do two things at once:

- avoid treating every concurrent write as a commit-DAG merge problem
- still provide a common, meaningful finalized Dolt history once some set of changes is no longer considered rewritable

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

The lesson is not only "use a Merkle clock." The deeper lesson is: **keep causal structure and data state separate.** The clock answers *what happened and what is concurrent*; the data layer answers *what state results when those changes are merged*. Fireproof can stop there because its clock/data model is already the system of record. Doltswarm cannot stop there, because it also owes users a common, stable Dolt commit DAG.

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

The protocol does not modify Dolt's merge engine. Instead, it introduces a Merkle clock layer that sits between prolly-tree data and the local commit DAG:

- while history is still tentative, the Merkle clock is the authoritative shared structure
- `RECONCILE` is local and silent: peers continuously merge roots without emitting commit-DAG merge artifacts
- tentative Dolt commits are projections, not globally stable history
- once the witness condition for finalization holds, some portion of that tentative arrangement hardens into the shared finalized Dolt DAG
- only if two already-finalized lineages later meet do we emit a real Dolt merge commit

This is what makes doltswarm different from the other systems above. DefraDB, OrbitDB, and Fireproof can stop at the converged clock/log/DAG level. Doltswarm cannot, because it must periodically compile that causal structure into a common, meaningful, user-visible Dolt history.

The result is a protocol with a sharper boundary:

- **below the boundary:** live concurrency, multiple heads, local reconciliation, no commit-DAG merges
- **at the boundary:** a witness condition decides that some tentative arrangement may harden into shared history
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

**Layer 2 (Dolt, local-only):** Each peer creates Dolt commits locally after reconciliation. Commits have peer-specific hashes (parents, timestamps differ). Split into a **finalized zone** (deterministic, identical across peers) and a **tentative zone** (local convenience, rewritten on finalization).

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

CIDPayload = (
  root_hash: Hash,
  anchor_root_hash: Hash,
  parents_sorted: List[EventCID],         -- parents sorted by EventCID bytes (lexicographic)
  hlc: HLC,
  peer: PeerID,
  resolved_finalized_conflict_id: Optional[FinalizedConflictID]
)

CanonicalEncode(v) = deterministic, injective byte encoding for protocol values.
                     -- This protocol does NOT mandate a concrete wire codec.
                     -- It mandates properties: fixed field order, canonical set/list ordering,
                     -- and canonical Optional encoding (present/absent is unambiguous).

EventCID   = hash(CanonicalEncode(CIDPayload))

Event = {
  cid:            EventCID,               -- redundant on wire (receivers recompute), useful for quick dedup
  root_hash:      Hash,                   -- RootValue hash after this peer's local write
  anchor_root_hash: Hash,                 -- finalized root against which this event's root_hash was produced
  resolved_finalized_conflict_id: Optional[FinalizedConflictID], -- null unless this event resolves a finalized conflict
  parents:        Set[EventCID],          -- causal parents in the Merkle clock
  hlc:            HLC,
  peer:           PeerID,
}

MergeResult =
  | Ok     { merged_root: Hash }
  | Conflict { tables: Set[string], details: string }

FinalizedConflictID = Hash
                    -- hash(serialize(ours_root, theirs_root, common_root))

FinalizedConflictInfo = {
  id:         FinalizedConflictID,
  ours_root:  Hash,
  theirs_root: Hash,
  common_root: Hash,
  tables:     Set[string],
  details:    string,
}

computeHeads(clock) = {
  cid ∈ clock.keys() | ¬∃ e ∈ clock.values(): cid ∈ e.parents
}

```

`parents_sorted` is the canonical deterministic ordering of `Event.parents` used only for CID canonicalization. It carries no additional causal semantics.

The core protocol state machine is defined over `Event`. Any transport/authentication envelope around `Event` is outside the core protocol and influences admission only through `ValidEvent(e)`.

### 2.2 Per-Peer State

All Layer 3 state is in-memory. Persistence comes from Dolt's Layer 1/2. On crash recovery, a peer rejoins from a live peer via `PEER_JOIN` (explicit state transfer of finalized state + retained clock events), not from message replay.

| Variable | Type | Description |
|---|---|---|
| `clock` | `Map[EventCID, Event]` | Locally retained Merkle clock events: all non-finalized events plus any retained finalized events not yet compacted by `GC_CLOCK`. The retained finalized subset `clock.keys() ∩ finalized_events.keys()` MUST stay parent-closed. |
| `heads` | `Set[EventCID]` | Clock heads (events with no children locally) |
| `chunks_ready` | `Set[EventCID]` | Announced events whose `root_hash` and `anchor_root_hash` chunks are locally available (`CHUNKS_READY`). |
| `latest_root` | `Hash` | Current merged RootValue |
| `hlc` | `HLC` | Local hybrid logical clock used only for deterministic event ordering and tie-breaking |
| `peer_seen_frontier` | `Map[PeerID, Set[EventCID]]` | Last known seen frontier per peer. A seen frontier is the maximal set of locally parent-closed events that peer has observed. `peer_seen_frontier[self]` MUST equal the locally derived seen frontier. |
| `finalized_root` | `Hash` | RootValue at the stability boundary |
| `finalized_events` | `Map[EventCID, Hash]` | Canonical finalized event catalog (`cid -> root_hash`). Set semantics are `finalized_events.keys()` |
| `finalized_parents` | `Map[Hash, Set[Hash]]` | Authoritative finalized checkpoint DAG parent links (`child_root -> parent_roots`) used for lineage ancestry/LCA |
| `parked_conflicts` | `Set[EventCID]` | Authoritative set of parked events (later in total order, conflicted at merge). Excluded from LOCAL_WRITE parents and finalization. During closure lag (some heads not mergeable yet), unresolved parked head IDs are retained conservatively until closure completes and reconciliation recomputes against a complete mergeable set. |
| `finalized_conflicts` | `Map[FinalizedConflictID, FinalizedConflictInfo]` | Open finalized-layer conflicts detected in `SYNC_FINALIZED` divergence merges. Cleared by local `RESOLVE_FINALIZED_CONFLICT` or by receiving an event carrying `resolved_finalized_conflict_id` for that conflict. |
| `active_peers` | `Set[PeerID]` | Peers seen in the last heartbeat window; this is the current witness set for finalization |

Retained finalized event bodies are a special case inside the mixed `clock` map: if retained, they MUST form a parent-closed subgraph. This is preserved by `GC_CLOCK` and by the retained-finalized sync rules in `SYNC_FINALIZED`.

### 2.2.1 Tentative State vs. Finalized State

The protocol has two explicit state zones:

- **Finalized state (monotone shared ground truth):** `finalized_root`, `finalized_events`, and `finalized_parents`. This state advances monotonically and is the shared checkpoint layer reconciled by `ADVANCE_STABILITY` and `SYNC_FINALIZED`.
- **Tentative state (revisable local projection):** `clock`, `heads`, `chunks_ready`, `latest_root`, and `parked_conflicts`. This is a deterministic projection from the currently-known frontier relative to the current finalized anchor, and it may be revised when new events/ancestors/chunks arrive or when the finalized anchor changes.

Conflict handling is also split:
- **Tentative-layer conflicts:** represented by `parked_conflicts`; non-blocking and resolved via `RESOLVE_CONFLICT`.
- **Finalized-layer conflicts:** represented by `finalized_conflicts`; non-blocking and resolved via `RESOLVE_FINALIZED_CONFLICT`.

### 2.3 Communication Model

The protocol assumes a **direct mesh** network: every peer can send messages to every other peer. Communication is via a **pluggable transport** abstraction. The protocol is transport-agnostic — the transport must provide broadcast delivery and point-to-point streams, but the protocol does not depend on any specific networking library.

**Control plane (broadcast)** — two message types, broadcast to all peers:

| Message Type | Payload | Rate |
|---|---|---|
| `Event` | See §2.1 | On local write |
| `Heartbeat` | `{ peer: PeerID, heads_digest: Hash, seen_frontier: Set[EventCID], finalized_root: Hash }` | Adaptive (event-driven + idle keepalive) |

**Data plane (chunk sync)** — prolly tree data is synced via **point-to-point streams** between specific peers, triggered by event receipt. When a peer receives an event, it requests the prolly tree chunks needed to evaluate that event: the chunks for `event.root_hash`, and the chunks for `event.anchor_root_hash` if they are not already local. The sync walks each prolly tree from its root hash, requesting only chunks not already present in the local `ChunkStore`. Point-to-point, not broadcast — only the peer that needs chunks requests them. Content-addressed deduplication via Dolt's SHA-512/20 hashing ensures structural sharing across events.

**No voting, no consensus, no coordinator.** There is no agreement mechanism in the protocol. Convergence follows from set union (the Merkle clock is a G-Set) and deterministic merge (same inputs → same output on every peer).

### 2.3.1 Event Identity and Admission

Core event identity is:
- `cid = hash(CanonicalEncode(CIDPayload))`, where `CIDPayload` includes `root_hash`, `anchor_root_hash`, canonical parent list, `hlc`, `peer`, and `resolved_finalized_conflict_id`.
- `CanonicalEncode(...)` MUST be deterministic, injective, and canonical over field order, set/list ordering, and Optional encoding.
- On the wire, `cid` is redundant (receivers recompute it) but useful for quick deduplication before full deserialization.

The normative admission rule is a swarm-wide predicate `ValidEvent(e)`.

- Every peer in one swarm MUST apply the same `ValidEvent(e)` predicate to all received events.
- `ValidEvent(e)` MUST be deterministic from the event payload plus shared swarm configuration.
- `ValidEvent(e)` MUST reject malformed events, CID/payload mismatches, and any event that fails the swarm's configured authenticity/integrity checks.
- Events for which `ValidEvent(e) = false` are **silently discarded** (not added to the clock, not forwarded).

How a deployment implements `ValidEvent(e)` is outside the core state machine. Authentication material, if any, is carried outside the core `Event` and MUST NOT change the logical event fields or `EventCID`.

What is forbidden is per-peer discretion. One peer MUST NOT accept an event that another compliant peer in the same swarm would reject under the shared `ValidEvent(e)` definition.

### 2.4 Protocol Actions

#### LOCAL_WRITE(peer, sql_ops) → Event

No conflict precondition — writes are never blocked.

1. Apply `sql_ops` to local Dolt instance → new `RootValue` with hash `r`.
2. `hlc ← tick(hlc)`.
3. `active_heads = heads \ parked_conflicts`.
4. Build `cid_payload = (r, finalized_root, sort(active_heads), hlc, peer, null)`.
5. `cid = hash(CanonicalEncode(cid_payload))`.
6. `e = Event { cid, root_hash: r, anchor_root_hash: finalized_root, resolved_finalized_conflict_id: null, parents: active_heads, hlc, peer }`.
7. `clock[e.cid] ← e`.
8. `chunks_ready ← chunks_ready ∪ {e.cid}` (local writes are immediately chunk-ready).
9. Recompute `heads` from clock. Note: `computeHeads` may still return parked CIDs as heads (since no event has them as parents). That's correct — they persist as forks until resolved.
10. Recompute local seen frontier: `peer_seen_frontier[self] ← SeenFrontier(clock)`.
11. If step 10 changed the local witness input, call `ADVANCE_STABILITY(peer)`.
12. If `|heads| > 1`: call `RECONCILE(peer)` — recomputes `latest_root` and `parked_conflicts` from the new head set.
13. If `|heads| = 1`: `latest_root ← r`.
14. Create tentative local Dolt commit.
15. Publish `e` to all peers.

#### RECEIVE_EVENT(peer, e) → Accept | Discard

Events follow a per-event lifecycle: **ANNOUNCED → PARENTS_READY → CHUNKS_READY → MERGEABLE**.

- **ANNOUNCED**: Event is in the clock, heads are recomputed, but its parent/ancestor closure may be incomplete locally and chunks for `e.root_hash` and/or `e.anchor_root_hash` may not yet be local. The event participates in causal ordering and head computation, but NOT in `RECONCILE` or finalization.
- **PARENTS_READY**: All ancestor CIDs reachable from `e.parents` are present in the local clock.
- **CHUNKS_READY**: Chunk sync for both `e.root_hash` and `e.anchor_root_hash` is complete.
- **MERGEABLE**: Event is both **PARENTS_READY** and **CHUNKS_READY**, and `RECONCILE` has processed it (either merged or parked).

Steps:

1. If `e.cid ∈ clock ∨ e.cid ∈ finalized_events.keys()`: discard (idempotent).
2. **Admission check:** Evaluate the swarm-wide predicate `ValidEvent(e)`. If `ValidEvent(e) = false`, discard silently.
3. **Strict admission rule:** If an event is valid and new, it MUST be accepted into `clock`. Peers MUST NOT discard based on local wall-time checks (e.g., `|now() - e.hlc.wall|`).
4. `hlc ← merge(hlc, e.hlc)`.
5. `clock[e.cid] ← e`. Event is now **ANNOUNCED**.
6. If `e.resolved_finalized_conflict_id` is present, remove that id from `finalized_conflicts` (idempotent; missing key is a no-op).
7. Recompute `heads` from clock: `heads = { cid ∈ clock | cid is not a parent of any event in clock }`.
8. Recompute local seen frontier: `peer_seen_frontier[self] ← SeenFrontier(clock)`. This frontier contains only locally **PARENTS_READY** events.
9. If step 8 changed the local witness input, call `ADVANCE_STABILITY(peer)`.
10. Request prolly tree chunks for `e.root_hash` and, if needed, for `e.anchor_root_hash` from the originating peer (or any peer that already has them) via point-to-point stream.
11. On completion of all required chunk sync for step 10: `chunks_ready ← chunks_ready ∪ {e.cid}` (event becomes **CHUNKS_READY**).
12. Parent-closure readiness is tracked independently: an event becomes **PARENTS_READY** only when all ancestor CIDs reachable from its parent links are present in the local `clock` (typically via heartbeat pull-on-demand).
13. If `|heads| = 1` and the lone head is both **PARENTS_READY** and **CHUNKS_READY**, recompute `latest_root` and `parked_conflicts` using the same singleton anchored-fold semantics as `RECONCILE` (equivalently: `MergeRoots(finalized_root, head.root_hash, head.anchor_root_hash)` with the same conflict/parking contract).
14. Call `RECONCILE(peer)` (if `|heads| > 1`). `RECONCILE` operates only on heads that are **PARENTS_READY** and **CHUNKS_READY** (or already **MERGEABLE**). Heads missing ancestors or chunks are skipped until a later reconciliation pass.

#### RECONCILE(peer) → unit

**Merges are deterministic local computations, not new information.** If peers A and B both have events `{e1, e2}` with two heads, they each independently call `MergeRoots` and arrive at the same `latest_root`. There is nothing to communicate — the merge result is fully determined by the inputs that all peers already have. No event is created, nothing is broadcast.

**Reconcile has an explicit closure-lag contract.** Reconcile operates on ALL heads (not pre-filtered by existing `parked_conflicts`). When all heads are mergeable, parking is recomputed deterministically from anchored fold semantics. When closure is incomplete (some heads are not yet mergeable), projection is provisional and unresolved parked head IDs are retained conservatively until closure completion.

`AnchoredFold(heads, start_root) -> (root, conflict_set)` is the shared merge primitive used by both `RECONCILE` and `ADVANCE_STABILITY`: sort heads by HLC, start `acc = start_root`, then fold `MergeRoots(acc, head.root_hash, head.anchor_root_hash)` so each event is replayed against the finalized anchor it was originally written on.

1. If `heads = ∅`: set `latest_root ← finalized_root`, set `parked_conflicts ← ∅`, return.
2. `mergeable_heads = { h ∈ heads | h is PARENTS_READY ∧ h is CHUNKS_READY }`.
3. `unready_heads = heads \ mergeable_heads`.
4. If `mergeable_heads = ∅`: no merge is possible yet. Keep `latest_root` unchanged and conservatively retain unresolved parked heads: `parked_conflicts ← parked_conflicts ∩ heads`. Return.
5. Sort `mergeable_heads` by HLC total order (deterministic).
6. Apply `AnchoredFold(mergeable_heads, finalized_root)` to compute `(merged_root, parked_ready)`:
   - This single fold contract is used for both single-head and multi-head reconciliation (no semantic single-head bypass).
   - On each conflicting fold step, the later head in total order is parked.
7. `retained_unready_parked = parked_conflicts ∩ unready_heads`.
8. `parked_conflicts ← parked_ready ∪ retained_unready_parked`.
9. `latest_root ← merged_root`.
10. **`heads` are unchanged.** Heads are always derived from `computeHeads(clock)` — since reconcile does not add events to the clock, the head set cannot change. Parked heads remain as persistent DAG forks until `RESOLVE_CONFLICT`.

> **Why no merge events?** Broadcasting merges would be pure redundancy — every peer holding the same events computes the same merged state. Worse, if merge events were broadcast, concurrent merges by different peers would create new DAG forks requiring further reconciliation (an infinite merge loop). Only three things produce network traffic: `LOCAL_WRITE` (new data), conflict-resolution actions (`RESOLVE_CONFLICT` and `RESOLVE_FINALIZED_CONFLICT`, both flowing through `LOCAL_WRITE`), and `HEARTBEAT` (which naturally reflects the reduced head set after merge).

#### RESOLVE_CONFLICT(peer, event_cid, resolution_ops) → unit

User (typically the author of the parked event) provides `resolution_ops` (SQL that fixes conflicting rows/schema). Resolution is async — no peer is blocked waiting for it.

1. Apply `resolution_ops` to local Dolt → `resolved_root`.
2. Remove `event_cid` from `parked_conflicts`.
3. Create new `Event` with `resolved_root`, `resolve_parents = (heads \ parked_conflicts) ∪ {event_cid}`, and `resolved_finalized_conflict_id = null`.
4. Normal `LOCAL_WRITE` flow from step 4 (same CID construction, with `resolved_finalized_conflict_id = null`).

#### RESOLVE_FINALIZED_CONFLICT(peer, conflict_id, resolution_ops) → unit

User resolves a tracked finalized-layer conflict (created by `SYNC_FINALIZED` conflict handling) without blocking finalization.

1. Precondition: `conflict_id ∈ finalized_conflicts`.
2. Apply `resolution_ops` to local Dolt → `resolved_root`.
3. Create new `Event` with `resolved_root`, parents = `active_heads` (`heads \ parked_conflicts`), and `resolved_finalized_conflict_id = conflict_id`.
4. Normal `LOCAL_WRITE` flow from step 4 (same CID construction, but with `resolved_finalized_conflict_id = conflict_id` from step 3).
5. On successful event creation/publication, remove `conflict_id` from `finalized_conflicts` (explicit resolution bookkeeping).
6. `RESOLVE_FINALIZED_CONFLICT` MUST NOT directly mutate `finalized_root` or `finalized_events`; those evolve only via existing finalization/sync paths.

#### HEARTBEAT(peer) → unit

Adaptive policy:
- Event-driven heartbeat on local frontier/finalized changes (new event, head digest change, finalized-root change).
- Idle keepalive heartbeat at low rate (default `HEARTBEAT_IDLE_INTERVAL = 10s`) with bounded jitter.
- Optional direct probe heartbeat on suspected peer lag.

On heartbeat send:

1. `heads_digest ← hash(sort(heads))`.
2. `seen_frontier ← peer_seen_frontier[self]`.
3. Broadcast `{ peer: self, heads_digest, seen_frontier, finalized_root }` to all peers.

On receive from peer `q`:

1. `active_peers ← active_peers ∪ {q}`.
2. **Head digest comparison:** Compute `own_heads_digest ← hash(sort(own heads))`. If `own_heads_digest ≠ received.heads_digest`, request the sender's full head set.
3. **Seen-frontier refresh:** `peer_seen_frontier[q] ← received.seen_frontier`. The received frontier is authoritative witness metadata for peer `q`; no separate digest/request round is used for it.
4. **Ancestor-closure pull-on-demand:** Pull missing ancestor closure for the union of the remote head set and the remote seen frontier:
   1. Let `remote_targets = remote_heads ∪ received.seen_frontier`.
   2. For each `t ∈ remote_targets`, recursively request every ancestor CID reachable from `t` that is missing locally.
   3. For each fetched event, pull chunks for its `root_hash`.
   Continue until each remote target is parent-closed locally (or no additional ancestors are available from the sender's clock view).
5. **Finalized-state synchronization trigger:** If `received.finalized_root ≠ finalized_root`, trigger `SYNC_FINALIZED(peer, received.finalized_root)`. `SYNC_FINALIZED` handles lineage-based ADOPT/NOOP/MERGE, with `EMPTY_ROOT` treated as the root of finalized lineage. See §6.5.
6. If step 1 or step 3 changed the witness inputs, call `ADVANCE_STABILITY(peer)`.

#### ADVANCE_STABILITY(peer) → unit

Triggered when `peer_seen_frontier[self]` changes, when a heartbeat updates any `peer_seen_frontier[q]`, or when `active_peers` changes.

Let `CoveredBy(frontier, e)` mean: some `f ∈ frontier` has `e` in its ancestor-or-self closure.

1. `newly_stable = { e ∈ clock | e.cid ∉ finalized_events.keys() ∧ e.cid ∉ parked_conflicts ∧ e is PARENTS_READY ∧ e is CHUNKS_READY ∧ ∀ q ∈ active_peers: CoveredBy(peer_seen_frontier[q], e) }`. Parked events cannot be finalized until resolved, and non-ready events (missing ancestors or chunks) cannot be finalized.
2. `stable_frontier = { e ∈ newly_stable | no child of e is in newly_stable }` (the maximal stable events by causality).
3. Sort `stable_frontier` by HLC total order (deterministic).
4. Snapshot previous finalized root once: `prev_finalized_root = finalized_root`, `acc = finalized_root`.
5. Apply `AnchoredFold(stable_frontier, prev_finalized_root)` (equivalently: for each frontier event `e` in order: `merged = MergeRoots(acc, e.root_hash, e.anchor_root_hash)`).
   - If `merged` is `Ok`: `acc ← merged`.
   - If `merged` is `Conflict`: skip `e`'s root for finalized-root advancement (do not update `acc`), but continue the batch.
6. Bookkeeping finalization: for each `e ∈ newly_stable`, set `finalized_events[e.cid] ← e.root_hash`.
7. Materialize deterministic Dolt commits (all fields from event metadata, HLC timestamp, deterministic parents).
8. Replace tentative commits. `finalized_root ← acc`.
9. If `acc ≠ prev_finalized_root`, update finalized checkpoint metadata:
   - `finalized_parents[acc] ← finalized_parents[acc] ∪ {prev_finalized_root}`, excluding self-edges and excluding any proposed parent already in `acc`'s descendant closure under the current `finalized_parents` graph. This preserves acyclic finalized lineage even if a previously-seen root hash reappears later.

> **Why frontier-only?** Replaying every stable event root can re-apply causal chains in the same batch and create false conflicts (e.g., parent then child over the same row). Merging only stable frontier heads applies each stable branch exactly once, while `finalized_events` still records all stabilized events for accounting.

#### GC_CLOCK(peer) → unit

Optional local compaction for finalized history retention.

1. `pruneable = { cid ∈ finalized_events.keys() ∩ clock.keys() | cid is not an ancestor of any other retained event in clock }`.
2. For each `cid ∈ pruneable`:
   - Remove `cid` from `clock`.
   - Remove `cid` from `chunks_ready`.
3. Recompute `heads = computeHeads(clock)`.
4. Recompute reconcile projection (`latest_root`, `parked_conflicts`) from current reconcile inputs (`heads`, `clock`, `chunks_ready`, `finalized_root`, `parked_conflicts`).

`GC_CLOCK` must preserve correctness: compacting finalized event bodies cannot remove ancestry needed by any retained event. The retained `clock` subgraph must stay parent-closed.

#### EVICT_PEER(peer) → unit

After `HEARTBEAT_TIMEOUT` (10s) with no heartbeat from peer `q`:

1. `active_peers ← active_peers \ {q}`.
2. Keep `peer_seen_frontier[q]` as cached metadata, but it no longer participates in the witness set.
3. Call `ADVANCE_STABILITY` — finalization resumes under the reduced witness set.

No coordination needed. Each peer makes the same eviction decision independently based on the same timeout.

#### SYNC_FINALIZED(peer, their_finalized_root) → unit

Triggered when a peer detects a different `finalized_root` from a reconnecting peer (via heartbeat, step 6). `EMPTY_ROOT` is treated as a normal finalized-lineage root, so `SYNC_FINALIZED` also covers empty-peer adoption/recovery. Every peer independently computes the same deterministic outcome from local+remote finalized metadata — no coordination, no consensus.

1. Exchange finalized metadata with the remote peer:
   - full canonical finalized event catalog (`finalized_events: Map[EventCID, Hash]`)
   - finalized checkpoint metadata (`finalized_parents`)
   - open finalized-conflict metadata (`finalized_conflicts`)
   - finalized conflict-resolution references (`resolved_finalized_conflict_id` values observed in known events)
   Compute merged metadata:
   - `merged_finalized_events = our_finalized_events ∪ their_finalized_events` (map key union)
   - `merged_finalized_parents = our_finalized_parents ∪ their_finalized_parents` (set union by root key)
   - `known_resolved_ids = observed resolution refs`
   - `merged_conflicts = (our_finalized_conflicts ∪ their_finalized_conflicts) \ known_resolved_ids`
2. Classify sync mode from finalized-root lineage (not event-set subset):
   - **ADOPT** if `our_finalized_root` is an ancestor of `their_finalized_root` in `merged_finalized_parents`.
   - **NOOP** if `their_finalized_root` is an ancestor of `our_finalized_root` in `merged_finalized_parents`.
   - **MERGE** otherwise.
3. Apply branch:
   - **ADOPT**:
     - Fetch chunks for `their_finalized_root`.
     - `finalized_root ← their_finalized_root`.
     - `finalized_events ← their_finalized_events`.
     - `finalized_parents ← merged_finalized_parents`.
     - `finalized_conflicts ← merged_conflicts`.
   - **NOOP**:
     - `finalized_root` and `finalized_events` unchanged.
     - `finalized_parents ← merged_finalized_parents`.
     - `finalized_conflicts ← merged_conflicts` (monotone unresolved propagation; no silent drops).
   - **MERGE**:
   1. `common_root = lineageLCA(our_finalized_root, their_finalized_root, merged_finalized_parents)`.
   2. Fetch chunks for `their_finalized_root` via SwarmChunkStore/Puller.
   3. Deterministic ours/theirs: compare `our_finalized_root` and `their_finalized_root` lexicographically by raw hash bytes. Lower hash = "ours", higher hash = "theirs".
   4. `result = MergeRoots(ours_finalized_root, theirs_finalized_root, common_root)`.
   5. Match:
      - `Ok(merged)`: `finalized_root ← merged`
      - `Conflict(tables, details)`:
        - `finalized_root ← ours_finalized_root` (deterministic)
        - `conflict_id = hash(serialize(ours_finalized_root, theirs_finalized_root, common_root))`
        - Start from `base_conflicts = merged_conflicts`.
        - If `conflict_id ∉ known_resolved_ids`, set `finalized_conflicts[conflict_id] ← { id: conflict_id, ours_root: ours_finalized_root, theirs_root: theirs_finalized_root, common_root, tables, details }` on top of `base_conflicts`; otherwise keep `base_conflicts` unchanged.
        - Surface conflict to user (which tables, which rows, `conflict_id`). No blocking — finalization continues immediately.
   6. Merge finalized bookkeeping:
      - `finalized_events ← merged_finalized_events`
      - `finalized_parents ← merged_finalized_parents`
   7. Update finalized checkpoint metadata for the resulting root:
      - `finalized_parents[finalized_root] ← finalized_parents[finalized_root] ∪ ({our_finalized_root, their_finalized_root} \ {finalized_root})`, again excluding any proposed parent already in `finalized_root`'s descendant closure. This preserves acyclic finalized lineage when a merge result lands on a root hash already known in the lineage map.
      (This also handles the conflict path where `finalized_root = ours_finalized_root`: self-edge is excluded and the other side is recorded as an additional parent.)
   8. Create a deterministic Dolt merge commit with both finalized tips as parents. All metadata fields are derived deterministically from finalized states with deterministic parent ordering and standard merge metadata.
4. **Retained finalized event bodies must stay ready or be compacted:** After metadata exchange and branch application, if some `cid ∈ clock.keys()` is now also in `finalized_events.keys()`, the peer MUST ensure that retained finalized event body remains both parent-closed in the retained `clock` subgraph and chunk-ready locally (fetch missing chunks if needed). If either condition cannot be maintained, that finalized event body MUST be compacted out of `clock` immediately. A retained finalized clock event must not remain finalized-but-unready or finalized-but-orphaned locally.
5. **Re-anchor tentative events when finalized root changes:** Every tentative event carries the `anchor_root_hash` of the finalized root it was originally written on. If `finalized_root` changed (adopt or merge), replay all non-finalized events over the new accumulator via `RECONCILE`, using each event's recorded `anchor_root_hash` in `AnchoredFold`. This is CPU-only for already-mergeable heads. If some head is still missing ancestors or root/anchor chunks, this projection is explicitly provisional; implementations MUST NOT silently clear unresolved parked head IDs in this state, and MUST recompute on closure completion (ancestor pull completion, chunk completion, or subsequent `RECEIVE_EVENT`/heartbeat-triggered reconcile).

> **Why lineage-LCA common ancestor lookup?** Replaying finalized events to reconstruct `common_root` is unsound after fork-and-join recoveries: finalized event sets contain divergent lineages, and from-scratch replay can overwrite previous merge results. Using finalized checkpoint lineage metadata (`finalized_parents`) gives a deterministic common-ancestor root for arbitrarily nested partition recoveries.

#### PEER_JOIN(new_peer) → unit

State-transfer join: the new peer requests a consistent snapshot from any active peer, then bootstraps local state from it.

1. Join the peer mesh (begin receiving broadcast messages).
2. Request `STATE_SNAPSHOT` from any active peer via point-to-point RPC:
   ```
   STATE_SNAPSHOT = {
     finalized_root:    Hash,              -- opaque value, NOT recomputed from events
     finalized_events:  Map[EventCID, Hash], -- canonical finalized catalog (cid -> root_hash)
     finalized_parents: Map[Hash, Set[Hash]],
     clock:             Map[EventCID, Event],  -- retained event clock (all non-finalized + unpruned finalized)
     chunks_ready:      Set[EventCID],     -- retained CHUNKS_READY flags for announced events
     parked_conflicts:  Set[EventCID],     -- currently parked events
     finalized_conflicts: Map[FinalizedConflictID, FinalizedConflictInfo], -- open finalized-layer conflicts
   }
   ```
   `finalized_root` must be transferred as an opaque value. `finalized_parents` is transferred with it so `SYNC_FINALIZED` can recover common ancestors via lineage LCA without event replay. `finalized_events` carries finalized CIDs even if their full Event bodies were compacted from `clock`.
3. Fetch chunks for `finalized_root` and retained event `root_hash` values via Puller (shallow clone — only chunks not already local).
4. Initialize local state from snapshot:
   - `clock ← snapshot.clock` (union with any events already received via broadcast during join).
   - `heads ← computeHeads(clock)`.
   - `chunks_ready ← snapshot.chunks_ready ∩ clock.keys()`.
   - `finalized_root ← snapshot.finalized_root`.
   - `finalized_events ← snapshot.finalized_events`.
   - `finalized_parents ← snapshot.finalized_parents`.
   - `parked_conflicts ← snapshot.parked_conflicts`.
   - `finalized_conflicts ← snapshot.finalized_conflicts`.
   - `peer_seen_frontier ← { p: ∅ for all p in PEERS }`.
   - `active_peers ← { self }` — grows from heartbeats.
   - `latest_root ← computeMergedRoot(...)` from heads and `finalized_root`.
5. Begin heartbeating. First full heartbeat round establishes `peer_seen_frontier` entries for all active peers.
6. Verification: recompute `heads` from `clock`, verify `parked_conflicts ⊂ clock.keys()`, verify `chunks_ready ⊂ clock.keys()`, and verify lineage metadata contains both `EMPTY_ROOT` and `finalized_root`.

#### PEER_LEAVE(peer) → unit

Graceful: publish leave, removed from `active_peers` immediately. Ungraceful: stops heartbeating, evicted after `HEARTBEAT_TIMEOUT`.

### 2.5 Key Invariants

**INV1a — Logical Event-Identity Convergence:** For any two peers `p, q` that have received the same events, canonical logical identity agrees over `clock.keys() ∪ finalized_events.keys()`.

**INV1b — Retained-Structure Convergence:** For any two peers `p, q`, if retained `clock.keys()` are equal, then retained derived structure agrees (`p.heads == q.heads`). This is intentionally conditioned on retained state, because compacted peers may keep different in-memory event bodies while remaining logically equivalent via `finalized_events`.

**INV1c — Retained Finalized Subgraph Closure:** For any peer `p`, the retained finalized subset `p.clock.keys() ∩ p.finalized_events.keys()` is parent-closed. A peer may compact finalized event bodies, but any finalized event body that remains in `clock` must retain the finalized ancestry needed by other retained events.

**INV2 — Data Convergence:** For any two peers that have processed the same set of events in the same total order, `p.latest_root == q.latest_root`.

**INV3 — Finalized History Agreement:** For any two peers with the same canonical `finalized_events` map, the finalized Dolt commit DAG is byte-identical: same hashes, same parents, same roots. During a network partition, peers may have different finalized catalogs (each side finalizes independently under its reduced `active_peers`). When the partition heals, `SYNC_FINALIZED` reconciles states (lineage ADOPT/NOOP/MERGE) into a shared fork-and-join DAG that is eventually identical across all peers. This invariant is unconditional — it also covers partition-merge convergence (same inputs → same synced result) because divergence merges handle conflicts deterministically by using the ours-side root.

**INV4 — Causal Consistency:** If `e1` is an ancestor of `e2` in the clock DAG, then `e1` precedes `e2` in the total order.

**INV5 — No Silent Data Loss:** Every event either enters the finalized set or is parked (awaiting human resolution). No event disappears silently. Formerly-parked events whose content is subsumed by a later resolution event are still accounted for in `finalized_events`; finalized-root advancement is driven by stable frontier roots, so subsumed intermediate roots are not re-applied. If finalized event bodies are compacted from `clock`, finalized CIDs remain represented in canonical `finalized_events`.

**INV6 — Witness Frontier Soundness:** `peer_seen_frontier[q]` is always interpreted as an observed causal frontier, not a prediction. Finalization depends only on ancestor coverage by the currently active witness set, never on wall-clock progress. `finalized_root` never moves backward (the partition merge point is strictly ahead of both sides' previous roots). When a peer is re-added to `active_peers` after partition recovery, its frontier may be behind the current witness boundary; this may delay further finalization, but it never invalidates already-finalized history.

**INV7 — Conflict Visibility:** All merge conflicts are captured and surfaced — none leak into working or finalized state, none are silently discarded. Nine sub-properties:

- **INV7a — No Unhandled Tentative Conflict:** `latest_root` is never a conflict sentinel. Every conflict detected by `MergeRoots` during reconciliation is captured in `parked_conflicts` (the later event by total order is parked). No conflict result propagates into the working state.
- **INV7b — No Unhandled Finalized Conflict:** `finalized_root` is never a conflict sentinel. `ADVANCE_STABILITY` skips conflicting stable-frontier roots during the anchored frontier fold (§7.1.1), and `SYNC_FINALIZED` divergence merges handle conflicts by using the ours-side root (deterministic). No conflict result propagates into the finalized state.
- **INV7c — Parked Event Integrity:** Every parked event references a known event in the clock. No conflict is silently discarded — parked events persist until explicitly resolved via `RESOLVE_CONFLICT`.
- **INV7d — Parking Agreement (closure-ready):** For any two peers with the same clock and the same `finalized_root`, parking decisions agree once relevant heads are mergeable. Under closure lag, parked tracking is conservative: unresolved parked head IDs may be retained provisionally until closure completes.
- **INV7e — Finalized Conflict Tracking:** Every finalized-layer conflict detected by `SYNC_FINALIZED` conflict handling is inserted into `finalized_conflicts` and remains present until resolved either locally (`RESOLVE_FINALIZED_CONFLICT`) or via receipt of an event carrying `resolved_finalized_conflict_id` for that conflict.
- **INV7f — Finalized Conflict Authenticity:** Every tracked `finalized_conflicts` entry corresponds to an actual merge conflict for its tuple `(ours_root, theirs_root, common_root)` under `MergeRoots`.
- **INV7g — Finalized Conflict ID Symmetry:** For the same finalized-merge conflict inputs, all peers derive the same deterministic `conflict_id`, regardless of which side initiates `SYNC_FINALIZED`.
- **INV7h — Finalized Conflict No-Resurrection:** Once a peer has observed a resolution reference for a finalized conflict id, that id must not remain or reappear in `finalized_conflicts`.
- **INV7i — Unresolved Conflict Monotonicity in Sync:** Across all `SYNC_FINALIZED` branches (ADOPT/NOOP/MERGE), unresolved finalized conflict ids propagate monotonically: `merged_conflicts = (our_conflicts ∪ their_conflicts) \ known_resolved_ids`.

**INV8 — Heads Derivation Consistency:** For every peer `p`, `p.heads == computeHeads(p.clock)`. Heads are a derived view of clock structure, never an independently-authoritative data source.

**INV9 — Finalized/Parked Disjointness:** For every peer `p`, `p.finalized_events.keys() ∩ p.parked_conflicts == ∅`. An event cannot be both parked (unresolved conflict) and finalized.

**INV10 — Reconcile Fixed-Point Consistency:** For every peer `p`, `p.latest_root` and `p.parked_conflicts` must equal the deterministic `computeMergedRoot(p)` projection of current state (including provisional closure-lag behavior). No action may leave stale reconcile outputs after changing `heads`, `clock`, or `finalized_root`.

**INV11 — Bounded Heartbeat Closure Catchup (spec regression check):** If peers `p` and `q` are connected and `q`'s head ancestry exists in `q.clock`, then one heartbeat closure pull from `q` plus enough `RECEIVE_EVENT` steps at `p` is sufficient to make those remote heads parent-closed in `p`'s clock view.

### 2.6 Eventual Consistency Assumptions

The eventual-convergence claims in Goal/INV2/INV3 require the following fairness/availability assumptions:

1. Connected peers eventually exchange heartbeats (with retries).
2. Ancestor-closure pull-on-demand eventually completes for reachable heads.
3. Referenced prolly-tree chunks eventually become available from at least one reachable peer.
4. Accepted events and resolution events are eventually delivered/retried across connected peers (no permanent starvation of one sender/receiver pair).

---

## 3. Quint Formal Specification

The formal Quint specification lives in `specs/`. See `specs/doltswarm_verify.qnt` for the model-checkable specification with invariants. The spec is the source of truth for the **core state machine**: event write, receive, reconcile, heartbeat, finalize, finalized-event compaction (`do_gc`), **tentative conflict resolution** (resolve_conflict), **finalized conflict resolution** (resolve_finalized_conflict), **network partitions** (connectivity model, peer eviction), and **partition recovery** (sync_finalized). Partition conflicts from divergence merges in `SYNC_FINALIZED` are handled inline using the ours-side root (no blocking), while conflict metadata is persisted in `finalized_conflicts` until cleared by a local resolution action or a received resolution-reference event. The spec models chunk availability with an explicit `chunks_ready` abstraction (`do_chunks_ready`). The swarm-wide admission predicate is modeled abstractly as `validEvent(e)`; concrete transport/auth envelopes remain outside the core model. Membership actions (join, leave) are partially modeled: eviction and reconnection (via heartbeat re-adding to `active_peers`) are in the spec; `PEER_JOIN` state-transfer (§2.4) and graceful leave are document-only. The spec starts all peers with identical empty state, which subsumes the post-join steady state. The state-transfer snapshot mechanism is an implementation concern that does not affect core safety properties (the invariants hold regardless of how a peer acquires its initial state, as long as the snapshot is consistent).

**Modeling notes (spec vs. this document):**

- **Finalization ordering:** `ADVANCE_STABILITY` (§2.4) processes a full batch each time it runs: compute `newly_stable` from active-peer seen-frontier coverage, derive the stable frontier (maximal stable events), sort that frontier by HLC total order, and fold from the current `finalized_root` accumulator while merging each event against its recorded `anchor_root_hash`. The Quint spec models the same batch semantics via `finalizeBatch`.
- **Finalized checkpoint lineage metadata:** The protocol treats `finalized_parents` as authoritative lineage state over finalized roots. `do_finalize` updates parents when `finalized_root` advances, and `do_sync_finalized` uses lineage-LCA (`lineageLcaRoot`) to compute `common_root` for divergence merges. This avoids unsound from-scratch replay of finalized events and preserves nested partition recovery correctness (INV3).
- **`mergeRoots` asymmetry:** In real Dolt, `MergeRoots(ours, theirs, ancestor)` is asymmetric — conflict markers reference "ours" vs "theirs". The protocol and Quint spec align on deterministic ours/theirs assignment in `do_sync_finalized`: lower finalized-root hash value = "ours". This is fully local and unambiguous after fork-and-join recoveries.
- **EventCID representation:** The protocol defines `EventCID = hash(CanonicalEncode(CIDPayload))`, where `CIDPayload = (root_hash, anchor_root_hash, sorted parents, hlc, peer, resolved_finalized_conflict_id)`. The core protocol is codec-agnostic: implementations need canonical bytes, not a mandated wire format. The spec uses `EventCID = (peer, wall, logical)` — a tuple derived from HLC — to avoid modeling hashing/byte encodings while preserving uniqueness.
- **Event fields and admission:** The spec's `Event` includes `anchor_root_hash` and `resolved_finalized_conflicts` (a set model of optional `resolved_finalized_conflict_id`) and matches the core protocol event shape. The swarm-wide admission predicate is abstracted as `validEvent(e)`, so the model captures uniform admission semantics without fixing one concrete auth envelope.
- **`parked_conflicts` type:** Authoritative semantics are set-based in both protocol and spec (`Set[EventCID]`). Optional local detail maps are implementation-local only.
- **`finalized_conflicts` type:** The protocol defines `finalized_conflicts: Map[FinalizedConflictID, FinalizedConflictInfo]` (includes table/detail presentation metadata). The spec models `finalized_conflicts` as a map keyed by the same deterministic conflict ID, but tracks only structural roots (`ours_root`, `theirs_root`, `common_root`) needed for state-machine checks.
- **Heartbeat frontier exchange:** The protocol heartbeat carries `heads_digest`, the full `seen_frontier`, and `finalized_root`. The spec's `do_heartbeat` reads the sender's node state directly, updates the stored `peer_seen_frontier`, and pulls the same closure target without modeling message serialization.
- **Heartbeat as direct state read:** The protocol broadcasts heartbeat messages carrying `heads_digest` plus the full seen frontier. Receivers compare only the head digest; the frontier itself is refreshed directly from the heartbeat. The spec's `do_heartbeat` reads the sender's node state directly (no heartbeat inbox, no digest compare for heads) as a standard model-checking simplification; it does not model heartbeat loss or reordering.
- **Pull-on-demand scope:** The protocol (§2.4 HEARTBEAT steps 2-4) compares `heads_digest`, requests the full remote head set only on mismatch, and always uses the received full seen frontier when recursively pulling missing ancestor closure for their union. The spec models the same closure pull target while still treating heartbeat as direct state read and skipping the head-digest message mechanics.
- **Finalized sync trigger and case split:** The protocol (§2.4 HEARTBEAT step 6) triggers `SYNC_FINALIZED` whenever finalized roots differ. `EMPTY_ROOT` is treated as the root of finalized lineage, so an empty peer can ADOPT a non-empty finalized state through the normal sync path. `SYNC_FINALIZED` case-splits by finalized-root lineage ancestry (ADOPT/NOOP/MERGE), not finalized-event subsets. Conflict maps are merged as `(ours ∪ theirs) \ known_resolved_ids` in every branch. The spec mirrors this contract via `syncMode`, `syncAdoptionReady`, `syncEnabled`, `applySyncFinalized`, and `resolvedFinalizedConflictRefs`.
- **SYNC_FINALIZED regression checks in spec:** The Quint model includes branch-specific contract invariants (`inv_sync_contract_adopt`, `inv_sync_contract_noop`, `inv_sync_contract_merge`, `inv_sync_unresolved_conflicts_monotone`, `inv_sync_conflict_recorded`, `inv_sync_conflict_id_symmetric`) and pairwise convergence scenario checks to catch lineage-mode and conflict-propagation regressions early.
- **Finalized-conflict hardening checks in spec:** The Quint model checks finalized-conflict structural integrity (`inv_finalized_conflict_visibility`), authenticity against merge semantics (`inv_finalized_conflict_authenticity`), explicit resolve-action contract behavior (`inv_resolve_finalized_conflict_contract`: remove exactly one conflict id, no direct finalized-root/events mutation, emitted event carries conflict reference), and anti-resurrection (`inv_finalized_conflict_no_resurrection`).
- **Lineage/LCA regression checks in spec:** The Quint model additionally checks finalized-lineage integrity (`inv_lineage_well_formed`, `inv_lineage_closure`), LCA soundness (`inv_lineage_lca_sound`), sync metadata-union correctness (`inv_sync_metadata_union`), merge-parent edge recording (`inv_sync_merge_parent_edges`), and nested merge composition determinism (`inv_sync_nested_merge_determinism`).
- **Tentative/finalized boundary checks in spec:** The Quint model now checks that tentative-layer actions do not silently mutate finalized history: `LOCAL_WRITE` preserves finalized state (`inv_write_preserves_finalized_state`), `RECONCILE` preserves finalized state (`inv_reconcile_preserves_finalized_state`), and normal `ADVANCE_STABILITY` adds at most the single lineage edge back to the previous `finalized_root` (`inv_finalize_linear_extension_only`). This is the executable version of the protocol rule "merge roots continuously, merge commits only when finalized lineages meet."
- **Tentative conflict-resolution checks in spec:** The Quint model checks the explicit `RESOLVE_CONFLICT` contract (`inv_resolve_conflict_contract`): the emitted event is anchored to the current `finalized_root`, references the resolved parked head as a parent, removes that parked head from the head/parked set after recomputation, and does not directly mutate finalized state.
- **Witness/frontier hardening checks in spec:** In addition to `inv_self_seen_frontier_exact` and singleton self-coverage, the Quint model now checks witness-set sanity (`inv_self_in_active_peers`), local seen-frontier antichain shape (`inv_self_seen_frontier_antichain`), and coverage of every locally parent-closed event by the self frontier (`inv_self_seen_frontier_covers_parent_closed_clock`). These checks keep the seen-frontier finalization boundary aligned with the protocol's causal witness semantics.
- **Closure/reconcile hardening checks in spec:** The Quint model enforces reconcile fixed-point consistency (`inv_latest_root_is_compute_merged_root`, `inv_parked_conflicts_is_compute_merged_root`), bounded heartbeat closure catchup (`inv_heartbeat_closure_catchup_bounded`), no redundant closure pull once remote ancestry is already present (`inv_heartbeat_no_redundant_pull_when_closed`), and exact remote-frontier refresh on heartbeat (`inv_heartbeat_refreshes_remote_witness_exactly`). Together these catch regressions where orphan heads, stale witness metadata, or unnecessary anti-entropy drift from the deterministic projection.
- **Non-blocking-progress and rejoin checks in spec:** The Quint model now includes bounded regressions for the protocol's "no peer is blocked" goal: writes remain enabled in the presence of either tentative or finalized conflicts when resource bounds allow (`inv_write_enabled_despite_conflicts`), and re-adding a lagging peer to `active_peers` via heartbeat cannot rewind already-finalized history (`inv_rejoin_does_not_rewind_finalized`).
- **Multi-peer partition-heal regression checks in spec:** Beyond the pairwise sync convergence checks, the Quint model now includes a 3-peer partition-heal regression (`inv_three_peer_partition_heal_converges`) to catch order-dependent finalized-merge behavior when independently hardened histories meet after asymmetric healing.
- **Finalized metadata exchange:** The protocol's `SYNC_FINALIZED` exchanges canonical finalized-event catalogs (`finalized_events` map), authoritative lineage parents (`finalized_parents`), open finalized conflicts (`finalized_conflicts`), and known conflict-resolution references (`resolved_finalized_conflict_id`). The spec reads full remote state directly (over-approximation), derives resolution references from clocks, and then applies the same merge/filter contract before case-split execution.
- **Action decoupling from heartbeat:** The protocol (§2.4 HEARTBEAT steps 5-6) triggers `SYNC_FINALIZED` inline whenever finalized roots differ, and implicitly triggers `ADVANCE_STABILITY` when witness inputs change (`peer_seen_frontier` or `active_peers`). The spec decouples both into standalone nondeterministic actions (`do_sync_finalized`, `do_finalize`) that can fire independently in the `step` relation. The preconditions are equivalent, so the reachable state space is the same — the spec just does not model the causal triggering chain.
- **`localWall` state variable:** The spec adds `localWall: int` to `NodeState`, not present in the protocol's §2.2 per-peer state table. This separates wall-clock progression into explicit `do_tick` actions, giving the model checker control over time advancement. In the protocol, wall time is implicit (read from the system clock).
- **`mergeRoots` abstraction:** The protocol delegates to Dolt's `MergeRoots`, which performs cell-level three-way merge over prolly trees. The spec now models two independent writable dimensions so disjoint concurrent writes auto-merge and only overlapping writes conflict. `CONFLICT_HASH` remains the model sentinel.
- **Event-only inbox:** The spec's `inbox: PeerID → Set[Event]` carries only `Event` messages. Heartbeats use direct state reads (`do_heartbeat`). This follows from the "heartbeat as direct state read" simplification above.
- **Chunk and parent-closure gates:** The protocol defines a per-event lifecycle (ANNOUNCED → PARENTS_READY → CHUNKS_READY → MERGEABLE). `CHUNKS_READY` means both `root_hash` and `anchor_root_hash` are locally available for that event. The spec models this with explicit `chunks_ready` state and a bounded `do_chunks_ready` action; reconciliation and finalization both require parent-closure and chunk-readiness for events they fold.
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
| **Merkle Clock** | In-memory `map[EventCID]Event` | Broadcast (Event messages) |
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
  ├─► ChunkSync(e.root_hash, e.peer) ► walk prolly tree from root
  │     p2p stream to e.peer               ChunkStore.HasMany (missing?)
  │                                        ChunkStore.PutMany (store)
  │
  ├─► MergeRoots(ours, theirs, anc) ──► merge/merge.go
  │     Ok  → latest_root = merged
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
5. **Conflict table event tagging** — annotate conflict rows with `EventCID`.
6. **`dolt_finalization_status` system table** — expose stability boundary via SQL.

---

## 6. Edge Cases

### 6.1 Old Events

Old events (e.g., from a peer reconnecting after a partition) are accepted into the clock like any other event. There is no protocol-level rejection mechanism — the Merkle clock is a G-Set (append-only, union-merge). Old events are placed in the correct position by the HLC total order, reconciled via `MergeRoots`, and eventually finalized. If they conflict with newer events, they are parked like any other conflict (§6.2).

**Strict convergence requirement:** Peers MUST NOT apply unilateral wall-time admission windows that discard otherwise-valid events. Local wall clocks can differ; dropping by `|now() - e.hlc.wall|` would violate the Merkle clock G-Set model and break INV1a (logical event-identity convergence). Skew handling is operational: peers may log/flag suspicious timestamps and apply transport/identity controls (rate-limit, disconnect, key revocation), but valid events still enter `clock`.

### 6.2 Conflicts (Data or Schema)

All conflicts are resolved deterministically using the HLC total order. When `MergeRoots` detects a conflict between two events, the earlier event (by total order) wins and its root becomes the working state. The later event is "parked": it stays in the clock (it happened), its data is preserved, but it is excluded from the active lineage (`latest_root`, `LOCAL_WRITE` parents, finalization).

All peers independently compute the same parked set from the same closure-ready view — no coordination needed. Under closure lag, parked tracking is conservative until required ancestors/chunks arrive. No peer is blocked from writing. Parked events surface as conflicts for human resolution via `RESOLVE_CONFLICT`. Finalized-layer conflicts (from partition recovery) are tracked separately in `finalized_conflicts` and resolved via `RESOLVE_FINALIZED_CONFLICT`.

### 6.3 Peer Goes Offline

Heartbeats stop. After `HEARTBEAT_TIMEOUT` (10s), each remaining peer independently evicts. The witness set shrinks, finalization resumes. No coordination. When peer returns: `PEER_JOIN` — shallow clone from finalized state, catch up clock, re-enter via heartbeat.

### 6.4 Concurrent Schema Changes

Same pipeline as data conflicts. `SchemaMerge` auto-resolves where possible (e.g., both add different columns). Non-resolvable cases → conflict → user.

### 6.5 Network Partition

Peers in each partition continue operating independently. Each side evicts unreachable peers after `HEARTBEAT_TIMEOUT`, reduces `active_peers`, and finalizes independently under its reduced membership. Both sides' finalized histories diverge.

**During partition:** Each partition operates as a fully functional cluster. Events flow within the partition, seen-frontier coverage advances based on the reduced `active_peers`, and events finalize. Each side's `finalized_root` advances along its own lineage. No data loss — all writes are preserved.

**When partition heals:** Heartbeats resume between previously-separated peers. Two things happen:

1. **Clock merge:** Events from both sides flow via broadcast and heartbeat pull-on-demand with head-to-ancestor closure fetch. Each peer recursively fetches missing ancestors for remote heads until those heads are parent-closed locally, then reconciles when chunk-ready. Each peer's clock grows to include all events from both partitions. Old events from the reconnecting peer are accepted unconditionally — the Merkle clock is a G-Set. Multiple heads emerge (at least one per partition lineage). Tentative `RECONCILE` runs normally against these heads once closure gates are satisfied.

2. **Finalized-state synchronization:** Heartbeats carry `finalized_root`. When a peer receives a heartbeat with a different `finalized_root`, it triggers `SYNC_FINALIZED`. `EMPTY_ROOT` is treated as ordinary lineage, so empty peers recover by the same ADOPT/NOOP/MERGE machinery. Divergence uses a single three-way `MergeRoots(ours, theirs, common_ancestor)` with lineage-LCA common ancestor recovery.

**Fork-and-join topology:** The finalized Dolt commit DAG is no longer strictly linear after partition recovery. It has a fork at the point of partition and a join at the merge commit. Both sides' finalized commits are preserved as history — `dolt log` shows the full picture: pre-split linear chain, two parallel branches during partition, merge commit at reconnection.

**Partition conflicts:** If both sides modified the same rows in independently finalized events (the divergence-merge branch of `SYNC_FINALIZED`), `MergeRoots` returns a `Conflict`. Rather than blocking finalization, the protocol uses the ours-side root (lower finalized-root hash, deterministic) as `finalized_root`, unions both sides' finalized events, records an entry in `finalized_conflicts`, and continues finalizing immediately. This preserves the "no peer is blocked" design principle while ensuring conflict tracking survives notifications/restarts (via state transfer).

**Re-anchoring:** After `SYNC_FINALIZED` changes `finalized_root` (adopt or merge), all tentative events are re-evaluated against the new `finalized_root`. Parking decisions may change since the ancestor for merge computations has changed. This is a CPU-only replay — all chunks are already local.

**Nested partitions:** If a partition splits further (e.g., all three peers isolated), the same logic applies recursively. Reconnection uses lineage-based `SYNC_FINALIZED` modes; merge cases produce deterministic merge commits in the Dolt DAG. The topology becomes more complex but remains well-defined and deterministic. Pairwise divergence merges are computed in deterministic order (by finalized-root hash comparison).

**Determinism:** All peers independently compute the same partition merge result. The "ours" vs "theirs" assignment is deterministic (lower finalized-root hash = "ours"). The merge commit metadata is deterministic (derived from the two finalized states). INV3 holds after convergence.

---

## 7. Design Decisions

These decisions refine the abstract protocol for the doltswarm implementation.

### 7.1 Conflict handling: park later event, block nobody

Conflicts detected by `MergeRoots` are handled using the HLC total order to deterministically select which event is "parked." The earlier event in total order wins; the later event is parked. All peers independently compute the same parking decision.

Parked events:
- Stay in the clock (the event happened, it's preserved)
- Are excluded from `LOCAL_WRITE` parents and finalization (reconciliation recomputes parking when closure is ready; during closure lag unresolved parked heads are retained conservatively)
- Are surfaced to the user for resolution via `RESOLVE_CONFLICT`
- Do not block any peer from writing

Resolution is async: any peer (typically the parked event's author) issues `RESOLVE_CONFLICT` with SQL that merges the parked data into the current state. The resolution event's parents are `active_heads ∪ {event_cid}` so one parked conflict is explicitly resolved per action.

Earlier designs blocked the conflicting peer from `LOCAL_WRITE` until resolution. This was too restrictive — a single conflict could stall a peer indefinitely. An even earlier design used First-Write-Wins (FWW), which auto-resolved by dropping the later commit. FWW causes silent data loss.

### 7.1.1 Finalization of formerly-parked events

When a parked event is resolved via `RESOLVE_CONFLICT`, it leaves `parked_conflicts` (the resolution event references it as a parent, making it no longer a head). The event is then in the clock, not finalized, not parked — eligible for finalization.

A formerly-parked event's `root_hash` branches from an earlier state — it does not incorporate concurrent changes that caused the parking. Replaying every stable event root can therefore re-introduce the same conflict at finalized layer.

**Fix:** `ADVANCE_STABILITY` merges only stable frontier heads, starting from the current `finalized_root` accumulator and using each frontier event's recorded `anchor_root_hash`. If a frontier merge conflicts, that head is skipped for finalized-root advancement while bookkeeping still finalizes the stabilized event set. This is safe because:

1. Events still in `parked_conflicts` are excluded from finalization candidates by step 1.
2. Resolved histories are represented by later frontier roots that subsume earlier conflicting branches.
3. Causal ancestors inside the stable set are accounted for in `finalized_events` but not re-applied as roots, avoiding duplicate application and false conflicts.

This approach is receipt-order independent: frontier selection and HLC ordering are pure functions of `(clock, chunks_ready, peer_seen_frontier, active_peers, finalized_events, parked_conflicts, finalized_root)`.

### 7.2 Event identity and admission: swarm-uniform

Every core event identity uses the canonical abstract payload:
- `CIDPayload = (root_hash, anchor_root_hash, sorted parents, hlc, peer, resolved_finalized_conflict_id)`

`EventCID = hash(CanonicalEncode(CIDPayload))`.
`CanonicalEncode` is intentionally codec-agnostic but must be deterministic/injective with canonical ordering and Optional encoding.

Admission is defined by the swarm-wide predicate `ValidEvent(e)`, not by per-peer optional checks. Every peer in a swarm MUST apply the same predicate. `ValidEvent(e)` MUST reject malformed payloads, CID mismatches, and any failure of the swarm's chosen authenticity/integrity checks. Any authentication mechanism lives outside the core `Event` and feeds only into `ValidEvent(e)`. What matters normatively is that admission is uniform within one swarm.

### 7.3 No epoch grouping

Events are processed individually via `MergeRoots`. There is no batching into wall-time epochs. Each event is replayed against its recorded `anchor_root_hash` over the current accumulator. This is simpler than epoch-based batching and avoids epoch boundary edge cases.

### 7.3.1 Merges are silent local state

Merges (RECONCILE) are deterministic local computations derived entirely from the Merkle clock and `finalized_root`. They produce no events, no network traffic, and no DAG entries. Every peer holding the same event set independently computes the same `latest_root`.

Only three protocol actions produce broadcast messages:
- **LOCAL_WRITE** — new data (the event's `parents = active_heads` naturally reflects the post-merge state).
- **RESOLVE_CONFLICT** / **RESOLVE_FINALIZED_CONFLICT** — user resolution SQL, both flow through LOCAL_WRITE.
- **HEARTBEAT** — adaptive event-driven/keepalive, carries `heads_digest` + full `seen_frontier` + `finalized_root`.

Heads (`computeHeads(clock)`) are always derived from the clock structure, never stored independently. After reconciliation, `heads` remain multi-valued until the next LOCAL_WRITE collapses them.

### 7.4 Data plane chunk-sync contract

Normative contract:

1. Chunk transfer is point-to-point and content-addressed.
2. Event receipt triggers chunk sync for the announced `root_hash`.
3. Reconcile/finalization operations must only use events whose required chunks are locally available.
4. Missing chunks may be fetched from any reachable peer; origin peer is a hint, not a hard requirement.

Any implementation that satisfies this contract is protocol-compliant. Concrete API choices (e.g., Puller wiring, chunk-store adapters, peer selection strategy, retries/caching) are implementation details and are maintained in [`doltswarm-implementation.md`](./doltswarm-implementation.md).

### 7.5 In-memory Layer 3 state

All Merkle clock state (events, heads, peer tracking) is held in-memory. There is no local persistence for Layer 3. On crash, a peer rejoins from a live peer via `PEER_JOIN` (shallow clone of finalized state + retained clock events). This requires at least one live peer for recovery.

Memory is bounded by optional compaction (`GC_CLOCK`): finalized event bodies can be pruned from `clock` once no retained event depends on them, while finalized CID accounting is retained in canonical `finalized_events`.

### 7.6 Seen-frontier heartbeats

Anti-entropy is handled by adaptive heartbeats (event-driven + low-rate idle keepalive, carrying `{ peer, heads_digest, seen_frontier, finalized_root }`). Heartbeats keep one compact digest for heads: `heads_digest = hash(sort(heads))`. They carry the full local seen frontier on every send: `seen_frontier = peer_seen_frontier[self]`. On receive, the receiver updates `peer_seen_frontier[q]` directly from the heartbeat, compares only `heads_digest`, requests the sender's full head set on mismatch, and then recursively pulls missing ancestor closure for `remote_heads ∪ received.seen_frontier` plus required root chunks. This removes the stale-frontier ambiguity from the digest-only design while keeping full-head-set transfer on-demand. The cost is slightly larger heartbeat control traffic; the benefit is simpler witness and anti-entropy semantics.

### 7.7 No pull-first optimization

The `LOCAL_WRITE` path does not include a pre-write sync pass. Peers write immediately and let `RECONCILE` handle convergence. This simplifies the write path at the cost of potentially more merge operations.

### 7.8 Pluggable transport

The core library uses abstract transport interfaces (`Transport`, `Gossip`, `Provider`). The protocol assumes a direct mesh (every peer can reach every other peer) but does not depend on any specific networking library. Transport implementations provide broadcast delivery for the control plane and point-to-point streams for the data plane. This enables testing with in-process transports and supports alternative network stacks (e.g., libp2p, gRPC, WebSocket).

### 7.9 Core package adaptation plan

Moved to [`doltswarm-implementation.md`](./doltswarm-implementation.md) to keep this document focused on normative protocol semantics.

### 7.10 Partition recovery: accept the fork

When a network partition separates peers into independent groups, each group evicts unreachable peers, reduces `active_peers`, and finalizes independently. The two sides' `finalized_root` values diverge. On reconnection, the protocol must reconcile these divergent finalized histories.

**Decision: accept the fork.** Both sides' finalized histories are treated as valid. The divergent finalized roots are merged via a single `MergeRoots` call to produce a new shared finalized base. The Dolt commit DAG gets a fork-and-join topology — a merge commit with both finalized tips as parents. No finalization is rolled back. No data is lost.

**Why not "re-tentativize"?** The alternative — treating one side's post-split finalized events as tentative again — violates INV6 (Stability Monotonicity) and requires unwinding finalization, which is complex and breaks the user expectation that finalized data is settled. Accept-the-fork is simpler and preserves all invariants with minor scoping adjustments.

**Why not "last-write-wins"?** Auto-resolving the partition merge by discarding one side's finalized data causes silent data loss — unacceptable for finalized content.

**Finalized-layer conflicts use the ours-side root and are explicitly tracked.** During normal operation, conflicts only occur at the tentative layer (parked events). Partition recovery may introduce conflicts at the finalized layer when `SYNC_FINALIZED` takes its divergence-merge path. Rather than blocking finalization, the protocol handles these conflicts inline: the ours-side root (lower finalized-root hash, deterministic) becomes `finalized_root`, both sides' finalized events are unioned, and a deterministic `finalized_conflicts` entry is recorded. Users resolve these entries via `RESOLVE_FINALIZED_CONFLICT`, which emits a normal write event carrying `resolved_finalized_conflict_id`. Receivers clear matching entries on event receipt, and `SYNC_FINALIZED` excludes known-resolved ids from conflict-map merges to prevent resurrection. This preserves the "no peer is blocked" design principle while preventing finalized conflicts from being silently forgotten.

**Finalized common-ancestor lookup uses checkpoint lineage, not event replay.** `SYNC_FINALIZED` computes `common_root` via lineage LCA over authoritative finalized checkpoint metadata (`finalized_parents`), not by replaying `finalized_events`. This preserves correctness under nested partition recoveries, where replay over unioned finalized events is unsound. General finalized-root advancement still uses anchored frontier folding in `ADVANCE_STABILITY`: start from the current `finalized_root`, merge stable frontier heads via `mergeRoots(acc, head.root_hash, head.anchor_root_hash)`, and mark all newly stable events finalized for bookkeeping.

**Deterministic merge commit.** The partition merge commit must be byte-identical across all peers for INV3. This requires: (1) deterministic ours/theirs assignment (lower finalized-root hash = ours), (2) deterministic metadata (derived from finalized states), (3) deterministic parent ordering. Dolt's `CreateDeterministicCommit` provides this.


## 8. Implementation Notes

Repository/package structure, public Go API, and integration test planning are maintained in [`doltswarm-implementation.md`](./doltswarm-implementation.md).
