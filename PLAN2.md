# PLAN2: Gossip-First DoltSwarm (Option B: Dolt-Native Fetch via `swarm://`)

This document proposes a **breaking redesign** of DoltSwarm to assume a **gossip P2P architecture by default**, while preserving the core goals:

- **Pull-only** (no remote writes)
- **Local writes only**; sync happens by receiving/repairing remote commits
- **Deterministic linear history** ordered by **HLC** (no merge commits)
- **Replay via cherry-pick** with original metadata so all peers converge to identical commit hashes
- **First-write-wins** on conflicts (later commit dropped deterministically)
- **Crash safety** using a temp branch and atomic fast-forward of `main`

Key requirement for this redesign:

- **Commits / chunks must be retrievable from any peers that have them.** The library must **not** mandate pulling from a specific peer ID, and must **not** expose “direct peer fetch” concepts in the public API. It should still work on full-mesh networks (full mesh becomes “a gossip overlay with high connectivity”).

## Option B (Selected): Use Dolt’s optimized fetch pipeline

The “old” approach (direct peer fetch using Dolt remotes) is dramatically more CPU/IO efficient because Dolt already has a highly optimized object transfer pipeline:

- negotiation based on hashes / has-many
- table-file aware transfer (bulk reads) instead of per-commit replay/import churn
- reuse of existing Dolt storage primitives and caches

**Option B** keeps the gossip control plane (ads/digests) but switches the data plane to “Dolt-native fetch”:

- Every node maintains a single Dolt remote named `swarm`.
- `CALL DOLT_FETCH('swarm')` is the only way we ingest remote objects.
- The remote URL uses a new scheme: `swarm://<org>/<repo>` (or `swarm://<repo>`).
- A `swarm://` dbfactory resolves that URL to **any currently-connected provider** via a provider picker (transport implementation detail).
- Providers expose Dolt’s `remotesapi.ChunkStoreService` (read-only subset) plus a small non-HTTP `Downloader` stream for table files.

This preserves “retrieve from any peer that has the data” without ever pinning to a specific peer in the public API.

## Migration Plan (PR-sized, keep integration green) — Option B

1) **PR1: `swarm://` dbfactory + provider registry (core)**
   - Add `RegisterSwarmProviders(repo, ProviderPicker)` and a `swarm://` dbfactory that opens a Dolt remote backed by a provider-selected chunk store.
   - Keep core protobuf-free; providers are Go interfaces.

2) **PR2: Serve Dolt chunkstore over the existing libp2p-gRPC transport (integration)**
   - Expose Dolt `remotesapi.ChunkStoreService` (read-only subset) over gRPC.
   - Add a small `Downloader` streaming service for table files / compressed chunks (protobuf lives only in `integration/proto/`).

3) **PR3: Switch Node sync to `DOLT_FETCH('swarm')`**
   - Add `EnsureSwarmRemote`, `FetchSwarm`, `MergeBase`, `GetBranchHead` helpers.
   - Update reconciler flow: fetch → merge-base → FF or deterministic replay on temp branch.

4) **PR4: Turn off and delete direct-peer code paths**
   - Delete peer-specific remotes / fetches.
   - Keep gossip-only control plane + dolt-native fetch data plane.

5) **PR5: Performance hardening**
   - Debounce sync triggers (`SyncDebounce`).
   - Avoid rewriting the `swarm` remote when URL is unchanged.
   - Add light instrumentation for fetch duration / replay duration.

---

## Historical note: Commit-bundle design (Option A)

The remainder of this document describes an earlier “commit bundle” approach (Option A). It’s kept for reference, but Option B is the current direction.

---

## 0. Scope and Terminology

### Terms

- **Node**: the local running instance (DB + background sync engine).
- **Repo**: the Dolt repo being synced (identified by `RepoID`).
- **HLC**: hybrid logical clock timestamp (total order).
- **CommitAd**: signed commit metadata (identity + ordering).
- **Bundle**: a provider-agnostic **content-addressed pack** of objects required to materialize a contiguous set of commits.
- **Provider**: any peer that can serve a requested bundle. The core library does not “know peers”; provider selection is an implementation detail of the transport.

### Non-goals (for the initial cut)

- Efficient syncing of extremely large repos (we’ll add negotiations/sketches later).
- Anonymous adversarial networking (we will harden signatures and add abuse controls, but not design a full DoS-proof system in v1).
- Supporting legacy `AdvertiseHead/RequestHead` (these remain removed/no-op in the linear pipeline).

---

## 1. High-Level Architecture (Gossip-first)

### Components

1) **Node**
- Owns the Dolt working directory, SQL connection, local commit/replay logic.
- Runs background loops: ingest gossip, periodic repair, replay/reorder.

2) **Transport (pluggable)**
- Provides:
  - **Gossip**: pubsub-style dissemination for small messages (ads/digests/requests).
  - **Exchange**: request/response API for **commit bundles**.
- Transport hides connectivity and provider selection. The Node never targets a peer ID.

3) **Bundle Builder (provider-side)**
- Given a request `(repo, base checkpoint, limit/bytes)`, produces a bundle stream.

4) **Bundle Importer (receiver-side)**
- Validates and imports bundle objects into local Dolt chunk store.
- Updates a local, non-replicated **HLC index**.

5) **Reconciler (deterministic linearizer)**
- Uses the HLC index and imported commits to fast-forward or reorder via replay on a temp branch.

### Data flow (happy path)

1. Local commit → `CommitAd` published via gossip.
2. Receiver validates `CommitAd` and enqueues it.
3. Receiver obtains the commit chain by requesting a **bundle since a compatible checkpoint**.
4. Receiver imports bundle objects.
5. Reconciler applies commits (FF-only if possible; otherwise deterministic replay).

### Data flow (repair / missed gossip)

1. Periodically: publish `Digest` (compact summary of applied history).
2. Peers compare digests and request bundles to fill gaps.

---

## 2. Breaking API: Node + Transport

### Public API (core library)

```go
type Node struct { /* ... */ }

type NodeConfig struct {
  Dir    string
  Repo   RepoID
  Signer Signer
  Transport Transport

  // Tunables
  MaxClockSkew     time.Duration
  BundleLimitCommits int
  BundleLimitBytes   int64
  RepairInterval     time.Duration
  GossipDedupTTL     time.Duration
}

func OpenNode(cfg NodeConfig) (*Node, error)
func (n *Node) Run(ctx context.Context) error
func (n *Node) Close() error

// Local writes
func (n *Node) Commit(msg string) (commitHash string, err error)
func (n *Node) ExecAndCommit(exec ExecFunc, msg string) (commitHash string, err error)

// Optional: observability hooks
func (n *Node) SetCommitRejectedCallback(cb CommitRejectedCallback)
```

### Transport API (gossip + exchange)

Core principle: **Node never addresses a peer.**

```go
type Transport interface {
  Gossip() Gossip
  Exchange() Exchange
}

type Gossip interface {
  Publish(ctx context.Context, topic string, msg []byte) error
  Subscribe(ctx context.Context, topic string) (Subscription, error)
}

type Subscription interface {
  Next(ctx context.Context) (Envelope, error)
  Close() error
}

type Envelope struct {
  From string // opaque sender identity (for abuse control / metrics)
  Data []byte
}

type Exchange interface {
  // Transport chooses a provider internally and handles retries/backoff.
  // The base checkpoint prevents importing incompatible histories.
  GetBundleSince(ctx context.Context, repo RepoID, base Checkpoint, req BundleRequest) (CommitBundle, error)
}
```

Notes:
- `Exchange` is where “provider selection” happens, but it is entirely internal to the transport implementation.
- A full mesh transport can implement `Gossip` as broadcast and `Exchange` as “pick any connected peer”.

---

## 3. Protocol: Suggested Wire Schemas (Ads, Digests, Bundles)

The **core library is protobuf-free** and operates only on native Go structs (e.g. `CommitAdV1`, `DigestV1`, `CommitBundle`).
Each transport is responsible for defining its own wire format and encoding/decoding (protobuf/JSON/CBOR/etc).

For concreteness and interoperability, this section provides **suggested protobuf message shapes** that transports can adopt.
In the integration demo, these should live under `integration/proto/` (and generated code should live under `integration/proto/` as well).

---

## Migration Plan (PR-sized, keep integration green)

This is a concrete sequence of small PRs to migrate from the current **direct-peer / fetch remotes** architecture to **gossip + commit bundles**, while keeping `task integration:quick` green throughout.

Important constraint: even when switching gossip transport away from gRPC, we can still keep **data defined in protobufs** by publishing **protobuf-encoded payloads** over libp2p GossipSub (or any other pubsub) as opaque bytes. The core library remains protobuf-free and sees only Go structs.

### PR 1 — Bundle plumbing + correctness hardening (no behavior change)
- Land `CommitBundle` / `Exchange` end-to-end without changing the existing sync pipeline.
- Harden bundle correctness:
  - Include local storage format in bundle header (`nbf_version`, `nbs_version`) and reject incompatible bundles on import.
  - Ensure chunk closure traversal uses the correct NomsBinFormat derived from `ChunkStore.Version()`.
  - Ensure exported bundle chunk bytes/hashes are copied (no stack-backed slices).
  - Make import commit robust when `ChunkStore.Commit(...)` returns `ok=false` (rebase + retry).
- Keep the current peer-based `AdvertiseCommit` + `DOLT_FETCH` reconciler logic active.

### PR 2 — Add gossip transport (integration-only), still using old pipeline
- Add a libp2p GossipSub-backed `doltswarm.Gossip` implementation in `integration/`.
- Define gossip payloads in protobuf under `integration/proto/` and publish them as `proto.Marshal(...)` bytes over GossipSub topics.
- Keep `Exchange` as the existing BundleExchange (gRPC) data plane for now.
- Do not wire Node into ddolt yet; tests unchanged.

### PR 3 — Run Node in ddolt, but keep DB’s old sync enabled
- In `integration/main.go`, construct a `doltswarm.Node` and run `node.Run(ctx)` in a goroutine.
- Keep `ExecSQL` using the existing DB write path; Node is “read-only” (observability only) to avoid changing heads.

### PR 4 — Switch write path to Node (dual-publish temporarily)
- Make integration `ExecSQL` call `node.ExecAndCommit(...)` so every commit produces a `CommitAdV1` for gossip.
- Temporarily keep the old broadcast (`DB.AdvertiseCommit`) enabled as a safety net while bundle apply loop matures.

### PR 5 — Enable bundle-based apply loop (still keep direct fetch enabled)
- Implement `Node.Run()` to:
  - Subscribe to gossip commit ads / digests
  - Dedup + validate
  - Request bundles via `Exchange.GetBundleSince(...)`
  - Import bundles and drive deterministic replay/FF to advance `main`
  - Publish periodic digests for anti-entropy repair
- Keep old peer-fetch reconciler as a fallback (lower frequency).
- Add an integration test that simulates missed gossip and validates digest/bundle repair.

### PR 6 — Turn off direct-peer fetch in integration runtime (keep code behind flags)
- Add a runtime flag/env to disable:
  - `DOLT_REMOTE('add', ...)` peer remotes
  - any `DOLT_FETCH('<peerID>', ...)` usage
  - peer-based “broadcast to all peers” commit adverts
- Integration tests run with the direct-peer path disabled and must still converge.

### PR 7 — Remove direct-peer networking surface from core
- Delete core interfaces and implementations that assume peer-targeted fetching:
  - `peer.go`, `remotecs.go`, `tablefile.go`, `cscache.go`, plus any remaining `remotesapi` references
  - DB peer membership/remotes (`AddPeer/RemovePeer/GetPeers/ensureRemoteExists`)
  - reconciler peer-ID-based fetching and peer-branch assumptions

### PR 8 — Remove legacy integration RPCs (DBSyncer + ChunkStoreService)
- Delete integration-only RPC surfaces used by the direct-peer design:
  - `integration/proto/dbsyncer.proto` and all downloader/chunkstore RPCs
  - server/client implementations that exist solely for peer-targeted fetch
- Keep only:
  - GossipSub (protobuf payloads) for gossip messages
  - BundleExchange (or replace it with a libp2p stream protocol if desired)

### PR 9 — Replace “Closure Bundles” with “Dolt-Native Pull Semantics” (performance)

Problem: the current `CommitBundle` implementation is a **server-side closure walk** that copies raw NBS chunks into a bundle per request.
This scales poorly at N=20+ because:
- provider repeats expensive closure traversal for each requester,
- bundles duplicate chunks across requests,
- CPU + disk I/O scale roughly with the number of sync attempts, not the number of *new chunks*.

Why the *old direct-pull* approach was “way more effective”:
- `DOLT_FETCH` / Dolt’s pull pipeline is **receiver-driven** and transfers **only missing chunks**, using batched `HasMany`/`GetMany` calls.
- The provider mostly does **constant-time lookups by hash**, not closure traversal per client.
- The algorithm benefits heavily from OS page cache and chunk-addressed deduplication.

Goal: keep the **gossip-first** UX (no peer-targeting in the core API), but adopt the **same optimized patterns** as Dolt’s fetch.

Design (breaking internal change, external UX stable):
1) Split “bundle” into a light **commit inventory** plus a receiver-driven **chunk fetch**:
   - Gossip still carries `CommitAdV1` and `DigestV1` for ordering + repair.
   - Data-plane becomes “fetch by hash” rather than “ship closure”.
2) Introduce a provider-agnostic “swarm remote” / “multi-source chunk store” behind the transport:
   - Node never targets a peer; transport picks providers per request/batch.
   - Receiver asks for missing chunks (batched) and imports them; replay/FF stays the same.

Two viable implementation options:

**Option A — Library-level puller (transport-agnostic, no Dolt remotes)**
- Add an `ExchangeChunks` interface (integration can keep protobuf; core stays Go structs):
  - `HasMany(repo, hashes) -> []bool`
  - `GetMany(repo, hashes) -> []chunk`
- Implement a receiver-side pull loop that:
  - computes missing set via `HasMany`,
  - downloads via `GetMany` with concurrency + byte budgets,
  - writes to local chunk store,
  - repeats until target commit roots exist locally.
- Pros: transport-independent, stable API surface.
- Cons: we re-implement some of Dolt’s tuning ourselves (but we can borrow patterns).

**Option B — Reintroduce a `swarm://` Dolt remote factory (max reuse of Dolt optimizations)**
- Provide a single Dolt remote like `swarm://doltswarmdemo` (no peer ID).
- The factory internally uses a multi-source chunk fetcher (providers chosen by transport).
- Node drives sync by calling `CALL DOLT_FETCH('swarm');` and then running replay/FF.
- Pros: leverages Dolt’s highly optimized fetch/pull pipeline directly.
- Cons: depends on Dolt internals (`dbfactory` hooks) and can be version-sensitive.

PR-sized rollout suggestion (keep tests green):
- PR 9a: Add `ExchangeChunks` RPCs in `integration/` (protobuf OK) and implement provider-agnostic chunk serving (read-only).
- PR 9b: Implement receiver-side “pull missing chunks for commit hashes” and switch `Node.syncOnce()` to use it (still keep old `CommitBundle` as fallback).
- PR 9c: Delete closure-walk bundling (`walkChunkClosure`) once N=20+ tests are stable.
- (Optional) PR 9d: Try Option B (`swarm://` remote) and compare performance; keep Option A as fallback if Dolt internals change.

### 3.1 Repo identity

```proto
message RepoID {
  string org = 1;
  string name = 2;
}
```

### 3.2 HLC

```proto
message HLCTimestamp {
  int64 wall = 1;
  int32 logical = 2;
  string peer_id = 3; // identity of the author / origin (tie-break)
}
```

### 3.3 CommitAd (gossip)

CommitAd is the signed identity for a commit (stable across replays).

```proto
message CommitAd {
  RepoID repo = 1;
  HLCTimestamp hlc = 2;

  // Signed metadata blob (authoritative).
  // This is the exact JSON commit message stored in Dolt commits today.
  bytes metadata_json = 3;

  // Signature over metadata_json by the author (hlc.peer_id identity).
  bytes metadata_sig = 4;
}
```

Transport envelope (not signed) should include:
- `msg_id` (for dedup / anti-loop)
- `sent_at` (optional)

### 3.4 Digest (gossip anti-entropy)

Digest is used to quickly discover divergence / missing ranges and to negotiate a compatible bundle base.

```proto
message Checkpoint {
  HLCTimestamp hlc = 1;
  string commit_hash = 2; // canonical commit hash at hlc on main
}

message Digest {
  RepoID repo = 1;

  // Current canonical head (as the sender believes).
  HLCTimestamp head_hlc = 2;
  string head_hash = 3;

  // A small, ordered list of recent checkpoints for negotiation.
  // MUST be stable ordering (e.g., newest->oldest).
  repeated Checkpoint checkpoints = 4;
}
```

Checkpoint negotiation rule:
- Receiver selects the newest checkpoint it shares with the provider (same HLC and commit hash).
- Bundle requests are made “since that checkpoint”.

### 3.5 Bundle request/response (exchange)

Bundle requests must be explicit about resource bounds.

```proto
message BundleRequest {
  // Return commits strictly after base.hlc.
  int32 max_commits = 1;

  // Hard cap for total bytes of chunk payload (best-effort).
  int64 max_bytes = 2;

  // Optional: allow returning fewer commits if bytes cap would be exceeded.
  bool allow_partial = 3;

  // Optional: ask provider to include extra checkpoints in the response on mismatch.
  bool include_diagnostics = 4;
}

message BundleHeader {
  RepoID repo = 1;
  Checkpoint base = 2;

  // Provider's current head view at the time of building the bundle.
  HLCTimestamp provider_head_hlc = 3;
  string provider_head_hash = 4;

  // If the base checkpoint doesn't match provider history, provider may return a mismatch header.
  bool base_mismatch = 5;
  repeated Checkpoint provider_checkpoints = 6;
}

message BundledCommit {
  HLCTimestamp hlc = 1;

  // The canonical commit hash for this hlc in the provider's history.
  // Receiver uses it only after base checkpoint match; otherwise it is discarded.
  string commit_hash = 2;

  // Re-send commit metadata so the receiver can verify it matches the advertised metadata_json.
  bytes metadata_json = 3;
  bytes metadata_sig = 4;
}

message BundledChunk {
  bytes hash = 1;   // raw chunk address bytes
  bytes data = 2;   // chunk payload (see encoding below)
  uint32 codec = 3; // 0=raw chunk bytes, 1=nbs-compressed bytes (preferred)
}

message CommitBundle {
  BundleHeader header = 1;
  repeated BundledCommit commits = 2;
  repeated BundledChunk chunks = 3;
}
```

### 3.6 Exchange service

Use streaming RPCs to avoid large memory spikes:

```proto
service BundleExchange {
  rpc GetBundleSince(GetBundleSinceRequest) returns (stream GetBundleSinceChunk);
}

message GetBundleSinceRequest {
  RepoID repo = 1;
  Checkpoint base = 2;
  BundleRequest req = 3;
}

message GetBundleSinceChunk {
  oneof item {
    BundleHeader header = 1;
    BundledCommit commit = 2;
    BundledChunk chunk = 3;
  }
}
```

Receiver reconstructs the `CommitBundle` incrementally.

---

## 4. Bundle Payload Encoding Choices

### Preferred: NBS-compressed chunk bytes

Rationale:
- Existing code already handles compressed chunks in `remotecs.go` (`nbs.NewCompressedChunk`).
- Saves bandwidth and maps well to Dolt storage.

Implementation approach:
- Provider reads chunks from local `chunks.ChunkStore` and emits payload as **NBS-compressed** bytes.
- Receiver:
  - validates chunk hash matches payload
  - inserts into local store (either via compressed insertion APIs if available, or by decompressing to raw `chunks.Chunk` and `Put`).

Fallback:
- If compressed insertion is too hard initially, use `codec=0` (raw bytes) and rely on local store compression.

---

## 5. Local Non-Replicated State: HLC Index

Create a local index file (suggested): `<workingDir>/<repo>/.doltswarm/index.bolt` (or sqlite).

### Required mappings

1) `hlc_key -> commit_hash` (canonical commit hash on `main`)
2) `hlc_key -> status` (Pending/Applied/Rejected/Deferred + nextRetryAt)
3) `head_hlc` and `head_hash` (for digest publishing)
4) Optional: `recent_checkpoints` cache for fast digest creation

### Why not store in Dolt tables?
- Index is operational metadata and would replicate; replication would create feedback loops and inflate history.

---

## 6. Provider-Side: Bundle Builder (how to build a bundle)

Inputs:
- `base` checkpoint `(hlc, commit_hash)`
- `max_commits`, `max_bytes`

Steps:

1) Validate base compatibility
- Confirm local index has `base.hlc -> base.commit_hash`.
- If mismatch:
  - return `BundleHeader{base_mismatch=true, provider_checkpoints=...}` and stop.

2) Select commit sequence
- From local index, collect the next commits after `base.hlc` in increasing HLC order.
- Cap to `max_commits`.

3) Collect required objects
Goal: receiver must be able to resolve all `commit_hash` objects and traverse any referenced state needed for cherry-pick/replay.

Practical strategy (v1):
- For each selected commit hash:
  - load commit object using doltcore libs (e.g., via `doltdb` from the local env)
  - collect all chunk addresses reachable from that commit’s root(s) **and its parent root(s)** (minimum needed for diff/cherry-pick).
  - union all addresses across commits; stop if `max_bytes` exceeded (if `allow_partial`).

Optimization (v2):
- Add a negotiation where the receiver provides a bloom/sketch of known chunks, so providers send mostly-missing chunks.

4) Emit stream
- send `BundleHeader`
- stream `BundledCommit`s (HLC + commit hash + metadata)
- stream `BundledChunk`s until done or byte limit

---

## 7. Receiver-Side: Bundle Importer

Inputs:
- stream of header/commits/chunks

Steps:

1) Validate header
- Ensure `repo` matches local.
- If `base_mismatch`, store provider checkpoints for future negotiation and return a typed error (so transport can try another provider).

2) Validate commits
- For each `BundledCommit`:
  - verify `metadata_sig` against `metadata_json`
  - parse metadata_json (existing `CommitMetadata` JSON)
  - ensure metadata HLC equals `BundledCommit.hlc`

3) Import chunks into local Dolt store
- Insert chunks (compressed preferred).
- Ensure commit hashes referenced by bundled commits are now resolvable locally.

4) Update local HLC index
- Record `hlc_key -> commit_hash` and status=Pending/AvailableForApply.

5) Trigger reconciler
- Wake up reconcile loop to apply commits deterministically.

---

## 8. Reconciler Changes (apply via bundle-imported commits)

Replace peer-branch logic (`peerID/main`, `DOLT_FETCH`, merge-base across peer branches) with:

1) **Import phase**: ensure objects for needed HLC range exist locally (bundle).
2) **Apply phase**:
   - If next HLC commit hash is a descendant of `main` head: fast-forward `main`.
   - Else: deterministic replay on temp branch using `DOLT_CHERRY_PICK(commit_hash)` in HLC order, then `DOLT_COMMIT --amend` to enforce original metadata (as today).

Critical invariant:
- **Never mark a commit “Applied” solely because retrieval failed.**
- Only mark Applied once it is reflected in local main history (or deterministically rejected as conflict/skew/invalid signature).

Conflict handling remains:
- On cherry-pick conflict: drop later commit (FWW), mark as RejectedConflict, publish optional rejection notice (future enhancement).

---

## 9. Gossip Layer Behavior (topics + dedup)

Topics (repo-scoped):
- `doltswarm/<repo>/commitad/v1`
- `doltswarm/<repo>/digest/v1`

### Dedup / loop prevention
- Every gossip message carries:
  - `msg_id` (random or hash of payload)
  - `ttl` hop count (optional, transport-dependent)
- Node maintains an LRU+TTL seen-cache to avoid reprocessing.

### Publish rules
- On local commit:
  - publish `CommitAd` immediately
  - schedule next digest publish soon (to speed repair)

### Subscribe rules
- On `CommitAd`:
  - verify signature and skew guard
  - enqueue HLC as “needed”
- On `Digest`:
  - compare to local; if provider is ahead and shares a checkpoint, request bundle since best shared checkpoint

---

## 10. Provider Selection + Retries (transport responsibility)

Transport must:
- Track candidate providers (from gossip membership / connected peers / routing table).
- Maintain a rolling score per provider:
  - recent success, latency, bytes served
  - failures (temporary)
- Implement retry/backoff:
  - on `base_mismatch`: try a different provider using returned checkpoints for negotiation
  - on network failure: exponential backoff + jitter

Node surfaces only:
- “need bundle since checkpoint X”
- “bundle import succeeded/failed with reason”

---

## 11. Identity + Signature Model (gossip-ready)

### Requirements
- `HLC.peer_id` must map to a stable public key identity.
- `CommitAd.metadata_sig` must be verifiable against that public key.

### Plan
1) Add `IdentityResolver` to NodeConfig:
```go
type IdentityResolver interface {
  PublicKeyForPeerID(ctx context.Context, peerID string) (pubKey []byte, err error)
}
```
2) Enforce signature verification for all remote ads and bundle commits.
3) Reject invalid signatures deterministically; do not enqueue.

---

## 12. Integration & Migration Plan (from current code)

This is a breaking rewrite. Suggested phases:

### Phase A: Lay foundations
- Introduce `Node` and `Transport` interfaces (new files; keep old DB API temporarily behind adapters).
- Define transport-owned wire schemas (protobuf recommended) and generate code in the transport/integration module (not the core library).
- Add local HLC index (bolt/sqlite) and wire digest generation.

### Phase B: Implement bundle exchange in the integration transport
- Add new gRPC service `BundleExchange` in `integration/proto`.
- Implement provider-side bundler using `db.GetChunkStore()` and doltcore commit traversal.
- Implement receiver-side importer inserting into local store and updating index.

### Phase C: Remove peer-based fetch paths
- Delete/stop using:
  - `FactorySwarm` remote factory for fetch
  - `DOLT_REMOTE add` / `DOLT_FETCH(peerID,'main')` flows
  - `GetPeers()/AddPeer()` usage from sync engine
- Replace with:
  - gossip digest negotiation
  - bundle requests

### Phase D: Reconciler refactor
- Drive apply/replay solely from local index + imported commit hashes.
- Preserve crash safety and FWW rules.

### Phase E: Tests
- Update integration harness to:
  - deliver gossip messages (even over full mesh, it’s still gossip semantics)
  - serve bundle exchange
- Add/port tests:
  - `TestConcurrentWrites` should converge without relying on direct fetch.
  - `TestMissingAdvertRepair` should converge using digest/bundles.

---

## 13. Detailed Work Plan (Implementation Tasks)

### 1) Define gossip-first Node API
- Add `node.go` with `Node`, `NodeConfig`, lifecycle, and wrappers around existing SQL commit path.
- Add `transport.go` with `Transport/Gossip/Exchange` interfaces.
- Add `identity.go` with `IdentityResolver`.
- Remove/avoid public peer management methods from Node.

### 2) Define commit bundle format
- Define the transport-owned wire schema (protobuf recommended) mapping 1:1 to the core Go structs:
  - `CommitAdV1`, `DigestV1`, `Checkpoint`
  - streaming `BundleExchange` request/response
- In the integration demo, add these to `integration/proto/` and generate Go there.

### 3) Implement bundle builder (provider)
- Implement `bundles/builder.go`:
  - Validate base checkpoint
  - Select next commits by HLC from local index
  - Traverse commit graph to collect required chunks
  - Stream header/commits/chunks with byte caps

### 4) Implement bundle importer (receiver)
- Implement `bundles/importer.go`:
  - Verify header
  - Verify signatures, parse metadata JSON, validate HLC match
  - Insert chunks into local chunk store
  - Confirm commit hashes are now resolvable
  - Write index updates

### 5) Provider selection + retry/backoff (transport impl)
- In integration transport:
  - Implement a simple provider picker (random among connected)
  - Retry on mismatch/failure
- In real libp2p transport:
  - Track providers via gossip membership / routing
  - Maintain score + jittered retries

### 6) Refactor reconciler to use bundles
- Replace `pullAndApply()`’s peer fetch with:
  - `ensureImportedSince(checkpoint)`
  - `applyPendingInHLCOrder()`
- Replace “mark applied on fetch failure” with state machine + backoff scheduling.
- Ensure deterministic replay uses only commit hashes from local index.

### 7) Gossip anti-entropy (digest/need)
- Implement digest publisher loop:
  - periodically publish `Digest` with recent checkpoints
- Implement digest consumer:
  - find shared checkpoint
  - request bundle since checkpoint if remote ahead
- Implement message dedup cache (LRU+TTL).

### 8) Strict signature and identity binding
- Add `IdentityResolver` and enforce verification for:
  - `CommitAd`
  - `BundledCommit`
- Add skew guard rejection path (don’t “apply”; mark rejected deterministically).

### 9) Update docs and integration harness
- Update `README.md` / `TESTING.md` to explain gossip-first Node usage.
- Update integration transport to:
  - publish/subscribe CommitAd + Digest
  - serve BundleExchange
- Keep full mesh integration but exercise gossip semantics.

---

## 14. Open Questions / Decisions to Lock Early

1) **Chunk encoding**: do we ship NBS-compressed bytes (preferred) or raw bytes for v1?
2) **Commit selection**: “next N commits after checkpoint” vs “up to max bytes” as the primary limiter.
3) **Commit traversal API**: which doltcore primitives best enumerate reachable chunks for commit roots efficiently?
4) **Index store**: bbolt vs sqlite (bbolt likely simpler).
5) **Backpressure**: how to avoid importing huge bundles when far behind (cap + incremental catch-up).
