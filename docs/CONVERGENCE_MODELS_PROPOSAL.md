# Alternative Convergence Models for DoltSwarm

This document analyzes the current linear convergence model and proposes alternative approaches that could converge faster while remaining leaderless and minimizing peer communication.

## Current Model Analysis

### How It Works

The current model uses **HLC-ordered linear replay**:

1. **Total Ordering via HLC**: Every commit gets a Hybrid Logical Clock timestamp `(Wall, Logical, PeerID)` that provides deterministic total ordering across all peers.

2. **Sync Flow**:
   - Receive commit advertisement via gossip
   - Fetch objects from swarm remote
   - Find merge base between local `main` and `remotes/swarm/main`
   - Collect all commits since merge base from both branches
   - Deduplicate by HLC (smallest hash wins ties)
   - Cherry-pick all commits in HLC order onto temp branch
   - Reset `main` to temp branch if it hasn't advanced

3. **Conflict Resolution**: First-Write-Wins (FWW) - conflicts drop the later commit deterministically.

### Performance Bottlenecks

| Bottleneck | Impact | Cost |
|------------|--------|------|
| Sequential cherry-pick | Each commit = DB operation | O(M) for M concurrent commits |
| Full index rebuild | Scans entire history after every sync | O(N) where N = total commits |
| History rewriting | Every sync rewrites post-merge-base | O(M) commits rewritten |
| Debounce latency | 250ms minimum before sync starts | Fixed overhead per sync |
| Restart on main advance | Replay aborts if local commit races | Full restart penalty |

### Convergence Time Estimate

For N peers with M concurrent commits:
- **Best case** (fast-forward): ~500ms-1s
- **Typical case** (replay): 5-15s for M=10-50 commits
- **Worst case** (conflicts + retries): 30s+

---

## Alternative Model 1: DAG Multi-Head with Lazy Linearization

### Concept

Instead of maintaining a single linear `main`, keep the commit DAG as-is with multiple heads (like a merge-pending state). Linearization happens:
- **On read** (query time) for consistency-sensitive reads
- **Periodically** in background for compaction
- **Never** for append-only/audit-log patterns

### How It Works

```
Peer A commits C1 ─┐
                   ├─► DAG has heads [C1, C2]
Peer B commits C2 ─┘

Sync: Just fetch + store objects. No replay needed.

Query: SELECT * FROM t
  → Materialize from virtual merge of [C1, C2]
  → Or pick "read head" using deterministic rule
```

### Trade-offs

| Pros | Cons |
|------|------|
| Instant convergence (fetch = done) | Complex query layer |
| No history rewriting | Storage overhead (DAG never compacts) |
| No cherry-pick cascade | Read latency increases with head count |
| Preserves all history | Harder to reason about "current state" |

### Communication

**Minimal change**: Same gossip (commit ads) + same data plane (Dolt fetch). No additional messages needed.

### Suitability

Best for: Audit logs, event sourcing, low-conflict workloads.
Poor for: High-conflict tables, simple query patterns, SQL compatibility.

---

## Alternative Model 2: Epoch-Based Batching

### Concept

Divide time into fixed epochs (e.g., 5-second windows). All commits within an epoch are processed as a single batch at epoch boundaries.

### How It Works

```
Epoch 0 (T=0-5s):
  Peer A: C1 @ T=1s
  Peer B: C2 @ T=2s
  Peer C: C3 @ T=4s

Epoch boundary (T=5s):
  All peers wait for stragglers (grace period)
  Sort [C1, C2, C3] by HLC
  Single batch cherry-pick
  Advance to epoch 1
```

### Trade-offs

| Pros | Cons |
|------|------|
| Single reconciliation per epoch | Fixed latency floor (epoch duration) |
| Batches cherry-picks efficiently | Requires loose time synchronization |
| Predictable convergence time | Grace period adds delay |
| Reduces sync frequency | Late commits may miss epoch |

### Communication

**New message**: `EpochBoundary { epoch_id, commit_list, head_hash }` for checkpoint agreement at epoch end. Adds ~1 message per peer per epoch.

### Suitability

Best for: Batch workloads, systems with NTP sync, predictable latency requirements.
Poor for: Real-time/low-latency needs, highly async environments.

---

## Alternative Model 3: Content-Based Conflict Detection

### Concept

Use lightweight conflict detection **before** cherry-picking:
- Compute "change set" (affected tables/rows) for each commit
- Batch commits with non-overlapping change sets
- Only serialize truly conflicting commits

### How It Works

```
Incoming commits:
  C1: UPDATE users SET name='X' WHERE id=1
  C2: UPDATE users SET name='Y' WHERE id=2
  C3: UPDATE orders SET status='done' WHERE id=5

Change sets:
  C1: {users:1}
  C2: {users:2}
  C3: {orders:5}

Conflict check: No overlaps!
  → Apply all in parallel (or single batch)
  → No sequential cherry-pick needed

If C4 arrives: UPDATE users SET name='Z' WHERE id=1
  C4: {users:1} conflicts with C1
  → Only C1 and C4 need ordering
```

### Trade-offs

| Pros | Cons |
|------|------|
| Parallel apply for non-conflicts | Requires change set extraction |
| Only orders what must be ordered | Bloom filter false positives |
| Preserves HLC semantics for conflicts | Additional metadata per commit |
| Major speedup for partitioned data | Complex for schema changes |

### Communication

**Extended commit ad**: Add bloom filter or compact change set to `CommitAdV1`. Adds ~200-500 bytes per commit.

### Suitability

Best for: Multi-tenant DBs, partitioned data, row-level changes.
Poor for: Schema changes, full-table operations, small tables.

---

## Alternative Model 4: Causal+ Consistency with Vector Clocks

### Concept

Replace HLC with vector clocks. Only order commits that have causal relationships. Concurrent independent commits don't need ordering.

### How It Works

```
Vector clock: map[peerID]→counter

Peer A commits C1: VC={A:1}
Peer B commits C2: VC={B:1}

C1 and C2 are concurrent (neither happened-before the other)
  → Both are equally valid "current state"
  → No replay needed

Peer A sees C2, commits C3: VC={A:2, B:1}
  → C3 causally follows both C1 and C2
  → C3 is the new head
```

### Trade-offs

| Pros | Cons |
|------|------|
| No ordering of independent commits | Vector clocks grow with peer count |
| Minimal replay (only causal chains) | More complex merge semantics |
| Preserves concurrency naturally | Harder to explain to users |
| Well-studied (Dynamo, Riak) | Conflict resolution still needed |

### Communication

**Modified commit ad**: Replace HLC with vector clock (O(N) bytes for N peers). For small clusters (N<100), acceptable.

### Suitability

Best for: Geo-distributed systems, eventually consistent reads OK.
Poor for: Strong consistency requirements, large peer counts.

---

## Alternative Model 5: Optimistic Accept + Background Linearization

### Concept

Accept all incoming commits immediately into an append-only log. Linearization runs as a separate background process.

### How It Works

```
Receive commit C1 → Append to local log (instant)
Receive commit C2 → Append to local log (instant)
...

Background process (every 5s or N commits):
  - Take snapshot of log
  - Sort by HLC
  - Cherry-pick to canonical main
  - Mark processed commits as "linearized"

Queries:
  - Hot path: Read from linearized main (consistent)
  - Best-effort: Read from tip of log (eventually consistent)
```

### Trade-offs

| Pros | Cons |
|------|------|
| Instant "soft" convergence | Two consistency levels |
| Background process can batch | Reads may lag writes |
| No blocking on sync | Log storage grows unbounded |
| Handles bursts well | Complex query routing |

### Communication

**No change**: Same gossip + data plane. Internal optimization only.

### Suitability

Best for: Write-heavy workloads, analytics (read lag OK).
Poor for: Real-time consistency needs, OLTP patterns.

---

## Alternative Model 6: Probabilistic Quorum Reads

### Concept

Instead of linearizing at write time, use quorum reads to establish consistency. Read from multiple peers and merge results.

### How It Works

```
Write: Local commit only + gossip (no sync)

Read: Query K peers (K < N)
  - Collect responses
  - Merge using deterministic rule (HLC max, LWW, etc.)
  - Return merged result

Consistency levels:
  - Eventual: K=1 (any peer)
  - Strong: K=N (all peers)
  - Quorum: K=N/2+1 (majority)
```

### Trade-offs

| Pros | Cons |
|------|------|
| No write-time linearization | Read amplification (K queries) |
| Tunable consistency | Requires peer-to-peer reads |
| Instant write convergence | Merge logic complexity |
| Well-known pattern (Cassandra) | Not pure "pull-only" model |

### Communication

**New capability**: Direct peer-to-peer read queries. Significant change to data plane.

### Suitability

Best for: Read-heavy workloads, tunable consistency needs.
Poor for: Complex queries, current pull-only architecture.

---

## Alternative Model 7: Operation-Log with Semantic Merge

### Concept

Store operations (SQL statements) rather than states. Replay operations with semantic understanding to merge intelligently.

### How It Works

```
Commit = { op: "INSERT INTO users VALUES (1, 'Alice')", hlc: ... }

Merge rules:
  - INSERT + INSERT same key → Keep first (FWW)
  - UPDATE + UPDATE same row → Merge fields or LWW
  - DELETE + UPDATE → DELETE wins (or configurable)
  - Non-overlapping ops → Commutative, order doesn't matter

Semantic analysis enables:
  - Parallel apply of non-conflicting ops
  - Automatic conflict resolution
  - Operation compression (10 UPDATEs → 1)
```

### Trade-offs

| Pros | Cons |
|------|------|
| Operations are naturally mergeable | Schema changes are hard |
| Can compress operation logs | Requires SQL parsing |
| Semantic conflict resolution | Complex implementation |
| Works with existing Dolt | Operation storage overhead |

### Communication

**Extended metadata**: Store SQL operation in commit metadata (already exists as content hash, could be expanded).

### Suitability

Best for: Known operation patterns, insert-heavy workloads.
Poor for: Arbitrary SQL, schema evolution, complex transactions.

---

## Comparison Matrix

| Model | Convergence Time | Communication | Complexity | Best For |
|-------|------------------|---------------|------------|----------|
| Current (HLC Replay) | 5-30s | Low | Medium | General purpose |
| DAG Multi-Head | ~0s (fetch only) | Low | High | Audit/append-only |
| Epoch Batching | Epoch duration | Medium | Low | Batch workloads |
| Content-Based | 1-5s | Low-Medium | Medium | Partitioned data |
| Vector Clocks | 1-5s | Medium | High | Geo-distributed |
| Optimistic Accept | ~0s soft, 5s hard | Low | Medium | Write-heavy |
| Quorum Reads | Write: 0s, Read: varies | High | High | Read-heavy |
| Operation Log | 1-5s | Low | High | Known patterns |

---

## Recommendations

### Quick Wins (Low effort, High impact)

1. **Incremental Index Updates**: Instead of full rebuild, track only new commits since last sync.
   - Impact: O(N) → O(M) where M << N
   - Effort: Modify `initIndexFromDB` to accept "since" parameter

2. **Parallel Change Set Check**: Before cherry-pick, compute change sets and batch non-conflicts.
   - Impact: O(M) sequential → O(1) + O(conflicts) sequential
   - Effort: Add change set extraction from Dolt diff

3. **Skip Metadata Rewrite for Local Commits**: Local commits already have correct metadata.
   - Impact: Saves one `DOLT_COMMIT --amend` per local commit in replay
   - Effort: Track which commits were local vs imported

### Medium-Term (Moderate effort)

4. **Epoch-Based Batching**: If latency floor is acceptable (e.g., 5s epochs).
   - Impact: Predictable convergence, batched operations
   - Effort: Add epoch tracking, gossip protocol changes

5. **Content-Based Conflict Detection**: For partitioned/multi-tenant use cases.
   - Impact: Major speedup for non-conflicting concurrent writes
   - Effort: Change set extraction, bloom filter in commit ads

### Long-Term (Architecture change)

6. **DAG Multi-Head**: If eventual consistency is acceptable.
   - Impact: Instant convergence for all cases
   - Effort: Major query layer changes

7. **Vector Clocks**: For true causal consistency.
   - Impact: Only orders what must be ordered
   - Effort: Replace HLC, change commit metadata schema

---

## Conclusion

The current HLC-ordered linear replay model prioritizes **correctness and simplicity** at the cost of **convergence speed**. For many use cases, this trade-off is acceptable.

For faster convergence while remaining leaderless and minimizing communication, the most promising approaches are:

1. **Content-Based Conflict Detection** - Biggest bang for buck. Most concurrent writes don't actually conflict, so detecting this early avoids expensive sequential cherry-picks.

2. **Incremental Index Updates** - Pure optimization, no semantic changes. Should be done regardless of other changes.

3. **Epoch Batching** - If predictable latency is more important than minimum latency. Simple to implement, well-understood trade-offs.

The choice depends on workload characteristics:
- High-conflict, real-time: Stick with current model + optimizations
- Partitioned data, low-conflict: Content-Based Detection
- Batch/analytics: Epoch Batching
- Audit/append-only: DAG Multi-Head
