# DoltSwarm Library

## Architecture Overview

DoltSwarm is a peer-to-peer synchronization library for Dolt databases. It provides P2P synchronization capabilities allowing multiple peers to maintain consistent database state without a central server.

### Core Principles

1. **Pull-only synchronization**: Peers only PULL data from other peers. No peer can trigger write operations on a remote peer.

2. **Local writes only**: All write operations (commits, inserts, updates) are performed locally on a peer's own database.

3. **Deterministic merges**: When pulling from peers, merges must be deterministic so all peers converge to the same state.

4. **Advertise and pull model**: After a local write, the peer advertises its new head to all connected peers. Other peers then pull from that peer to get the new data.

### Data Flow

```
Peer A (writes locally) -> Advertises head to Peer B, C, D
                        -> Peer B pulls from A
                        -> Peer C pulls from A
                        -> Peer D pulls from A
```

### Remote Chunk Store Operations

The `RemoteChunkStore` implements read-only access to a remote peer's data:

- **Supported (read operations)**: `Get`, `GetMany`, `Has`, `HasMany`, `Root`, `Rebase` (refresh local view), `Sources`, `Size`
- **Not supported (write operations)**: `Put`, `Commit`, `WriteTableFile`, `AddTableFilesToManifest`, `PruneTableFiles`, `SetRootChunk`

Note: `Rebase` is a READ operation - it refreshes this client's cached view of the remote's current state by fetching the latest root hash. It does not modify the remote peer.

## Key Components

### DB (db.go)
The main database wrapper that manages:
- SQL database connection via Dolt driver
- Peer client connections (gRPC)
- Remote management for P2P sync
- Event queue for processing external heads
- Commit signing and verification

### Event Processing (distributed.go)
- `eventHandler()`: Processes incoming head advertisements
- `AdvertiseHead()`: Broadcasts local HEAD to all peers
- `RequestHeadFromAllPeers()`: Polls all peers for their current HEAD

### Merge Logic (db.go - Merge function)
Current merge strategy:
1. Find merge base between peer branch and main
2. Create temporary branches for isolation
3. Determine merge direction deterministically (string comparison of commit hashes)
4. Perform merge with conflict detection
5. Fast-forward main if possible, otherwise create merge commit
6. Advertise new head after successful merge

### SQL Interface (sql.go)
- `doCommit()`: Creates a signed commit with timestamp
- `ExecAndCommit()`: Execute SQL and commit atomically
- Commits are signed and tagged for verification

### Syncer (syncer.go)
gRPC server implementation for:
- `AdvertiseHead`: Receive head advertisements from peers
- `RequestHead`: Respond to head requests from peers

## Database Interactions

All database interactions go through the SQL interface using the Dolt driver:
- `DOLT_COMMIT()` - Create commits
- `DOLT_MERGE()` - Merge branches
- `DOLT_FETCH()` - Fetch from remotes
- `DOLT_CHECKOUT()` - Switch branches
- `DOLT_BRANCH()` - Create/delete branches
- `DOLT_REMOTE()` - Manage remotes
- `DOLT_TAG()` - Create signature tags

## Commit Verification

Commits are verified using a signature scheme:
1. Each commit is signed by the creating peer
2. Signature stored as a Dolt tag with public key in message
3. On pull, signatures are verified against known public keys

---

# Integration Tests

The integration test suite is located in `integration/` and provides a demo CLI (`ddolt`) for testing P2P database synchronization.

## Building the Demo CLI

```bash
task build
# Or manually:
cd integration && CGO_LDFLAGS="-L/opt/homebrew/opt/icu4c@78/lib" CGO_CPPFLAGS="-I/opt/homebrew/opt/icu4c@78/include" CGO_CFLAGS="-I/opt/homebrew/opt/icu4c@78/include" go build -o ddolt .
```

## Demo CLI Usage

### Initialize a new database locally
```bash
./integration/ddolt --port 10501 --db /path/to/db init --local
```

### Initialize from a peer
```bash
./integration/ddolt --port 10502 --db /path/to/db2 init --peer <PEER_ID>
```

### Start the server
```bash
./integration/ddolt --port 10501 --db /path/to/db server
```

### Start with custom listen address and bootstrap peers
```bash
# Listen on all interfaces (for Docker/containers)
./integration/ddolt --port 10501 --db /path/to/db --listen-addr 0.0.0.0 server

# With bootstrap peers (skip mDNS discovery)
./integration/ddolt --port 10501 --db /path/to/db \
  --bootstrap-peer /ip4/192.168.1.100/udp/10500/quic-v1/p2p/PEER_ID \
  --bootstrap-peer /ip4/192.168.1.101/udp/10500/quic-v1/p2p/PEER_ID2 \
  server
```

## Testing Strategy

The test suite in `integration/integration_test.go` validates that the P2P database synchronization works correctly. Tests run in Docker containers (one container per peer).

### Current Test Cases

| Test | Purpose |
|------|---------|
| `TestIntegration` | Basic init + convergence + late joiner |
| `TestSequentialWritesPropagation` | Sequential commits from one peer propagate |
| `TestConcurrentWrites` | Simultaneous writes converge deterministically |
| `TestCommitOrderConsistency` | Round-robin writes keep consistent order |

### Proposed Additions (to be implemented)

| Test | Purpose |
|------|---------|
| `TestLinearNoMergeCommits` | Assert no merge commits are created in the new pipeline |
| `TestHashStabilityAcrossPeers` | Same HLC chain yields identical commit hashes on all peers after replay |
| `TestConflictDropMarkedApplied` | Induce row-level conflict; ensure losing commit is dropped, marked applied, and callback fired |
| `TestReplayCrashSafety` | Simulate crash during temp-branch replay; main must remain intact; second run converges |
| `TestFutureSkewRejection` | Advert with future HLC (>MaxClockSkew) is rejected and marked applied |
| `TestMissingAdvertRepair` | Drop adverts for some peers; `RequestCommitsSince` heals and converges |
| `TestDebounceBatching` | Burst of near-simultaneous commits triggers a single replay batch (no O(N²) reorders) |

### Key Verification Points

1. **Linear history**: No merge commits exist in `main`.
2. **Hash equality**: All peers share identical commit hashes in the same order.
3. **Conflict determinism**: Conflicting commits are dropped identically on all peers and marked applied.
4. **Crash safety**: Crashes during replay do not corrupt `main`; rerun converges.
5. **Skew guard**: Future-skew adverts are rejected consistently.
6. **Repair**: Missed adverts are healed by `RequestCommitsSince`.

### Running Tests

```bash
# Build Docker image and run all tests
task integration

# Run tests without rebuilding Docker image (faster iteration)
task integration:quick

# Run with custom number of instances
NR_INSTANCES=10 task integration

# Keep containers after test for log inspection
task integration KEEP=1

# Then inspect logs
task docker:logs                          # All containers
task docker:logs CONTAINER=ddolt-test-1   # Specific container
task docker:logs GREP="reorder|retry"     # Filter by pattern

# Clean up when done
task docker:clean
```

### Environment Variables

| Variable | Description |
|----------|-------------|
| `NR_INSTANCES` | Number of peer instances to spawn (default: 5) |
| `KEEP_DOCKER_CONTAINERS` | Leave containers running after tests |

### Convergence Timeouts

The tests use configurable timeouts with periodic progress logging:

- **Default convergence timeout**: 120 seconds
- **Poll interval**: 500ms
- **Progress log interval**: 10 seconds
- **Per-gRPC-call timeout**: 10 seconds

When convergence fails, tests report:
- Which peers are responsive vs unresponsive
- Error messages from unresponsive peers
- Distribution of different HEAD commits (if peers diverged)

## Docker

Each ddolt peer runs in its own Docker container, connected via Docker networking. The test harness runs locally and manages the containers.

**Requirements:**
- Docker daemon running
- Docker image built (`task docker:build`)

**How it works:**
1. Test creates a Docker network for container communication
2. First container initializes the database locally
3. Other containers clone from the first via P2P
4. Test harness connects to containers via exposed ports
5. Containers discover each other via bootstrap peer addresses

### Container Configuration

The Docker image accepts these environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | 10500 | P2P port |
| `LISTEN_ADDR` | 0.0.0.0 | Listen address |
| `DB_PATH` | /data | Database path |
| `LOG_LEVEL` | info | Log level |
| `INIT_PEER` | - | Peer ID to clone from |
| `BOOTSTRAP_PEERS` | - | Comma-separated bootstrap peer multiaddrs |

### Running a Standalone Container

```bash
# Initialize locally
docker run --rm -v mydata:/data -e PORT=10500 doltswarmdemo init --local

# Start server
docker run --rm -v mydata:/data -e PORT=10500 -p 10500:10500/udp doltswarmdemo server
```

## Taskfile Commands

This project uses [Task](https://taskfile.dev) for common operations:

| Command | Description |
|---------|-------------|
| `task build` | Build the ddolt binary |
| `task integration` | Build Docker image and run all integration tests |
| `task integration:quick` | Run integration tests without rebuilding Docker image |
| `task docker:build` | Build the Docker image |
| `task docker:logs` | Show logs from test containers |
| `task docker:ps` | List running test containers |
| `task docker:shell` | Open shell in Docker container |
| `task docker:clean` | Clean up Docker containers/networks/images |
| `task clean` | Clean test artifacts and kill processes |
| `task kill` | Kill all running ddolt processes |

### Integration Test Options

Both `task integration` and `task integration:quick` support these options:

| Option | Description |
|--------|-------------|
| `KEEP=1` | Leave containers running after tests (for log inspection) |
| `TIMEOUT=30m` | Test timeout (default: 20m) |
| `NR_INSTANCES=3` | Number of peer instances (default: 5) |
