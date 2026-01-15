package grpcswarm

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/nustiueudinastea/doltswarm"
	"github.com/nustiueudinastea/doltswarm/integration/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	remotesapi "github.com/dolthub/dolt/go/gen/proto/dolt/services/remotesapi/v1alpha1"
)

// ConnSnapshot is implemented by integration transports that maintain gRPC conns to peers (e.g. p2p.P2P).
type ConnSnapshot interface {
	SnapshotConns() map[string]grpc.ClientConnInterface
}

// Providers implements doltswarm.ProviderPicker using a snapshot of connected peers.
type Providers struct {
	Src      ConnSnapshot
	rr       uint64
	lastUsed atomic.Value // stores string of last used provider ID
}

// LastUsedProvider returns the ID of the most recently selected provider.
// This is useful when context-based tracking doesn't propagate through external layers.
func (p *Providers) LastUsedProvider() string {
	if p == nil {
		return ""
	}
	v := p.lastUsed.Load()
	if v == nil {
		return ""
	}
	return v.(string)
}

func (p *Providers) PickProvider(ctx context.Context, repo doltswarm.RepoID) (doltswarm.Provider, error) {
	_ = repo
	if p == nil || p.Src == nil {
		return nil, fmt.Errorf("no conn source")
	}
	conns := p.Src.SnapshotConns()
	if len(conns) == 0 {
		return nil, fmt.Errorf("no connected providers")
	}

	// Build set of excluded providers (ones that returned stale data on previous attempts)
	excluded := make(map[string]struct{})
	for _, id := range doltswarm.ExcludedProvidersFromContext(ctx) {
		excluded[id] = struct{}{}
	}

	// Helper to record which provider was selected (for retry logic)
	recordUsed := func(id string) {
		p.lastUsed.Store(id)
		if tracker := doltswarm.UsedProviderTrackerFromContext(ctx); tracker != nil {
			tracker.Set(id)
		}
	}

	// If the core sync engine provides a provider hint (e.g. from the peer that advertised a commit),
	// honor it when possible to avoid fetching from stale providers.
	if preferred, ok := doltswarm.PreferredProviderFromContext(ctx); ok {
		if _, isExcluded := excluded[preferred]; !isExcluded {
			if cc, ok := conns[preferred]; ok {
				recordUsed(preferred)
				return &provider{
					id: preferred,
					cs: &chunkStoreClient{c: remotesapi.NewChunkStoreServiceClient(cc)},
					dl: &downloaderClient{c: proto.NewDownloaderClient(cc)},
				}, nil
			}
		}
	}

	// Filter out excluded providers
	ids := make([]string, 0, len(conns))
	for id := range conns {
		if _, isExcluded := excluded[id]; !isExcluded {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		// All providers are excluded, fall back to any available
		for id := range conns {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	idx := atomic.AddUint64(&p.rr, 1) - 1
	id := ids[int(idx%uint64(len(ids)))]
	cc := conns[id]
	recordUsed(id)
	return &provider{
		id: id,
		cs: &chunkStoreClient{c: remotesapi.NewChunkStoreServiceClient(cc)},
		dl: &downloaderClient{c: proto.NewDownloaderClient(cc)},
	}, nil
}

type provider struct {
	id string
	cs doltswarm.ChunkStoreClient
	dl doltswarm.DownloaderClient
}

func (p *provider) ID() string                             { return p.id }
func (p *provider) ChunkStore() doltswarm.ChunkStoreClient { return p.cs }
func (p *provider) Downloader() doltswarm.DownloaderClient { return p.dl }

type chunkStoreClient struct {
	c remotesapi.ChunkStoreServiceClient
}

func (c *chunkStoreClient) GetRepoMetadata(ctx context.Context, repoID doltswarm.RepoID, repoPath string, format doltswarm.ClientRepoFormat) (doltswarm.RepoMetadata, error) {
	resp, err := c.c.GetRepoMetadata(ctx, &remotesapi.GetRepoMetadataRequest{
		RepoId:   &remotesapi.RepoId{Org: repoID.Org, RepoName: repoID.RepoName},
		RepoPath: repoPath,
		ClientRepoFormat: &remotesapi.ClientRepoFormat{
			NbfVersion: format.NbfVersion,
			NbsVersion: format.NbsVersion,
		},
	})
	if err != nil {
		return doltswarm.RepoMetadata{}, err
	}
	return doltswarm.RepoMetadata{
		NbfVersion:  resp.NbfVersion,
		NbsVersion:  resp.NbsVersion,
		StorageSize: resp.StorageSize,
	}, nil
}

func (c *chunkStoreClient) HasChunks(ctx context.Context, repoPath string, hashes [][]byte) ([]int32, error) {
	resp, err := c.c.HasChunks(ctx, &remotesapi.HasChunksRequest{RepoPath: repoPath, Hashes: hashes})
	if err != nil {
		return nil, err
	}
	return resp.Absent, nil
}

func (c *chunkStoreClient) Root(ctx context.Context, repoPath string) ([]byte, error) {
	resp, err := c.c.Root(ctx, &remotesapi.RootRequest{RepoPath: repoPath})
	if err != nil {
		return nil, err
	}
	return resp.RootHash, nil
}

func (c *chunkStoreClient) ListTableFiles(ctx context.Context, repoID doltswarm.RepoID, repoPath, repoToken string) ([]byte, []doltswarm.TableFileInfo, []doltswarm.TableFileInfo, error) {
	resp, err := c.c.ListTableFiles(ctx, &remotesapi.ListTableFilesRequest{
		RepoId:    &remotesapi.RepoId{Org: repoID.Org, RepoName: repoID.RepoName},
		RepoPath:  repoPath,
		RepoToken: repoToken,
	})
	if err != nil {
		return nil, nil, nil, err
	}

	toInfo := func(list []*remotesapi.TableFileInfo) []doltswarm.TableFileInfo {
		out := make([]doltswarm.TableFileInfo, 0, len(list))
		for _, t := range list {
			var refresh *time.Time
			if t.RefreshAfter != nil {
				v := t.RefreshAfter.AsTime()
				refresh = &v
			}
			out = append(out, doltswarm.TableFileInfo{
				FileID:       t.FileId,
				NumChunks:    t.NumChunks,
				URL:          t.Url,
				SplitOffset:  t.SplitOffset,
				RefreshAfter: refresh,
			})
		}
		return out
	}

	return resp.RootHash, toInfo(resp.TableFileInfo), toInfo(resp.AppendixTableFileInfo), nil
}

func (c *chunkStoreClient) RefreshTableFileURL(ctx context.Context, repoID doltswarm.RepoID, repoPath, fileID string) (string, *time.Time, error) {
	resp, err := c.c.RefreshTableFileUrl(ctx, &remotesapi.RefreshTableFileUrlRequest{
		RepoId:   &remotesapi.RepoId{Org: repoID.Org, RepoName: repoID.RepoName},
		RepoPath: repoPath,
		FileId:   fileID,
	})
	if err != nil {
		return "", nil, err
	}
	var t *time.Time
	if resp.RefreshAfter != nil {
		v := resp.RefreshAfter.AsTime()
		t = &v
	}
	return resp.Url, t, nil
}

type downloaderClient struct {
	c proto.DownloaderClient
}

func (d *downloaderClient) DownloadFile(ctx context.Context, id string) (io.ReadCloser, uint64, error) {
	ctx, cancel := context.WithCancel(ctx)
	stream, err := d.c.DownloadFile(ctx, &proto.DownloadFileRequest{Id: id})
	if err != nil {
		cancel()
		return nil, 0, err
	}

	md, err := stream.Header()
	if err != nil {
		cancel()
		return nil, 0, err
	}

	var size uint64
	if v := md.Get("file-size"); len(v) > 0 {
		if parsed, err := strconv.ParseUint(v[0], 10, 64); err == nil {
			size = parsed
		}
	}

	pr, pw := io.Pipe()
	go func() {
		defer cancel()
		defer pw.Close()
		for {
			resp, err := stream.Recv()
			if err != nil {
				if err == io.EOF {
					return
				}
				_ = pw.CloseWithError(err)
				return
			}
			if len(resp.Chunk) == 0 {
				continue
			}
			if _, err := pw.Write(resp.Chunk); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
		}
	}()

	return &pipeReadCloser{ReadCloser: pr, cancel: cancel}, size, nil
}

func (d *downloaderClient) DownloadChunks(ctx context.Context, hashes []string, onChunk func(hash string, compressed []byte) error) error {
	stream, err := d.c.DownloadChunks(ctx, &proto.DownloadChunksRequest{Hashes: hashes})
	if err != nil {
		return err
	}
	for {
		resp, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if resp.Hash == "" || len(resp.Chunk) == 0 {
			continue
		}
		if err := onChunk(resp.Hash, resp.Chunk); err != nil {
			return err
		}
	}
}

type pipeReadCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (p *pipeReadCloser) Close() error {
	p.cancel()
	return p.ReadCloser.Close()
}

// Ensure we don't keep metadata import unused if it changes.
var _ = metadata.MD{}
