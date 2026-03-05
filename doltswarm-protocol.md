# Doltswarm: P2P Sync Protocol over Dolt

## 0. Motivation and Research Background

### Goal

Design a peer-to-peer synchronization protocol for Dolt databases that allows multiple peers to concurrently read and write SQL data, automatically merge non-conflicting changes, surface conflicts to users for resolution, and converge on a shared finalized history — all without a central coordinator, with minimal inter-peer communication, and with no consensus protocol.

### The problem with Dolt's commit model in a P2P setting

Dolt stores data in content-addressed prolly trees (deterministic B-trees where the same dataset always produces the same root hash). This is excellent for sync — identical data produces identical chunks regardless of which peer produced it. However, Dolt layers a Git-style linear commit DAG on top of this data layer. Each commit's hash is computed from its serialized flatbuffer, which includes `parent_addrs` — the 20-byte hashes of parent commits. This means a commit's identity depends on its position in the DAG. Two commits with identical data but different parents produce different hashes.

In a P2P setting, this creates a fundamental tension. If peers create commits independently and later try to stitch them into a shared history, every rebase or parent reassignment produces new commit hashes, which cascade through all descendants. Any external reference to the original hash becomes stale. Git solves this with a central server that establishes ordering. We cannot rely on that.

### What DefraDB taught us: the CRDT approach avoids linear history entirely

DefraDB (by Source Network) takes a radically different approach. Instead of a global linear commit history, each document has its own Merkle DAG. Every mutation to a document creates a new DAG node whose hash includes the content delta and the parent node hash(es). Concurrent edits to the same document produce multiple DAG heads — a natural representation of divergence. Merging happens per-document by walking the DAG to find the common ancestor and applying CRDT merge semantics at the field level.

The key insight from DefraDB is that **a per-document Merkle DAG CRDT requires no coordination for non-conflicting changes.** Peers simply exchange DAG nodes. The DAG union is itself a CRDT (a G-Set of immutable nodes). Each peer independently computes the merged state from the same set of nodes and arrives at the same result. There is no rebase, no linear ordering to agree on, no epoch to finalize. The tradeoff is that DefraDB cannot easily prune history (every DAG node is needed for future merge-base computation) and its merge semantics are limited to CRDT-compatible operations on schemaless JSON documents — it cannot do the rich SQL-level three-way merge that Dolt provides.

The lesson for doltswarm: **minimize coordination by making the shared data structure a CRDT.** Don't try to get peers to agree on a linear commit chain in real-time. Use an append-only structure that converges via set union.

### What Fireproof taught us: separate the clock from the data

Fireproof (by Chris Anderson, co-creator of CouchDB) provides the architectural blueprint. It cleanly separates two layers:

**Merkle Clock (Pail):** A DAG of events where each event carries an operation payload and parent CIDs. The clock is a G-Set CRDT — merging two clocks is set union. Concurrent writes produce multiple heads. A deterministic total order (causal ordering from the DAG, with CID-based tiebreaker for concurrent events) allows any peer to replay events in the same sequence.

**Prolly Trees:** Deterministic B-trees that store the actual data. Events from the clock are applied to the prolly tree in the deterministic order. Because the tree is history-independent (same data = same tree regardless of insertion order), all peers converge to the same root hash.

The separation means the clock handles *what happened and when*, while the prolly tree handles *what is the current state*. This is exactly the split Dolt already has — prolly trees for data, commit DAG for ordering — but Fireproof's clock is designed for P2P from the ground up, whereas Dolt's commit DAG assumes a linear history with content-addressed identity that breaks under concurrent independent commits.

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

From **DefraDB** we take the principle: use a CRDT-based shared structure (the Merkle clock) so that peers converge without coordination. The clock DAG is a G-Set — append-only, union-merge, no rebase, no hash instability.

From **Fireproof** we take the architecture: separate the ordering layer (Merkle clock) from the data layer (prolly trees). The clock establishes causal ordering and provides merge-base discovery. The prolly trees store the actual SQL data and provide efficient diffing and structural sharing.

From **Dolt** we keep everything valuable: the SQL interface, the prolly tree storage engine, the three-way merge with cell-level conflict detection, the chunk-level sync protocol, and the local commit DAG for `dolt log` / `dolt diff` on each peer.

The protocol does not modify Dolt's commit model. Instead, it introduces a new Merkle clock layer that sits between the prolly tree data and the local commit DAG. Peers exchange clock events (small metadata) and prolly tree chunks over a direct mesh network. The clock's causal ordering + HLC tiebreaker establishes a deterministic total order that all peers compute identically. `MergeRoots` is called with ancestor roots discovered from the clock, not from Dolt's commit parents. Local Dolt commits are created after reconciliation — they provide SQL tooling compatibility but their hashes are peer-specific in the tentative zone and byte-identical in the finalized zone (once causal stability confirms the ordering is settled).

The result: Dolt's full SQL merge semantics in a P2P setting, with coordination limited to broadcast message exchange over a direct mesh, no consensus, and no central coordinator.

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

UnsignedEvent = serialize(root_hash, parents, hlc, peer, op_summary, resolved_finalized_conflict_id)
                -- op_summary is optional; if absent, serialize uses a sentinel
                -- empty OpSummary (tables_modified: {}, is_schema_change: false,
                -- description: ""). Peers optimizing for bandwidth may omit it.
                -- resolved_finalized_conflict_id is optional; absent/null for
                -- normal writes, present only for RESOLVE_FINALIZED_CONFLICT.
EventCID      = hash(UnsignedEvent)       -- hash of unsigned event bytes (no circularity)
Signature     = Sign(private_key, UnsignedEvent)  -- signs same unsigned bytes

Event = {
  cid:            EventCID,               -- redundant on wire (receivers recompute), useful for quick dedup
  root_hash:      Hash,                   -- RootValue hash after this peer's local write
  op_summary:     OpSummary,              -- optional (nullable); if absent, sentinel empty summary used
  resolved_finalized_conflict_id: Optional[FinalizedConflictID], -- null unless this event resolves a finalized conflict
  parents:        Set[EventCID],          -- causal parents in the Merkle clock
  hlc:            HLC,
  peer:           PeerID,
  signature:      Signature,              -- signs UnsignedEvent bytes
}

OpSummary = {
  tables_modified:  Set[string],
  is_schema_change: bool,
  description:      string,
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

```

### 2.2 Per-Peer State

All Layer 3 state is in-memory. Persistence comes from Dolt's Layer 1/2. On crash recovery, a peer rejoins from a live peer via `PEER_JOIN` (explicit state transfer of finalized state + clock events), not from message replay.

| Variable | Type | Description |
|---|---|---|
| `clock` | `Map[EventCID, Event]` | Local Merkle clock (G-Set of events) |
| `heads` | `Set[EventCID]` | Clock heads (events with no children locally) |
| `latest_root` | `Hash` | Current merged RootValue |
| `hlc` | `HLC` | Local hybrid logical clock |
| `peer_hlc` | `Map[PeerID, HLC]` | Latest observed HLC per peer (from heartbeats) |
| `stable_hlc` | `HLC` | `min(peer_hlc)` — everything below is causally stable |
| `finalized_root` | `Hash` | RootValue at the stability boundary |
| `finalized_events` | `Set[EventCID]` | Events that have been finalized |
| `finalized_parents` | `Map[Hash, Set[Hash]]` | Finalized checkpoint DAG parent links at root level (`child_root -> parent_roots`) |
| `finalized_ancestors` | `Map[Hash, Set[Hash]]` | Transitive finalized-root ancestry cache (`root -> ancestor_roots`, including self) |
| `finalized_depth` | `Map[Hash, int]` | Depth cache for finalized-root LCA selection (`root -> max depth from EMPTY_ROOT`) |
| `parked_conflicts` | `Map[EventCID, MergeResult]` | Parked events (later in total order, conflicted at merge). Excluded from LOCAL_WRITE parents and finalization. Included in reconciliation (RECONCILE recomputes parking from all heads each invocation). Awaiting human resolution. |
| `finalized_conflicts` | `Map[FinalizedConflictID, FinalizedConflictInfo]` | Open finalized-layer conflicts detected in `SYNC_FINALIZED` divergence merges. Cleared by local `RESOLVE_FINALIZED_CONFLICT` or by receiving an event carrying `resolved_finalized_conflict_id` for that conflict. |
| `active_peers` | `Set[PeerID]` | Peers seen in the last heartbeat window |

### 2.3 Communication Model

The protocol assumes a **direct mesh** network: every peer can send messages to every other peer. Communication is via a **pluggable transport** abstraction. The protocol is transport-agnostic — the transport must provide broadcast delivery and point-to-point streams, but the protocol does not depend on any specific networking library.

**Control plane (broadcast)** — two message types, broadcast to all peers:

| Message Type | Payload | Rate |
|---|---|---|
| `Event` | See §2.1 | On local write |
| `Heartbeat` | `{ peer: PeerID, hlc: HLC, heads_digest: Hash, finalized_root: Hash }` | Periodic (every 2s) |

**Data plane (chunk sync)** — prolly tree data is synced via **point-to-point streams** between specific peers, triggered by event receipt. When a peer receives an event, it requests the prolly tree chunks for the event's `root_hash` from the originating peer (or any peer that has them). The sync walks the prolly tree from the root hash, requesting only chunks not already present in the local `ChunkStore`. Point-to-point, not broadcast — only the peer that needs chunks requests them. Content-addressed deduplication via Dolt's SHA-512/20 hashing ensures structural sharing across events.

> **Implementation note:** Chunk sync uses Dolt's `Puller` API with a `SwarmChunkStore` as the remote source (no `swarm://` remote or `DOLT_FETCH` — the data plane operates directly on chunk stores). See §7.4 for the complete design.

**No voting, no consensus, no coordinator.** There is no agreement mechanism in the protocol. Convergence follows from set union (the Merkle clock is a G-Set) and deterministic merge (same inputs → same output on every peer).

### 2.3.1 Event Signing and Verification

Every event carries a `signature` over its `UnsignedEvent` bytes (`root_hash`, `parents`, `hlc`, `peer`, `op_summary`, `resolved_finalized_conflict_id`). The `EventCID` is the hash of the same unsigned bytes — no circular dependency between CID and signature. The signing scheme is pluggable (the `Signer` interface). On the wire, the `cid` field is redundant (receivers recompute it from the unsigned bytes) but useful for quick deduplication before full deserialization.

On event receipt:
- If the receiving peer has an `IdentityResolver` configured, the event's signature is verified against the sender's known public key.
- Events that fail verification are **silently discarded** (not added to the clock, not forwarded).
- If no `IdentityResolver` is configured, events are accepted best-effort (useful for testing or trusted networks).

### 2.4 Protocol Actions

#### LOCAL_WRITE(peer, sql_ops) → Event

No conflict precondition — writes are never blocked.

1. Apply `sql_ops` to local Dolt instance → new `RootValue` with hash `r`.
2. `hlc ← tick(hlc)`.
3. `active_heads = heads \ parked_conflicts.keys()`.
4. `unsigned = serialize(r, active_heads, hlc, peer, op_summary, resolved_finalized_conflict_id = null)`.
5. `cid = hash(unsigned)`, `sig = Sign(private_key, unsigned)`.
6. `e = Event { cid, root_hash: r, op_summary, resolved_finalized_conflict_id: null, parents: active_heads, hlc, peer, signature: sig }`.
7. `clock[e.cid] ← e`.
8. Recompute `heads` from clock. Note: `computeHeads` may still return parked CIDs as heads (since no event has them as parents). That's correct — they persist as forks until resolved.
9. If `|heads| > 1`: call `RECONCILE(peer)` — recomputes `latest_root` and `parked_conflicts` from the new head set.
10. If `|heads| = 1`: `latest_root ← r`.
11. Create tentative local Dolt commit.
12. Publish `e` to all peers.

#### RECEIVE_EVENT(peer, e) → Accept | Discard

Events follow a per-event lifecycle: **ANNOUNCED → PARENTS_READY → CHUNKS_READY → MERGEABLE**.

- **ANNOUNCED**: Event is in the clock, heads are recomputed, but its parent/ancestor closure may be incomplete locally and chunks for `e.root_hash` may not yet be local. The event participates in causal ordering and head computation, but NOT in `RECONCILE` or finalization.
- **PARENTS_READY**: All ancestor CIDs reachable from `e.parents` are present in the local clock.
- **CHUNKS_READY**: Chunk sync for `e.root_hash` is complete.
- **MERGEABLE**: Event is both **PARENTS_READY** and **CHUNKS_READY**, and `RECONCILE` has processed it (either merged or parked).

Steps:

1. If `e.cid ∈ clock`: discard (idempotent).
2. **Signature check:** If `IdentityResolver` is configured, verify `e.signature` against `e.peer`'s public key. If invalid, discard silently.
3. **Strict admission rule:** If an event is valid and new, it MUST be accepted into `clock`. Peers MUST NOT discard based on local wall-time checks (e.g., `|now() - e.hlc.wall|`).
4. `hlc ← merge(hlc, e.hlc)`.
5. `clock[e.cid] ← e`. Event is now **ANNOUNCED**.
6. If `e.resolved_finalized_conflict_id` is present, remove that id from `finalized_conflicts` (idempotent; missing key is a no-op).
7. Recompute `heads` from clock: `heads = { cid ∈ clock | cid is not a parent of any event in clock }`.
8. Request prolly tree chunks for `e.root_hash` from originating peer via point-to-point stream. On completion, event becomes **CHUNKS_READY**.
9. Parent-closure readiness is tracked independently: an event becomes **PARENTS_READY** only when all ancestor CIDs reachable from its parent links are present in the local `clock` (typically via heartbeat pull-on-demand).
10. If `|heads| = 1` and the lone head is both **PARENTS_READY** and **CHUNKS_READY**, fast-forward `latest_root ← clock[head].root_hash`.
11. Call `RECONCILE(peer)` (if `|heads| > 1`). `RECONCILE` operates only on heads that are **PARENTS_READY** and **CHUNKS_READY** (or already **MERGEABLE**). Heads missing ancestors or chunks are skipped until a later reconciliation pass.

#### RECONCILE(peer) → unit

**Merges are deterministic local computations, not new information.** If peers A and B both have events `{e1, e2}` with two heads, they each independently call `MergeRoots` and arrive at the same `latest_root`. There is nothing to communicate — the merge result is fully determined by the inputs that all peers already have. No event is created, nothing is broadcast.

**Parking is a pure function of the full state.** Reconcile operates on ALL heads (not pre-filtered by existing `parked_conflicts`). It computes the **complete** parked set from scratch each invocation, making the result a deterministic function of `(heads, clock, finalized_root)` — independent of event receipt order. This ensures all peers with the same event set and finalized root agree on which events are parked, regardless of when or in what order they received events.

1. If `|heads| ≤ 1`: nothing to do.
2. `mergeable_heads = { h ∈ heads | h is PARENTS_READY ∧ h is CHUNKS_READY }`.
3. If `|mergeable_heads| ≤ 1`: nothing to do (insufficient parent/chunk closure for multi-head merge).
4. Sort `mergeable_heads` by HLC total order (deterministic).
5. For each mergeable head `h` (in sorted order), fold into the accumulated root:
   a. `ancestor_root = finalized_root`. The merge ancestor for all reconciliation merges is the current `finalized_root`.
   b. `result = MergeRoots(current_root, clock[h].root_hash, ancestor_root)`.
   c. Match:
      - `Ok(merged)`: `current_root ← merged`.
      - `Conflict(tables, details)`: **Park `h`** (the later event in total order, since we fold in sorted order). Store in `parked_conflicts[h]`. `latest_root` not updated for this head. Notify `h.peer` that their event was parked due to conflict.
6. `parked_conflicts ← { all mergeable heads parked in step 5 }` (complete replacement, not delta).
7. `latest_root ← current_root` (the accumulated merge result).
8. **`heads` are unchanged.** Heads are always derived from `computeHeads(clock)` — since reconcile does not add events to the clock, the head set cannot change. Parked heads remain as persistent DAG forks until `RESOLVE_CONFLICT`.

> **Why no merge events?** Broadcasting merges would be pure redundancy — every peer holding the same events computes the same merged state. Worse, if merge events were broadcast, concurrent merges by different peers would create new DAG forks requiring further reconciliation (an infinite merge loop). Only three things produce network traffic: `LOCAL_WRITE` (new data), conflict-resolution actions (`RESOLVE_CONFLICT` and `RESOLVE_FINALIZED_CONFLICT`, both flowing through `LOCAL_WRITE`), and `HEARTBEAT` (which naturally reflects the reduced head set after merge).

#### RESOLVE_CONFLICT(peer, event_cid, resolution_ops) → unit

User (typically the author of the parked event) provides `resolution_ops` (SQL that fixes conflicting rows/schema). Resolution is async — no peer is blocked waiting for it.

1. Apply `resolution_ops` to local Dolt → `resolved_root`.
2. Remove `event_cid` from `parked_conflicts`.
3. Create new `Event` with `resolved_root`, parents = heads (ALL heads, including the formerly-parked `event_cid` — this subsumes it in the DAG), and `resolved_finalized_conflict_id = null`.
4. Normal `LOCAL_WRITE` flow from step 4 (same signing/CID mechanics, with `resolved_finalized_conflict_id = null`).

#### RESOLVE_FINALIZED_CONFLICT(peer, conflict_id, resolution_ops) → unit

User resolves a tracked finalized-layer conflict (created by `SYNC_FINALIZED` conflict handling) without blocking finalization.

1. Precondition: `conflict_id ∈ finalized_conflicts`.
2. Apply `resolution_ops` to local Dolt → `resolved_root`.
3. Create new `Event` with `resolved_root`, parents = `active_heads` (`heads \ parked_conflicts.keys()`), and `resolved_finalized_conflict_id = conflict_id`.
4. Normal `LOCAL_WRITE` flow from step 4 (same signing/CID mechanics, but with `resolved_finalized_conflict_id = conflict_id` from step 3).
5. On successful event creation/publication, remove `conflict_id` from `finalized_conflicts` (explicit resolution bookkeeping).
6. `RESOLVE_FINALIZED_CONFLICT` MUST NOT directly mutate `finalized_root` or `finalized_events`; those evolve only via existing finalization/sync paths.

#### HEARTBEAT(peer) → unit

Every 2 seconds:

1. `hlc ← tick(hlc)`.
2. `heads_digest ← hash(sort(heads))`.
3. Broadcast `{ peer: self, hlc, heads_digest, finalized_root }` to all peers.

On receive from peer `q`:

1. `peer_hlc[q] ← max(peer_hlc[q], received.hlc)`.
2. `peer_hlc[self] ← max(peer_hlc[self], hlc)` (a peer always knows its own clock is at least this advanced; without this, `stable_hlc` could be held back by the peer's own outdated entry).
3. `active_peers ← active_peers ∪ {q}`.
4. `stable_hlc ← min(peer_hlc[p] for p in active_peers)`.
5. **Frontier digest comparison:** Compute `own_digest ← hash(sort(own heads))`. If `own_digest ≠ received.heads_digest`, request full head set from sender via point-to-point, then perform **ancestor-closure pull-on-demand**:
   1. Let `remote_heads` be the sender's full head set.
   2. For each `h ∈ remote_heads`, recursively request every ancestor CID reachable from `h` that is missing locally.
   3. For each fetched event, pull chunks for its `root_hash`.
   Continue until each remote head is parent-closed locally (or no additional ancestors are available from the sender's clock view). If digests match, no action needed — peers are in sync.
6. **Finalized-state synchronization trigger:** If `received.finalized_root ≠ finalized_root` **and both roots are non-empty** (`received.finalized_root ≠ EMPTY_ROOT ∧ finalized_root ≠ EMPTY_ROOT`), trigger `SYNC_FINALIZED(peer, received.finalized_root)`. `SYNC_FINALIZED` handles both catch-up adoption (subset case) and divergent merge (incomparable case). See §6.5. (Before either side has finalized at least one event, normal event exchange/finalization drives convergence.)

#### ADVANCE_STABILITY(peer) → unit

Triggered when `stable_hlc` advances.

1. `newly_stable = { e ∈ clock | e.hlc < stable_hlc ∧ e ∉ finalized_events ∧ e ∉ parked_conflicts ∧ e is PARENTS_READY }`. Parked events cannot be finalized until resolved, and non-parent-closed events cannot be finalized.
2. `stable_frontier = { e ∈ newly_stable | no child of e is in newly_stable }` (the maximal stable events by causality).
3. Sort `stable_frontier` by HLC total order (deterministic).
4. Snapshot batch anchor once: `anchor = finalized_root`, `acc = finalized_root`.
5. For each frontier event `e` in order: `merged = MergeRoots(acc, e.root_hash, anchor)`.
   - If `merged` is `Ok`: `acc ← merged`.
   - If `merged` is `Conflict`: skip `e`'s root for finalized-root advancement (do not update `acc`), but continue the batch.
6. Bookkeeping finalization: `finalized_events ← finalized_events ∪ newly_stable`.
7. Materialize deterministic Dolt commits (all fields from event metadata, HLC timestamp, deterministic parents).
8. Replace tentative commits. `finalized_root ← acc`.
9. If `acc ≠ anchor`, update finalized checkpoint metadata:
   - `finalized_parents[acc] ← finalized_parents[acc] ∪ {anchor}` (self-edge excluded)
   - `finalized_ancestors[acc] ← {acc} ∪ ⋃ finalized_ancestors[parent]`
   - `finalized_depth[acc] ← 1 + max(finalized_depth[parent])`

> **Why frontier-only?** Replaying every stable event root can re-apply causal chains in the same batch and create false conflicts (e.g., parent then child over the same row). Merging only stable frontier heads applies each stable branch exactly once, while `finalized_events` still records all stabilized events for accounting.

#### EVICT_PEER(peer) → unit

After `HEARTBEAT_TIMEOUT` (10s) with no heartbeat from peer `q`:

1. `active_peers ← active_peers \ {q}`.
2. Recompute `stable_hlc` without `q`.
3. Call `ADVANCE_STABILITY` — finalization resumes.

No coordination needed. Each peer makes the same eviction decision independently based on the same timeout.

#### SYNC_FINALIZED(peer, their_finalized_root) → unit

Triggered when a peer detects a different `finalized_root` from a reconnecting peer (via heartbeat, step 6), with precondition: both roots are non-empty (`our_finalized_root ≠ EMPTY_ROOT ∧ their_finalized_root ≠ EMPTY_ROOT`). Every peer independently computes the same deterministic outcome from local+remote finalized metadata — no coordination, no consensus.

1. Exchange finalized metadata with the remote peer:
   - finalized-event metadata (only the **divergent suffix** in practice — events finalized after the last known common root)
   - finalized checkpoint metadata (`finalized_parents`, `finalized_ancestors`, `finalized_depth`)
   - open finalized-conflict metadata (`finalized_conflicts`)
   - finalized conflict-resolution references (`resolved_finalized_conflict_id` values observed in known events)
   Merge local+remote metadata by key union:
   - checkpoint metadata: set union for parents/ancestors, max for depth
   - finalized conflict metadata: map key union for `finalized_conflicts`
   - conflict-resolution references: set union
2. Compare finalized-event sets:
   - If `our_finalized_events ⊂ their_finalized_events` (**we are behind**): first ensure local finalized-event metadata is complete for the remote set (at minimum, remote finalized CIDs are present in local finalized-event metadata; in the current Quint model this is represented as `their_finalized_events ⊆ clock.keys()`). If incomplete, pull missing finalized-event metadata/chunks and retry `SYNC_FINALIZED`. Once complete, fetch chunks for `their_finalized_root`, then adopt remote finalized state:
     - `finalized_root ← their_finalized_root`
     - `finalized_events ← their_finalized_events`
     - `finalized_conflicts ← their_finalized_conflicts \ known_resolved_finalized_conflict_ids`
   - If `their_finalized_events ⊂ our_finalized_events` (**we are ahead**): finalized state no-op (remote will adopt from us when it runs `SYNC_FINALIZED`), but keep merged finalized-checkpoint metadata from step 1. `finalized_conflicts` is intentionally **not** replaced/unioned in this branch.
   - Otherwise (**independent divergence** OR **equal finalized-event sets with mismatched roots**): run the merge path below.
3. **Divergence merge path (non-subset residual case):**
   1. `common_root = lineageLCA(our_finalized_root, their_finalized_root, finalized_ancestors, finalized_depth)` over the merged checkpoint metadata.
   2. Fetch chunks for `their_finalized_root` via SwarmChunkStore/Puller.
   3. Deterministic ours/theirs: compare the two finalized roots' associated tip events by HLC total order. Earlier = "ours", later = "theirs".
   4. `result = MergeRoots(ours_finalized_root, theirs_finalized_root, common_root)`.
   5. Match:
      - `Ok(merged)`: `finalized_root ← merged`
      - `Conflict(tables, details)`:
        - `finalized_root ← ours_finalized_root` (deterministic)
        - `conflict_id = hash(serialize(ours_finalized_root, theirs_finalized_root, common_root))`
        - Start from `base_conflicts = (our_finalized_conflicts ∪ their_finalized_conflicts) \ known_resolved_finalized_conflict_ids`.
        - If `conflict_id ∉ known_resolved_finalized_conflict_ids`, set `finalized_conflicts[conflict_id] ← { id: conflict_id, ours_root: ours_finalized_root, theirs_root: theirs_finalized_root, common_root, tables, details }` on top of `base_conflicts`; otherwise keep `base_conflicts` unchanged.
        - Surface conflict to user (which tables, which rows, `conflict_id`). No blocking — finalization continues immediately.
   6. `finalized_events ← our_finalized_events ∪ their_finalized_events`
   7. Update finalized checkpoint metadata for the resulting root:
      - `finalized_parents[finalized_root] ← finalized_parents[finalized_root] ∪ ({our_finalized_root, their_finalized_root} \ {finalized_root})`
      - `finalized_ancestors[finalized_root] ← {finalized_root} ∪ ⋃ finalized_ancestors[parent]`
      - `finalized_depth[finalized_root] ← 1 + max(finalized_depth[parent])`
      (This also handles the conflict path where `finalized_root = ours_finalized_root`: self-edge is excluded and the other side is recorded as an additional parent.)
   8. Create a deterministic Dolt merge commit with both finalized tips as parents. All metadata fields are derived deterministically from the two tip events (HLC, peer from "ours" side, deterministic description).
4. **Re-anchor tentative events when finalized root changes:** All post-finalization tentative events used the previous `finalized_root` as merge ancestor. If `finalized_root` changed (adopt or merge), replay all non-finalized, non-rejected events from the new `finalized_root` via `RECONCILE`. CPU-only — all chunks already local.

> **Why lineage-LCA common ancestor lookup?** Replaying finalized events to reconstruct `common_root` is unsound after fork-and-join recoveries: finalized event sets contain divergent lineages, and from-scratch replay can overwrite previous merge results. Using finalized checkpoint lineage metadata (`finalized_parents/ancestors/depth`) gives a deterministic common-ancestor root for arbitrarily nested partition recoveries.

#### PEER_JOIN(new_peer) → unit

State-transfer join: the new peer requests a consistent snapshot from any active peer, then bootstraps local state from it.

1. Join the peer mesh (begin receiving broadcast messages).
2. Request `STATE_SNAPSHOT` from any active peer via point-to-point RPC:
   ```
   STATE_SNAPSHOT = {
     finalized_root:    Hash,              -- opaque value, NOT recomputed from events
     finalized_events:  Set[EventCID],     -- full finalized event set
     finalized_parents: Map[Hash, Set[Hash]],
     finalized_ancestors: Map[Hash, Set[Hash]],
     finalized_depth:   Map[Hash, int],
     clock:             Map[EventCID, Event],  -- complete event clock (finalized + non-finalized)
     parked_conflicts:  Set[EventCID],     -- currently parked events
     finalized_conflicts: Map[FinalizedConflictID, FinalizedConflictInfo], -- open finalized-layer conflicts
   }
   ```
   `finalized_root` must be transferred as an opaque value. `finalized_parents/finalized_ancestors/finalized_depth` are transferred with it so `SYNC_FINALIZED` can recover common ancestors via lineage LCA without event replay. `clock` is the full event G-Set and must include every CID referenced by `finalized_events` (`finalized_events ⊆ clock.keys()`).
3. Fetch chunks for `finalized_root` and all event `root_hash` values via Puller (shallow clone — only chunks not already local).
4. Initialize local state from snapshot:
   - `clock ← snapshot.clock` (union with any events already received via broadcast during join).
   - `heads ← computeHeads(clock)`.
   - `finalized_root ← snapshot.finalized_root`.
   - `finalized_events ← snapshot.finalized_events`.
   - `finalized_parents ← snapshot.finalized_parents`.
   - `finalized_ancestors ← snapshot.finalized_ancestors`.
   - `finalized_depth ← snapshot.finalized_depth`.
   - `parked_conflicts ← snapshot.parked_conflicts`.
   - `finalized_conflicts ← snapshot.finalized_conflicts`.
   - `peer_hlc ← { p: ZERO for all p in PEERS }` — initialized to zero for all peers.
   - `stable_hlc ← ZERO` — conservative; advances naturally from subsequent heartbeats.
   - `active_peers ← { self }` — grows from heartbeats.
   - `latest_root ← computeMergedRoot(...)` from heads and `finalized_root`.
5. Begin heartbeating. First full heartbeat round establishes `peer_hlc` entries for all active peers.
6. Verification: recompute `heads` from `clock`, verify all `finalized_events` are in `clock`, verify `parked_conflicts ⊂ clock.keys()`, and verify lineage metadata contains both `EMPTY_ROOT` and `finalized_root`.

#### PEER_LEAVE(peer) → unit

Graceful: publish leave, removed from `active_peers` immediately. Ungraceful: stops heartbeating, evicted after `HEARTBEAT_TIMEOUT`.

### 2.5 Key Invariants

**INV1 — Clock Convergence:** For any two peers `p, q` that have received the same set of events, `p.clock == q.clock` and `p.heads == q.heads`.

**INV2 — Data Convergence:** For any two peers that have processed the same set of events in the same total order, `p.latest_root == q.latest_root`.

**INV3 — Finalized History Agreement:** For any two peers with the same `finalized_events` set, the finalized Dolt commit DAG is byte-identical: same hashes, same parents, same roots. During a network partition, peers may have different `finalized_events` sets (each side finalizes independently under its reduced `active_peers`). When the partition heals, `SYNC_FINALIZED` reconciles states (adopt for subset, merge for incomparable) into a shared fork-and-join DAG that is eventually identical across all peers. This invariant is unconditional — it also covers partition-merge convergence (same inputs → same synced result) because divergence merges handle conflicts deterministically by using the ours-side root.

**INV4 — Causal Consistency:** If `e1` is an ancestor of `e2` in the clock DAG, then `e1` precedes `e2` in the total order.

**INV5 — No Silent Data Loss:** Every event either enters the finalized set or is parked (awaiting human resolution). No event disappears silently. Formerly-parked events whose content is subsumed by a later resolution event are still accounted for in `finalized_events`; finalized-root advancement is driven by stable frontier roots, so subsumed intermediate roots are not re-applied.

**INV6 — Stability Monotonicity:** Within a continuous `active_peers` membership, `stable_hlc` never moves backward. `finalized_root` never moves backward (the partition merge point is strictly ahead of both sides' previous roots). When a peer is re-added to `active_peers` after partition recovery, `stable_hlc` may temporarily decrease if the returning peer's HLC is behind the current minimum — this is expected and does not compromise finalization safety.

**INV7 — Conflict Visibility:** All merge conflicts are captured and surfaced — none leak into working or finalized state, none are silently discarded. Eight sub-properties:

- **INV7a — No Unhandled Tentative Conflict:** `latest_root` is never a conflict sentinel. Every conflict detected by `MergeRoots` during reconciliation is captured in `parked_conflicts` (the later event by total order is parked). No conflict result propagates into the working state.
- **INV7b — No Unhandled Finalized Conflict:** `finalized_root` is never a conflict sentinel. `ADVANCE_STABILITY` skips conflicting stable-frontier roots during the fixed-anchor fold (§7.1.1), and `SYNC_FINALIZED` divergence merges handle conflicts by using the ours-side root (deterministic). No conflict result propagates into the finalized state.
- **INV7c — Parked Event Integrity:** Every parked event references a known event in the clock. No conflict is silently discarded — parked events persist until explicitly resolved via `RESOLVE_CONFLICT`.
- **INV7d — Parking Agreement:** For any two peers with the same clock and the same `finalized_root`, `parked_conflicts` is identical. Parking is a pure function of `(heads, clock, finalized_root)` via `computeMergedRoot` — independent of event receipt order.
- **INV7e — Finalized Conflict Tracking:** Every finalized-layer conflict detected by `SYNC_FINALIZED` conflict handling is inserted into `finalized_conflicts` and remains present until resolved either locally (`RESOLVE_FINALIZED_CONFLICT`) or via receipt of an event carrying `resolved_finalized_conflict_id` for that conflict.
- **INV7f — Finalized Conflict Authenticity:** Every tracked `finalized_conflicts` entry corresponds to an actual merge conflict for its tuple `(ours_root, theirs_root, common_root)` under `MergeRoots`.
- **INV7g — Finalized Conflict ID Symmetry:** For the same finalized-merge conflict inputs, all peers derive the same deterministic `conflict_id`, regardless of which side initiates `SYNC_FINALIZED`.
- **INV7h — Finalized Conflict No-Resurrection:** Once a peer has observed a resolution reference for a finalized conflict id, that id must not remain or reappear in `finalized_conflicts`.

**INV8 — Heads Derivation Consistency:** For every peer `p`, `p.heads == computeHeads(p.clock)`. Heads are a derived view of clock structure, never an independently-authoritative data source.

**INV9 — Finalized/Parked Disjointness:** For every peer `p`, `p.finalized_events ∩ p.parked_conflicts.keys() == ∅`. An event cannot be both parked (unresolved conflict) and finalized.

**INV10 — Reconcile Fixed-Point Consistency:** For every peer `p`, `p.latest_root` and `p.parked_conflicts` must equal the deterministic `computeMergedRoot(p.heads, p.clock, p.finalized_root)` projection of current state. No action may leave stale reconcile outputs after changing `heads`, `clock`, or `finalized_root`.

**INV11 — Bounded Heartbeat Closure Catchup (spec regression check):** If peers `p` and `q` are connected and `q`'s head ancestry exists in `q.clock`, then one heartbeat closure pull from `q` plus enough `RECEIVE_EVENT` steps at `p` is sufficient to make those remote heads parent-closed in `p`'s clock view.

---

## 3. Quint Formal Specification

The formal Quint specification lives in `specs/`. See `specs/doltswarm_verify.qnt` for the model-checkable specification with invariants. The spec is the source of truth for the **core state machine**: event write, receive, reconcile, heartbeat, finalize, **tentative conflict resolution** (resolve_conflict), **finalized conflict resolution** (resolve_finalized_conflict), **network partitions** (connectivity model, peer eviction), and **partition recovery** (sync_finalized). Partition conflicts from divergence merges in `SYNC_FINALIZED` are handled inline using the ours-side root (no blocking), while conflict metadata is persisted in `finalized_conflicts` until cleared by a local resolution action or a received resolution-reference event. Signature verification and data-plane transfer (chunk fetching) are specified only in this document — they are not yet modeled in the Quint spec. Membership actions (join, leave) are partially modeled: eviction and reconnection (via heartbeat re-adding to `active_peers`) are in the spec; `PEER_JOIN` state-transfer (§2.4) and graceful leave are document-only. The spec starts all peers with identical empty state, which subsumes the post-join steady state. The state-transfer snapshot mechanism is an implementation concern that does not affect core safety properties (the invariants hold regardless of how a peer acquires its initial state, as long as the snapshot is consistent).

**Modeling notes (spec vs. this document):**

- **Finalization ordering:** `ADVANCE_STABILITY` (§2.4) processes a full batch each time it runs: compute `newly_stable`, derive the stable frontier (maximal stable events), sort that frontier by HLC total order, and fold with a fixed batch ancestor `anchor = finalized_root_at_batch_start`. The Quint spec models the same batch semantics via `finalizeBatch`.
- **Finalized checkpoint lineage metadata:** The protocol tracks `finalized_parents`, `finalized_ancestors`, and `finalized_depth` over finalized roots. `do_finalize` updates this metadata when `finalized_root` advances, and `do_sync_finalized` uses lineage-LCA (`lineageLcaRoot`) to compute `common_root` for divergence merges. This avoids unsound from-scratch replay of finalized events and preserves nested partition recovery correctness (INV3).
- **`mergeRoots` asymmetry:** In real Dolt, `MergeRoots(ours, theirs, ancestor)` is asymmetric — conflict markers reference "ours" vs "theirs". The Quint spec models this asymmetry in `do_sync_finalized`'s divergence branch: ours/theirs are assigned deterministically (lower finalized root value = "ours"), approximating the protocol's HLC-based tip comparison. Both methods produce a deterministic total order; the specific comparison differs but correctness (INV3, finalized agreement) holds either way.
- **EventCID representation:** The protocol defines `EventCID = hash(UnsignedEvent)` — a content hash of the canonical unsigned event bytes (`root_hash`, `parents`, `hlc`, `peer`, `op_summary`, `resolved_finalized_conflict_id`). The signature is excluded from the CID to avoid circularity. The spec uses `EventCID = (peer, wall, logical)` — a tuple derived from the HLC. This avoids modeling hash functions while preserving uniqueness (HLC tick guarantees unique CIDs per peer). The spec's CID does not depend on `root_hash` or `parents`.
- **Event fields:** The spec's `Event` omits `op_summary` and `signature`, but includes `resolved_finalized_conflicts` (a set model of optional `resolved_finalized_conflict_id`). `op_summary` is optional in the protocol (nullable, with a sentinel empty summary when absent) and does not affect core state machine logic — no protocol action branches on it. `signature` omission is noted above (signature verification is document-only).
- **`parked_conflicts` type:** The protocol defines `parked_conflicts: Map[EventCID, MergeResult]`, mapping parked events to conflict details (tables, description). The spec uses `Set[EventCID]` — tracking only which events are parked, not conflict presentation details.
- **`finalized_conflicts` type:** The protocol defines `finalized_conflicts: Map[FinalizedConflictID, FinalizedConflictInfo]` (includes table/detail presentation metadata). The spec models `finalized_conflicts` as a map keyed by the same deterministic conflict ID, but tracks only structural roots (`ours_root`, `theirs_root`, `common_root`) needed for state-machine checks.
- **Heartbeat HLC tick:** The protocol ticks the sender's HLC on heartbeat send (`hlc ← tick(hlc)`, §2.4 HEARTBEAT step 1). The spec's `do_heartbeat` models only the receiver side and does not tick `hlc_clock`. Wall-time advancement is provided by the separate `do_tick` action. This may cause `stable_hlc` to advance slightly slower in the spec than in the protocol — a liveness concern, not a safety issue.
- **Heartbeat as direct state read:** The protocol broadcasts heartbeat messages carrying `heads_digest` (a hash of the sorted head set), not the full `heads`. Receivers compare digests and request full heads on mismatch. The spec's `do_heartbeat` reads the sender's node state directly (no message inbox, no digest). This is a standard model-checking simplification and does not model heartbeat message loss, reordering, or the digest-compare step.
- **Pull-on-demand scope:** The protocol (§2.4 HEARTBEAT step 5) compares frontier digests and, on mismatch, requests the full remote head set and recursively pulls missing ancestor closure for those heads. The spec models the same closure pull target (remote head ancestry), while still treating heartbeat as direct state read and skipping digest message mechanics.
- **Finalized sync trigger and case split:** The protocol (§2.4 HEARTBEAT step 6) triggers `SYNC_FINALIZED` when finalized roots differ **and both roots are non-empty**. `SYNC_FINALIZED` then case-splits by finalized-event subset relations: adopt when behind (`A ⊂ B`), no-op when ahead (`B ⊂ A`), and merge in the non-subset residual case (incomparable sets, plus equal-set/root-mismatch repair). Adopt/merge conflict maps are filtered by known resolution references (`resolved_finalized_conflict_id`) so resolved ids are not resurrected. The spec mirrors this contract via `syncMode`, `syncAdoptionReady`, `syncEnabled`, `applySyncFinalized`, and `resolvedFinalizedConflictRefs`.
- **SYNC_FINALIZED regression checks in spec:** The Quint model includes branch-specific contract invariants (`inv_sync_contract_adopt`, `inv_sync_contract_noop`, `inv_sync_contract_merge`, `inv_sync_conflict_recorded`, `inv_sync_conflict_id_symmetric`) and pairwise convergence scenario checks (`inv_sync_subset_roundtrip_converges`, `inv_sync_incomparable_pairwise_converges`) to catch subset/no-op/merge regressions early.
- **Finalized-conflict hardening checks in spec:** The Quint model checks finalized-conflict structural integrity (`inv_finalized_conflict_visibility`), authenticity against merge semantics (`inv_finalized_conflict_authenticity`), explicit resolve-action contract behavior (`inv_resolve_finalized_conflict_contract`: remove exactly one conflict id, no direct finalized-root/events mutation, emitted event carries conflict reference), and anti-resurrection (`inv_finalized_conflict_no_resurrection`).
- **Lineage/LCA regression checks in spec:** The Quint model additionally checks finalized-lineage integrity (`inv_lineage_well_formed`, `inv_lineage_closure`), LCA soundness (`inv_lineage_lca_sound`), sync metadata-union correctness (`inv_sync_metadata_union`), merge-parent edge recording (`inv_sync_merge_parent_edges`), and nested merge composition determinism (`inv_sync_nested_merge_determinism`).
- **Closure/reconcile hardening checks in spec:** The Quint model enforces reconcile fixed-point consistency (`inv_latest_root_is_compute_merged_root`, `inv_parked_conflicts_is_compute_merged_root`) and a bounded heartbeat closure-catchup scenario (`inv_heartbeat_closure_catchup_bounded`) to prevent regressions where orphan heads or stale parked/root state silently drift from the deterministic projection.
- **Finalized metadata exchange:** The protocol's `SYNC_FINALIZED` exchanges finalized-event metadata (divergent suffix in practice), finalized checkpoint metadata (`finalized_parents/ancestors/depth`), open finalized conflicts (`finalized_conflicts`), and known conflict-resolution references (`resolved_finalized_conflict_id`). The spec reads full remote state directly (over-approximation), derives resolution references from clocks, and then applies the same merge/filter contract before case-split execution.
- **Action decoupling from heartbeat:** The protocol (§2.4 HEARTBEAT step 6) triggers `SYNC_FINALIZED` inline when finalized roots differ and both roots are non-empty, and implicitly triggers `ADVANCE_STABILITY` when `stable_hlc` advances. The spec decouples both into standalone nondeterministic actions (`do_sync_finalized`, `do_finalize`) that can fire independently in the `step` relation. The preconditions are equivalent, so the reachable state space is the same — the spec just does not model the causal triggering chain.
- **`localWall` state variable:** The spec adds `localWall: int` to `NodeState`, not present in the protocol's §2.2 per-peer state table. This separates wall-clock progression into explicit `do_tick` actions, giving the model checker control over time advancement. In the protocol, wall time is implicit (read from the system clock).
- **`mergeRoots` abstraction:** The protocol delegates to Dolt's `MergeRoots`, which performs cell-level three-way merge over prolly trees. The spec uses a stylized pure function: identical roots return unchanged, one-side-unchanged returns the other, and two raw `CONTENTS` values diverging from the ancestor produce `CONFLICT_HASH`. The `CONFLICT_HASH` sentinel replaces the protocol's `MergeResult = Ok | Conflict` sum type. This fires with any ancestor (including post-finalization merged roots) to exercise conflicts in partition-recovery and post-finalization scenarios. The non-conflict merge hash combination uses `1000 + ours * 31 + theirs * 17 + ancestor * 7` to guarantee no collision with sentinel values (`CONFLICT_HASH = -1`, `EMPTY_ROOT = 0`) or raw content values (`CONTENTS = {100, 200}`).
- **Event-only inbox:** The spec's `inbox: PeerID → Set[Event]` carries only `Event` messages. Heartbeats use direct state reads (`do_heartbeat`). This follows from the "heartbeat as direct state read" simplification above.
- **Chunk and parent-closure gates:** The protocol defines a per-event lifecycle (ANNOUNCED → PARENTS_READY → CHUNKS_READY → MERGEABLE). `RECONCILE` and finalization require parent-closure; `RECONCILE` additionally requires chunk readiness. The spec models the parent-closure gate explicitly, but does not model chunk transfer latency — it still inlines reconciliation relative to event receipt once parent-closure conditions are satisfied.
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
| **HLC Module** | In-memory per peer | Embedded in events + heartbeats |
| **Heartbeat** | In-memory peer tracking | Broadcast (Heartbeat messages) |
| **Total Order** | Pure computation | N/A |
| **Event Signer** | N/A (pluggable `Signer`) | Signature embedded in events |
| **Chunk Sync** | Calls ChunkStore APIs | Point-to-point streams for chunk requests |
| **Reconciliation Loop** | Calls Dolt Go APIs | N/A (local after chunk sync) |
| **Stability Tracker** | In-memory `stable_hlc` | Derived from heartbeats |
| **Finalization Engine** | Writes to Dolt Layer 2 | Local only |
| **Conflict Surface** | Dolt conflict tables + metadata | Local SQL |
| **Peer Manager** | In-memory `active_peers` | Derived from heartbeats |

### 4.3 Boundary

```
doltswarm (Go)                              Dolt (Go, unmodified)
──────────────                              ─────────────────────
RECEIVE_EVENT(e)
  │
  ├─► Verify(e.signature, e.peer) ────► discard if invalid
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

**Strict convergence requirement:** Peers MUST NOT apply unilateral wall-time admission windows that discard otherwise-valid events. Local wall clocks can differ; dropping by `|now() - e.hlc.wall|` would violate the Merkle clock G-Set model and break INV1 (Clock Convergence). Skew handling is operational: peers may log/flag suspicious timestamps and apply transport/identity controls (rate-limit, disconnect, key revocation), but valid events still enter `clock`.

### 6.2 Conflicts (Data or Schema)

All conflicts are resolved deterministically using the HLC total order. When `MergeRoots` detects a conflict between two events, the earlier event (by total order) wins and its root becomes the working state. The later event is "parked": it stays in the clock (it happened), its data is preserved, but it is excluded from the active lineage (`latest_root`, `LOCAL_WRITE` parents, finalization).

All peers independently compute the same parked set from the same events — no coordination needed. No peer is blocked from writing. Parked events surface as conflicts for human resolution via `RESOLVE_CONFLICT`. Finalized-layer conflicts (from partition recovery) are tracked separately in `finalized_conflicts` and resolved via `RESOLVE_FINALIZED_CONFLICT`.

### 6.3 Peer Goes Offline

Heartbeats stop. After `HEARTBEAT_TIMEOUT` (10s), each remaining peer independently evicts. `stable_hlc` recomputes, finalization resumes. No coordination. When peer returns: `PEER_JOIN` — shallow clone from finalized state, catch up clock, re-enter via heartbeat.

### 6.4 Concurrent Schema Changes

Same pipeline as data conflicts. `SchemaMerge` auto-resolves where possible (e.g., both add different columns). Non-resolvable cases → conflict → user.

### 6.5 Network Partition

Peers in each partition continue operating independently. Each side evicts unreachable peers after `HEARTBEAT_TIMEOUT`, reduces `active_peers`, and finalizes independently under its reduced membership. Both sides' finalized histories diverge.

**During partition:** Each partition operates as a fully functional cluster. Events flow within the partition, `stable_hlc` advances based on the reduced `active_peers`, and events finalize. Each side's `finalized_root` advances along its own lineage. No data loss — all writes are preserved.

**When partition heals:** Heartbeats resume between previously-separated peers. Two things happen:

1. **Clock merge:** Events from both sides flow via broadcast and heartbeat pull-on-demand with head-to-ancestor closure fetch. Each peer recursively fetches missing ancestors for remote heads until those heads are parent-closed locally, then reconciles when chunk-ready. Each peer's clock grows to include all events from both partitions. Old events from the reconnecting peer are accepted unconditionally — the Merkle clock is a G-Set. Multiple heads emerge (at least one per partition lineage). Tentative `RECONCILE` runs normally against these heads once closure gates are satisfied.

2. **Finalized-state synchronization:** Heartbeats carry `finalized_root`. When a peer receives a heartbeat with a different `finalized_root` and both roots are non-empty, it triggers `SYNC_FINALIZED`. If one side is behind (`finalized_events` subset), it adopts the ahead side's finalized state once finalized-event metadata/chunks are locally complete. If both sides finalized independently (incomparable sets), it performs a single three-way `MergeRoots(ours, theirs, common_ancestor)` where the common ancestor is recovered via lineage LCA over merged finalized-checkpoint metadata (`finalized_parents/ancestors/depth`).

**Fork-and-join topology:** The finalized Dolt commit DAG is no longer strictly linear after partition recovery. It has a fork at the point of partition and a join at the merge commit. Both sides' finalized commits are preserved as history — `dolt log` shows the full picture: pre-split linear chain, two parallel branches during partition, merge commit at reconnection.

**Partition conflicts:** If both sides modified the same rows in independently finalized events (the divergence-merge branch of `SYNC_FINALIZED`), `MergeRoots` returns a `Conflict`. Rather than blocking finalization, the protocol uses the ours-side root (earlier by HLC, deterministic) as `finalized_root`, unions both sides' finalized events, records an entry in `finalized_conflicts`, and continues finalizing immediately. This preserves the "no peer is blocked" design principle while ensuring conflict tracking survives notifications/restarts (via state transfer).

**Re-anchoring:** After `SYNC_FINALIZED` changes `finalized_root` (adopt or merge), all tentative events are re-evaluated against the new `finalized_root`. Parking decisions may change since the ancestor for merge computations has changed. This is a CPU-only replay — all chunks are already local.

**Nested partitions:** If a partition splits further (e.g., all three peers isolated), the same logic applies recursively. Reconnection uses `SYNC_FINALIZED`: subset cases adopt, incomparable cases produce merge commits in the Dolt DAG. The topology becomes more complex but remains well-defined and deterministic. Pairwise divergence merges are computed in deterministic order (by HLC of finalized tips).

**Determinism:** All peers independently compute the same partition merge result. The "ours" vs "theirs" assignment is deterministic (earlier finalized tip by HLC total order = "ours"). The merge commit metadata is deterministic (derived from the two tip events). INV3 holds after convergence.

---

## 7. Design Decisions

These decisions refine the abstract protocol for the doltswarm implementation.

### 7.1 Conflict handling: park later event, block nobody

Conflicts detected by `MergeRoots` are handled using the HLC total order to deterministically select which event is "parked." The earlier event in total order wins; the later event is parked. All peers independently compute the same parking decision.

Parked events:
- Stay in the clock (the event happened, it's preserved)
- Are excluded from `LOCAL_WRITE` parents and finalization (but are included in reconciliation — parking is recomputed from scratch each time)
- Are surfaced to the user for resolution via `RESOLVE_CONFLICT`
- Do not block any peer from writing

Resolution is async: any peer (typically the parked event's author) issues `RESOLVE_CONFLICT` with SQL that merges the parked data into the current state. The resolution event's parents include ALL heads (including the parked one), subsuming it in the DAG.

Earlier designs blocked the conflicting peer from `LOCAL_WRITE` until resolution. This was too restrictive — a single conflict could stall a peer indefinitely. An even earlier design used First-Write-Wins (FWW), which auto-resolved by dropping the later commit. FWW causes silent data loss.

### 7.1.1 Finalization of formerly-parked events

When a parked event is resolved via `RESOLVE_CONFLICT`, it leaves `parked_conflicts` (the resolution event references it as a parent, making it no longer a head). The event is then in the clock, not finalized, not parked — eligible for finalization.

A formerly-parked event's `root_hash` branches from an earlier state — it does not incorporate concurrent changes that caused the parking. Replaying every stable event root can therefore re-introduce the same conflict at finalized layer.

**Fix:** `ADVANCE_STABILITY` merges only stable frontier heads using a fixed batch ancestor (`anchor = finalized_root` at batch start). If a frontier merge conflicts, that head is skipped for finalized-root advancement while bookkeeping still finalizes the stabilized event set. This is safe because:

1. Events still in `parked_conflicts` are excluded from finalization candidates by step 1.
2. Resolved histories are represented by later frontier roots that subsume earlier conflicting branches.
3. Causal ancestors inside the stable set are accounted for in `finalized_events` but not re-applied as roots, avoiding duplicate application and false conflicts.

This approach is receipt-order independent: frontier selection and HLC ordering are pure functions of `(clock, stable_hlc, finalized_events, parked_conflicts, finalized_root)`.

### 7.2 Event signing: required

Every event carries a `signature` field computed over its `UnsignedEvent` bytes — the canonical serialization of `(root_hash, parents, hlc, peer, op_summary, resolved_finalized_conflict_id)`. The `EventCID` is the hash of the same unsigned bytes, avoiding any circular dependency between CID and signature. The signing scheme is pluggable via a `Signer` interface.

When an `IdentityResolver` is configured on a peer, incoming events are verified against the sender's public key. Invalid or unverifiable events are silently discarded. When no `IdentityResolver` is configured, events are accepted best-effort.

### 7.3 No epoch grouping

Events are processed individually via `MergeRoots`. There is no batching into wall-time epochs. Each event triggers its own merge operation with `finalized_root` as the merge ancestor. This is simpler than epoch-based batching and avoids epoch boundary edge cases.

### 7.3.1 Merges are silent local state

Merges (RECONCILE) are deterministic local computations derived entirely from the Merkle clock and `finalized_root`. They produce no events, no network traffic, and no DAG entries. Every peer holding the same event set independently computes the same `latest_root`.

Only three protocol actions produce broadcast messages:
- **LOCAL_WRITE** — new data (the event's `parents = heads` naturally reflects the post-merge state).
- **RESOLVE_CONFLICT** / **RESOLVE_FINALIZED_CONFLICT** — user resolution SQL, both flow through LOCAL_WRITE.
- **HEARTBEAT** — periodic, carries current `heads` (which may reflect a post-merge single head if the peer has subsequently written).

Heads (`computeHeads(clock)`) are always derived from the clock structure, never stored independently. After reconciliation, `heads` remain multi-valued until the next LOCAL_WRITE collapses them.

### 7.4 Data plane: Puller-based chunk sync via SwarmChunkStore

Prolly tree data is synced using **Dolt's existing `Puller` API** (`dolt/go/store/datas/pull`), which is entirely hash-based — not commit- or ref-based. The Puller accepts arbitrary root hashes and a `WalkAddrs` function, recursively fetches all reachable chunks that the local store doesn't have, and writes them into local NBS table files.

#### Why Puller works for root-hash-based sync

Investigation of Dolt's Puller confirmed that:

1. **`pull.NewPuller`** takes `hashes []hash.Hash` as starting points — these can be prolly tree root hashes, not just commit hashes. The Puller never references commits, branches, or refs internally.
2. **`types.WalkAddrsForChunkStore`** returns a `WalkAddrs` function that handles both prolly tree and old Noms binary formats, automatically dispatching based on `cs.Version()`. No manual prolly tree parsing needed.
3. **The Puller pipeline** (tracker → fetcher → walker → writer) handles batching, missing-chunk detection via `HasMany`, multi-threaded transfer, and NBS table file creation.
4. **The source `ChunkStore` must implement `nbs.NBSCompressedChunkStore`** (i.e., provide `GetManyCompressed`). The existing `RemoteChunkStore` already satisfies this.

#### SwarmChunkStore architecture

The data plane uses a **single `SwarmChunkStore`** per database that represents the entire swarm as a read-only chunk source. It implements `nbs.NBSCompressedChunkStore` and routes chunk requests to appropriate peers internally:

```
SwarmChunkStore (single instance, represents whole swarm)
  │
  ├─ HasMany(hashes)     → queries local cache, then asks peers
  ├─ GetManyCompressed() → fetches from best peer(s)
  │
  ├─ Peer selection logic:
  │   ├─ Event metadata provides originating peer hint
  │   ├─ Fallback to any peer that responds to HasMany
  │   └─ Future: parallel fetch from multiple peers
  │
  └─ Backed by transport's data plane interface
```

When a peer receives an event with `root_hash`:

```go
walkAddrs, _ := types.WalkAddrsForChunkStore(swarmCS)
puller, _ := pull.NewPuller(ctx, tempDir, targetFileSz,
    swarmCS,              // source: the swarm (routes to peers)
    localCS,              // sink: local ChunkStore
    walkAddrs,
    []hash.Hash{rootHash},
    statsCh,
)
puller.Pull(ctx)
```

The `SwarmChunkStore` is not per-peer — it is a swarm-level abstraction. Internally, it uses the event's originating peer as a hint for where to fetch, but falls back to other peers if the originator is unavailable. Future optimization: fetch different chunks from different peers in parallel (chunked BitTorrent-style).

#### Current RemoteChunkStore analysis

The existing `core/remote_chunk_store.go` already implements `nbs.NBSCompressedChunkStore` (required by Puller) and provides a solid foundation for the `SwarmChunkStore`:

**What it has (keep and extend):**
- `GetManyCompressed(ctx, hashes, found)` → cache-first, then download missing chunks. The download path calls `DownloaderClient.DownloadChunks(ctx, hashStrings, callback)` which streams compressed chunks back.
- `HasMany(ctx, hashes)` → cache-first, then batch `ChunkStoreClient.HasChunks(ctx, repoPath, byteSlices)`. Returns absent set.
- `Get`/`GetMany` → delegate to `GetManyCompressed` with decompression.
- LRU chunk cache (`globalRemoteChunkCache`) with `InsertChunks`/`GetCachedChunks`/`InsertHas`/`GetCachedHas`.
- `Sources()` → `ListTableFiles` for TableFileStore interface (may be needed by Puller internals).
- `Version()` → returns `nbfVersion` (needed by Puller for format dispatch).
- Read-only enforcement (`Put`/`Commit` return errors).

**What changes for SwarmChunkStore:**
- Currently takes a **single `Provider`** in `NewRemoteChunkStore(provider, repo, nbfVersion)`. The `SwarmChunkStore` must accept a `ProviderPicker` (or equivalent) and route requests to the best peer.
- `chunkClient` and `downloader` are currently bound to one provider. These must become dynamic — selected per-request or per-batch based on peer availability.
- Add **peer hint** parameter: when syncing chunks for a specific event, the originating peer is preferred. The hint comes from the event's `peer` field.
- Add **retry with fallback**: if the preferred peer fails, fall back to other peers.
- `Root()` / `loadRoot()` — currently loads the root from a single remote peer. For SwarmChunkStore, `Root()` is less meaningful (we don't need a single root for the swarm). May return a sentinel or the local root.
- `repoSize` / `GetRepoMetadata` — initialization step that queries a single peer. May need to be lazy or skipped for SwarmChunkStore.

**SwarmChunkStore constructor sketch:**
```go
type SwarmChunkStore struct {
    repo       RepoID
    repoPath   string
    picker     ProviderPicker  // selects peers dynamically
    cache      ChunkCache
    nbfVersion string
    log        *logrus.Entry

    // Per-request hint: prefer this peer for chunk fetches
    hintPeerMu sync.RWMutex
    hintPeer   string
}

func (scs *SwarmChunkStore) SetHintPeer(peerID string)
func (scs *SwarmChunkStore) ClearHintPeer()
```

The key insight: `GetManyCompressed` and `HasMany` already have the right signatures. The only change is making the underlying `chunkClient`/`downloader` dynamic instead of fixed.

#### Remaining implementation work

- Refactor `RemoteChunkStore` into `SwarmChunkStore` with `ProviderPicker` instead of single `Provider`
- Make `chunkClient`/`downloader` selection dynamic per-request (hint peer → fallback)
- Wire up `Puller` integration: on event receipt, create `Puller` with `SwarmChunkStore` as source, local `ChunkStore` as sink, event's `root_hash` as starting hash
- Handle `ErrDBUpToDate` (all chunks already present — skip reconciliation)
- Remove `Root()`/`loadRoot()` initialization requirement (SwarmChunkStore doesn't represent a single remote root)
- Remove `swarm_dbfactory.go` and `swarm_registry.go` (no more `swarm://` scheme)

### 7.5 In-memory Layer 3 state

All Merkle clock state (events, heads, peer tracking) is held in-memory. There is no local persistence for Layer 3. On crash, a peer rejoins from a live peer via `PEER_JOIN` (shallow clone of finalized state + clock events). This requires at least one live peer for recovery.

> **Current-model note (memory vs correctness):** The protocol/spec currently use a single-clock model where finalized-event CIDs remain present in `clock` (`finalized_events ⊆ clock.keys()`). This keeps `PEER_JOIN`/`SYNC_FINALIZED` contracts aligned with the current Quint invariants. The unbounded-memory cost is real but orthogonal and is deferred to a future GC design: once finalized event bodies are pruned from `clock`, the snapshot contract and INV5 safety approximation must be updated to use retained finalized metadata/CID accounting rather than `clock` membership for finalized events.

### 7.6 Frontier digest heartbeats

Anti-entropy is handled entirely by heartbeats (every 2s, carrying `{ peer, hlc, heads_digest, finalized_root }`). Heartbeats carry a compact frontier digest — `heads_digest = hash(sort(heads))` — instead of the full head set. On receive, the receiver compares `hash(sort(own heads))` against the received `heads_digest`. Digest match = peers are in sync, no action needed. Digest mismatch = request the sender's full head set, then recursively pull missing ancestor closure for those heads (head-to-ancestor closure fetch) plus required root chunks. This makes the common case (peers in sync) a cheap fixed-size comparison; only divergence triggers additional traffic. Heartbeats also drive causal stability computation.

### 7.7 No pull-first optimization

The `LOCAL_WRITE` path does not include a pre-write sync pass. Peers write immediately and let `RECONCILE` handle convergence. This simplifies the write path at the cost of potentially more merge operations.

### 7.8 Pluggable transport

The core library uses abstract transport interfaces (`Transport`, `Gossip`, `Provider`). The protocol assumes a direct mesh (every peer can reach every other peer) but does not depend on any specific networking library. Transport implementations provide broadcast delivery for the control plane and point-to-point streams for the data plane. This enables testing with in-process transports and supports alternative network stacks (e.g., libp2p, gRPC, WebSocket).

### 7.9 Core package adaptation plan

The `core/` package currently implements the epoch-merge/FWW sync protocol. Below is a file-by-file plan for adapting it to the Merkle clock protocol.

#### Files to DELETE (no longer applicable)

| File | Reason |
|------|--------|
| `epoch.go` | Epoch grouping removed; events are processed individually |
| `bundles.go` | Bundle building/importing replaced by Puller-based chunk sync |
| `swarm_dbfactory.go` | `swarm://` URL scheme no longer used; data plane uses SwarmChunkStore directly |
| `swarm_registry.go` | Global provider registry for `swarm://` no longer needed |
| `commit_ad.go` | `CommitAd` struct replaced by `Event` from Merkle clock |

#### Files to REWRITE (fundamentally different logic)

**`index.go` → Merkle clock interface**
- Current: `CommitIndex` with HLC-keyed entries, checkpoints, finalized base
- New: `MerkleClock` interface with event DAG, heads tracking
- Key methods: `AddEvent(e Event)`, `Heads() []EventCID`, `GetEvent(cid EventCID) (Event, bool)`, `AllEvents() map[EventCID]Event`

**`index_mem.go` → In-memory Merkle clock**
- Current: `MemoryCommitIndex` with LRU map of HLC→entry
- New: `MemMerkleClock` with `map[EventCID]Event`, heads set, peer_hlc map, stable_hlc, finalized event set, parked conflicts

**`reconciler_core.go` → Merkle clock reconciliation**
- Current: `ReplayImportedEpochMerges` — epoch grouping, temp branch, HLC-sorted replay, FWW on conflict
- New: `Reconcile` — sort ALL heads by HLC, fold-merge via `MergeRoots` with `finalized_root` as ancestor. Compute the complete parked set from scratch each invocation (pure function of heads, clock, finalized_root). On conflict: park the later event (by HLC total order), notify user. Create tentative Dolt commit for the merged result.
- Remove: epoch metadata, FWW conflict resolution, parent chain collapsing, replay stall detection
- Add: conflict surfacing, conflict resolution path

**`finalization.go` → Causal stability finalization**
- Current: Checkpoint-based watermarks with slack, `ComputeWatermark` from peer activity
- New: `stable_hlc = min(peer_hlc[p] for p in active_peers)`. When stable_hlc advances, identify `newly_stable` events (`hlc < stable_hlc`), compute stable frontier heads, merge those heads in HLC order using fixed batch ancestor `anchor = finalized_root_at_batch_start`, mark all `newly_stable` finalized, create deterministic Dolt commits
- Much simpler: no epoch checkpoints, no slack duration, no commonly-known threshold (root-level finalized checkpoint lineage metadata is retained for `SYNC_FINALIZED` LCA lookup)

**`node.go` → Merkle clock sync engine**
- Current: CommitAd/Digest broadcast → debounced sync → swarm:// fetch → epoch replay
- New: Event/Heartbeat broadcast → per-event chunk sync via Puller → Merkle clock reconcile
- Major changes:
  - Replace `onCommitAd` with `onEvent`: add to Merkle clock, merge HLC, trigger chunk sync for `root_hash` via SwarmChunkStore/Puller, call reconcile
  - Replace `onDigest` with `onHeartbeat`: update `peer_hlc[sender]`, recompute `stable_hlc`, compare frontier digests (`heads_digest`), pull-on-demand on mismatch
  - Replace `syncOnce` (fetch → epoch replay) with `syncEvent` (Puller chunk sync → reconcile)
  - Remove pull-first from `Commit`/`ExecAndCommit`
  - Add heartbeat publishing loop (every 2s)
  - `Commit`/`ExecAndCommit`: create Event with `parents = active_heads` (`heads \ parked_conflicts`), add to clock, publish (writes are never blocked by pending conflicts)
  - Remove swarm:// remote setup (`EnsureSwarmRemote`, `RegisterSwarmProviders`)
- Remove: hint providers, sync debounce, repair interval, digest publishing, epoch config
- Add: heartbeat interval config

#### Files to ADAPT (modify, not rewrite)

**`remote_chunk_store.go` → `swarm_chunk_store.go`**
- Rename and refactor into `SwarmChunkStore` (see section 7.4 for details)
- Change constructor from single `Provider` to `ProviderPicker`
- Make `chunkClient`/`downloader` dynamic per-request
- Add hint peer support
- Remove `Root()`/`loadRoot()` initialization

**`db.go` → Simplified database wrapper**
- Remove: `EnsureSwarmRemote`, `FetchSwarm` (no swarm:// remote)
- Keep: `GetChunkStore()` (needed as Puller sink), `Open`, `Close`, `InitLocal`, commit helpers, branch helpers
- Keep: `GetBranchHead`, `MergeBase` (still useful for Dolt branch management)
- Adapt: `Commit`/`ExecAndCommit` in `sql.go` — now creates Events instead of CommitAds, checks pending conflicts precondition

**`commit_sql_helpers.go` → Remove epoch merge metadata**
- Remove: `NewEpochMergeMetadata` helper
- Keep: `doCommitWithMetadata`, `CreateCommitMetadata`, `escapeSQL`
- Adapt: metadata format may change (Event-aware)

**`protocol_helpers.go` → Remove epoch helpers**
- Remove: `NewEpochMergeMetadata`, `NewHLC` (HLC construction moves to protocol)
- Keep: `ParseCommitMetadata`, `IsMetadataCommit`, `NewCommitMetadata`

**`aliases.go` → Update type re-exports**
- Remove: `CommitAdV1`, `DigestV1`, `Checkpoint`, `BundleRequest`, `BundleHeader`, `BundledCommit`, `BundledChunk`, `CommitBundle`, `ChunkCodec`, `CommitKindEpochMerge`
- Add: Event types from protocol (if defined there)
- Update message types: `GossipEvent` changes to carry Event/Heartbeat instead of CommitAd/Digest

#### Files to KEEP (unchanged or minimal changes)

| File | Status |
|------|--------|
| `cache.go` | Keep — LRU chunk cache reused by SwarmChunkStore |
| `remote_table_file.go` | Keep — may be needed for TableFileStore interface in SwarmChunkStore |
| `sql.go` | Keep — SQL types, mappers, `ExecContext`/`QueryContext` wrappers |
| `utils.go` | Keep — `ensureDir` utility |

#### Dependency changes in transport/

The `transport/` package interfaces also need updates:

- `Gossip` interface: replace `PublishCommitAd`/`PublishDigest` with `BroadcastEvent`/`BroadcastHeartbeat` (heartbeat carries `heads_digest`, not full `heads`)
- `GossipSubscription`/`GossipEvent`: carry Event/Heartbeat instead of CommitAd/Digest
- `Provider` interface: keep `ChunkStore()`/`Downloader()` for data plane; may simplify
- `ProviderPicker`: keep — used by SwarmChunkStore for peer selection

### 7.10 Partition recovery: accept the fork

When a network partition separates peers into independent groups, each group evicts unreachable peers, reduces `active_peers`, and finalizes independently. The two sides' `finalized_root` values diverge. On reconnection, the protocol must reconcile these divergent finalized histories.

**Decision: accept the fork.** Both sides' finalized histories are treated as valid. The divergent finalized roots are merged via a single `MergeRoots` call to produce a new shared finalized base. The Dolt commit DAG gets a fork-and-join topology — a merge commit with both finalized tips as parents. No finalization is rolled back. No data is lost.

**Why not "re-tentativize"?** The alternative — treating one side's post-split finalized events as tentative again — violates INV6 (Stability Monotonicity) and requires unwinding finalization, which is complex and breaks the user expectation that finalized data is settled. Accept-the-fork is simpler and preserves all invariants with minor scoping adjustments.

**Why not "last-write-wins"?** Auto-resolving the partition merge by discarding one side's finalized data causes silent data loss — unacceptable for finalized content.

**Finalized-layer conflicts use the ours-side root and are explicitly tracked.** During normal operation, conflicts only occur at the tentative layer (parked events). Partition recovery may introduce conflicts at the finalized layer when `SYNC_FINALIZED` takes its divergence-merge path. Rather than blocking finalization, the protocol handles these conflicts inline: the ours-side root (earlier by HLC, deterministic) becomes `finalized_root`, both sides' finalized events are unioned, and a deterministic `finalized_conflicts` entry is recorded. Users resolve these entries via `RESOLVE_FINALIZED_CONFLICT`, which emits a normal write event carrying `resolved_finalized_conflict_id`. Receivers clear matching entries on event receipt, and `SYNC_FINALIZED` excludes known-resolved ids from conflict-map merges to prevent resurrection. This preserves the "no peer is blocked" design principle while preventing finalized conflicts from being silently forgotten.

**Finalized common-ancestor lookup uses checkpoint lineage, not event replay.** `SYNC_FINALIZED` computes `common_root` via lineage LCA over finalized checkpoint metadata (`finalized_parents`, `finalized_ancestors`, `finalized_depth`), not by replaying `finalized_events`. This preserves correctness under nested partition recoveries, where replay over unioned finalized events is unsound. General finalized-root advancement still uses fixed-anchor frontier folding in `ADVANCE_STABILITY`: snapshot `anchor = finalized_root` at batch start, merge stable frontier heads via `mergeRoots(acc, head.root_hash, anchor)`, and mark all newly stable events finalized for bookkeeping.

**Deterministic merge commit.** The partition merge commit must be byte-identical across all peers for INV3. This requires: (1) deterministic ours/theirs assignment (earlier tip by HLC = ours), (2) deterministic metadata (HLC from the merge event, standard description), (3) deterministic parent ordering. Dolt's `CreateDeterministicCommit` provides this.

## 8. Repository Structure

The repo is organized into small packages so the "what" (protocol), "how" (core engine), and "I/O boundary" (transport) are separated. The top-level `doltswarm` package remains the public import path and re-exports the main API for convenience.

```
doltswarm.go          — public facade (type aliases + wrapper functions)
protocol/             — wire- and identity-level types used across the system
  protocol/hlc.go       — HLC implementation
  protocol/repo_id.go   — repo identity
  protocol/metadata.go  — commit metadata format + signing
  protocol/signer.go    — signing interface
  protocol/messages.go  — broadcast message payload structs
transport/            — pluggable networking boundary (no Dolt logic)
  transport/transport.go      — control plane interfaces
  transport/provider.go       — data plane provider interfaces
  transport/provider_hint.go  — best-effort provider hints + retry exclusions via context
core/                 — synchronization engine and Dolt integration
  core/node.go              — Node (message loop, heartbeat, fetch+reconcile orchestration)
  core/reconciler_core.go   — merge orchestration via MergeRoots with finalized_root ancestor
  core/db.go / core/sql.go  — Dolt SQL driver wrapper + commit helpers
  core/swarm_chunk_store.go  — SwarmChunkStore (swarm-level read-only chunk store, Puller source)
  core/index.go / core/index_mem.go — in-memory Merkle clock (event DAG, heads)
specs/                — Quint formal specification (model-checkable with Apalache)
integration/          — demo + docker-based integration tests + transport implementations
  integration/main.go             — ddolt demo CLI used by tests
  integration/integration_test.go — container-per-peer test harness
  integration/transport/          — sample transports (libp2p gossipsub, grpcswarm, overlay)
```

## 9. Public Go API

Most consumers should use `Node` and treat transport as a plug-in.

### Core types

- `OpenNode(cfg NodeConfig) (*Node, error)` and `(*Node).Run(ctx)` start the background message+heartbeat+sync loops.
- `(*Node).Commit(msg)` and `(*Node).ExecAndCommit(exec, msg)` perform local writes and automatically publish events.
- `(*Node).Sync(ctx)` / `(*Node).SyncHint(ctx, hlc)` allow manual/one-shot sync passes (useful for CLIs/tests).

`NodeConfig` requires:
- `Repo` (`RepoID{Org, RepoName}`): logical repo identity.
- `Signer`: signs event metadata and provides `Verify` for remote signature checks.
- `Identity` (optional): if set, incoming events are verified against peer public keys; unverifiable/invalid events are rejected.
- `Transport`: provides (1) broadcast message delivery (events, heartbeats) and (2) a provider picker for the read-only data plane.

### Minimal usage (transport omitted)

```go
cfg := doltswarm.NodeConfig{
  Dir:    "/path/to/dolt/working/dir",
  Repo:   doltswarm.RepoID{RepoName: "mydb"},
  Signer: signer,        // implements doltswarm.Signer
  Transport: transport,  // implements doltswarm.Transport (details omitted)
}

n, _ := doltswarm.OpenNode(cfg)
go n.Run(ctx)

_, _ = n.ExecAndCommit(func(tx *sql.Tx) error {
  _, err := tx.Exec("CREATE TABLE IF NOT EXISTS t (pk INT PRIMARY KEY, v TEXT)")
  return err
}, "init schema")
```

## 10. Integration Tests

Integration scaffolding lives in `integration/`:
- `integration/main.go` contains the `ddolt` demo used by tests.
- A libp2p GossipSub control-plane transport is implemented in `integration/transport/gossipsub/`.
- A data-plane is implemented in `integration/transport/grpcswarm/` by exposing Dolt's read-only chunk store for root-hash-based chunk fetching via the `SwarmChunkStore` + `Puller` pipeline.

To run Docker-based integration tests, use the Taskfile (`Taskfile.yaml`):
- `task integration`
- `task integration:quick`

### Log Line Dependencies (IMPORTANT)

The integration tests compute sync statistics by grepping container logs for specific patterns.
**DO NOT change these log lines in `core/node.go` without updating `computeSyncStats()` in `integration/integration_test.go`.**

The following log patterns are used for statistics:
- `[sync] syncOnce completed` - counts total sync passes
- `[sync] Importing` - counts successful imports
- `[sync] Fast-forward succeeded` - counts fast-forward merges
- `[sync] Epoch merging` - counts merge passes
- `[sync] Merge commits:` - counts merge commits created
- `retrying with different provider` - counts provider retry attempts
- `Exhausted 3 provider retries` - counts exhausted retries

If you need to change any of these log lines, update both:
1. The log statement in `core/node.go`
2. The corresponding `strings.Count()` call in `integration/integration_test.go:computeSyncStats()`
