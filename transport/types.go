package transport

import "github.com/nustiueudinastea/doltswarm/protocol"

// Transport-level aliases for protocol types, so transport interfaces stay readable.
type RepoID = protocol.RepoID
type HLCTimestamp = protocol.HLCTimestamp
type CommitAdV1 = protocol.CommitAdV1
type DigestV1 = protocol.DigestV1
