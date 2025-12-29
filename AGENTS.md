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
