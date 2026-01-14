# DoltSwarm

DoltSwarm is a peer-to-peer synchronization library for [Dolt](https://www.dolthub.com/) databases.
It is designed to make a group of peers converge to the **same linear `main` history** (no merge commits) using a **total order based on Hybrid Logical Clocks (HLC)**, without requiring a central coordinator.

## What This Repo Implements (Today)

- **Pull-only sync**: a peer never causes remote writes; it only reads remote objects and updates its own local `main`.
- **Local writes**: mutations happen locally via SQL + `DOLT_COMMIT`.
- **Deterministic ordering**: commits carry an HLC timestamp and are ordered by `(wall, logical, peerID)` everywhere.
- **Deterministic reconciliation**: when concurrent commits “cross”, peers deterministically replay (cherry-pick) in HLC order so history converges.
- **Finalized prefix (watermark)**: peers compute a conservative “finalized base” from digests/checkpoints and avoid rewriting history at or before that point (tail-only replay), reducing replay churn as history grows.
- **First write wins (FWW) on conflict**: later conflicting commits are dropped deterministically.
- **Offline commit resubmission**: commits created while offline that are “too old” relative to the network watermark are re-submitted as new commits with provenance instead of attempting to rewrite finalized history (enabled by default).
- **Skew guard**: adverts with HLC wall time too far in the future are rejected (`MaxClockSkew`).
- **Anti-entropy repair**: periodic digests/checkpoints trigger re-sync even if commit adverts are missed.

## Architecture

At a high level there are two planes:

- **Control plane (gossip)**: small messages that advertise “there is a commit with HLC X” and periodically summarize history.
- **Data plane (Dolt-native fetch)**: bulk object transfer is done through Dolt’s existing fetch pipeline, using a read-only `swarm://` remote.

## Code Structure

This repo is organized into small packages so the “what” (protocol), “how” (core engine), and “I/O boundary” (transport) are separated.
The top-level `doltswarm` package remains the public import path and re-exports the main API for convenience.

- `doltswarm.go`: public facade (type aliases + wrapper functions) so consumers can keep importing `github.com/nustiueudinastea/doltswarm`.
- `protocol/`: wire- and identity-level types used across the system:
  - HLC (`protocol/hlc.go`), repo identity (`protocol/repo_id.go`)
  - commit metadata format + signing (v2 metadata with optional `kind`/`origin`) (`protocol/metadata.go`, `protocol/signer.go`)
  - gossip payload structs (`protocol/messages.go`)
- `transport/`: the pluggable networking boundary (no Dolt logic here):
  - control plane interfaces (`transport/transport.go`)
  - data plane provider interfaces (`transport/provider.go`)
  - best-effort provider hint via context (`transport/provider_hint.go`)
- `core/`: the synchronization engine and Dolt integration:
  - `core/node.go`: `Node` (gossip loop, debounce, fetch+reconcile orchestration)
  - `core/reconciler_core.go`: deterministic linearizer (temp-branch replay + conflict drops)
  - `core/finalization.go`: watermark computation + finalized base helpers (tail-only replay)
  - `core/db.go` / `core/sql.go`: Dolt SQL driver wrapper + commit helpers (`DOLT_COMMIT`, `DOLT_FETCH`, merge-base helpers)
  - `core/swarm_dbfactory.go` / `core/swarm_registry.go`: `swarm://` dbfactory registration + provider picker registry
  - `core/remote_chunk_store.go` / `core/remote_table_file.go`: read-only chunk store/table-file adapters used by Dolt’s fetch pipeline
  - `core/index.go` / `core/index_mem.go`: local-only commit index used for pending/applied/rejected HLC tracking, digest checkpoints, finalized base, and resubmission idempotency
  - `core/bundles.go`: (reference/experimental) commit-bundle builder/importer; current pipeline primarily uses Dolt-native fetch
- `integration/`: demo + docker-based integration tests and transport implementations (libp2p gossip + gRPC chunkstore):
  - `integration/main.go`: `ddolt` demo CLI used by tests
  - `integration/integration_test.go`: container-per-peer test harness
  - `integration/transport/`: sample transports (`gossipsub`, `grpcswarm`, `overlay`)

### Key Components

- `Node` (`core/node.go`): the sync engine you embed; runs gossip loops, performs fetch/reconcile, and exposes `Commit` / `ExecAndCommit`.
- `DB` (`core/db.go`, `core/sql.go`): thin wrapper around the Dolt SQL driver; creates signed metadata commits; provides helpers like `FetchSwarm`, `MergeBase`.
- `Reconciler` (`core/reconciler_core.go`): deterministic linearizer (no networking). Given local+imported commits after a merge base, it produces a linear `main`.
- `CommitMetadata` (`protocol/metadata.go`): JSON stored in the Dolt commit message, including `(HLC, author, date, content_hash)` plus a signature.
- Resubmission provenance (`protocol/metadata.go`): resubmitted commits set `kind:"resubmission"` and include an `origin` envelope containing the original commit’s metadata/signature/HLC and a deterministic `origin_event_id`.
- `swarm://` dbfactory (`core/swarm_dbfactory.go`, `core/swarm_registry.go`, `core/remote_chunk_store.go`): a process-global hook that lets Dolt treat “the swarm” as a remote by selecting any live provider and serving chunks/table files read-only.
- Transport boundary (`transport/`): `Transport`/`Gossip` control plane and provider interfaces for the read-only data plane.
- Local “anti-entropy” index (`core/index.go`, `core/index_mem.go`): tracks which HLCs are pending/applied/rejected, publishes checkpoints in digests, and stores a local finalized base.

### Data Flow

```
Local write:
  Pull-first sync (best-effort)
  Resubmit any late offline commits (best-effort, if enabled)
  Exec SQL locally → Commit on local main (message = signed CommitMetadata JSON)
  → Publish CommitAd(repo, HLC, metadata_json, metadata_sig)
  → Publish Digest(repo, head, checkpoints)   (best-effort, immediate)

Remote reception:
  Receive CommitAd → validate basic invariants + skew guard → verify metadata signature (when IdentityResolver is configured) → mark HLC as pending
  → trigger a sync pass (debounced)

Sync pass:
  DOLT_FETCH('swarm')               (transport picks any provider; may honor a preferred-provider hint)
  find merge base: MergeBase(main, remotes/swarm/main)
  try fast-forward main to remote head (ff-only merge)
  else replay deterministically on a temp branch in HLC order (tail-only when a finalized base is set)
  attempt to advance finalized base from digests/checkpoints
  resubmit any newly-detected late offline commits (best-effort, if enabled)
```

Provider hints are best-effort: the node remembers which peer mentioned a given HLC for a short TTL and consumes that hint when choosing a fetch provider.

## Commit Dissemination, Concurrency, and Reconciliation

### Dissemination (advertise → pull)

When a node commits locally (`Node.Commit` / `Node.ExecAndCommit`):
1. It generates an HLC timestamp (`HLC.Now()`).
2. It builds `CommitMetadata` (message, HLC, content hash, author/email/date) and signs it.
3. It creates a Dolt commit whose **commit message is the metadata JSON** and tags the commit hash with a signature (secondary).
4. It gossips a `CommitAdV1` containing the HLC and the metadata bytes.
5. It publishes a `DigestV1` best-effort so peers can repair missed adverts without waiting for `RepairInterval`.

Peers do not push objects to one another. A received advert is only a hint to **pull**.

### “Pull-first” optimization (reduces replay)

Before committing, `Node` does a short best-effort sync pass (`PullFirstTimeout`, `PullFirstPasses`) so that, when possible, new commits are made on top of already-known earlier HLC commits. This reduces the number of times history must be rewritten via replay.

### Deterministic reconciliation (linear history, no merges)

After a fetch, if `main` cannot be fast-forwarded to the fetched remote head, the node deterministically linearizes:

1. Compute the merge base between `main` and `remotes/swarm/main`.
2. Collect commits after the base from:
   - local `main` (already present)
   - remote `remotes/swarm/main` (now present locally after fetch)
3. Deduplicate and sort by HLC (tie-break by `peerID`).
4. Create a temp branch (`replay_tmp`) at the merge base.
5. Cherry-pick commits in HLC order onto `replay_tmp`, and rewrite commit metadata so all peers replay with the same author/date/message.
6. Update `main` to the temp result after verifying `main` did not advance during replay (crash-safe / restartable).

### Finalization (watermark) and tail-only replay (faster convergence)

As histories grow, replaying from the merge base can become expensive. To reduce replay churn, nodes maintain a local **finalized base**:

- Nodes publish digests containing recent **checkpoints** (HLC + commit hash).
- From recent digests, a node computes a conservative **watermark** based on checkpoints that appear in at least `MinPeersForWatermark` active peers (within `PeerActivityTTL`), minus `WatermarkSlack`.
- The node advances its **finalized base** to the highest commonly-known checkpoint at or before the watermark.

When a finalized base is set, reconciliation becomes **tail-only**:
- Replay starts from the finalized base’s local commit hash (not the merge base), and commits at or before the finalized HLC are filtered out.
- This guarantees the finalized prefix is never rewritten, improving convergence time in long-running clusters.

### Conflict resolution: First Write Wins (FWW)

If a cherry-pick encounters a conflict, the reconciler **drops the later commit** (the one being applied when the conflict occurs).
Dropped commits are recorded locally as **rejected** (handled) to avoid livelock via repeated adverts/checkpoints. An optional callback can be invoked (`CommitRejectedCallback`).

### Anti-entropy repair (missed adverts)

Periodically (and also immediately after local commits), each node publishes a compact `DigestV1` that contains:
- its head HLC/hash, and
- recent “checkpoints” (HLC + commit hash).

If you missed an advert (packet loss, temporary disconnect), a peer’s digest will usually expose a checkpoint you don’t have, which triggers a sync pass to fetch and reconcile.

### Offline commit resubmission (late commits without rewriting finalized history)

If a peer was offline and created commits with an HLC that is at or before the current watermark, those commits are “late” relative to the network. Because the finalized prefix is not rewritten, DoltSwarm treats these as **resubmissions**:

- The node cherry-picks the original commit onto the current `main`, producing a new commit with a fresh HLC.
- The new commit’s metadata sets `kind:"resubmission"` and includes an `origin` envelope with the original metadata/signature/HLC plus a deterministic `origin_event_id` for idempotency.
- Resubmissions are advertised like normal commits, and conflicts are handled with the usual FWW rule.

## Public Interface (What Library Users Use)

Most consumers should use `Node` and treat transport as a plug-in.

### Core types

- `OpenNode(cfg NodeConfig) (*Node, error)` and `(*Node).Run(ctx)` start the background gossip+sync loops.
- `(*Node).Commit(msg)` and `(*Node).ExecAndCommit(exec, msg)` perform local writes and automatically advertise.
- `(*Node).Sync(ctx)` / `(*Node).SyncHint(ctx, hlc)` allow manual/one-shot sync passes (useful for CLIs/tests).

`NodeConfig` requires:
- `Repo` (`RepoID{Org, RepoName}`): logical repo identity.
- `Signer`: signs metadata for local commits and provides `Verify` for remote signature checks.
- `Identity` (optional): if set, incoming commit adverts are verified against peer public keys; unverifiable/invalid adverts are rejected.
- `Transport`: provides (1) gossip message delivery and (2) a provider picker for the read-only data plane.

Optional convergence-related knobs (all have conservative defaults):
- `MinPeersForWatermark`: minimum active peers required to compute a finalized base watermark (default: 2).
- `WatermarkSlack`: slack subtracted from the watermark to be conservative about finalization (default: 30s).
- `PeerActivityTTL`: how long a peer remains “active” for watermark computation (default: 5m).
- `EnableResubmission`: enable/disable offline commit resubmission (default: enabled).

### Minimal usage (transport ignored)

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
