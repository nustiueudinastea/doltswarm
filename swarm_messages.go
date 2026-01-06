package doltswarm

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

// Checkpoint is a verifiable point in the canonical history.
type Checkpoint struct {
	HLC        HLCTimestamp
	CommitHash string
}

// DigestV1 is a compact anti-entropy summary used to negotiate a shared base.
type DigestV1 struct {
	Repo RepoID

	HeadHLC  HLCTimestamp
	HeadHash string

	// Newest->oldest checkpoints; used for base negotiation.
	Checkpoints []Checkpoint

	// ObservedAt is optional and not covered by signature; transports may set it.
	ObservedAt time.Time
}

type BundleRequest struct {
	MaxCommits         int
	MaxBytes           int64
	AllowPartial       bool
	IncludeDiagnostics bool
}

type BundleHeader struct {
	Repo RepoID
	Base Checkpoint

	ProviderHeadHLC  HLCTimestamp
	ProviderHeadHash string

	BaseMismatch        bool
	ProviderCheckpoints []Checkpoint
}

type BundledCommit struct {
	HLC HLCTimestamp

	// Canonical commit hash for this HLC in the provider's history.
	CommitHash string

	MetadataJSON []byte
	MetadataSig  []byte
}

type ChunkCodec uint32

const (
	ChunkCodecRaw          ChunkCodec = 0
	ChunkCodecNBSCompressed ChunkCodec = 1
)

type BundledChunk struct {
	Hash  []byte
	Data  []byte
	Codec ChunkCodec
}

// CommitBundle is a provider-agnostic pack of objects required to materialize a
// contiguous sequence of commits after a negotiated base checkpoint.
type CommitBundle struct {
	Header  BundleHeader
	Commits []BundledCommit
	Chunks  []BundledChunk
}
