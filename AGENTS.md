# DoltSwarm Library

## Architecture Overview

DoltSwarm is a peer-to-peer synchronization library for Dolt databases. The current design targets **linear, merge-free history** using Hybrid Logical Clocks (HLC) so that all peers converge to the **same commit hashes** without centralized coordination.

### Core Principles

1. **Pull-only sync**: Peers only PULL; no remote writes.
2. **Local writes only**: Every mutation happens locally, then is advertised.
3. **Deterministic ordering (HLC)**: Commits carry HLC (wall, logical, peerID). Reordering uses HLC order everywhere.
4. **Pull-first**: Before committing, a peer pulls any known earlier HLC commits to avoid reordering.
5. **Linear history, no merge commits**: If commits cross, peers replay via cherry-pick **with original author/date/message**, producing identical hashes.
6. **First Write Wins on conflict**: Cherry-pick conflicts drop the later commit; dropped commits are marked applied and notified.
7. **Crash safety**: Replays happen on a temp branch; `main` is fast-forwarded atomically. If replay fails or main advances, restart from the new merge base—never force-reset main.
8. **Skew guard**: Reject adverts whose HLC wall time is more than `MaxClockSkew` (e.g., 5s) in the future.
9. **Advertisement repair**: Periodic `RequestCommitsSince` (by max applied HLC) fills gaps from missed adverts.

### Data Flow (new pipeline)

```
Local commit (HLCx) → AdvertiseCommit(HLCx, metadata, signature)
Peer receives advert:
  - Verify metadata signature; reject future-skew HLC
  - If HLCx < local HEAD HLC → reorder on temp branch in HLC order; conflicts drop the commit
  - Else pull/fast-forward
Periodic: RequestCommitsSince(max-applied HLC) to heal missed adverts
```

## Key Components

### DB (db.go)
- Manages SQL connection, peer clients (gRPC), event queue, and reconciler.
- `Commit/ExecAndCommit`: pull-first, commit on `main` with HLC metadata/signature, then AdvertiseCommit.

### Reconciler (reconciler.go)
- Queue commits by HLC; pull earlier commits pre-commit; reorder earlier adverts via temp-branch replay with original metadata; fast-forward main.
- Marks rejected/already-present commits as applied to avoid livelock.

### Event Processing (distributed.go)
- `AdvertiseCommit` broadcast; `RequestCommitsSince` periodic repair.
- Legacy `AdvertiseHead/RequestHead` paths are removed in the linear pipeline.

### Syncer (syncer.go)
- `AdvertiseCommit` handler: validate skew, verify metadata signature, enqueue, then handle (pull or reorder).
- `RequestCommitsSince` handler: returns commits after a given HLC in batches.

### SQL Interface (sql.go)
- Commits carry JSON metadata (message, HLC, content_hash, author, email, date) signed by the author; metadata signature is authoritative.
- Tag on commit hash is optional; hash can change on replay, but metadata remains verifiable.

### Replay Logic
1. Fetch peer branch, find merge base.
2. Reset temp branch to base.
3. Cherry-pick `--no-commit` each commit in HLC order; `DOLT_COMMIT` with original metadata; conflicts → drop + mark applied.
4. Fast-forward main to temp; delete temp. If main moved, restart from new base.

### Remote Chunk Store
Read-only operations only (`Get`, `GetMany`, `Has`, `HasMany`, `Root`, `Rebase`, `Sources`, `Size`). No remote writes.

## Commit Verification

1. Each commit's metadata is signed; verification happens on receive before enqueue.
2. Dropped/invalid adverts are marked applied to keep queues draining.
3. Optional commit-hash tag signatures are secondary and may be invalidated by replay.

---

# Integration Tests

The integration test suite is located in `integration/` and validates that P2P database synchronization works correctly. Tests run in Docker containers (one container per peer).

## Test Cases

| Test | Purpose |
|------|---------|
| `TestIntegration` | Basic init + convergence + late joiner |
| `TestSequentialWritesPropagation` | Sequential commits from one peer propagate |
| `TestConcurrentWrites` | Simultaneous writes converge deterministically |
| `TestCommitOrderConsistency` | Round-robin writes keep consistent order |

## Proposed Test Additions

| Test | Purpose |
|------|---------|
| `TestLinearNoMergeCommits` | Assert no merge commits are created in the new pipeline |
| `TestHashStabilityAcrossPeers` | Same HLC chain yields identical commit hashes on all peers after replay |
| `TestConflictDropMarkedApplied` | Induce row-level conflict; ensure losing commit is dropped, marked applied, and callback fired |
| `TestReplayCrashSafety` | Simulate crash during temp-branch replay; main must remain intact; second run converges |
| `TestFutureSkewRejection` | Advert with future HLC (>MaxClockSkew) is rejected and marked applied |
| `TestMissingAdvertRepair` | Drop adverts for some peers; `RequestCommitsSince` heals and converges |
| `TestDebounceBatching` | Burst of near-simultaneous commits triggers a single replay batch (no O(N²) reorders) |

## Key Verification Points

1. **Linear history**: No merge commits exist in `main`.
2. **Hash equality**: All peers share identical commit hashes in the same order.
3. **Conflict determinism**: Conflicting commits are dropped identically on all peers and marked applied.
4. **Crash safety**: Crashes during replay do not corrupt `main`; rerun converges.
5. **Skew guard**: Future-skew adverts are rejected consistently.
6. **Repair**: Missed adverts are healed by `RequestCommitsSince`.

## Running Tests

```bash
# Build Docker image and run all tests
task integration

# Run tests without rebuilding Docker image (faster iteration)
task integration:quick

# Run with custom number of instances
NR_INSTANCES=10 task integration

# Keep containers after test for log inspection
task integration KEEP=1

# Inspect logs
task docker:logs

# Clean up
task docker:clean
```

## Taskfile Commands

| Command | Description |
|---------|-------------|
| `task build` | Build the ddolt binary |
| `task integration` | Build Docker image and run all integration tests |
| `task integration:quick` | Run integration tests without rebuilding Docker image |
| `task docker:build` | Build the Docker image |
| `task docker:logs` | Show logs from test containers |
| `task docker:ps` | List running test containers |
| `task docker:clean` | Clean up Docker containers/networks/images |
| `task clean` | Clean test artifacts and kill processes |
