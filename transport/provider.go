package transport

import (
	"context"
	"io"
	"time"
)

// ProviderPicker selects a live provider for a given repo.
//
// The core library never targets a specific peer. Transports are free to use
// hints (e.g. from commit ads) internally, but the public API stays provider-agnostic.
type ProviderPicker interface {
	PickProvider(ctx context.Context, repo RepoID) (Provider, error)
}

// LastUsedProviderGetter is an optional interface that ProviderPicker implementations
// can satisfy to report which provider was most recently selected.
// This is useful when context-based tracking doesn't propagate through external layers (e.g., Dolt SQL).
type LastUsedProviderGetter interface {
	LastUsedProvider() string
}

// Provider is a read-only data-plane provider (any peer that can serve chunks / table files).
type Provider interface {
	ID() string
	ChunkStore() ChunkStoreClient
	Downloader() DownloaderClient
}

// ClientRepoFormat describes the formats a client supports when talking to a provider.
type ClientRepoFormat struct {
	NbfVersion string
	NbsVersion string
}

// RepoMetadata describes a provider repo's storage format and approximate size.
type RepoMetadata struct {
	NbfVersion  string
	NbsVersion  string
	StorageSize uint64
}

// TableFileInfo describes a table file on a provider.
type TableFileInfo struct {
	FileID      string
	NumChunks   uint32
	URL         string
	SplitOffset uint64

	RefreshAfter *time.Time
}

// ChunkStoreClient is the minimal, read-only remote-chunk-store API needed by RemoteChunkStore.
//
// Implementations typically wrap a transport-specific RPC layer (gRPC over libp2p, etc).
type ChunkStoreClient interface {
	GetRepoMetadata(ctx context.Context, repoID RepoID, repoPath string, format ClientRepoFormat) (RepoMetadata, error)
	HasChunks(ctx context.Context, repoPath string, hashes [][]byte) (absentIndices []int32, err error)
	Root(ctx context.Context, repoPath string) (rootHash []byte, err error)
	ListTableFiles(ctx context.Context, repoID RepoID, repoPath, repoToken string) (rootHash []byte, tables []TableFileInfo, appendix []TableFileInfo, err error)

	// Optional; transports that don't support expiring URLs may return ErrUnimplemented.
	RefreshTableFileURL(ctx context.Context, repoID RepoID, repoPath, fileID string) (url string, refreshAfter *time.Time, err error)
}

// DownloaderClient transports file and chunk downloads used by RemoteChunkStore.
type DownloaderClient interface {
	DownloadFile(ctx context.Context, id string) (r io.ReadCloser, size uint64, err error)
	DownloadChunks(ctx context.Context, hashes []string, onChunk func(hash string, compressed []byte) error) error
}
