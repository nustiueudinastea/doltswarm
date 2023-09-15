// Copyright 2019 Dolthub, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package doltswarm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/nustiueudinastea/doltswarm/proto"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	remotesapi "github.com/dolthub/dolt/go/gen/proto/dolt/services/remotesapi/v1alpha1"
	"github.com/dolthub/dolt/go/libraries/doltcore/remotestorage"
	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/nbs"
)

var ErrUnimplemented = errors.New("unimplemented")
var chunkSize = 1024 * 3

const RepoPathField = "repo_path"

func NewServerChunkStore(logger *logrus.Entry, csCache DBCache, filePath string) *ServerChunkStore {
	return &ServerChunkStore{
		csCache: csCache,
		bucket:  "",
		log: logger.WithFields(logrus.Fields{
			"service": "ChunkStoreService",
		}),
		filePath: filePath,
	}
}

type repoRequest interface {
	GetRepoId() *remotesapi.RepoId
	GetRepoPath() string
}

type ServerChunkStore struct {
	csCache  DBCache
	bucket   string
	log      *logrus.Entry
	filePath string

	remotesapi.UnimplementedChunkStoreServiceServer
	proto.UnimplementedDownloaderServer
}

func (rs *ServerChunkStore) HasChunks(ctx context.Context, req *remotesapi.HasChunksRequest) (*remotesapi.HasChunksResponse, error) {

	if err := ValidateHasChunksRequest(req); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	defer func() { rs.log.Info("finished") }()

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

	indices := make([]int32, len(absent))

	n := 0
	for h := range absent {
		indices[n] = int32(hashToIndex[h])
		n++
	}

	resp := &remotesapi.HasChunksResponse{
		Absent: indices,
	}

	return resp, nil
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

	if err := ValidateRootRequest(req); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	defer func() { rs.log.Info("finished Root") }()

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
	if err := ValidateGetRepoMetadataRequest(req); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	defer func() { rs.log.Info("finished") }()

	cs, err := rs.getOrCreateStore()
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
	if err := ValidateListTableFilesRequest(req); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	defer func() { rs.log.Info("finished ListTableFiles") }()

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

	resp := &remotesapi.ListTableFilesResponse{
		RootHash:              root[:],
		TableFileInfo:         tableFileInfo,
		AppendixTableFileInfo: appendixTableFileInfo,
	}

	return resp, nil
}

// AddTableFiles updates the remote manifest with new table files without modifying the root hash.
func (rs *ServerChunkStore) AddTableFiles(ctx context.Context, req *remotesapi.AddTableFilesRequest) (*remotesapi.AddTableFilesResponse, error) {
	return nil, status.Error(codes.PermissionDenied, "this server only provides read-only access")
}

func (rs *ServerChunkStore) getStore() (RemoteSrvStore, error) {
	return rs.getOrCreateStore()
}

func (rs *ServerChunkStore) getOrCreateStore() (RemoteSrvStore, error) {
	cs, err := rs.csCache.Get()
	if err != nil {
		rs.log.WithError(err).Error("Failed to retrieve chunkstore")
		if errors.Is(err, ErrUnimplemented) {
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

//
// FileDownloaderServer implementation
//

func (rs *ServerChunkStore) DownloadFile(req *proto.DownloadFileRequest, server proto.Downloader_DownloadFileServer) error {
	if req.GetId() == "" {
		return status.Error(codes.InvalidArgument, "id is required")
	}

	filePath := filepath.Join(rs.filePath, ".dolt", "noms", req.Id)
	rs.log.Info(fmt.Sprintf("Sending file %s", filePath))
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		return fmt.Errorf("failed to send file %s: %w", filePath, err)
	}

	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to open file %s: %w", filePath, err)
	}

	md := metadata.New(map[string]string{
		"file-name": req.Id,
		"file-size": strconv.FormatInt(fileInfo.Size(), 10),
	})

	err = server.SendHeader(md)
	if err != nil {
		return status.Error(codes.Internal, "error during sending header")
	}

	chunk := &proto.DownloadFileResponse{Chunk: make([]byte, chunkSize)}
	var n int

Loop:
	for {
		n, err = f.Read(chunk.Chunk)
		switch err {
		case nil:
		case io.EOF:
			break Loop
		default:
			return status.Errorf(codes.Internal, "io.ReadAll: %v", err)
		}
		chunk.Chunk = chunk.Chunk[:n]
		serverErr := server.Send(chunk)
		if serverErr != nil {
			return status.Errorf(codes.Internal, "server.Send: %v", serverErr)
		}
	}
	return nil
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

		chunk, err := cs.Get(context.TODO(), h)
		if err != nil {
			return err
		}
		compressedChunk := nbs.ChunkToCompressedChunk(chunk)

		chunkResponse := &proto.DownloadChunksResponse{Hash: hashStr, Chunk: compressedChunk.FullCompressedChunk}
		serverErr := server.Send(chunkResponse)
		if serverErr != nil {
			return status.Errorf(codes.Internal, "server.Send: %v", serverErr)
		}

	}
	return nil
}
