package doltswarm

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync/atomic"

	remotesapi "github.com/dolthub/dolt/go/gen/proto/dolt/services/remotesapi/v1alpha1"
	"github.com/dolthub/dolt/go/libraries/doltcore/remotestorage"
	"github.com/dolthub/dolt/go/store/atomicerr"
	"github.com/dolthub/dolt/go/store/chunks"
	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/nbs"
	"github.com/nustiueudinastea/doltswarm/proto"
	"github.com/sirupsen/logrus"
)

const (
	getLocsBatchSize    = 256
	chunkAggDistance    = 8 * 1024
	maxHasManyBatchSize = 16 * 1024
)

func NewRemoteChunkStore(client Client, peerID string, dbName string, nbfVersion string, logger *logrus.Entry) (*RemoteChunkStore, error) {
	rcs := &RemoteChunkStore{
		dbName:      dbName,
		client:      client,
		peerID:      peerID,
		cache:       newMapChunkCache(),
		httpFetcher: &http.Client{},
		concurrency: ConcurrencyParams{
			ConcurrentSmallFetches: 64,
			ConcurrentLargeFetches: 2,
			LargeFetchSize:         2 * 1024 * 1024,
		},
		nbfVersion: nbfVersion,
		log:        logger,
	}

	metadata, err := client.GetRepoMetadata(context.Background(), &remotesapi.GetRepoMetadataRequest{
		RepoId:   rcs.getRepoId(),
		RepoPath: "",
		ClientRepoFormat: &remotesapi.ClientRepoFormat{
			NbfVersion: nbfVersion,
			NbsVersion: nbs.StorageVersion,
		},
	})
	if err != nil {
		return nil, err
	}

	rcs.repoSize = metadata.StorageSize

	err = rcs.loadRoot(context.Background())
	if err != nil {
		return nil, err
	}

	return rcs, nil
}

type HTTPFetcher interface {
	Do(req *http.Request) (*http.Response, error)
}

type ConcurrencyParams struct {
	ConcurrentSmallFetches int
	ConcurrentLargeFetches int
	LargeFetchSize         int
}

type RemoteChunkStore struct {
	dbName      string
	client      Client
	peerID      string
	cache       remotestorage.ChunkCache
	httpFetcher HTTPFetcher
	concurrency ConcurrencyParams
	nbfVersion  string
	repoSize    uint64
	root        hash.Hash
	log         *logrus.Entry
}

func (rcs *RemoteChunkStore) Get(ctx context.Context, h hash.Hash) (chunks.Chunk, error) {
	rcs.log.Info("calling Get")

	hashes := hash.HashSet{h: struct{}{}}
	var found *chunks.Chunk
	err := rcs.GetMany(ctx, hashes, func(_ context.Context, c *chunks.Chunk) { found = c })
	if err != nil {
		return chunks.EmptyChunk, err
	}
	if found != nil {
		return *found, nil
	} else {
		return chunks.EmptyChunk, nil
	}
}

func (rcs *RemoteChunkStore) GetMany(ctx context.Context, hashes hash.HashSet, found func(context.Context, *chunks.Chunk)) error {
	rcs.log.Info("calling GetMany")
	ae := atomicerr.New()
	decompressedSize := uint64(0)
	err := rcs.GetManyCompressed(ctx, hashes, func(ctx context.Context, cc nbs.CompressedChunk) {
		if ae.IsSet() {
			return
		}
		c, err := cc.ToChunk()
		if ae.SetIfErrAndCheck(err) {
			return
		}
		atomic.AddUint64(&decompressedSize, uint64(len(c.Data())))
		found(ctx, &c)
	})
	if err != nil {
		return err
	}
	if err = ae.Get(); err != nil {
		return err
	}
	return nil
}

func (rcs *RemoteChunkStore) GetManyCompressed(ctx context.Context, hashes hash.HashSet, found func(context.Context, nbs.CompressedChunk)) error {
	rcs.log.Info("calling GetManyCompressed")
	hashToChunk := rcs.cache.Get(hashes)

	notCached := make([]hash.Hash, 0, len(hashes))
	for h := range hashes {
		c := hashToChunk[h]

		if c.IsEmpty() {
			notCached = append(notCached, h)
		} else {
			found(ctx, c)
		}
	}

	if len(notCached) > 0 {
		err := rcs.downloadChunksAndCache(ctx, hashes, notCached, found)

		if err != nil {
			return err
		}
	}

	return nil
}

func (rcs *RemoteChunkStore) downloadChunksAndCache(ctx context.Context, hashes hash.HashSet, notCached []hash.Hash, found func(context.Context, nbs.CompressedChunk)) error {
	toSend := make(map[hash.Hash]struct{}, len(notCached))
	for _, h := range notCached {
		toSend[h] = struct{}{}
	}

	hashesToDownload := make([]string, len(notCached))
	for i, h := range notCached {
		hashesToDownload[i] = h.String()
	}

	response, err := rcs.client.DownloadChunks(ctx, &proto.DownloadChunksRequest{Hashes: hashesToDownload})
	if err != nil {
		return fmt.Errorf("failed to download chunks: %w", err)
	}

	chunkMsg := new(proto.DownloadChunksResponse)
	for {
		err = response.RecvMsg(chunkMsg)
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("failed to receive chunk: %w", err)
		}

		if len(chunkMsg.GetChunk()) > 0 {
			h := hash.Parse(chunkMsg.GetHash())
			compressedChunk, err := nbs.NewCompressedChunk(h, chunkMsg.GetChunk())
			if err != nil {
				return fmt.Errorf("failed to create compressed chunk for hash '%s': %w", chunkMsg.GetHash(), err)
			}
			if rcs.cache.PutChunk(compressedChunk) {
				return fmt.Errorf("cache full")
			}

			if _, send := toSend[h]; send {
				found(ctx, compressedChunk)
			}
		}
		chunkMsg.Chunk = chunkMsg.Chunk[:0]
	}

	return nil
}

func (rcs *RemoteChunkStore) Has(ctx context.Context, h hash.Hash) (bool, error) {
	rcs.log.Info("calling Has")
	hashes := hash.HashSet{h: struct{}{}}
	absent, err := rcs.HasMany(ctx, hashes)

	if err != nil {
		return false, err
	}

	return len(absent) == 0, nil
}

func (rcs *RemoteChunkStore) HasMany(ctx context.Context, hashes hash.HashSet) (hash.HashSet, error) {
	rcs.log.Info("calling HasMany")

	notCached := rcs.cache.Has(hashes)

	if len(notCached) == 0 {
		return notCached, nil
	}

	// convert the set to a slice of hashes and a corresponding slice of the byte encoding for those hashes
	hashSl, byteSl := remotestorage.HashSetToSlices(notCached)

	absent := make(hash.HashSet)
	var found []nbs.CompressedChunk
	var err error

	batchItr(len(hashSl), maxHasManyBatchSize, func(st, end int) (stop bool) {
		// slice the slices into a batch of hashes
		currHashSl := hashSl[st:end]
		currByteSl := byteSl[st:end]

		// send a request to the remote api to determine which chunks the remote api already has
		req := &remotesapi.HasChunksRequest{Hashes: currByteSl, RepoPath: rcs.peerID}
		var resp *remotesapi.HasChunksResponse
		resp, err = rcs.client.HasChunks(ctx, req)
		if err != nil {
			err = remotestorage.NewRpcError(err, "HasChunks", rcs.peerID, req)
			return true
		}

		numAbsent := len(resp.Absent)
		sort.Slice(resp.Absent, func(i, j int) bool {
			return resp.Absent[i] < resp.Absent[j]
		})

		// loop over every hash in the current batch, and if they are absent from the remote host add them to the
		// absent set, otherwise append them to the found slice
		for i, j := 0, 0; i < len(currHashSl); i++ {
			currHash := currHashSl[i]

			nextAbsent := -1
			if j < numAbsent {
				nextAbsent = int(resp.Absent[j])
			}

			if i == nextAbsent {
				absent[currHash] = struct{}{}
				j++
			} else {
				c := nbs.ChunkToCompressedChunk(chunks.NewChunkWithHash(currHash, []byte{}))
				found = append(found, c)
			}
		}

		return false
	})

	if err != nil {
		return nil, err
	}

	if len(found)+len(absent) != len(notCached) {
		panic("not all chunks were accounted for")
	}

	if len(found) > 0 {
		if rcs.cache.Put(found) {
			return hash.HashSet{}, remotestorage.ErrCacheCapacityExceeded
		}
	}

	return absent, nil
}

func (rcs *RemoteChunkStore) Put(ctx context.Context, c chunks.Chunk, getAddrs chunks.GetAddrsCb) error {
	rcs.log.Info("calling Put")
	return fmt.Errorf("not supported")
}

func (rcs *RemoteChunkStore) Version() string {
	rcs.log.Info("calling Version: ", rcs.nbfVersion)
	return rcs.nbfVersion
}

func (rcs *RemoteChunkStore) Rebase(ctx context.Context) error {
	rcs.log.Info("calling Rebase")
	return fmt.Errorf("not supported")
}

func (rcs *RemoteChunkStore) loadRoot(ctx context.Context) error {
	req := &remotesapi.RootRequest{RepoPath: rcs.peerID}
	resp, err := rcs.client.Root(ctx, req)
	if err != nil {
		return remotestorage.NewRpcError(err, "Root", rcs.peerID, req)
	}
	rcs.root = hash.New(resp.RootHash)
	return nil
}

func (rcs *RemoteChunkStore) Root(ctx context.Context) (hash.Hash, error) {
	rcs.log.Info("calling Root")
	return rcs.root, nil
}

func (rcs *RemoteChunkStore) Commit(ctx context.Context, current, last hash.Hash) (bool, error) {
	rcs.log.Info("calling Commit")
	return false, fmt.Errorf("not supported")
}

func (rcs *RemoteChunkStore) Stats() interface{} {
	rcs.log.Info("calling Stats")
	return nil
}

func (rcs *RemoteChunkStore) StatsSummary() string {
	rcs.log.Info("calling StatsSummary")
	return "Unsupported"
}

func (rcs *RemoteChunkStore) Close() error {
	rcs.log.Info("calling Close")
	return nil
}

func (rcs *RemoteChunkStore) getRepoId() *remotesapi.RepoId {
	return &remotesapi.RepoId{Org: rcs.peerID, RepoName: rcs.dbName}
}

//
// TableFileStore implementation
//

func (rcs *RemoteChunkStore) Sources(ctx context.Context) (hash.Hash, []chunks.TableFile, []chunks.TableFile, error) {
	rcs.log.Info("calling Sources")
	id := rcs.getRepoId()
	req := &remotesapi.ListTableFilesRequest{RepoId: id, RepoPath: "", RepoToken: ""}
	resp, err := rcs.client.ListTableFiles(ctx, req)
	if err != nil {
		return hash.Hash{}, nil, nil, fmt.Errorf("failed to list table files: %w", err)
	}
	sourceFiles := getTableFiles(rcs.client, resp.TableFileInfo)
	// TODO: remove this
	for _, nfo := range resp.TableFileInfo {
		rcs.log.Info(nfo)
	}
	appendixFiles := getTableFiles(rcs.client, resp.AppendixTableFileInfo)
	return hash.New(resp.RootHash), sourceFiles, appendixFiles, nil
}

func getTableFiles(client Client, infoList []*remotesapi.TableFileInfo) []chunks.TableFile {
	tableFiles := make([]chunks.TableFile, 0)
	for _, nfo := range infoList {
		tableFiles = append(tableFiles, RemoteTableFile{client, nfo})
	}
	return tableFiles
}

func (rcs *RemoteChunkStore) Size(ctx context.Context) (uint64, error) {
	rcs.log.Info("calling Size")
	return rcs.repoSize, nil
}

func (rcs *RemoteChunkStore) WriteTableFile(ctx context.Context, fileId string, numChunks int, contentHash []byte, getRd func() (io.ReadCloser, uint64, error)) error {
	return fmt.Errorf("not supported")
}

func (rcs *RemoteChunkStore) AddTableFilesToManifest(ctx context.Context, fileIdToNumChunks map[string]int) error {
	return fmt.Errorf("not supported")
}

func (rcs *RemoteChunkStore) PruneTableFiles(ctx context.Context) error {
	return fmt.Errorf("not supported")
}

func (rcs *RemoteChunkStore) SetRootChunk(ctx context.Context, root, previous hash.Hash) error {
	return fmt.Errorf("not supported")
}

func (rcs *RemoteChunkStore) SupportedOperations() chunks.TableFileStoreOps {
	return chunks.TableFileStoreOps{
		CanRead:  true,
		CanWrite: false,
		CanPrune: false,
		CanGC:    false,
	}
}
