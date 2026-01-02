# DoltSwarm Testing Guide

This repository targets a **linear, merge-free, HLC-ordered** sync pipeline. Tests must assert identical commit hashes across peers, no merge commits, deterministic conflict drops, crash-safe replay, skew guarding, and advert-repair.

**Policy:** Integration tests run *only* in Docker (one container per peer). Process-mode integration runners are deprecated and should be removed.

## Test Matrix

| Layer | Focus | How to run |
|-------|-------|------------|
| Unit  | HLC ordering, skew guard, metadata signing/verify, queue semantics, debounce logic | `go test ./...` (non-docker packages) |
| Integration (docker, 1 container/peer) | Linear history, hash equality, conflict drop, replay crash safety, skew rejection, advert repair, debounce | `task integration` (builds image, runs all tests with `-tags docker`) |

## Required Flags/Deps
- ICU flags (arm64/mac):  
  `CGO_LDFLAGS="-L/opt/homebrew/opt/icu4c@78/lib" CGO_CPPFLAGS="-I/opt/homebrew/opt/icu4c@78/include" CGO_CFLAGS="-I/opt/homebrew/opt/icu4c@78/include"`
- Docker daemon for container tests.
- `task` runner for Taskfile targets (optional convenience).

## Unit Tests (additions to cover new pipeline)
- HLC: comparison, update, future-skew rejection.
- Metadata: serialize/deserialize with author/date; signature verification success/fail.
- Queue: ordered insert, GetPendingBefore/After, mark-applied on reject/duplicate.
- Debounce: burst adverts yield one reorder trigger (counter-based).

## Integration Tests (docker only)
Suite includes existing cases plus new edge cases:
1) `TestLinearNoMergeCommits` – assert zero merge commits on `main` across all peers.  
2) `TestHashStabilityAcrossPeers` – concurrent/reordered commits end with identical hash lists everywhere.  
3) `TestConflictDropMarkedApplied` – force same-row writes; losing commit is dropped, marked applied, and callback observed.  
4) `TestReplayCrashSafety` – kill during temp-branch replay; main untouched; rerun converges.  
5) `TestFutureSkewRejection` – advert with HLC > MaxClockSkew ahead is rejected/marked applied everywhere.  
6) `TestMissingAdvertRepair` – suppress adverts, rely on `RequestCommitsSince` to heal and converge.  
7) `TestDebounceBatching` – burst of N commits triggers ≤1 reorder per peer (use exposed counter).  
8) `TestInvalidSignatureRejection` – bad metadata signature rejected and marked applied.  
9) `TestMainAdvancesDuringReplay` – advance main mid-replay; replay restarts from new merge base and converges.

### How to run (single target)
```bash
task integration        # builds docker image and runs all integration tests with -tags docker
```
Env knobs:
- `NR_INSTANCES` (default 5) passed through Taskfile
- Adjust `TIMEOUT` via `task integration TIMEOUT=25m`

## Integration Tests (docker)
- Container-per-peer only (Taskfile `integration` target). Process-mode integration runners are removed.

## Observability / Hooks for tests
- Expose counters from reconciler (optional helpers for tests): `reorder_runs`, `debounce_triggers`, `conflicts_dropped`, `adverts_rejected_skew`, `adverts_rejected_sig`.
- Provide `MaxClockSkew` config for tests.

## Flakiness/Timeouts
- Default convergence timeout: 120s; poll 500ms; progress log 10s; per-RPC 10s.
- For docker runs, allow longer timeouts (network startup).
- If a test times out, log responsive vs unresponsive peers, head distribution, and commit-count distribution (already in harness).

## Cleanup
- Remove/ignore legacy head-based tests and any process-mode integration runners.
- Ensure temp directories/containers are removed unless `KEEP_TEST_DIR` is set.
