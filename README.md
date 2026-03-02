# DoltSwarm

DoltSwarm is a peer-to-peer synchronization library for [Dolt](https://www.dolthub.com/) databases.
It enables multiple peers to concurrently read and write SQL data, automatically merge non-conflicting changes, surface conflicts for user resolution, and converge on a shared finalized history — all without a central coordinator.

See [`doltswarm-protocol.md`](./doltswarm-protocol.md) for the full protocol specification, architecture, code structure, and public API.
