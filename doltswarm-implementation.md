# DoltSwarm Implementation Notes

Implementation-specific architecture, package/file planning, public Go API shape, and integration test notes moved here from `doltswarm-protocol.md`.

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
- New: `MemMerkleClock` with `map[EventCID]Event`, heads set, peer_hlc map, stable_hlc, canonical finalized event map (`cid -> root_hash`), parked conflicts

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
  - Add adaptive heartbeat loop (event-driven + idle keepalive)
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
- Adapt: `Commit`/`ExecAndCommit` in `sql.go` — now creates Events instead of CommitAds, uses `active_heads = heads \ parked_conflicts` for parent selection (no blocking precondition)

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
