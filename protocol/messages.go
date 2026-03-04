package protocol

import "time"

// CommitAdV1 is the gossip payload for a commit advertisement.
//
// It is intentionally transport-agnostic: transports are free to serialize it
// as protobuf, JSON, CBOR, etc. The core library only depends on these structs.
type CommitAdV1 struct {
	Repo RepoID
	HLC  HLCTimestamp

	// MetadataJSON is expected to be the exact commit message JSON written to Dolt.
	MetadataJSON []byte

	// MetadataSig is a signature over MetadataJSON by the author identified by HLC.PeerID.
	MetadataSig []byte

	// ObservedAt is optional and not covered by signature; transports may set it.
	ObservedAt time.Time
}

// HeartbeatV1 is a periodic liveness message carrying the sender's current HLC.
// Receivers use it to compute stable_hlc = min(peer_hlc[active_peers]) for finalization.
type HeartbeatV1 struct {
	Repo   RepoID
	HLC    HLCTimestamp
	PeerID string
}
