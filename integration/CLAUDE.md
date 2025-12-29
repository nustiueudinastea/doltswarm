# DoltSwarm Integration Tests

## Architecture Overview

This is the integration test suite and demo CLI for [doltswarm](../), providing comprehensive testing of P2P database synchronization.

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

The `RemoteChunkStore` in doltswarm implements read-only access to a remote peer's data:

- **Supported (read operations)**: `Get`, `GetMany`, `Has`, `HasMany`, `Root`, `Rebase` (refresh local view), `Sources`, `Size`
- **Not supported (write operations)**: `Put`, `Commit`, `WriteTableFile`, `AddTableFilesToManifest`, `PruneTableFiles`, `SetRootChunk`

Note: `Rebase` is a READ operation - it refreshes this client's cached view of the remote's current state by fetching the latest root hash. It does not modify the remote peer.

## Building

```bash
CGO_LDFLAGS="-L/opt/homebrew/opt/icu4c@78/lib" CGO_CPPFLAGS="-I/opt/homebrew/opt/icu4c@78/include" CGO_CFLAGS="-I/opt/homebrew/opt/icu4c@78/include" go build -o ddolt .
```

## Usage

### Initialize a new database locally
```bash
./ddolt --port 10501 --db /path/to/db init --local
```

### Initialize from a peer
```bash
./ddolt --port 10502 --db /path/to/db2 init --peer <PEER_ID>
```

### Start the server
```bash
./ddolt --port 10501 --db /path/to/db server
```

### Start with custom listen address and bootstrap peers
```bash
# Listen on all interfaces (for Docker/containers)
./ddolt --port 10501 --db /path/to/db --listen-addr 0.0.0.0 server

# With bootstrap peers (skip mDNS discovery)
./ddolt --port 10501 --db /path/to/db \
  --bootstrap-peer /ip4/192.168.1.100/udp/10500/quic-v1/p2p/PEER_ID \
  --bootstrap-peer /ip4/192.168.1.101/udp/10500/quic-v1/p2p/PEER_ID2 \
  server
```

## Testing Strategy

The test suite in `swarm_test.go` validates that the P2P database synchronization works correctly according to the core principles above.

### Test Cases

| Test | Purpose |
|------|---------|
| `TestIntegration` | Basic integration test: initializes peers, verifies connectivity, inserts data from each peer, and validates convergence |
| `TestSequentialWritesPropagation` | Verifies that 5 sequential commits from one peer propagate to all other peers |
| `TestConcurrentWrites` | Validates that simultaneous writes from all peers converge deterministically |
| `TestCommitOrderConsistency` | Tests interleaved writes (round-robin from each peer) maintain consistent ordering across all peers |

### Key Verification Points

1. **Head Convergence**: All peers must have the same HEAD commit after writes propagate
2. **Commit History Consistency**: All peers must have identical commit histories (same commits in same order)
3. **Deterministic Merge**: Concurrent writes from different peers must result in the same final state on all peers

### Running Tests

```bash
# Run with default 5 instances
CGO_LDFLAGS="-L/opt/homebrew/opt/icu4c@78/lib" CGO_CPPFLAGS="-I/opt/homebrew/opt/icu4c@78/include" CGO_CFLAGS="-I/opt/homebrew/opt/icu4c@78/include" go test -v -run TestIntegration

# Run with custom number of instances
CGO_LDFLAGS="-L/opt/homebrew/opt/icu4c@78/lib" CGO_CPPFLAGS="-I/opt/homebrew/opt/icu4c@78/include" CGO_CFLAGS="-I/opt/homebrew/opt/icu4c@78/include" NR_INSTANCES=10 go test -v -run TestIntegration

# Enable verbose output from init process
ENABLE_INIT_PROCESS_OUTPUT=true NR_INSTANCES=5 go test -v -run TestIntegration

# Keep test directory for debugging
KEEP_TEST_DIR=true go test -v -run TestIntegration
```

### Environment Variables

| Variable | Description |
|----------|-------------|
| `NR_INSTANCES` | Number of peer instances to spawn (default: 5) |
| `ENABLE_INIT_PROCESS_OUTPUT` | Show output from initialization processes |
| `ENABLE_PROCESS_OUTPUT` | Show output from server processes |
| `KEEP_TEST_DIR` | Don't delete temporary test directory after test |

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

The project supports two Docker testing modes:

### Mode 1: Container-per-Peer Testing (Recommended)

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

**Run container-based tests:**
```bash
# Build image and run integration test
task docker:test:containers

# Run concurrent writes test
task docker:test:containers:concurrent

# Clean up containers and network
task docker:clean
```

Or manually:
```bash
# Build image (must use build script to include doltswarm dependencies)
./docker/build-docker.sh

# Run tests with docker build tag
CGO_LDFLAGS="-L/opt/homebrew/opt/icu4c@78/lib" CGO_CPPFLAGS="-I/opt/homebrew/opt/icu4c@78/include" CGO_CFLAGS="-I/opt/homebrew/opt/icu4c@78/include" \
  NR_INSTANCES=5 go test -tags docker -v -timeout 15m -run TestDockerIntegration
```

### Mode 2: Single Container (All Peers)

All peers run as processes inside a single container. Useful for CI/CD or environments without Docker-in-Docker.

```bash
docker run --rm -it doltswarmdemo /bin/bash
# Inside container:
go test -v -timeout 30m ./...
```

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
| `task test` | Run all tests locally (processes) |
| `task test:integration` | Run only the integration test |
| `task test:sequential` | Run sequential writes test |
| `task test:concurrent` | Run concurrent writes test |
| `task test:order` | Run commit order test |
| `task docker:build` | Build Docker image |
| `task docker:test:containers` | Run integration test with containers |
| `task docker:test:containers:concurrent` | Run concurrent test with containers |
| `task docker:shell` | Open shell in Docker container |
| `task docker:clean` | Clean up Docker containers/networks |
| `task clean` | Clean test artifacts and kill processes |
| `task kill` | Kill all running ddolt processes |
