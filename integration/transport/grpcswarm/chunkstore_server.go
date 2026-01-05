package grpcswarm

import (
	"context"
	"errors"
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

var chunkSize = 1024 * 3

func NewServerChunkStore(logger *logrus.Entry, csCache doltswarm.DBCache, filePath string) *ServerChunkStore {
	return &ServerChunkStore{
		csCache: csCache,
		log: logger.WithFields(logrus.Fields{
			"service": "ChunkStoreService",
		}),
		filePath: filePath,
	}
}

type ServerChunkStore struct {
	csCache  doltswarm.DBCache
	log      *logrus.Entry
	filePath string

	remotesapi.UnimplementedChunkStoreServiceServer
	proto.UnimplementedDownloaderServer
}

func (rs *ServerChunkStore) HasChunks(ctx context.Context, req *remotesapi.HasChunksRequest) (*remotesapi.HasChunksResponse, error) {
	if err := validateRepoRequest(req); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := validateHashes("hashes", req.Hashes); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	cs, err := rs.getStore()
	if err != nil {
		return nil, err
	}

	hashes, hashToIndex := remotestorage.ParseByteSlices(req.Hashes)
	absent, err := cs.HasMany(ctx, hashes)
	if err != nil {
		rs.log.WithError(err).Error("error calling HasMany")
		return nil, status.Error(codes.Internal, "HasMany failure:"+err.Error())
	}

	indices := make([]int32, 0, len(absent))
	for h := range absent {
		indices = append(indices, int32(hashToIndex[h]))
	}

	return &remotesapi.HasChunksResponse{Absent: indices}, nil
}

func (rs *ServerChunkStore) GetDownloadLocations(ctx context.Context, req *remotesapi.GetDownloadLocsRequest) (*remotesapi.GetDownloadLocsResponse, error) {
	return nil, status.Error(codes.PermissionDenied, "HTTP download locations are not supported.")
}

func (rs *ServerChunkStore) StreamDownloadLocations(stream remotesapi.ChunkStoreService_StreamDownloadLocationsServer) error {
	return status.Error(codes.PermissionDenied, "HTTP download locations are not supported.")
}

func (rs *ServerChunkStore) GetUploadLocations(ctx context.Context, req *remotesapi.GetUploadLocsRequest) (*remotesapi.GetUploadLocsResponse, error) {
	return nil, status.Error(codes.PermissionDenied, "this server only provides read-only access")
}

func (rs *ServerChunkStore) Rebase(ctx context.Context, req *remotesapi.RebaseRequest) (*remotesapi.RebaseResponse, error) {
	return nil, status.Error(codes.PermissionDenied, "this server only provides read-only access")
}

func (rs *ServerChunkStore) Root(ctx context.Context, req *remotesapi.RootRequest) (*remotesapi.RootResponse, error) {
	if err := validateRepoRequest(req); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	cs, err := rs.getStore()
	if err != nil {
		return nil, err
	}

	h, err := cs.Root(ctx)
	if err != nil {
		rs.log.WithError(err).Error("error calling Root on chunk store.")
		return nil, status.Error(codes.Internal, "Failed to get root")
	}

	return &remotesapi.RootResponse{RootHash: h[:]}, nil
}

func (rs *ServerChunkStore) Commit(ctx context.Context, req *remotesapi.CommitRequest) (*remotesapi.CommitResponse, error) {
	return nil, status.Error(codes.PermissionDenied, "this server only provides read-only access")
}

func (rs *ServerChunkStore) GetRepoMetadata(ctx context.Context, req *remotesapi.GetRepoMetadataRequest) (*remotesapi.GetRepoMetadataResponse, error) {
	if err := validateRepoRequest(req); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if req.ClientRepoFormat == nil {
		return nil, status.Error(codes.InvalidArgument, "expected non-nil client_repo_format")
	}

	cs, err := rs.getStore()
	if err != nil {
		return nil, err
	}

	size, err := cs.Size(ctx)
	if err != nil {
		rs.log.WithError(err).Error("error calling Size")
		return nil, err
	}

	return &remotesapi.GetRepoMetadataResponse{
		NbfVersion:  cs.Version(),
		NbsVersion:  req.ClientRepoFormat.NbsVersion,
		StorageSize: size,
	}, nil
}

func (rs *ServerChunkStore) ListTableFiles(ctx context.Context, req *remotesapi.ListTableFilesRequest) (*remotesapi.ListTableFilesResponse, error) {
	if err := validateRepoRequest(req); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	cs, err := rs.getStore()
	if err != nil {
		return nil, err
	}

	root, tables, appendixTables, err := cs.Sources(ctx)
	if err != nil {
		rs.log.WithError(err).Error("error getting chunk store Sources")
		return nil, status.Error(codes.Internal, "failed to get sources")
	}

	tableFileInfo := buildTableFileInfo(tables)
	appendixTableFileInfo := buildTableFileInfo(appendixTables)

	return &remotesapi.ListTableFilesResponse{
		RootHash:              root[:],
		TableFileInfo:         tableFileInfo,
		AppendixTableFileInfo: appendixTableFileInfo,
	}, nil
}

func (rs *ServerChunkStore) AddTableFiles(ctx context.Context, req *remotesapi.AddTableFilesRequest) (*remotesapi.AddTableFilesResponse, error) {
	return nil, status.Error(codes.PermissionDenied, "this server only provides read-only access")
}

func (rs *ServerChunkStore) getStore() (remotesrv.RemoteSrvStore, error) {
	cs, err := rs.csCache.Get()
	if err != nil {
		rs.log.WithError(err).Error("Failed to retrieve chunkstore")
		if errors.Is(err, doltswarm.ErrUnimplemented) {
			return nil, status.Error(codes.Unimplemented, err.Error())
		}
		return nil, err
	}
	if cs == nil {
		rs.log.Error("internal error getting chunk store; csCache.Get returned nil")
		return nil, status.Error(codes.Internal, "Could not get chunkstore")
	}
	return cs, nil
}

// ---- Downloader service ----

func (rs *ServerChunkStore) DownloadFile(req *proto.DownloadFileRequest, server proto.Downloader_DownloadFileServer) error {
	if req.GetId() == "" {
		return status.Error(codes.InvalidArgument, "id is required")
	}

	filePath := filepath.Join(rs.filePath, ".dolt", "noms", req.Id)
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		return fmt.Errorf("failed to stat file %s: %w", filePath, err)
	}

	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to open file %s: %w", filePath, err)
	}
	defer f.Close()

	md := metadata.New(map[string]string{
		"file-name": req.Id,
		"file-size": strconv.FormatInt(fileInfo.Size(), 10),
	})
	if err := server.SendHeader(md); err != nil {
		return status.Error(codes.Internal, "error during sending header")
	}

	chunk := &proto.DownloadFileResponse{Chunk: make([]byte, chunkSize)}
	for {
		n, err := f.Read(chunk.Chunk)
		switch err {
		case nil:
		case io.EOF:
			return nil
		default:
			return status.Errorf(codes.Internal, "file read: %v", err)
		}
		chunk.Chunk = chunk.Chunk[:n]
		if err := server.Send(chunk); err != nil {
			return status.Errorf(codes.Internal, "server send: %v", err)
		}
	}
}

func (rs *ServerChunkStore) DownloadChunks(req *proto.DownloadChunksRequest, server proto.Downloader_DownloadChunksServer) error {
	if len(req.Hashes) == 0 {
		return status.Error(codes.InvalidArgument, "at least one hash is required")
	}

	cs, err := rs.getStore()
	if err != nil {
		return err
	}

	for _, hashStr := range req.Hashes {
		h := hash.Parse(hashStr)
		chunk, err := cs.Get(server.Context(), h)
		if err != nil {
			return err
		}
		compressedChunk := nbs.ChunkToCompressedChunk(chunk)

		if err := server.Send(&proto.DownloadChunksResponse{Hash: hashStr, Chunk: compressedChunk.FullCompressedChunk}); err != nil {
			return status.Errorf(codes.Internal, "server send: %v", err)
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
	} else if req.GetRepoPath() == "" {
		id := req.GetRepoId()
		if id.Org == "" || id.RepoName == "" {
			return fmt.Errorf("expected repo_id.org and repo_id.repo_name, missing at least one")
		}
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
	tableFileInfo := make([]*remotesapi.TableFileInfo, 0, len(tableList))
	for _, t := range tableList {
		fileID := t.FileID()
		url := t.LocationPrefix() + fileID + t.LocationSuffix()
		tableFileInfo = append(tableFileInfo, &remotesapi.TableFileInfo{
			FileId:      fileID,
			NumChunks:   uint32(t.NumChunks()),
			Url:         url,
			SplitOffset: t.SplitOffset(),
		})
	}
	return tableFileInfo
}
