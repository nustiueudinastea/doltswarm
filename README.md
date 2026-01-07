# DoltSwarm

DoltSwarm is a peer-to-peer synchronization library for [Dolt](https://www.dolthub.com/) databases.
It is designed to make a group of peers converge to the **same linear `main` history** (no merge commits) using a **total order based on Hybrid Logical Clocks (HLC)**, without requiring a central coordinator.

## What This Repo Implements (Today)

- **Pull-only sync**: a peer never causes remote writes; it only reads remote objects and updates its own local `main`.
- **Local writes**: mutations happen locally via SQL + `DOLT_COMMIT`.
- **Deterministic ordering**: commits carry an HLC timestamp and are ordered by `(wall, logical, peerID)` everywhere.
- **Deterministic reconciliation**: when concurrent commits “cross”, peers deterministically replay (cherry-pick) in HLC order so history converges.
- **First write wins (FWW) on conflict**: later conflicting commits are dropped deterministically.
- **Skew guard**: adverts with HLC wall time too far in the future are rejected (`MaxClockSkew`).
- **Anti-entropy repair**: periodic digests/checkpoints trigger re-sync even if commit adverts are missed.

## Architecture

At a high level there are two planes:

- **Control plane (gossip)**: small messages that advertise “there is a commit with HLC X” and periodically summarize history.
- **Data plane (Dolt-native fetch)**: bulk object transfer is done through Dolt’s existing fetch pipeline, using a read-only `swarm://` remote.

### Key Components

- `Node` (`node.go`): the sync engine you embed; runs gossip loops, performs fetch/reconcile, and exposes `Commit` / `ExecAndCommit`.
- `DB` (`db.go`, `sql.go`): thin wrapper around the Dolt SQL driver; creates signed metadata commits; provides helpers like `FetchSwarm`, `MergeBase`.
- `Reconciler` (`reconciler_core.go`): deterministic linearizer (no networking). Given local+imported commits after a merge base, it produces a linear `main`.
- `CommitMetadata` (`metadata.go`): JSON stored in the Dolt commit message, including `(HLC, author, date, content_hash)` plus a signature.
- `swarm://` dbfactory (`swarm_dbfactory.go`, `swarm_registry.go`, `remote_chunk_store.go`): a process-global hook that lets Dolt treat “the swarm” as a remote by selecting any live provider and serving chunks/table files read-only.
- Local “anti-entropy” index (`index.go`, `index_mem.go`): tracks which HLCs are pending/applied and publishes checkpoints in digests.

### Data Flow

```
Local write:
  Exec SQL locally → Commit on local main (message = signed CommitMetadata JSON)
  → Publish CommitAd(repo, HLC, metadata_json, metadata_sig)

Remote reception:
  Receive CommitAd → validate basic invariants + skew guard → verify metadata signature (when IdentityResolver is configured) → mark HLC as pending
  → trigger a sync pass (debounced)

Sync pass:
  DOLT_FETCH('swarm')               (transport picks any provider; may honor a preferred-provider hint)
  find merge base: MergeBase(main, remotes/swarm/main)
  try fast-forward main to remote head (ff-only merge)
  else replay deterministically on a temp branch in HLC order
```

## Commit Dissemination, Concurrency, and Reconciliation

### Dissemination (advertise → pull)

When a node commits locally (`Node.Commit` / `Node.ExecAndCommit`):
1. It generates an HLC timestamp (`HLC.Now()`).
2. It builds `CommitMetadata` (message, HLC, content hash, author/email/date) and signs it.
3. It creates a Dolt commit whose **commit message is the metadata JSON** and tags the commit hash with a signature (secondary).
4. It gossips a `CommitAdV1` containing the HLC and the metadata bytes.

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

### Conflict resolution: First Write Wins (FWW)

If a cherry-pick encounters a conflict, the reconciler **drops the later commit** (the one being applied when the conflict occurs).
Dropped commits never become part of the canonical `main`, so they won’t appear in future digests/checkpoints after peers converge. An optional callback can be invoked (`CommitRejectedCallback`).

### Anti-entropy repair (missed adverts)

Periodically, each node publishes a compact `DigestV1` that contains:
- its head HLC/hash, and
- recent “checkpoints” (HLC + commit hash).

If you missed an advert (packet loss, temporary disconnect), a peer’s digest will usually expose a checkpoint you don’t have, which triggers a sync pass to fetch and reconcile.

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
