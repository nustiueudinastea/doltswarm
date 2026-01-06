package grpcswarm

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/nustiueudinastea/doltswarm"
	"github.com/nustiueudinastea/doltswarm/integration/proto"
	"google.golang.org/grpc"

	remotesapi "github.com/dolthub/dolt/go/gen/proto/dolt/services/remotesapi/v1alpha1"
)

type Peer struct {
	id string

	syncer     proto.DBSyncerClient
	bundles    proto.BundleExchangeClient
	downloader proto.DownloaderClient
	chunkStore remotesapi.ChunkStoreServiceClient
}

var _ doltswarm.Peer = (*Peer)(nil)
var _ doltswarm.SyncClient = (*Peer)(nil)
var _ doltswarm.DownloaderClient = (*Peer)(nil)
var _ doltswarm.ChunkStoreClient = (*Peer)(nil)

func NewPeer(id string, conn grpc.ClientConnInterface) *Peer {
	return &Peer{
		id:         id,
		syncer:     proto.NewDBSyncerClient(conn),
		bundles:    proto.NewBundleExchangeClient(conn),
		downloader: proto.NewDownloaderClient(conn),
		chunkStore: remotesapi.NewChunkStoreServiceClient(conn),
	}
}

func (p *Peer) ID() string { return p.id }

func (p *Peer) Sync() doltswarm.SyncClient { return p }

func (p *Peer) ChunkStore() doltswarm.ChunkStoreClient { return p }

func (p *Peer) Downloader() doltswarm.DownloaderClient { return p }

// ---- doltswarm.SyncClient ----

func (p *Peer) AdvertiseCommit(ctx context.Context, ad *doltswarm.CommitAd) (bool, error) {
	resp, err := p.syncer.AdvertiseCommit(ctx, commitAdToProto(ad), grpc.WaitForReady(true))
	if err != nil {
		return false, err
	}
	return resp.Accepted, nil
}

func (p *Peer) RequestCommitsSince(ctx context.Context, since doltswarm.HLCTimestamp) ([]*doltswarm.CommitAd, error) {
	resp, err := p.syncer.RequestCommitsSince(ctx, &proto.RequestCommitsSinceRequest{
		SinceHlc: &proto.HLCTimestamp{
			Wall:    since.Wall,
			Logical: since.Logical,
			PeerId:  since.PeerID,
		},
	}, grpc.WaitForReady(true))
	if err != nil {
		return nil, err
	}

	out := make([]*doltswarm.CommitAd, 0, len(resp.Commits))
	for _, c := range resp.Commits {
		out = append(out, commitAdFromProto(c))
	}
	return out, nil
}

func (p *Peer) GetBundleSince(ctx context.Context, base doltswarm.Checkpoint, req doltswarm.BundleRequest) (doltswarm.CommitBundle, error) {
	stream, err := p.bundles.GetBundleSince(ctx, &proto.GetBundleSinceRequest{
		Base: &proto.Checkpoint{
			Hlc: &proto.HLCTimestamp{
				Wall:    base.HLC.Wall,
				Logical: base.HLC.Logical,
				PeerId:  base.HLC.PeerID,
			},
			CommitHash: base.CommitHash,
		},
		Req: &proto.BundleRequest{
			MaxCommits:         int32(req.MaxCommits),
			MaxBytes:           req.MaxBytes,
			AllowPartial:       req.AllowPartial,
			IncludeDiagnostics: req.IncludeDiagnostics,
		},
	}, grpc.WaitForReady(true))
	if err != nil {
		return doltswarm.CommitBundle{}, err
	}

	var out doltswarm.CommitBundle
	headerSet := false

	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return doltswarm.CommitBundle{}, err
		}

		switch it := msg.Item.(type) {
		case *proto.GetBundleSinceChunk_Header:
			h := it.Header
			if h == nil || h.Base == nil || h.Base.Hlc == nil {
				continue
			}
			headerSet = true
			out.Header = doltswarm.BundleHeader{
				Base: doltswarm.Checkpoint{
					HLC: doltswarm.HLCTimestamp{
						Wall:    h.Base.Hlc.Wall,
						Logical: h.Base.Hlc.Logical,
						PeerID:  h.Base.Hlc.PeerId,
					},
					CommitHash: h.Base.CommitHash,
				},
				ProviderHeadHLC: doltswarm.HLCTimestamp{
					Wall:    h.ProviderHeadHlc.GetWall(),
					Logical: h.ProviderHeadHlc.GetLogical(),
					PeerID:  h.ProviderHeadHlc.GetPeerId(),
				},
				ProviderHeadHash: h.ProviderHeadHash,
				BaseMismatch:     h.BaseMismatch,
			}
			for _, cp := range h.ProviderCheckpoints {
				if cp == nil || cp.Hlc == nil {
					continue
				}
				out.Header.ProviderCheckpoints = append(out.Header.ProviderCheckpoints, doltswarm.Checkpoint{
					HLC: doltswarm.HLCTimestamp{
						Wall:    cp.Hlc.Wall,
						Logical: cp.Hlc.Logical,
						PeerID:  cp.Hlc.PeerId,
					},
					CommitHash: cp.CommitHash,
				})
			}
		case *proto.GetBundleSinceChunk_Commit:
			c := it.Commit
			if c == nil || c.Hlc == nil {
				continue
			}
			out.Commits = append(out.Commits, doltswarm.BundledCommit{
				HLC: doltswarm.HLCTimestamp{
					Wall:    c.Hlc.Wall,
					Logical: c.Hlc.Logical,
					PeerID:  c.Hlc.PeerId,
				},
				CommitHash:   c.CommitHash,
				MetadataJSON: c.MetadataJson,
				MetadataSig:  c.MetadataSig,
			})
		case *proto.GetBundleSinceChunk_Chunk:
			ch := it.Chunk
			if ch == nil {
				continue
			}
			out.Chunks = append(out.Chunks, doltswarm.BundledChunk{
				Hash:  ch.Hash,
				Data:  ch.Data,
				Codec: doltswarm.ChunkCodec(ch.Codec),
			})
		}
	}

	if !headerSet {
		// Best effort: return empty header; callers should treat this as an error.
	}
	return out, nil
}

// ---- doltswarm.ChunkStoreClient ----

func (p *Peer) GetRepoMetadata(ctx context.Context, repoID doltswarm.RepoID, repoPath string, format doltswarm.ClientRepoFormat) (doltswarm.RepoMetadata, error) {
	req := &remotesapi.GetRepoMetadataRequest{
		RepoId:   &remotesapi.RepoId{Org: repoID.Org, RepoName: repoID.RepoName},
		RepoPath: repoPath,
		ClientRepoFormat: &remotesapi.ClientRepoFormat{
			NbfVersion: format.NbfVersion,
			NbsVersion: format.NbsVersion,
		},
	}
	resp, err := p.chunkStore.GetRepoMetadata(ctx, req, grpc.WaitForReady(true))
	if err != nil {
		return doltswarm.RepoMetadata{}, err
	}
	return doltswarm.RepoMetadata{
		NbfVersion:  resp.NbfVersion,
		NbsVersion:  resp.NbsVersion,
		StorageSize: resp.StorageSize,
	}, nil
}

func (p *Peer) HasChunks(ctx context.Context, repoPath string, hashes [][]byte) ([]int32, error) {
	resp, err := p.chunkStore.HasChunks(ctx, &remotesapi.HasChunksRequest{
		RepoPath: repoPath,
		Hashes:   hashes,
	}, grpc.WaitForReady(true))
	if err != nil {
		return nil, err
	}
	return resp.Absent, nil
}

func (p *Peer) Root(ctx context.Context, repoPath string) ([]byte, error) {
	resp, err := p.chunkStore.Root(ctx, &remotesapi.RootRequest{
		RepoPath: repoPath,
	}, grpc.WaitForReady(true))
	if err != nil {
		return nil, err
	}
	return resp.RootHash, nil
}

func (p *Peer) ListTableFiles(ctx context.Context, repoID doltswarm.RepoID, repoPath, repoToken string) ([]byte, []doltswarm.TableFileInfo, []doltswarm.TableFileInfo, error) {
	resp, err := p.chunkStore.ListTableFiles(ctx, &remotesapi.ListTableFilesRequest{
		RepoId:    &remotesapi.RepoId{Org: repoID.Org, RepoName: repoID.RepoName},
		RepoPath:  repoPath,
		RepoToken: repoToken,
	}, grpc.WaitForReady(true))
	if err != nil {
		return nil, nil, nil, err
	}

	sources := make([]doltswarm.TableFileInfo, 0, len(resp.TableFileInfo))
	for _, t := range resp.TableFileInfo {
		var refreshAfter *time.Time
		if t.RefreshAfter != nil {
			rt := t.RefreshAfter.AsTime()
			refreshAfter = &rt
		}
		sources = append(sources, doltswarm.TableFileInfo{
			FileID:       t.FileId,
			NumChunks:    t.NumChunks,
			URL:          t.Url,
			SplitOffset:  t.SplitOffset,
			RefreshAfter: refreshAfter,
		})
	}

	appendix := make([]doltswarm.TableFileInfo, 0, len(resp.AppendixTableFileInfo))
	for _, t := range resp.AppendixTableFileInfo {
		var refreshAfter *time.Time
		if t.RefreshAfter != nil {
			rt := t.RefreshAfter.AsTime()
			refreshAfter = &rt
		}
		appendix = append(appendix, doltswarm.TableFileInfo{
			FileID:       t.FileId,
			NumChunks:    t.NumChunks,
			URL:          t.Url,
			SplitOffset:  t.SplitOffset,
			RefreshAfter: refreshAfter,
		})
	}

	return resp.RootHash, sources, appendix, nil
}

func (p *Peer) RefreshTableFileURL(ctx context.Context, repoID doltswarm.RepoID, repoPath, fileID string) (string, *time.Time, error) {
	resp, err := p.chunkStore.RefreshTableFileUrl(ctx, &remotesapi.RefreshTableFileUrlRequest{
		RepoId:   &remotesapi.RepoId{Org: repoID.Org, RepoName: repoID.RepoName},
		RepoPath: repoPath,
		FileId:   fileID,
	}, grpc.WaitForReady(true))
	if err != nil {
		return "", nil, err
	}
	if resp.RefreshAfter == nil {
		return resp.Url, nil, nil
	}
	t := resp.RefreshAfter.AsTime()
	return resp.Url, &t, nil
}

// ---- doltswarm.DownloaderClient ----

func (p *Peer) DownloadFile(ctx context.Context, id string) (io.ReadCloser, uint64, error) {
	stream, err := p.downloader.DownloadFile(ctx, &proto.DownloadFileRequest{Id: id}, grpc.WaitForReady(true))
	if err != nil {
		return nil, 0, err
	}

	md, err := stream.Header()
	if err != nil {
		return nil, 0, err
	}

	var size uint64
	if sizes := md.Get("file-size"); len(sizes) > 0 {
		size, err = strconv.ParseUint(sizes[0], 10, 64)
		if err != nil {
			return nil, 0, fmt.Errorf("invalid file-size header: %w", err)
		}
	}

	r, w := io.Pipe()
	go func() {
		defer func() { _ = w.Close() }()
		for {
			msg, err := stream.Recv()
			if err == io.EOF {
				return
			}
			if err != nil {
				_ = w.CloseWithError(err)
				return
			}
			if len(msg.GetChunk()) > 0 {
				if _, err := w.Write(msg.Chunk); err != nil {
					_ = stream.CloseSend()
					return
				}
			}
		}
	}()

	return r, size, nil
}

func (p *Peer) DownloadChunks(ctx context.Context, hashes []string, onChunk func(hash string, compressed []byte) error) error {
	stream, err := p.downloader.DownloadChunks(ctx, &proto.DownloadChunksRequest{Hashes: hashes}, grpc.WaitForReady(true))
	if err != nil {
		return err
	}
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := onChunk(msg.Hash, msg.Chunk); err != nil {
			return err
		}
	}
}
