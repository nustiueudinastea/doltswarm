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

The protocol does not modify Dolt's commit model. Instead, it introduces a new Merkle clock layer that sits between the prolly tree data and the local commit DAG. Peers exchange clock events (small metadata) via libp2p gossipsub and prolly tree chunks via libp2p streams. The clock's causal ordering + HLC tiebreaker establishes a deterministic total order that all peers compute identically. `MergeRoots` is called with ancestor roots discovered from the clock, not from Dolt's commit parents. Local Dolt commits are created after reconciliation — they provide SQL tooling compatibility but their hashes are peer-specific in the tentative zone and byte-identical in the finalized zone (once causal stability confirms the ordering is settled).

The result: Dolt's full SQL merge semantics in a P2P setting, with coordination limited to gossip protocol message exchange, no consensus, and no central coordinator.

---

## 1. Architecture Overview

Three layers, two provided by Dolt, one new:

```
┌──────────────────────────────────────────────────────┐
│  Layer 3: Reconciliation (NEW — doltswarm Go)        │
│  Merkle clock (in-memory), causal stability,         │
│  merge orchestration, libp2p gossipsub transport     │
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

**Layer 3 (NEW — this protocol):** A Merkle clock DAG of events held in-memory and disseminated via libp2p gossipsub. Causal stability determines finalization (no voting). Merge orchestration calls into Layer 1. All conflicts — data and schema — are rejected and pushed back to the user.

---

## 2. Abstract Protocol

### 2.1 Data Types

```
Hash        = byte[20]                    -- SHA-512/20
PeerID      = libp2p.PeerID
HLC         = { wall: uint64, logical: uint16, peer: PeerID }
EventCID    = Hash                        -- hash of serialized Event

Event = {
  cid:            EventCID,
  root_hash:      Hash,                   -- RootValue hash after this peer's local write
  op_summary:     OpSummary,
  parents:        Set[EventCID],          -- causal parents in the Merkle clock
  hlc:            HLC,
  peer:           PeerID,
  signature:      Signature,              -- metadata signature from originating peer
}

Signature = byte[]                        -- signs (root_hash, parents, hlc, peer, op_summary)

OpSummary = {
  tables_modified:  Set[string],
  is_schema_change: bool,
  description:      string,
}

StaleVote = {
  event:    EventCID,
  voter:    PeerID,
  reason:   StaleReason,
}

StaleReason = TooOld | MalformedOp

MergeResult =
  | Ok     { merged_root: Hash }
  | Conflict { tables: Set[string], details: string }

PartitionConflict = {
  ours:       Hash,                   -- our finalized_root before merge (earlier by HLC)
  theirs:     Hash,                   -- their finalized_root before merge (later by HLC)
  ancestor:   Hash,                   -- common pre-split finalized_root
  tables:     Set[string],            -- conflicting tables
  details:    string,
}
```

### 2.2 Per-Peer State

All Layer 3 state is in-memory. Persistence comes from Dolt's Layer 1/2. On crash recovery, a peer rejoins from a live peer via `PEER_JOIN` (explicit state transfer of finalized state + clock events), not from gossipsub message replay.

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
| `stale_votes` | `Map[EventCID, Set[PeerID]]` | Peers that voted to reject an event |
| `rejected_events` | `Set[EventCID]` | Globally rejected events (≥30% voted stale) |
| `parked_conflicts` | `Map[EventCID, MergeResult]` | Parked events (later in total order, conflicted at merge). Excluded from active heads, LOCAL_WRITE parents, reconciliation, and finalization. Awaiting human resolution. |
| `partition_conflict` | `Option[PartitionConflict]` | Active finalized-layer conflict from partition recovery. When set, `ADVANCE_STABILITY` is blocked until resolved via `RESOLVE_PARTITION_CONFLICT`. `LOCAL_WRITE` and tentative reconciliation continue normally. |
| `active_peers` | `Set[PeerID]` | Peers seen in the last heartbeat window |

### 2.3 Communication Model

Communication is via a **pluggable transport** abstraction. The reference implementation uses libp2p gossipsub, but the protocol is transport-agnostic.

**Control plane (gossip)** — three logical topics:

| Topic | Message Type | Rate |
|---|---|---|
| `/doltswarm/events/1.0` | `Event` | On local write |
| `/doltswarm/heartbeat/1.0` | `{ peer: PeerID, hlc: HLC, heads: Set[EventCID], finalized_root: Hash }` | Periodic (every 2s) |
| `/doltswarm/stale/1.0` | `StaleVote` | Rare (only on staleness detection) |

**Data plane (chunk sync)** — prolly tree data is synced via **libp2p direct streams** between specific peers, triggered by event receipt. When a peer receives an event, it requests the prolly tree chunks for the event's `root_hash` from the originating peer (or any peer that has them). The sync walks the prolly tree from the root hash, requesting only chunks not already present in the local `ChunkStore`. Point-to-point, not gossip — only the peer that needs chunks requests them. Content-addressed deduplication via Dolt's SHA-512/20 hashing ensures structural sharing across events.

> **Implementation note:** Chunk sync uses Dolt's `Puller` API with a `SwarmChunkStore` as the remote source (no `swarm://` remote or `DOLT_FETCH` — the data plane operates directly on chunk stores). See §7.4 for the complete design.

**No voting, no consensus, no coordinator.** The only "agreement" mechanism is the staleness vote, which is a distributed threshold check with no leader, and fires rarely (<10s-old events are an edge case).

### 2.3.1 Event Signing and Verification

Every event carries a `signature` over its metadata fields (`root_hash`, `parents`, `hlc`, `peer`, `op_summary`). The signing scheme is pluggable (the `Signer` interface).

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
4. `sig = Sign(r, active_heads, hlc, peer, op_summary)`.
5. `e = Event { cid: hash(r, active_heads, hlc, peer), root_hash: r, parents: active_heads, hlc, peer, signature: sig }`.
6. `clock[e.cid] ← e`.
7. Recompute `heads` from clock. Note: `computeHeads` may still return parked CIDs as heads (since no event has them as parents). That's correct — they persist as forks until resolved.
8. `latest_root ← r`.
9. Create tentative local Dolt commit.
10. Publish `e` on `/doltswarm/events/1.0`.

#### RECEIVE_EVENT(peer, e) → Accept | Reject

1. If `e.cid ∈ clock ∪ rejected_events`: discard (idempotent).
2. **Signature check:** If `IdentityResolver` is configured, verify `e.signature` against `e.peer`'s public key. If invalid, discard silently.
3. **Staleness check:** If `now() - e.hlc.wall > 10 seconds`:
   - **Lineage exemption:** If `e` is an ancestor of a non-stale event from the same peer already in `clock` (i.e., the peer sent a chain of events and a recent tip has arrived), skip the stale vote. The entire lineage is accepted as a unit — this prevents the staleness mechanism from punching holes in a reconnecting peer's event chain during partition recovery. See §6.1.
   - Otherwise, publish `StaleVote { event: e.cid, voter: self, reason: TooOld }` on `/doltswarm/stale/1.0`.
   - The event is still added to `clock` and processed normally (steps 4–9 proceed). The staleness vote is asynchronous — if enough peers vote stale (≥30% of `active_peers`), the event is rejected via `RECEIVE_STALE_VOTE` and its effects are rolled back.
4. `hlc ← merge(hlc, e.hlc)`.
5. `clock[e.cid] ← e`.
6. Recompute `heads` from clock: `heads = { cid ∈ clock | cid is not a parent of any event in clock }`.
7. If `|heads| = 1`: fast-forward `latest_root ← clock[head].root_hash`.
8. Request prolly tree chunks for `e.root_hash` from originating peer via libp2p stream.
9. Call `RECONCILE(peer)` (if `|heads| > 1`).

#### RECEIVE_STALE_VOTE(peer, vote) → unit

1. `stale_votes[vote.event] ← stale_votes[vote.event] ∪ {vote.voter}`.
2. If `vote.event ∈ finalized_events`: ignore (already finalized, cannot be rejected).
3. If `|stale_votes[vote.event]| ≥ ceil(0.3 × |active_peers|)`:
   - `rejected_events ← rejected_events ∪ {vote.event}`.
   - Remove event from `clock`, recompute `heads`.
   - If event was already merged, roll back: replay non-rejected events from `finalized_root`.
   - Notify originating peer that their event was globally rejected.

#### RECONCILE(peer) → unit

**Merges are deterministic local computations, not new information.** If peers A and B both have events `{e1, e2}` with two heads, they each independently call `MergeRoots` and arrive at the same `latest_root`. There is nothing to communicate — the merge result is fully determined by the inputs that all peers already have. No event is created, nothing is broadcast.

**Parking is a pure function of the full state.** Reconcile operates on ALL heads (not pre-filtered by existing `parked_conflicts`). It computes the **complete** parked set from scratch each invocation, making the result a deterministic function of `(heads, clock, finalized_root)` — independent of event receipt order. This ensures all peers with the same event set and finalized root agree on which events are parked, regardless of when or in what order they received events.

1. If `|heads| ≤ 1`: nothing to do.
2. Sort `heads` by HLC total order (deterministic).
3. For each head `h` (in sorted order), fold into the accumulated root:
   a. **Ancestor selection:** Ideally, find the **LCA** in the Merkle clock DAG between the accumulated state and `h` → `ancestor_event`, then `ancestor_root = ancestor_event.root_hash`. If LCA is not found, fall back to `finalized_root`. **Current simplification:** both the Quint spec and Go implementation use `finalized_root` as the ancestor for all merges (no LCA computation). This is sound but may produce more conflicts than necessary.
   b. `result = MergeRoots(current_root, clock[h].root_hash, ancestor_root)`.
   c. Match:
      - `Ok(merged)`: `current_root ← merged`.
      - `Conflict(tables, details)`: **Park `h`** (the later event in total order, since we fold in sorted order). Store in `parked_conflicts[h]`. `latest_root` not updated for this head. Notify `h.peer` that their event was parked due to conflict.
4. `parked_conflicts ← { all heads parked in step 3 }` (complete replacement, not delta).
5. `latest_root ← current_root` (the accumulated merge result).
6. **`heads` are unchanged.** Heads are always derived from `computeHeads(clock)` — since reconcile does not add events to the clock, the head set cannot change. Parked heads remain as persistent DAG forks until `RESOLVE_CONFLICT`.

> **Why no merge events?** Broadcasting merges would be pure redundancy — every peer holding the same events computes the same merged state. Worse, if merge events were broadcast, concurrent merges by different peers would create new DAG forks requiring further reconciliation (an infinite merge loop). Only three things produce gossipsub traffic: `LOCAL_WRITE` (new data), `RESOLVE_CONFLICT` (user resolution, which flows through `LOCAL_WRITE`), and `HEARTBEAT` (which naturally reflects the reduced head set after merge).

#### RESOLVE_CONFLICT(peer, event_cid, resolution_ops) → unit

User (typically the author of the parked event) provides `resolution_ops` (SQL that fixes conflicting rows/schema). Resolution is async — no peer is blocked waiting for it.

1. Apply `resolution_ops` to local Dolt → `resolved_root`.
2. Remove `event_cid` from `parked_conflicts`.
3. Create new `Event` with `resolved_root`, parents = heads (ALL heads, including the formerly-parked `event_cid` — this subsumes it in the DAG).
4. Normal `LOCAL_WRITE` flow from step 4.

#### HEARTBEAT(peer) → unit

Every 2 seconds:

1. `hlc ← tick(hlc)`.
2. Publish `{ peer: self, hlc, heads, finalized_root }` on `/doltswarm/heartbeat/1.0`.

On receive from peer `q`:

1. `peer_hlc[q] ← max(peer_hlc[q], received.hlc)`.
2. `peer_hlc[self] ← max(peer_hlc[self], hlc)` (a peer always knows its own clock is at least this advanced; without this, `stable_hlc` could be held back by the peer's own stale entry).
3. `active_peers ← active_peers ∪ {q}`.
4. `stable_hlc ← min(peer_hlc[p] for p in active_peers)`.
5. If receiver is missing events referenced by sender's `heads`, request them (pull-on-demand).
6. **Partition divergence detection:** If `received.finalized_root ≠ finalized_root` and neither root is an ancestor of the other (i.e., they diverged independently during a partition), trigger `MERGE_FINALIZED(peer, received.finalized_root)`. See §6.8.

#### ADVANCE_STABILITY(peer) → unit

Triggered when `stable_hlc` advances.

0. If `partition_conflict` is set: **block**. No new finalization until the partition conflict is resolved via `RESOLVE_PARTITION_CONFLICT`. Return immediately.
1. `newly_stable = { e ∈ clock | e.hlc < stable_hlc ∧ e ∉ finalized_events ∧ e ∉ rejected_events ∧ e ∉ parked_conflicts }`. Parked events cannot be finalized until resolved.
2. Sort `newly_stable` by total order (causal → HLC → CID).
3. Replay from `finalized_root` via `MergeRoots` → `new_finalized_root`.
4. Materialize deterministic Dolt commits (all fields from event metadata, HLC timestamp, deterministic parents).
5. Replace tentative commits. `finalized_root ← new_finalized_root`. Prune stale votes.

#### EVICT_STALE_PEER(peer) → unit

After `HEARTBEAT_TIMEOUT` (10s) with no heartbeat from peer `q`:

1. `active_peers ← active_peers \ {q}`.
2. Recompute `stable_hlc` without `q`.
3. Call `ADVANCE_STABILITY` — finalization resumes.

No coordination needed. Each peer makes the same eviction decision independently based on the same timeout.

#### EPOCH_FORCE_FINALIZE(peer) → unit

Fallback: if `stable_hlc` hasn't advanced for `EPOCH_TIMEOUT` (60s) despite heartbeating active peers:

1. Evict the blocking peer(s).
2. Recompute `stable_hlc`, call `ADVANCE_STABILITY`.

#### MERGE_FINALIZED(peer, their_finalized_root) → unit

Triggered when a peer detects a different `finalized_root` from a reconnecting peer (via heartbeat, step 6) and the two roots diverged independently during a network partition. Every peer that detects the divergence independently computes the same merge — deterministic, no coordination needed.

1. **Identify common ancestor:** Exchange `finalized_events` sets with the remote peer. The intersection gives the common pre-split base: `common_events = our_finalized_events ∩ their_finalized_events`. Compute `common_root = computeFinalizedRoot(common_events)`. Both sides had this same `finalized_root` before the partition.
2. **Fetch chunks:** Request prolly tree chunks for `their_finalized_root` via SwarmChunkStore/Puller.
3. **Deterministic ours/theirs:** Compare the two finalized roots' associated tip events by HLC total order. The earlier one is "ours", the later one is "theirs". All peers make the same selection.
4. `result = MergeRoots(ours_finalized_root, theirs_finalized_root, common_root)`.
5. Match:
   - `Ok(merged)`:
     - `finalized_root ← merged`.
     - `finalized_events ← our_finalized_events ∪ their_finalized_events`.
     - Create a deterministic Dolt merge commit with both finalized tips as parents. All metadata fields derived deterministically from the two tip events (HLC, peer from "ours" side, deterministic description).
     - **Re-anchor tentative events:** All post-finalization tentative events used the old `finalized_root` as their merge ancestor. With the new merged `finalized_root`, parking decisions may change. Replay all non-finalized, non-rejected events from the new `finalized_root` via `RECONCILE`. CPU-only — all chunks already local.
   - `Conflict(tables, details)`:
     - `partition_conflict ← PartitionConflict { ours: ours_finalized_root, theirs: theirs_finalized_root, ancestor: common_root, tables, details }`.
     - `ADVANCE_STABILITY` is now blocked until resolved.
     - `LOCAL_WRITE` and tentative reconciliation continue normally — peers are not blocked from writing.
     - Surface conflict to user (which tables, which rows).

> **Why a single three-way merge?** The current `computeFinalizedRoot` replays finalized events as a fast-forward fold: `mergeRoots(acc, x, acc) = x`. This works because each event's `root_hash` incorporates all previous state — the fold is just fast-forwarding to the latest. But after a partition, events from side A don't incorporate side B's state. Interleaving them in HLC order and fast-forward folding would silently lose one side's changes. A single `MergeRoots` call with the common ancestor correctly diffs both sides against their shared base and merges at the cell level.

#### RESOLVE_PARTITION_CONFLICT(peer, resolution_ops) → unit

User provides `resolution_ops` (SQL that fixes conflicting rows/schema from the partition merge). Any peer can resolve, though the author of "theirs" changes is in the best position since they know what they intended. Resolution is async — no peer is blocked from tentative writes while awaiting resolution.

1. Apply `resolution_ops` to local Dolt → `resolved_root`.
2. Create a deterministic Dolt merge commit with both partition tips as parents and `resolved_root` as tree. All metadata fields deterministic (HLC from resolution event, deterministic description).
3. `finalized_root ← resolved_root`.
4. `finalized_events ← partition_conflict.ours_events ∪ partition_conflict.theirs_events` (the union).
5. `partition_conflict ← None`.
6. **Re-anchor tentative events** against the new `finalized_root`. Replay all non-finalized, non-rejected events via `RECONCILE`.
7. `ADVANCE_STABILITY` resumes.
8. **Broadcast resolution:** Create a new `Event` with `root_hash = resolved_root`, `parents = current heads` (subsuming all active lineages), and publish on `/doltswarm/events/1.0`. This event flows through normal `RECEIVE_EVENT` on other peers. When a peer receives this event and detects it resolves their own `partition_conflict` (the event's root is from a peer that had the same conflict), it adopts the resolved `finalized_root` and `finalized_events` union, clearing its own `partition_conflict`. Peers without an active partition conflict process the event normally via `RECONCILE`.

#### PEER_JOIN(new_peer) → unit

1. Subscribe to all three gossipsub topics.
2. Heartbeats from active peers provide current `heads`.
3. Request from any active peer: `finalized_root` + finalized Dolt commits (shallow clone via `Puller`), all post-finalization clock events, chunks for `latest_root`.
4. Reconstruct local state. Join `active_peers` via heartbeat.

#### PEER_LEAVE(peer) → unit

Graceful: publish leave, removed from `active_peers` immediately. Ungraceful: stops heartbeating, evicted after `HEARTBEAT_TIMEOUT`.

### 2.5 Key Invariants

**INV1 — Clock Convergence:** For any two peers `p, q` that have received the same set of events, `p.clock == q.clock` and `p.heads == q.heads`.

**INV2 — Data Convergence:** For any two peers that have processed the same set of non-rejected events in the same total order, `p.latest_root == q.latest_root`.

**INV3 — Finalized History Agreement:** For any two peers with the same `finalized_events` set, the finalized Dolt commit DAG is byte-identical: same hashes, same parents, same roots. During a network partition, peers may have different `finalized_events` sets (each side finalizes independently under its reduced `active_peers`). When the partition heals, `MERGE_FINALIZED` reconciles the divergent histories into a shared fork-and-join DAG that is eventually identical across all peers.

**INV4 — Causal Consistency:** If `e1` is an ancestor of `e2` in the clock DAG, then `e1` precedes `e2` in the total order.

**INV5 — No Silent Data Loss:** Every event either enters the finalized set, is explicitly rejected, or is parked (awaiting human resolution). No event disappears silently. Parked events are never subsumed in the DAG without explicit resolution.

**INV6 — Stability Monotonicity:** Within a continuous `active_peers` membership, `stable_hlc` never moves backward. `finalized_root` never moves backward (the partition merge point is strictly ahead of both sides' previous roots). When a peer is re-added to `active_peers` after partition recovery, `stable_hlc` may temporarily decrease if the returning peer's HLC is behind the current minimum — this is expected and does not compromise finalization safety.

**INV7 — Rejection Consistency:** If ≥30% of `active_peers` vote stale on event `e`, then eventually all peers mark `e` as rejected.

**INV8 — Conflict Visibility:** Every `MergeConflict` result parks the later event (by total order) and surfaces it to the user. All peers deterministically agree on which events are parked. No conflict is auto-resolved or silently discarded. Every partition merge conflict (`MERGE_FINALIZED` returning `Conflict`) is surfaced to the user as a `PartitionConflict`. No partition conflict is auto-resolved or silently discarded.

**INV9 — Finalization/Rejection Exclusivity:** No event is both finalized and rejected. `finalized_events ∩ rejected_events = ∅` on every peer. Events rejected during a partition remain rejected after reconnection — rejections propagate across partition boundaries.

**INV10 — Partition Merge Convergence:** For any two peers that have completed `MERGE_FINALIZED` with the same inputs (same two `finalized_root` values and same `common_root`), the resulting `finalized_root` is identical. The merge is a deterministic function of its inputs.

---

## 3. Quint Formal Specification

The formal Quint specification lives in `specs/`. See `specs/doltswarm_verify.qnt` for the model-checkable specification with invariants. The spec is the source of truth for the **core state machine**: event write, receive, reconcile, heartbeat, finalize, stale-vote/rejection, **tentative conflict resolution** (resolve_conflict), **network partitions** (connectivity model, peer eviction), **partition recovery** (merge_finalized, resolve_partition_conflict), and **staleness lineage exemption**. Signature verification and data-plane transfer (chunk fetching) are specified only in this document — they are not yet modeled in the Quint spec. `EPOCH_FORCE_FINALIZE` is not modeled (timeout-based fallback, difficult to express as a model-checkable action). Membership actions (join, leave) are partially modeled: eviction and reconnection (via heartbeat re-adding to active_peers) are in the spec; graceful join/leave are document-only.

**Modeling notes (spec vs. this document):**

- **Finalization ordering:** `ADVANCE_STABILITY` (§2.4) specifies sorting all newly-stable events by total order and replaying them in a single batch. The Quint spec instead finalizes one nondeterministic candidate per step, then recomputes `computeFinalizedRoot` from scratch over the full finalized event set. Because `computeFinalizedRoot` is a pure function of the event set (it sorts internally), both approaches produce the same finalized root regardless of finalization order. The spec's approach is easier to model-check.
- **`mergeRoots` asymmetry:** In real Dolt, `MergeRoots(ours, theirs, ancestor)` is asymmetric — conflict markers reference "ours" vs "theirs". The Quint spec models this asymmetry: `do_merge_finalized` assigns ours/theirs deterministically (lower finalized root value = "ours") matching the protocol's HLC-based assignment (§2.4 MERGE_FINALIZED step 3).

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
| **Merkle Clock** | In-memory `map[EventCID]Event` | gossipsub `/doltswarm/events/1.0` |
| **HLC Module** | In-memory per peer | Embedded in events + heartbeats |
| **Heartbeat** | In-memory peer tracking | gossipsub `/doltswarm/heartbeat/1.0` |
| **Stale Vote** | In-memory vote tally | gossipsub `/doltswarm/stale/1.0` |
| **Total Order** | Pure computation | N/A |
| **LCA Finder** | Pure computation over clock | N/A |
| **Event Signer** | N/A (pluggable `Signer`) | Signature embedded in events |
| **Chunk Sync** | Calls ChunkStore APIs | libp2p direct streams for chunk requests |
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
  │     libp2p stream to e.peer            ChunkStore.HasMany (missing?)
  │                                        ChunkStore.PutMany (store)
  │
  ├─► find_lca(clock, h1, h2)
  │     walks in-memory Merkle clock
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

### 6.1 Stale Event (>10s old)

Receiving peer votes stale, publishes on `/doltswarm/stale/1.0`. The event is still added to `clock` and processed normally (including reconciliation) — the staleness vote is asynchronous. If <30% vote stale, event remains accepted. If ≥30%, globally rejected: removed from clocks, originator notified. If already merged by some peers before rejection, those peers roll back by replaying non-rejected events from `finalized_root`. Already-finalized events cannot be rejected. All chunks already local — CPU-only, no network.

**Lineage exemption for partition recovery:** The staleness mechanism is designed for individual straggler events during normal operation. During partition recovery, a reconnecting peer sends a chain of events spanning the entire partition duration — most are >10s old by wall time. Without an exemption, the staleness mechanism would reject most of the chain, leaving dangling references from surviving tip events to rejected ancestors.

The exemption rule: an event is exempt from staleness voting if it is an ancestor of a non-stale event from the same peer already in the clock. The entire lineage is treated as a unit — either the chain is accepted or rejected, but holes cannot be punched in the middle. Implementation: when evaluating staleness for event `e`, check if any event `e'` in the clock satisfies `e'.peer == e.peer ∧ e.cid ∈ ancestors(e') ∧ (now() - e'.hlc.wall ≤ 10s)`. If so, skip the stale vote for `e`.

### 6.2 Conflicts (Data or Schema)

All conflicts are resolved deterministically using the HLC total order. When `MergeRoots` detects a conflict between two events, the earlier event (by total order) wins and its root becomes the working state. The later event is "parked": it stays in the clock (it happened), its data is preserved, but it is excluded from the active lineage (`latest_root`, `LOCAL_WRITE` parents, finalization).

All peers independently compute the same parked set from the same events — no coordination needed. No peer is blocked from writing. Parked events surface as conflicts for human resolution via `RESOLVE_CONFLICT`. The author of the parked event is in the best position to resolve, since they know what they intended.

### 6.3 No Common Ancestor

`find_lca` returns nothing. Use `finalized_root` as ancestor (or `EMPTY_ROOT`). All rows treated as insertions. Identical rows auto-merge. Schema clashes on same table → conflict per §6.2.

### 6.4 Peer Goes Offline

Heartbeats stop. After `HEARTBEAT_TIMEOUT` (10s), each remaining peer independently evicts. `stable_hlc` recomputes, finalization resumes. No coordination. When peer returns: `PEER_JOIN` — shallow clone from finalized state, catch up clock, re-enter via heartbeat.

### 6.5 Forced Epoch Finalization

`stable_hlc` stuck for `EPOCH_TIMEOUT` (60s). Evict blocking peer(s). Recompute, advance stability. Fallback only — causal stability handles normal case.

### 6.6 Concurrent Schema Changes

Same pipeline as data conflicts. `SchemaMerge` auto-resolves where possible (e.g., both add different columns). Non-resolvable cases → conflict → user.

### 6.7 Rollback After Late Rejection

Replay all non-rejected events in total order from `finalized_root`. Chunks already in `ChunkStore`. CPU-only operation.

### 6.8 Network Partition

Peers in each partition continue operating independently. Each side evicts unreachable peers after `HEARTBEAT_TIMEOUT`, reduces `active_peers`, and finalizes independently under its reduced membership. Both sides' finalized histories diverge.

**During partition:** Each partition operates as a fully functional cluster. Events flow within the partition, `stable_hlc` advances based on the reduced `active_peers`, and events finalize. Each side's `finalized_root` advances along its own lineage. No data loss — all writes are preserved.

**When partition heals:** Heartbeats resume between previously-separated peers. Three things happen:

1. **Staleness lineage exemption:** The reconnecting peer's event chain spans the partition duration. Most events are >10s old. The lineage exemption (§6.1) prevents the staleness mechanism from rejecting the chain — only the tip is evaluated for staleness. If the tip is fresh, the entire ancestor chain is accepted.

2. **Clock merge:** Events from both sides flow via gossip. Each peer's clock grows to include all events from both partitions. Multiple heads emerge (at least one per partition lineage). Tentative `RECONCILE` runs normally against these heads.

3. **Finalized root divergence detection and merge:** Heartbeats carry `finalized_root`. When a peer receives a heartbeat with a different `finalized_root` that isn't an ancestor of its own, it triggers `MERGE_FINALIZED`. This performs a single three-way `MergeRoots(ours, theirs, common_ancestor)` where the common ancestor is the pre-split finalized root (recovered from the intersection of both sides' `finalized_events` sets).

**Fork-and-join topology:** The finalized Dolt commit DAG is no longer strictly linear after partition recovery. It has a fork at the point of partition and a join at the merge commit. Both sides' finalized commits are preserved as history — `dolt log` shows the full picture: pre-split linear chain, two parallel branches during partition, merge commit at reconnection.

**Partition conflicts:** If both sides modified the same rows in their independently finalized events, `MERGE_FINALIZED` returns a `Conflict`. This creates a `PartitionConflict` — a new conflict surface distinct from tentative parked conflicts. Both sides already told their users the data is finalized. Resolution is via `RESOLVE_PARTITION_CONFLICT`: a peer (typically the author of "theirs" changes) provides SQL that resolves the conflicting rows. During the conflict period, `ADVANCE_STABILITY` is blocked but `LOCAL_WRITE` continues normally.

**Re-anchoring:** After `MERGE_FINALIZED` completes (with or without conflict resolution), all tentative events are re-evaluated against the new `finalized_root`. Parking decisions may change since the ancestor for merge computations has changed. This is a CPU-only replay — all chunks are already local.

**Cross-partition rejections:** Events rejected on one side of the partition (via stale votes within that partition's `active_peers`) remain rejected after reconnection. The rejection was valid within its context and propagates to the other side.

**Nested partitions:** If a partition splits further (e.g., all three peers isolated), the same logic applies recursively. Each reconnection produces a merge commit in the Dolt DAG via `MERGE_FINALIZED`. The topology becomes more complex but remains well-defined and deterministic. Pairwise merges are computed in deterministic order (by HLC of finalized tips).

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

### 7.2 Event signing: required

Every event carries a `signature` field computed over its metadata (`root_hash`, `parents`, `hlc`, `peer`, `op_summary`). The signing scheme is pluggable via a `Signer` interface.

When an `IdentityResolver` is configured on a peer, incoming events are verified against the sender's public key. Invalid or unverifiable events are silently discarded. When no `IdentityResolver` is configured, events are accepted best-effort.

### 7.3 No epoch grouping

Events are processed individually via `MergeRoots`. There is no batching into wall-time epochs. Each event triggers its own merge operation with the ancestor discovered via LCA in the Merkle clock. This is simpler than epoch-based batching and avoids epoch boundary edge cases.

### 7.3.1 Merges are silent local state

Merges (RECONCILE) are deterministic local computations derived entirely from the Merkle clock and `finalized_root`. They produce no events, no gossip traffic, and no DAG entries. Every peer holding the same event set independently computes the same `latest_root`.

Only three protocol actions produce gossipsub messages:
- **LOCAL_WRITE** — new data (the event's `parents = heads` naturally reflects the post-merge state).
- **RESOLVE_CONFLICT** — user resolution SQL, which flows through LOCAL_WRITE.
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

All Merkle clock state (events, heads, peer tracking, stale votes) is held in-memory. There is no local persistence for Layer 3. On crash, a peer rejoins from a live peer via `PEER_JOIN` (shallow clone of finalized state + clock events). This requires at least one live peer for recovery.

### 7.6 Heartbeats only (no digest messages)

Anti-entropy is handled entirely by heartbeats (every 2s, carrying `{ peer, hlc, heads }`). There are no separate digest/checkpoint messages. If a receiver is missing events referenced by a sender's heads, it requests them (pull-on-demand). Heartbeats also drive causal stability computation.

### 7.7 No pull-first optimization

The `LOCAL_WRITE` path does not include a pre-write sync pass. Peers write immediately and let `RECONCILE` handle convergence. This simplifies the write path at the cost of potentially more merge operations.

### 7.8 Pluggable transport

The core library uses abstract transport interfaces (`Transport`, `Gossip`, `Provider`). It does not depend on libp2p directly. The reference implementation uses libp2p GossipSub for the control plane and gRPC for the data plane, but other transports are possible. This enables testing with in-process transports and supports alternative network stacks.

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
- New: `MerkleClock` interface with event DAG, heads tracking, LCA computation
- Key methods: `AddEvent(e Event)`, `Heads() []EventCID`, `LCA(h1, h2 EventCID) (EventCID, bool)`, `GetEvent(cid EventCID) (Event, bool)`, `AllEvents() map[EventCID]Event`

**`index_mem.go` → In-memory Merkle clock**
- Current: `MemoryCommitIndex` with LRU map of HLC→entry
- New: `MemMerkleClock` with `map[EventCID]Event`, heads set, peer_hlc map, stable_hlc, finalized/rejected event sets, parked conflicts

**`reconciler_core.go` → Merkle clock reconciliation**
- Current: `ReplayImportedEpochMerges` — epoch grouping, temp branch, HLC-sorted replay, FWW on conflict
- New: `Reconcile` — sort ALL heads by HLC, fold-merge via `MergeRoots` with `finalized_root` as ancestor (LCA as future optimization). Compute the complete parked set from scratch each invocation (pure function of heads, clock, finalized_root). On conflict: park the later event (by HLC total order), notify user. Create tentative Dolt commit for the merged result.
- Remove: epoch metadata, FWW conflict resolution, parent chain collapsing, replay stall detection
- Add: LCA computation (ancestor set intersection), conflict surfacing, conflict resolution path

**`finalization.go` → Causal stability finalization**
- Current: Checkpoint-based watermarks with slack, `ComputeWatermark` from peer activity
- New: `stable_hlc = min(peer_hlc[p] for p in active_peers)`. When stable_hlc advances, identify events with `hlc < stable_hlc`, sort by total order (causal→HLC→CID), replay from `finalized_root` via `MergeRoots`, create deterministic Dolt commits
- Much simpler: no checkpoints, no slack duration, no commonly-known threshold

**`node.go` → Merkle clock sync engine**
- Current: CommitAd/Digest gossip → debounced sync → swarm:// fetch → epoch replay
- New: Event/Heartbeat/StaleVote gossip → per-event chunk sync via Puller → Merkle clock reconcile
- Major changes:
  - Replace `onCommitAd` with `onEvent`: add to Merkle clock, merge HLC, trigger chunk sync for `root_hash` via SwarmChunkStore/Puller, call reconcile
  - Replace `onDigest` with `onHeartbeat`: update `peer_hlc[sender]`, recompute `stable_hlc`, pull-on-demand for missing events
  - Add `onStaleVote`: collect votes, reject events with ≥30% votes
  - Replace `syncOnce` (fetch → epoch replay) with `syncEvent` (Puller chunk sync → reconcile)
  - Remove pull-first from `Commit`/`ExecAndCommit`
  - Add heartbeat publishing loop (every 2s)
  - `Commit`/`ExecAndCommit`: check no pending conflicts, create Event, add to clock, publish
  - Remove swarm:// remote setup (`EnsureSwarmRemote`, `RegisterSwarmProviders`)
- Remove: hint providers, sync debounce, repair interval, digest publishing, epoch config
- Add: heartbeat interval, staleness threshold, rejection ratio config

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
- Update gossip types: `GossipEvent` changes to carry Event/Heartbeat/StaleVote instead of CommitAd/Digest

#### Files to KEEP (unchanged or minimal changes)

| File | Status |
|------|--------|
| `cache.go` | Keep — LRU chunk cache reused by SwarmChunkStore |
| `remote_table_file.go` | Keep — may be needed for TableFileStore interface in SwarmChunkStore |
| `sql.go` | Keep — SQL types, mappers, `ExecContext`/`QueryContext` wrappers |
| `utils.go` | Keep — `ensureDir` utility |

#### Dependency changes in transport/

The `transport/` package interfaces also need updates:

- `Gossip` interface: replace `PublishCommitAd`/`PublishDigest` with `PublishEvent`/`PublishHeartbeat`/`PublishStaleVote`
- `GossipSubscription`/`GossipEvent`: carry Event/Heartbeat/StaleVote instead of CommitAd/Digest
- `Provider` interface: keep `ChunkStore()`/`Downloader()` for data plane; may simplify
- `ProviderPicker`: keep — used by SwarmChunkStore for peer selection

### 7.10 Partition recovery: accept the fork

When a network partition separates peers into independent groups, each group evicts unreachable peers, reduces `active_peers`, and finalizes independently. The two sides' `finalized_root` values diverge. On reconnection, the protocol must reconcile these divergent finalized histories.

**Decision: accept the fork.** Both sides' finalized histories are treated as valid. The divergent finalized roots are merged via a single `MergeRoots` call to produce a new shared finalized base. The Dolt commit DAG gets a fork-and-join topology — a merge commit with both finalized tips as parents. No finalization is rolled back. No data is lost.

**Why not "re-tentativize"?** The alternative — treating one side's post-split finalized events as tentative again — violates INV6 (Stability Monotonicity) and requires unwinding finalization, which is complex and breaks the user expectation that finalized data is settled. Accept-the-fork is simpler and preserves all invariants with minor scoping adjustments.

**Why not "last-write-wins"?** Auto-resolving the partition merge by discarding one side's finalized data causes silent data loss — unacceptable for finalized content.

**The staleness exemption is essential.** Without the lineage exemption (§6.1), the staleness mechanism would reject most of a reconnecting peer's event chain (events >10s old). This would leave the surviving tip events with dangling parent references, breaking the Merkle clock DAG. The lineage exemption treats a peer's chain as an atomic unit for staleness purposes.

**Finalized-layer conflicts are a new surface.** During normal operation, conflicts only occur at the tentative layer (parked events). Partition recovery introduces conflicts at the finalized layer — both sides already told their users the data is settled. These require a dedicated resolution path (`RESOLVE_PARTITION_CONFLICT`) and are surfaced as `PartitionConflict` with explicit table/row details.

**`computeFinalizedRoot` changes.** The existing fast-forward fold assumes finalized events form a linear causal chain. After a partition, the finalized event set contains two divergent lineages. The fold must be replaced with a single three-way merge for the partition case. Detection: if the finalized event set contains events from multiple partition contexts (disjoint causal chains), use `MERGE_FINALIZED` instead of the fold.

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
  protocol/messages.go  — gossip payload structs
transport/            — pluggable networking boundary (no Dolt logic)
  transport/transport.go      — control plane interfaces
  transport/provider.go       — data plane provider interfaces
  transport/provider_hint.go  — best-effort provider hints + retry exclusions via context
core/                 — synchronization engine and Dolt integration
  core/node.go              — Node (gossip loop, heartbeat, fetch+reconcile orchestration)
  core/reconciler_core.go   — merge orchestration via Merkle clock LCA + MergeRoots
  core/db.go / core/sql.go  — Dolt SQL driver wrapper + commit helpers
  core/swarm_chunk_store.go  — SwarmChunkStore (swarm-level read-only chunk store, Puller source)
  core/index.go / core/index_mem.go — in-memory Merkle clock (event DAG, heads, LCA)
specs/                — Quint formal specification (model-checkable with Apalache)
integration/          — demo + docker-based integration tests + transport implementations
  integration/main.go             — ddolt demo CLI used by tests
  integration/integration_test.go — container-per-peer test harness
  integration/transport/          — sample transports (gossipsub, grpcswarm, overlay)
```

## 9. Public Go API

Most consumers should use `Node` and treat transport as a plug-in.

### Core types

- `OpenNode(cfg NodeConfig) (*Node, error)` and `(*Node).Run(ctx)` start the background gossip+heartbeat+sync loops.
- `(*Node).Commit(msg)` and `(*Node).ExecAndCommit(exec, msg)` perform local writes and automatically publish events.
- `(*Node).Sync(ctx)` / `(*Node).SyncHint(ctx, hlc)` allow manual/one-shot sync passes (useful for CLIs/tests).

`NodeConfig` requires:
- `Repo` (`RepoID{Org, RepoName}`): logical repo identity.
- `Signer`: signs event metadata and provides `Verify` for remote signature checks.
- `Identity` (optional): if set, incoming events are verified against peer public keys; unverifiable/invalid events are rejected.
- `Transport`: provides (1) gossip message delivery (events, heartbeats, stale votes) and (2) a provider picker for the read-only data plane.

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
- A libp2p GossipSub control-plane is implemented in `integration/transport/gossipsub/`.
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
