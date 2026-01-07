package grpcswarm

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/dolthub/dolt/go/libraries/doltcore/remotesrv"
	"github.com/dolthub/dolt/go/libraries/doltcore/remotestorage"
	"github.com/dolthub/dolt/go/store/chunks"
	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/nbs"
	"github.com/nustiueudinastea/doltswarm"
	"github.com/nustiueudinastea/doltswarm/integration/proto"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	remotesapi "github.com/dolthub/dolt/go/gen/proto/dolt/services/remotesapi/v1alpha1"
)

const downloadChunkSize = 3 * 1024

// NewChunkStoreServer exposes a read-only Dolt remotesapi.ChunkStoreService plus a custom Downloader service.
func NewChunkStoreServer(db *doltswarm.DB, logger *logrus.Entry) (*ChunkStoreServer, error) {
	if db == nil {
		return nil, fmt.Errorf("db is nil")
	}
	if logger == nil {
		logger = logrus.NewEntry(logrus.StandardLogger())
	}
	cs, err := db.GetChunkStore()
	if err != nil {
		return nil, err
	}
	store, ok := any(cs).(remotesrv.RemoteSrvStore)
	if !ok {
		return nil, fmt.Errorf("chunk store is %T (expected remotesrv.RemoteSrvStore)", cs)
	}
	return &ChunkStoreServer{
		store:    store,
		filePath: db.GetFilePath(),
		log:      logger.WithField("context", "chunkstore"),
	}, nil
}

type ChunkStoreServer struct {
	store    remotesrv.RemoteSrvStore
	filePath string
	log      *logrus.Entry

	remotesapi.UnimplementedChunkStoreServiceServer
	proto.UnimplementedDownloaderServer
}

func (s *ChunkStoreServer) HasChunks(ctx context.Context, req *remotesapi.HasChunksRequest) (*remotesapi.HasChunksResponse, error) {
	if err := validateRepoRequest(req); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := validateHashes("hashes", req.Hashes); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	if err := s.store.Rebase(ctx); err != nil {
		return nil, status.Errorf(codes.Internal, "rebase: %v", err)
	}

	hashes, hashToIndex := remotestorage.ParseByteSlices(req.Hashes)
	absent, err := s.store.HasMany(ctx, hashes)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "HasMany: %v", err)
	}

	indices := make([]int32, 0, len(absent))
	for h := range absent {
		indices = append(indices, int32(hashToIndex[h]))
	}
	return &remotesapi.HasChunksResponse{Absent: indices}, nil
}

func (s *ChunkStoreServer) GetDownloadLocations(ctx context.Context, req *remotesapi.GetDownloadLocsRequest) (*remotesapi.GetDownloadLocsResponse, error) {
	return nil, status.Error(codes.PermissionDenied, "HTTP download locations are not supported")
}

func (s *ChunkStoreServer) StreamDownloadLocations(stream remotesapi.ChunkStoreService_StreamDownloadLocationsServer) error {
	return status.Error(codes.PermissionDenied, "HTTP download locations are not supported")
}

func (s *ChunkStoreServer) GetUploadLocations(ctx context.Context, req *remotesapi.GetUploadLocsRequest) (*remotesapi.GetUploadLocsResponse, error) {
	return nil, status.Error(codes.PermissionDenied, "read-only")
}

func (s *ChunkStoreServer) Rebase(ctx context.Context, req *remotesapi.RebaseRequest) (*remotesapi.RebaseResponse, error) {
	return nil, status.Error(codes.PermissionDenied, "read-only")
}

func (s *ChunkStoreServer) Root(ctx context.Context, req *remotesapi.RootRequest) (*remotesapi.RootResponse, error) {
	if err := validateRepoRequest(req); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := s.store.Rebase(ctx); err != nil {
		return nil, status.Errorf(codes.Internal, "rebase: %v", err)
	}
	h, err := s.store.Root(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "root: %v", err)
	}
	return &remotesapi.RootResponse{RootHash: h[:]}, nil
}

func (s *ChunkStoreServer) Commit(ctx context.Context, req *remotesapi.CommitRequest) (*remotesapi.CommitResponse, error) {
	return nil, status.Error(codes.PermissionDenied, "read-only")
}

func (s *ChunkStoreServer) GetRepoMetadata(ctx context.Context, req *remotesapi.GetRepoMetadataRequest) (*remotesapi.GetRepoMetadataResponse, error) {
	if err := validateRepoRequest(req); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if req.ClientRepoFormat == nil {
		return nil, status.Error(codes.InvalidArgument, "expected non-nil client_repo_format")
	}

	if err := s.store.Rebase(ctx); err != nil {
		return nil, status.Errorf(codes.Internal, "rebase: %v", err)
	}
	size, err := s.store.Size(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "size: %v", err)
	}
	return &remotesapi.GetRepoMetadataResponse{
		NbfVersion:  s.store.Version(),
		NbsVersion:  req.ClientRepoFormat.NbsVersion,
		StorageSize: size,
	}, nil
}

func (s *ChunkStoreServer) ListTableFiles(ctx context.Context, req *remotesapi.ListTableFilesRequest) (*remotesapi.ListTableFilesResponse, error) {
	if err := validateRepoRequest(req); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := s.store.Rebase(ctx); err != nil {
		return nil, status.Errorf(codes.Internal, "rebase: %v", err)
	}

	root, tables, appendixTables, err := s.store.Sources(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "sources: %v", err)
	}

	return &remotesapi.ListTableFilesResponse{
		RootHash:              root[:],
		TableFileInfo:         buildTableFileInfo(tables),
		AppendixTableFileInfo: buildTableFileInfo(appendixTables),
	}, nil
}

func (s *ChunkStoreServer) RefreshTableFileUrl(ctx context.Context, req *remotesapi.RefreshTableFileUrlRequest) (*remotesapi.RefreshTableFileUrlResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not supported")
}

func (s *ChunkStoreServer) AddTableFiles(ctx context.Context, req *remotesapi.AddTableFilesRequest) (*remotesapi.AddTableFilesResponse, error) {
	return nil, status.Error(codes.PermissionDenied, "read-only")
}

// ---- Downloader service ----

func (s *ChunkStoreServer) DownloadFile(req *proto.DownloadFileRequest, server proto.Downloader_DownloadFileServer) error {
	if req.GetId() == "" {
		return status.Error(codes.InvalidArgument, "id is required")
	}

	filePath := filepath.Join(s.filePath, ".dolt", "noms", req.Id)
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		return status.Errorf(codes.NotFound, "stat %s: %v", filePath, err)
	}

	f, err := os.Open(filePath)
	if err != nil {
		return status.Errorf(codes.Internal, "open %s: %v", filePath, err)
	}
	defer f.Close()

	md := metadata.New(map[string]string{
		"file-name": req.Id,
		"file-size": strconv.FormatInt(fileInfo.Size(), 10),
	})
	if err := server.SendHeader(md); err != nil {
		return status.Error(codes.Internal, "send header failed")
	}

	chunk := &proto.DownloadFileResponse{Chunk: make([]byte, downloadChunkSize)}
	for {
		n, err := f.Read(chunk.Chunk)
		switch err {
		case nil:
		case io.EOF:
			return nil
		default:
			return status.Errorf(codes.Internal, "read: %v", err)
		}
		chunk.Chunk = chunk.Chunk[:n]
		if err := server.Send(chunk); err != nil {
			return status.Errorf(codes.Internal, "send: %v", err)
		}
	}
}

func (s *ChunkStoreServer) DownloadChunks(req *proto.DownloadChunksRequest, server proto.Downloader_DownloadChunksServer) error {
	if len(req.Hashes) == 0 {
		return status.Error(codes.InvalidArgument, "at least one hash is required")
	}
	if err := s.store.Rebase(server.Context()); err != nil {
		return status.Errorf(codes.Internal, "rebase: %v", err)
	}
	for _, hashStr := range req.Hashes {
		h := hash.Parse(hashStr)
		ch, err := s.store.Get(server.Context(), h)
		if err != nil {
			return status.Errorf(codes.Internal, "get %s: %v", hashStr, err)
		}
		cc := nbs.ChunkToCompressedChunk(ch)
		if err := server.Send(&proto.DownloadChunksResponse{Hash: hashStr, Chunk: cc.FullCompressedChunk}); err != nil {
			return status.Errorf(codes.Internal, "send: %v", err)
		}
	}
	return nil
}

// ---- helpers ----

type repoRequest interface {
	GetRepoId() *remotesapi.RepoId
	GetRepoPath() string
}

func validateRepoRequest(req repoRequest) error {
	if req.GetRepoPath() == "" && req.GetRepoId() == nil {
		return fmt.Errorf("expected repo_path or repo_id, got neither")
	}
	return nil
}

func validateHashes(field string, hashes [][]byte) error {
	for i, bs := range hashes {
		if len(bs) != hash.ByteLen {
			return fmt.Errorf("expected %s[%d] hash to be %d bytes long, was %d", field, i, hash.ByteLen, len(bs))
		}
	}
	return nil
}

func buildTableFileInfo(tableList []chunks.TableFile) []*remotesapi.TableFileInfo {
	out := make([]*remotesapi.TableFileInfo, 0, len(tableList))
	for _, t := range tableList {
		fileID := t.FileID()
		url := t.LocationPrefix() + fileID + t.LocationSuffix()
		out = append(out, &remotesapi.TableFileInfo{
			FileId:      fileID,
			NumChunks:   uint32(t.NumChunks()),
			Url:         url,
			SplitOffset: t.SplitOffset(),
		})
	}
	return out
}
