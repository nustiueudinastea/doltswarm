package doltswarm

import (
	"context"
	"io"
	"time"
)

// Peer represents a remote peer and the communication primitives DoltSwarm needs
// to synchronize state and fetch chunks/files.
//
// The core library is transport-agnostic: callers provide Peer implementations
// backed by gRPC, libp2p, HTTP, in-memory, etc.
type Peer interface {
	ID() string
	Sync() SyncClient
	ChunkStore() ChunkStoreClient
	Downloader() DownloaderClient
}

// SyncClient transports DoltSwarm's commit advertisements and repair requests.
type SyncClient interface {
	AdvertiseCommit(ctx context.Context, ad *CommitAd) (accepted bool, err error)
	RequestCommitsSince(ctx context.Context, since HLCTimestamp) ([]*CommitAd, error)
}

// RepoID identifies a repo on a peer (mirrors Dolt's remote API identity without
// leaking protobuf types into the core).
type RepoID struct {
	Org      string
	RepoName string
}

type ClientRepoFormat struct {
	NbfVersion string
	NbsVersion string
}

type RepoMetadata struct {
	NbfVersion  string
	NbsVersion  string
	StorageSize uint64
}

type TableFileInfo struct {
	FileID      string
	NumChunks   uint32
	URL         string
	SplitOffset uint64

	// Optional: some transports may provide expiring URLs.
	RefreshAfter *time.Time
}

// ChunkStoreClient transports the subset of Dolt remote-chunk-store operations
// DoltSwarm requires for fetch/clone.
type ChunkStoreClient interface {
	GetRepoMetadata(ctx context.Context, repoID RepoID, repoPath string, format ClientRepoFormat) (RepoMetadata, error)
	HasChunks(ctx context.Context, repoPath string, hashes [][]byte) (absentIndices []int32, err error)
	Root(ctx context.Context, repoPath string) (rootHash []byte, err error)
	ListTableFiles(ctx context.Context, repoID RepoID, repoPath, repoToken string) (rootHash []byte, tables []TableFileInfo, appendix []TableFileInfo, err error)

	// Optional; transports that don't support expiring URLs may return ErrUnimplemented.
	RefreshTableFileURL(ctx context.Context, repoID RepoID, repoPath, fileID string) (url string, refreshAfter *time.Time, err error)
}

// DownloaderClient transports file and chunk downloads used by Dolt's storage.
type DownloaderClient interface {
	DownloadFile(ctx context.Context, id string) (r io.ReadCloser, size uint64, err error)
	DownloadChunks(ctx context.Context, hashes []string, onChunk func(hash string, compressed []byte) error) error
}
