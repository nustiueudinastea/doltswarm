package doltswarm

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/dolthub/dolt/go/libraries/doltcore/remotestorage"
	"github.com/dolthub/dolt/go/store/atomicerr"
	"github.com/dolthub/dolt/go/store/chunks"
	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/nbs"
	"github.com/sirupsen/logrus"
)

const (
	getLocsBatchSize    = 256
	chunkAggDistance    = 8 * 1024
	maxHasManyBatchSize = 16 * 1024
)

var chunkCache = newMapChunkCache()

func NewRemoteChunkStore(peer Peer, dbName string, nbfVersion string, logger *logrus.Entry) (*RemoteChunkStore, error) {
	peerID := peer.ID()
	rcs := &RemoteChunkStore{
		dbName:      dbName,
		chunkClient: peer.ChunkStore(),
		downloader:  peer.Downloader(),
		peerID:      peerID,
		cache:       chunkCache,
		httpFetcher: &http.Client{},
		concurrency: ConcurrencyParams{
			ConcurrentSmallFetches: 64,
			ConcurrentLargeFetches: 2,
			LargeFetchSize:         2 * 1024 * 1024,
		},
		nbfVersion: nbfVersion,
		log:        logger,
	}

	metadata, err := rcs.chunkClient.GetRepoMetadata(
		context.Background(),
		rcs.getRepoId(),
		"",
		ClientRepoFormat{NbfVersion: nbfVersion, NbsVersion: nbs.StorageVersion},
	)
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
	chunkClient ChunkStoreClient
	downloader  DownloaderClient
	peerID      string
	cache       remotestorage.ChunkCache
	httpFetcher HTTPFetcher
	concurrency ConcurrencyParams
	nbfVersion  string
	repoSize    uint64
	root        hash.Hash
	rootMu      sync.RWMutex // Protects root field for concurrent access
	log         *logrus.Entry
}

func (rcs *RemoteChunkStore) Get(ctx context.Context, h hash.Hash) (chunks.Chunk, error) {
	rcs.log.Trace("calling Get")

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
	rcs.log.Trace("calling GetMany")
	ae := atomicerr.New()
	decompressedSize := uint64(0)
	err := rcs.GetManyCompressed(ctx, hashes, func(ctx context.Context, cc nbs.ToChunker) {
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

func (rcs *RemoteChunkStore) GetManyCompressed(ctx context.Context, hashes hash.HashSet, found func(context.Context, nbs.ToChunker)) error {
	rcs.log.Trace("calling GetManyCompressed")
	hashToChunk := rcs.cache.GetCachedChunks(hashes)

	notCached := make([]hash.Hash, 0, len(hashes))
	for h := range hashes {
		c, ok := hashToChunk[h]

		if !ok || c == nil || c.IsEmpty() {
			notCached = append(notCached, h)
		} else {
			found(ctx, c)
		}
	}

	if len(notCached) > 0 {
		err := rcs.downloadChunksAndCache(ctx, notCached, found)

		if err != nil {
			return err
		}
	}

	return nil
}

func (rcs *RemoteChunkStore) downloadChunksAndCache(ctx context.Context, notCached []hash.Hash, found func(context.Context, nbs.ToChunker)) error {
	toSend := make(map[hash.Hash]struct{}, len(notCached))
	for _, h := range notCached {
		toSend[h] = struct{}{}
	}

	hashesToDownload := make([]string, len(notCached))
	for i, h := range notCached {
		hashesToDownload[i] = h.String()
	}

	return rcs.downloader.DownloadChunks(ctx, hashesToDownload, func(hashStr string, compressed []byte) error {
		if len(compressed) == 0 {
			return nil
		}
		h := hash.Parse(hashStr)
		compressedChunk, err := nbs.NewCompressedChunk(h, compressed)
		if err != nil {
			return fmt.Errorf("failed to create compressed chunk for hash '%s': %w", hashStr, err)
		}

		rcs.cache.InsertChunks([]nbs.ToChunker{compressedChunk})

		if _, send := toSend[h]; send {
			found(ctx, compressedChunk)
		}
		return nil
	})
}

func (rcs *RemoteChunkStore) Has(ctx context.Context, h hash.Hash) (bool, error) {
	rcs.log.Trace("calling Has")
	hashes := hash.HashSet{h: struct{}{}}
	absent, err := rcs.HasMany(ctx, hashes)

	if err != nil {
		return false, err
	}

	return len(absent) == 0, nil
}

func (rcs *RemoteChunkStore) HasMany(ctx context.Context, hashes hash.HashSet) (hash.HashSet, error) {
	rcs.log.Trace("calling HasMany")

	notCached := rcs.cache.GetCachedHas(hashes)

	if len(notCached) == 0 {
		return notCached, nil
	}

	// convert the set to a slice of hashes and a corresponding slice of the byte encoding for those hashes
	hashSl, byteSl := remotestorage.HashSetToSlices(notCached)

	absent := make(hash.HashSet)
	// var found []nbs.CompressedChunk
	found := make(hash.HashSet)
	var err error

	batchItr(len(hashSl), maxHasManyBatchSize, func(st, end int) (stop bool) {
		// slice the slices into a batch of hashes
		currHashSl := hashSl[st:end]
		currByteSl := byteSl[st:end]

		// send a request to the remote api to determine which chunks the remote api already has
		var absentIndices []int32
		absentIndices, err = rcs.chunkClient.HasChunks(ctx, rcs.peerID, currByteSl)
		if err != nil {
			err = remotestorage.NewRpcError(err, "HasChunks", rcs.peerID, nil)
			return true
		}

		numAbsent := len(absentIndices)
		sort.Slice(absentIndices, func(i, j int) bool {
			return absentIndices[i] < absentIndices[j]
		})

		// loop over every hash in the current batch, and if they are absent from the remote host add them to the
		// absent set, otherwise append them to the found slice
		for i, j := 0, 0; i < len(currHashSl); i++ {
			currHash := currHashSl[i]

			nextAbsent := -1
			if j < numAbsent {
				nextAbsent = int(absentIndices[j])
			}

			if i == nextAbsent {
				absent[currHash] = struct{}{}
				j++
			} else {
				found.Insert(currHash)
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
		rcs.cache.InsertHas(found)
	}

	return absent, nil
}

func (rcs *RemoteChunkStore) Put(ctx context.Context, c chunks.Chunk, getAddrs chunks.GetAddrsCurry) error {
	rcs.log.Trace("calling Put")
	return fmt.Errorf("not supported")
}

func (rcs *RemoteChunkStore) Version() string {
	rcs.log.Trace("calling Version: ", rcs.nbfVersion)
	return rcs.nbfVersion
}

func (rcs *RemoteChunkStore) Rebase(ctx context.Context) error {
	rcs.log.Trace("calling Rebase")
	return rcs.loadRoot(ctx)
}

func (rcs *RemoteChunkStore) loadRoot(ctx context.Context) error {
	rootHash, err := rcs.chunkClient.Root(ctx, rcs.peerID)
	if err != nil {
		return remotestorage.NewRpcError(err, "Root", rcs.peerID, nil)
	}
	rcs.rootMu.Lock()
	rcs.root = hash.New(rootHash)
	rcs.rootMu.Unlock()
	return nil
}

func (rcs *RemoteChunkStore) Root(ctx context.Context) (hash.Hash, error) {
	rcs.log.Trace("calling Root")
	rcs.rootMu.RLock()
	root := rcs.root
	rcs.rootMu.RUnlock()
	return root, nil
}

func (rcs *RemoteChunkStore) Commit(ctx context.Context, current, last hash.Hash) (bool, error) {
	rcs.log.Trace("calling Commit")
	return false, fmt.Errorf("not supported")
}

func (rcs *RemoteChunkStore) Stats() interface{} {
	rcs.log.Trace("calling Stats")
	return nil
}

func (rcs *RemoteChunkStore) StatsSummary() string {
	rcs.log.Trace("calling StatsSummary")
	return "Unsupported"
}

func (rcs *RemoteChunkStore) PersistGhostHashes(ctx context.Context, refs hash.HashSet) error {
	rcs.log.Trace("calling PersistGhostHashes")
	return fmt.Errorf("not supported")
}

func (rcs *RemoteChunkStore) Close() error {
	rcs.log.Trace("calling Close")
	return nil
}

func (rcs *RemoteChunkStore) AccessMode() chunks.ExclusiveAccessMode {
	rcs.log.Trace("calling AccessMode")
	return chunks.ExclusiveAccessMode_ReadOnly
}

func (rcs *RemoteChunkStore) getRepoId() RepoID {
	return RepoID{Org: rcs.peerID, RepoName: rcs.dbName}
}

//
// TableFileStore implementation
//

func (rcs *RemoteChunkStore) Sources(ctx context.Context) (hash.Hash, []chunks.TableFile, []chunks.TableFile, error) {
	rcs.log.Trace("calling Sources")
	id := rcs.getRepoId()
	rootHash, sourceInfos, appendixInfos, err := rcs.chunkClient.ListTableFiles(ctx, id, "", "")
	if err != nil {
		return hash.Hash{}, nil, nil, fmt.Errorf("failed to list table files: %w", err)
	}
	sourceFiles := getTableFiles(rcs.downloader, rcs.chunkClient, sourceInfos)
	appendixFiles := getTableFiles(rcs.downloader, rcs.chunkClient, appendixInfos)
	return hash.New(rootHash), sourceFiles, appendixFiles, nil
}

func getTableFiles(downloader DownloaderClient, chunkClient ChunkStoreClient, infoList []TableFileInfo) []chunks.TableFile {
	tableFiles := make([]chunks.TableFile, 0)
	for _, nfo := range infoList {
		tableFiles = append(tableFiles, RemoteTableFile{downloader: downloader, chunkClient: chunkClient, info: nfo})
	}
	return tableFiles
}

func (rcs *RemoteChunkStore) Size(ctx context.Context) (uint64, error) {
	rcs.log.Trace("calling Size")
	return rcs.repoSize, nil
}

func (rcs *RemoteChunkStore) WriteTableFile(ctx context.Context, fileId string, splitOffset uint64, numChunks int, contentHash []byte, getRd func() (io.ReadCloser, uint64, error)) error {
	return fmt.Errorf("not supported")
}

func (rcs *RemoteChunkStore) AddTableFilesToManifest(ctx context.Context, fileIdToNumChunks map[string]int, getAddrs chunks.GetAddrsCurry) error {
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
