# Alternative Convergence Models for DoltSwarm

This document analyzes the current linear convergence model and proposes alternative approaches that could converge faster while remaining leaderless and minimizing peer communication.

## Understanding the Core Problem

### Dolt's Commit Model

Dolt is Git-like: commits form a DAG where each commit's hash depends on:

```
commit_hash = hash(tree_hash, parent_hash(es), author, date, message)
```

**Critical constraint**: If the parent changes, the commit hash changes.

This means for all peers to have **identical commit histories**, they must:
1. Apply the same set of commits
2. In the **exact same order** (determining parent relationships)
3. With identical metadata

### Why Ordering Matters

```
Scenario: Peers A and B both commit concurrently from base B0

Peer A's local view:          Peer B's local view:
  B0 → CA                       B0 → CB

After sync, both need the same linear history. Two options:

Option 1: B0 → CA → CB'        (CA is parent of CB')
Option 2: B0 → CB → CA'        (CB is parent of CA')

The ' indicates the commit hash changed because the parent changed.
```

All peers must pick the **same option** deterministically. The current system uses HLC ordering: lower HLC wins, so if `HLC(CA) < HLC(CB)`, everyone picks Option 1.

### What "Convergence" Means

**Convergence** = all peers have:
- Same `main` branch HEAD commit hash
- Same commit ancestry (parent chain)
- Same tree content at HEAD

The current model achieves this via **deterministic replay**: cherry-pick all commits in HLC order onto a temp branch, then move `main` to the result.

---

## Current Model Analysis

### The HLC-Ordered Linear Replay

```
1. Receive commit advertisement via gossip
2. Fetch objects from swarm:// remote (Prolly tree chunks)
3. Find merge base between local main and remote main
4. Collect all commits since merge base (local + remote)
5. Deduplicate by HLC (smallest commit hash breaks ties)
6. Sort by HLC: (wall_time, logical_counter, peer_id)
7. Cherry-pick each commit in order onto temp branch
8. Amend each cherry-pick to preserve original metadata
9. Move main to temp branch result
```

### Why It's Slow

| Operation | Cost | Why Expensive |
|-----------|------|---------------|
| Cherry-pick | O(M) sequential | Each cherry-pick: read tree, apply diff, write new tree, create commit |
| Metadata amend | O(M) sequential | Each amend: rewrite commit with original author/date/message |
| Index rebuild | O(N) | Scan entire commit history, parse JSON metadata for each |
| Restart on race | Full replay | If local commit happens during replay, restart from scratch |

For M concurrent commits on a history of N total commits:
- **Best case (fast-forward)**: ~500ms
- **Typical (replay)**: 5-15s for M=10-50
- **Worst case**: 30s+ with conflicts/retries

### The Fundamental Bottleneck

The bottleneck is **not** reaching agreement on ordering (gossip is fast). It's **executing the replay**:

```
For each of M commits:
  1. DOLT_CHERRY_PICK(hash)           -- applies diff to Prolly tree
  2. DOLT_COMMIT('--amend', ...)      -- rewrites commit metadata
```

Each operation involves Prolly tree traversal and modification. With Dolt's storage model, this is inherently sequential because each commit's tree depends on the previous commit's tree.

---

## Alternative Convergence Models

### Model 1: Deterministic N-Way Merge Instead of Linear Replay

#### Concept

Instead of cherry-picking M commits sequentially to create M new commits, create a **single merge commit** that combines all concurrent commits.

```
Current approach (M=3):
  B0 → C1' → C2' → C3'     (3 cherry-picks, 3 new commits)

Merge approach:
  B0 → C1 ─┐
  B0 → C2 ─┼─► M           (1 merge commit with 3 parents)
  B0 → C3 ─┘
```

#### How It Would Work

```
1. Collect all commits since merge base (same as now)
2. Sort by HLC to get deterministic parent order
3. Create single merge commit:
   - Parents: [merge_base, C1, C2, C3, ...] in HLC order
   - Tree: result of deterministic N-way merge
   - Message: canonical metadata with combined info
4. Move main to merge commit
```

#### Dolt Considerations

- Dolt supports merge commits with multiple parents
- The tree content must be computed deterministically (same merge algorithm everywhere)
- **Key question**: Is Dolt's N-way merge deterministic? If `DOLT_MERGE` produces the same tree given the same inputs, this works.

#### Trade-offs

| Pros | Cons |
|------|------|
| O(1) commits instead of O(M) | Complex merge tree computation |
| Preserves original commit hashes as parents | Dolt merge may not be N-way deterministic |
| Single Prolly tree write | Merge conflicts harder to handle |
| History shows true concurrency | Non-linear history (some tools struggle) |

#### Communication

**No change**: Same gossip + data plane.

---

### Model 2: Stable Commit Identity with Deferred Parent Assignment

#### Concept

Separate the **stable identity** (content + HLC) from the **transient identity** (commit hash with parent).

```
Commit = {
  stable_id:   hash(content_tree, hlc, author, message)  // Never changes
  commit_hash: hash(tree, parent, metadata)              // Changes during replay
}
```

Peers track commits by `stable_id`. The actual commit hash is a local implementation detail that converges once everyone has applied the same commits in the same order.

#### How It Would Work

```
1. When committing locally:
   - Compute stable_id from content
   - Create local commit (commit_hash depends on local parent)
   - Gossip: advertise stable_id

2. When syncing:
   - Fetch commits by stable_id
   - Have all stable_ids I need? Yes → ready to finalize
   - Sort by HLC, replay to get final commit_hash
   - Index tracks stable_id → commit_hash mapping

3. Fast path optimization:
   - If my local history already has all stable_ids in correct HLC order
   - AND parent relationships match expected order
   - THEN no replay needed (already converged)
```

#### Dolt Considerations

The `stable_id` could be:
- `hash(tree_hash, hlc)` - content + logical time
- Already exists as `ContentHash` in current metadata

This doesn't reduce replay work, but enables **detecting when replay is unnecessary**.

#### Trade-offs

| Pros | Cons |
|------|------|
| Clear separation of concerns | Still requires replay |
| Enables fast convergence detection | Additional tracking overhead |
| Works with current Dolt primitives | Complexity in index management |

---

### Model 3: Epoch-Based Batch Commits

#### Concept

Divide time into epochs. All commits within an epoch are combined into a **single epoch commit** at the epoch boundary.

```
Epoch 0 [T=0s to T=5s]:
  Peer A: op1 @ T=1s
  Peer B: op2 @ T=3s
  Peer C: op3 @ T=4s

Epoch boundary (T=5s):
  All peers create: E0 = commit(merge(op1, op2, op3))
  Parent: previous epoch commit or genesis
```

#### How It Would Work

```
1. Within epoch: accumulate operations locally (don't commit yet)
2. At epoch boundary:
   - Gossip: exchange operation lists
   - Wait for grace period (handle stragglers)
   - Deterministic merge: sort operations by HLC
   - Create single epoch commit
   - All peers create identical commit (same tree, same parent)

3. Reads during epoch:
   - Option A: Read from last epoch commit (stale)
   - Option B: Apply pending ops speculatively (may rollback)
```

#### Dolt Considerations

- Reduces commit count: O(M) operations → 1 commit per epoch
- Prolly tree writes batched: single tree update per epoch
- **Requires operation-level tracking** rather than commit-level

This is a significant model change: instead of "commits sync between peers", it's "operations sync, then everyone commits identically".

#### Trade-offs

| Pros | Cons |
|------|------|
| O(1) commits per epoch regardless of M | Fixed latency floor (epoch duration) |
| All peers create identical commits | Requires operation log |
| No replay needed | Grace period for stragglers |
| Batched Prolly tree updates | Changes commit granularity |

#### Communication

**New message**: Operation advertisement instead of (or in addition to) commit advertisement.

---

### Model 4: Prolly Tree Delta Shipping

#### Concept

Instead of replaying commits (which recomputes Prolly tree changes), ship the **computed deltas** directly.

```
Current:
  Peer A computes: tree(B0) + diff(C1) → tree(C1)
  Peer B fetches C1, recomputes: tree(B0) + diff(C1) → tree(C1')

  If parents differ, trees may differ even with same diff.

Delta shipping:
  Peer A computes tree for canonical order
  Peer A ships: "after commit X, tree_hash = Y, chunks = [...]"
  Peer B receives and validates, adopts tree directly
```

#### How It Would Work

```
1. Designated "fast peer" (first to complete replay) computes canonical tree
2. Gossips: TreeAnnouncement { hlc_tip, tree_hash, chunk_manifest }
3. Other peers:
   - Fetch chunks if needed
   - Validate: replay first K commits, check intermediate tree matches
   - If valid, adopt tree (skip remaining replay)
   - Create commit pointing to validated tree
```

#### Dolt Considerations

- Prolly trees are content-addressed: same tree_hash = same content
- Chunks are already shipped via data plane
- This is essentially "trust but verify" for tree computation

**Risk**: A malicious peer could ship wrong tree. Mitigations:
- Spot-check validation (replay random commits)
- Signature over tree_hash from commit author
- Eventual full validation in background

#### Trade-offs

| Pros | Cons |
|------|------|
| Skip replay for followers | Requires validation strategy |
| Leverages Prolly tree properties | First peer still does full replay |
| Works with existing chunk shipping | Trust model changes |

#### Communication

**New message**: TreeAnnouncement with tree hash and chunk manifest.

---

### Model 5: Causal Ordering with Lazy Linearization

#### Concept

Don't require linear history. Allow concurrent commits to coexist as multiple heads. Linearize only when necessary.

```
Traditional (forces linear):
  B0 → C1 → C2 → C3

Causal (allows DAG):
  B0 → C1 (from A)
   └→ C2 (from B, concurrent with C1)
   └→ C3 (from C, concurrent with C1 and C2)

All three are valid "current state" until someone needs a single answer.
```

#### How It Would Work

```
1. Commits include causality info:
   - seen_heads: [commit hashes this commit has "seen"]
   - If C2.seen_heads doesn't include C1, they're concurrent

2. Sync:
   - Fetch all commits (as now)
   - No replay needed - just store in DAG
   - Track multiple heads

3. Query time:
   - Option A: Pick deterministic head (max HLC)
   - Option B: Merge heads on-the-fly for query
   - Option C: Require explicit linearization point

4. Periodic compaction:
   - Background: linearize old concurrent commits
   - Recent: keep as DAG for fast sync
```

#### Dolt Considerations

- Dolt's branch model already supports multiple heads
- Could use branches internally: `main` (linearized), `pending/*` (unlinearized)
- Queries against `main` give consistent view
- Sync updates `pending/*` branches instantly

#### Trade-offs

| Pros | Cons |
|------|------|
| Instant sync (no replay) | Complex query layer |
| Preserves true concurrency | Multiple "current states" |
| Background linearization | Storage overhead |
| Scales with write rate | Harder to reason about |

#### Communication

**Extended commit ad**: Include `seen_heads` for causality tracking.

---

### Model 6: Optimistic Locking with Rollback

#### Concept

Apply commits optimistically without replay. If conflict detected later, rollback and replay correctly.

```
1. Receive commit C2 while at C1
2. Optimistic: append C2 as child of C1 (assume compatible)
3. Later: discover C0 should come between C1 and C2
4. Rollback: reset to C1
5. Replay: C1 → C0' → C2'
```

#### How It Would Work

```
1. On commit receive:
   - Fast path: if HLC > all known HLCs, just append (likely correct)
   - Store speculative commit chain

2. On discovering older commit:
   - Mark speculation as invalid
   - Trigger replay from last known-good point

3. Convergence:
   - Eventually all commits discovered
   - All peers replay to same final state
   - Speculation useful during "hot" period
```

#### Dolt Considerations

- Requires cheap rollback (Dolt branches provide this)
- Could use: `main` (speculative), `canonical` (known-good)
- Queries default to `main` (fast, possibly wrong)
- Critical queries use `canonical` (slow, guaranteed correct)

#### Trade-offs

| Pros | Cons |
|------|------|
| Fast path for in-order commits | Rollback cost when wrong |
| Good for mostly-ordered workloads | Two consistency levels |
| Progressive convergence | Wasted work on rollback |

---

## Comparison Matrix (Dolt-Aware)

| Model | Replay Cost | Commit Identity | Communication | Dolt Changes |
|-------|-------------|-----------------|---------------|--------------|
| Current (Linear) | O(M) cherry-picks | Hash changes | Low | None |
| N-Way Merge | O(1) merge | Originals preserved | Low | Merge algorithm |
| Stable ID | O(M) but skippable | Separate stable/hash | Low | Index changes |
| Epoch Batching | O(1) per epoch | New model | Medium | Operation log |
| Delta Shipping | O(1) for followers | Hash from leader | Medium | Trust model |
| Causal DAG | O(1) sync | Multiple heads | Low | Query layer |
| Optimistic | O(M) on rollback | Speculative | Low | Dual branch |

---

## Recommendations for Dolt-Based System

### Understanding the Constraints

1. **Commit hashes must match** for true convergence
2. **Parent determines hash** so ordering is mandatory
3. **Prolly tree writes are expensive** (the real bottleneck)
4. **Content-addressed chunks** can be shared (already optimized)

### Most Promising Approaches

#### 1. N-Way Merge (if Dolt supports deterministic N-way)

**Why**: Reduces O(M) sequential cherry-picks to O(1) merge operation. The Prolly tree is written once, not M times.

**Investigation needed**: Does `DOLT_MERGE` with multiple branches produce deterministic output? If yes, this is the fastest path.

**Estimated improvement**: M concurrent commits → single merge instead of M cherry-picks. 10x+ speedup for M=10.

#### 2. Epoch Batching (architectural change)

**Why**: Changes the model from "sync commits" to "sync operations, then commit identically". All peers create the same commit with the same parent and same tree.

**Trade-off**: Adds latency floor (epoch duration) but guarantees O(1) commits per epoch.

**Estimated improvement**: Predictable convergence time (epoch duration + grace period), regardless of M.

#### 3. Delta Shipping (trust-but-verify)

**Why**: First peer to complete replay computes the canonical tree. Others adopt it directly, skipping replay entirely.

**Trade-off**: Requires validation strategy to prevent malicious tree injection.

**Estimated improvement**: Only 1 peer does full replay. Others converge in O(fetch time).

### Quick Wins (No Architecture Change)

#### A. Detect Fast-Forward Earlier

Current code tries fast-forward after computing merge base. If HLCs are totally ordered (no true concurrency), fast-forward always works.

```go
// Before fetching, check if all pending HLCs are > local head HLC
// If yes, guaranteed fast-forward - skip replay setup
```

#### B. Batch Cherry-Picks

If Dolt supports applying multiple diffs before committing:

```go
// Instead of:
for commit := range commits {
    DOLT_CHERRY_PICK(commit)  // writes tree
    DOLT_COMMIT('--amend')    // writes commit
}

// Try:
DOLT_CHECKOUT('replay_tmp')
for commit := range commits {
    DOLT_CHERRY_PICK('--no-commit', commit)  // stage changes only
}
DOLT_COMMIT(...)  // single tree write, single commit
```

**Investigation needed**: Does Dolt support `--no-commit` for cherry-pick?

#### C. Parallel Fetch + Speculative Replay

Start replay while still fetching:

```go
go fetch(remaining_commits)
replay(already_fetched_commits)  // may need adjustment later
```

---

## Conclusion

The fundamental challenge is that **Dolt's commit hash depends on parent hash**, forcing sequential ordering for convergence. The current HLC-ordered replay is correct but slow.

The most impactful improvements, in order of feasibility:

1. **Investigate N-way merge** - If deterministic, provides O(M)→O(1) improvement with minimal changes

2. **Delta shipping** - Let one peer do the work, others adopt the result. Requires trust/validation model.

3. **Epoch batching** - Architectural change that guarantees bounded convergence time.

4. **Causal DAG** - Most flexible but requires significant query layer changes.

The choice depends on:
- **Latency requirements**: Need sub-second? Avoid epoch batching.
- **Trust model**: Can peers trust each other's trees? Enables delta shipping.
- **Write patterns**: Mostly ordered? Optimistic locking helps. Highly concurrent? N-way merge or epochs.

For a leaderless system with minimal communication, **N-way merge** (if feasible in Dolt) or **epoch batching** offer the best convergence/communication trade-off.
