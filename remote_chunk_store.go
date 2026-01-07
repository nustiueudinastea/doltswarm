package doltswarm

import (
	"context"
	"fmt"
	"io"
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

const maxHasManyBatchSize = 16 * 1024

var globalRemoteChunkCache = newMapChunkCache()

// NewRemoteChunkStore creates a read-only ChunkStore backed by a single selected provider.
//
// This is the Option B data-plane: let Dolt's fetch logic pull missing objects using HasMany+GetMany
// patterns instead of shipping explicit commit bundles.
func NewRemoteChunkStore(provider Provider, repo RepoID, nbfVersion string) (*RemoteChunkStore, error) {
	if provider == nil {
		return nil, fmt.Errorf("provider is nil")
	}
	if repo.RepoName == "" {
		return nil, fmt.Errorf("repo name is empty")
	}

	rcs := &RemoteChunkStore{
		repo:        repo,
		repoPath:    repoPath(repo),
		chunkClient: provider.ChunkStore(),
		downloader:  provider.Downloader(),
		cache:       globalRemoteChunkCache,
		nbfVersion:  nbfVersion,
		log: logrus.NewEntry(logrus.StandardLogger()).WithFields(logrus.Fields{
			"component": "remote_chunk_store",
			"provider":  provider.ID(),
			"repo":      repo.RepoName,
		}),
	}

	meta, err := rcs.chunkClient.GetRepoMetadata(
		context.Background(),
		rcs.repo,
		rcs.repoPath,
		ClientRepoFormat{NbfVersion: nbfVersion, NbsVersion: nbs.StorageVersion},
	)
	if err != nil {
		return nil, err
	}
	rcs.repoSize = meta.StorageSize

	if err := rcs.loadRoot(context.Background()); err != nil {
		return nil, err
	}
	return rcs, nil
}

type RemoteChunkStore struct {
	repo     RepoID
	repoPath string

	chunkClient ChunkStoreClient
	downloader  DownloaderClient
	cache       remotestorage.ChunkCache

	nbfVersion string
	repoSize   uint64

	root   hash.Hash
	rootMu sync.RWMutex

	log *logrus.Entry
}

func repoPath(r RepoID) string {
	if r.Org != "" {
		return r.Org + "/" + r.RepoName
	}
	return r.RepoName
}

func (rcs *RemoteChunkStore) Get(ctx context.Context, h hash.Hash) (chunks.Chunk, error) {
	hashes := hash.HashSet{h: struct{}{}}
	var found *chunks.Chunk
	if err := rcs.GetMany(ctx, hashes, func(_ context.Context, c *chunks.Chunk) { found = c }); err != nil {
		return chunks.EmptyChunk, err
	}
	if found != nil {
		return *found, nil
	}
	return chunks.EmptyChunk, nil
}

func (rcs *RemoteChunkStore) GetMany(ctx context.Context, hashes hash.HashSet, found func(context.Context, *chunks.Chunk)) error {
	ae := atomicerr.New()
	var decompressedSize uint64
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
	return ae.Get()
}

func (rcs *RemoteChunkStore) GetManyCompressed(ctx context.Context, hashes hash.HashSet, found func(context.Context, nbs.ToChunker)) error {
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

	if len(notCached) == 0 {
		return nil
	}
	return rcs.downloadChunksAndCache(ctx, notCached, found)
}

func (rcs *RemoteChunkStore) downloadChunksAndCache(ctx context.Context, notCached []hash.Hash, found func(context.Context, nbs.ToChunker)) error {
	toSend := make(map[hash.Hash]struct{}, len(notCached))
	hashesToDownload := make([]string, len(notCached))
	for i, h := range notCached {
		toSend[h] = struct{}{}
		hashesToDownload[i] = h.String()
	}

	return rcs.downloader.DownloadChunks(ctx, hashesToDownload, func(hashStr string, compressed []byte) error {
		if len(compressed) == 0 {
			return nil
		}
		h := hash.Parse(hashStr)
		cc, err := nbs.NewCompressedChunk(h, compressed)
		if err != nil {
			return fmt.Errorf("create compressed chunk %s: %w", hashStr, err)
		}

		rcs.cache.InsertChunks([]nbs.ToChunker{cc})

		if _, send := toSend[h]; send {
			found(ctx, cc)
		}
		return nil
	})
}

func (rcs *RemoteChunkStore) Has(ctx context.Context, h hash.Hash) (bool, error) {
	hashes := hash.HashSet{h: struct{}{}}
	absent, err := rcs.HasMany(ctx, hashes)
	if err != nil {
		return false, err
	}
	return len(absent) == 0, nil
}

func (rcs *RemoteChunkStore) HasMany(ctx context.Context, hashes hash.HashSet) (hash.HashSet, error) {
	notCached := rcs.cache.GetCachedHas(hashes)
	if len(notCached) == 0 {
		return notCached, nil
	}

	hashSl, byteSl := remotestorage.HashSetToSlices(notCached)

	absent := make(hash.HashSet)
	found := make(hash.HashSet)

	var err error
	batchItr(len(hashSl), maxHasManyBatchSize, func(st, end int) (stop bool) {
		currHashSl := hashSl[st:end]
		currByteSl := byteSl[st:end]

		var absentIndices []int32
		absentIndices, err = rcs.chunkClient.HasChunks(ctx, rcs.repoPath, currByteSl)
		if err != nil {
			return true
		}

		sort.Slice(absentIndices, func(i, j int) bool { return absentIndices[i] < absentIndices[j] })
		numAbsent := len(absentIndices)

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

	if len(found) > 0 {
		rcs.cache.InsertHas(found)
	}
	return absent, nil
}

func (rcs *RemoteChunkStore) Put(ctx context.Context, c chunks.Chunk, getAddrs chunks.GetAddrsCurry) error {
	return fmt.Errorf("remote chunk store is read-only")
}

func (rcs *RemoteChunkStore) Version() string { return rcs.nbfVersion }

func (rcs *RemoteChunkStore) AccessMode() chunks.ExclusiveAccessMode {
	return chunks.ExclusiveAccessMode_ReadOnly
}

func (rcs *RemoteChunkStore) Rebase(ctx context.Context) error {
	return rcs.loadRoot(ctx)
}

func (rcs *RemoteChunkStore) loadRoot(ctx context.Context) error {
	rootHash, err := rcs.chunkClient.Root(ctx, rcs.repoPath)
	if err != nil {
		return err
	}
	rcs.rootMu.Lock()
	rcs.root = hash.New(rootHash)
	rcs.rootMu.Unlock()
	return nil
}

func (rcs *RemoteChunkStore) Root(ctx context.Context) (hash.Hash, error) {
	rcs.rootMu.RLock()
	root := rcs.root
	rcs.rootMu.RUnlock()
	return root, nil
}

func (rcs *RemoteChunkStore) Commit(ctx context.Context, current, last hash.Hash) (bool, error) {
	return false, fmt.Errorf("remote chunk store is read-only")
}

func (rcs *RemoteChunkStore) Stats() interface{}   { return nil }
func (rcs *RemoteChunkStore) StatsSummary() string { return "Unsupported" }
func (rcs *RemoteChunkStore) Close() error         { return nil }
func (rcs *RemoteChunkStore) PersistGhostHashes(ctx context.Context, refs hash.HashSet) error {
	return fmt.Errorf("not supported")
}

// ---- TableFileStore ----

func (rcs *RemoteChunkStore) Sources(ctx context.Context) (hash.Hash, []chunks.TableFile, []chunks.TableFile, error) {
	rootHash, tables, appendix, err := rcs.chunkClient.ListTableFiles(ctx, rcs.repo, rcs.repoPath, "")
	if err != nil {
		return hash.Hash{}, nil, nil, err
	}

	sourceFiles := getTableFiles(rcs.downloader, rcs.chunkClient, tables)
	appendixFiles := getTableFiles(rcs.downloader, rcs.chunkClient, appendix)
	return hash.New(rootHash), sourceFiles, appendixFiles, nil
}

func getTableFiles(downloader DownloaderClient, chunkClient ChunkStoreClient, infoList []TableFileInfo) []chunks.TableFile {
	out := make([]chunks.TableFile, 0, len(infoList))
	for _, nfo := range infoList {
		out = append(out, RemoteTableFile{downloader: downloader, chunkClient: chunkClient, info: nfo})
	}
	return out
}

func (rcs *RemoteChunkStore) Size(ctx context.Context) (uint64, error) {
	return rcs.repoSize, nil
}

func (rcs *RemoteChunkStore) WriteTableFile(ctx context.Context, fileId string, splitOffset uint64, numChunks int, contentHash []byte, getRd func() (io.ReadCloser, uint64, error)) error {
	return fmt.Errorf("remote chunk store is read-only")
}

func (rcs *RemoteChunkStore) AddTableFilesToManifest(ctx context.Context, fileIdToNumChunks map[string]int, getAddrs chunks.GetAddrsCurry) error {
	return fmt.Errorf("remote chunk store is read-only")
}

func (rcs *RemoteChunkStore) PruneTableFiles(ctx context.Context) error {
	return fmt.Errorf("remote chunk store is read-only")
}

func (rcs *RemoteChunkStore) SupportedOperations() chunks.TableFileStoreOps {
	return chunks.TableFileStoreOps{CanRead: true, CanWrite: false, CanPrune: false, CanGC: false}
}

func batchItr(total, batchSize int, cb func(st, end int) (stop bool)) {
	if batchSize <= 0 {
		batchSize = total
	}
	for st := 0; st < total; st += batchSize {
		end := st + batchSize
		if end > total {
			end = total
		}
		if cb(st, end) {
			return
		}
	}
}
