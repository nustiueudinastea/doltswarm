# DoltSwarm (Current Design Notes)

This file is maintained as a high-level overview of the **current** DoltSwarm pipeline implemented in this repo.
The source of truth for behavior is the code in the root package (`node.go`, `db.go`, `reconciler_core.go`) and `README.md`.

## Architecture Overview

DoltSwarm is a peer-to-peer synchronization library for Dolt databases. The current design targets:

- **Pull-only sync**: peers only read from other peers; no remote writes are triggered.
- **Local writes only**: all SQL mutations and commits happen locally.
- **Deterministic ordering** using **Hybrid Logical Clocks (HLC)** so peers converge to the same linear `main`.
- **No merge commits**: when histories cross, peers deterministically replay (cherry-pick) commits in HLC order.
- **First write wins** on conflict: later conflicting commits are dropped deterministically.
- **Skew guard**: future-skewed HLC adverts are rejected (`MaxClockSkew`).
- **Anti-entropy repair**: periodic digests/checkpoints trigger sync even if commit adverts are missed.

### Control plane vs data plane

- **Control plane (gossip)**: disseminates small typed messages:
  - `CommitAdV1` (commit advertisement)
  - `DigestV1` (anti-entropy summary)
- **Data plane (Dolt-native fetch)**: uses Dolt’s optimized transfer pipeline by maintaining a read-only remote named `swarm` whose URL is `swarm://<org>/<repo>` (or `swarm://<repo>`).

The core library does not “address peers”. Provider selection is an implementation detail of the transport.

## Key Components (Code Map)

- `Node` (`node.go`): embed this in an app; runs gossip ingestion + periodic repair and drives fetch/reconcile.
- `DB` (`db.go`, `sql.go`): wraps the Dolt SQL driver and provides:
  - local commit helpers that embed signed metadata in commit messages
  - `EnsureSwarmRemote`, `FetchSwarm`, `MergeBase`, `GetBranchHead`
- `Reconciler` (`reconciler_core.go`): deterministic linearizer that replays commits in HLC order on a temp branch.
- `CommitMetadata` (`metadata.go`): JSON stored in the commit message: message, HLC, content hash, author/email/date, signature.
- `swarm://` dbfactory (`swarm_dbfactory.go`, `swarm_registry.go`, `remote_chunk_store.go`):
  - lets Dolt treat “the swarm” as a remote by selecting any live provider and serving chunks/table files read-only.
- Local index (`index.go`, `index_mem.go`): local-only state to support digests/checkpoints and “pull-first”.

## How commits disseminate and reconcile

### Local commit path

`Node.Commit` / `Node.ExecAndCommit`:
1. Runs a short **pull-first** best-effort sync (`PullFirstTimeout`, `PullFirstPasses`) to reduce later replays.
2. Creates a local commit on `main` with message = `CommitMetadata` JSON (signed).
3. Publishes a `CommitAdV1` via `Transport.Gossip()` containing:
   - repo id
   - HLC
   - metadata json + signature

### Receiving an advert

`Node.Run` ingests gossip events:
- For `CommitAdV1`:
  - rejects adverts with future-skewed HLC wall time
  - checks metadata JSON parses and HLC matches
  - **TODO**: verify remote metadata signatures via `NodeConfig.Identity`
  - marks the HLC as pending in the local index and triggers a debounced sync pass
- For `DigestV1`:
  - triggers sync if the remote head is ahead, or if checkpoints reveal a gap

### Sync pass (fetch → reconcile)

`Node.syncOnce`:
1. Ensures there is a Dolt remote named `swarm` pointing at `swarm://...`.
2. Calls `DOLT_FETCH('swarm')` (data plane).
3. Computes merge base of `main` and `remotes/swarm/main`.
4. Tries the cheap path first: fast-forward-only merge to remote head.
5. If not fast-forwardable, runs deterministic replay via `Reconciler.ReplayImported`:
   - collect commits after merge base from local `main` and the fetched remote ref
   - dedupe + sort by HLC
   - cherry-pick onto a temp branch (`replay_tmp`) and rewrite metadata so replay is identical everywhere
   - if a cherry-pick conflicts, drop the later commit (FWW)
   - update `main` only if it did not advance during replay (restart-safe)

## Commit metadata and signatures

- Each commit message is JSON metadata (`CommitMetadata`) signed by the author.
- Commit-hash tag signatures exist but are **secondary** (commit hashes can change during replay).
- Remote signature verification is planned via `NodeConfig.Identity` and is not fully enforced yet.

## Integration demo and tests

Integration scaffolding lives in `integration/`:
- `integration/main.go` contains the `ddolt` demo used by tests.
- A libp2p GossipSub control-plane is implemented in `integration/transport/gossipsub/`.
- A provider-based data-plane is implemented in `integration/transport/grpcswarm/` by exposing Dolt’s read-only chunk store + a downloader stream.

To run Docker-based integration tests, use the Taskfile (`Taskfile.yaml`):
- `task integration`
- `task integration:quick`
