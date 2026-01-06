package doltswarm

import (
	"context"
	"fmt"
	"sort"

	"github.com/dolthub/dolt/go/store/chunks"
	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/types"
)

// BuildBundleSince builds a commit bundle containing commits strictly after base.HLC.
//
// This is a first, correctness-oriented implementation intended to enable end-to-end wiring.
// It is not optimized and may over-include chunks; callers should set BundleRequest limits.
func (db *DB) BuildBundleSince(ctx context.Context, base Checkpoint, req BundleRequest) (CommitBundle, error) {
	if req.MaxCommits <= 0 {
		req.MaxCommits = 256
	}
	if req.MaxBytes <= 0 {
		req.MaxBytes = 32 << 20
	}

	all, err := db.GetAllCommits()
	if err != nil {
		return CommitBundle{}, err
	}

	type commitSel struct {
		hlc    HLCTimestamp
		hash   string
		msgRaw []byte
		sigRaw []byte
	}

	// Collect checkpoints and (optionally) validate the base checkpoint.
	checkpoints := make([]Checkpoint, 0)
	var baseMatch bool

	selected := make([]commitSel, 0, req.MaxCommits)
	for _, c := range all {
		meta, err := ParseCommitMetadata(c.Message)
		if err != nil || meta == nil {
			continue
		}
		checkpoints = append(checkpoints, Checkpoint{HLC: meta.HLC, CommitHash: c.Hash})
		if base.CommitHash != "" && base.HLC.Equal(meta.HLC) && c.Hash == base.CommitHash {
			baseMatch = true
		}
		if base.HLC.IsZero() || base.HLC.Less(meta.HLC) {
			selected = append(selected, commitSel{
				hlc:    meta.HLC,
				hash:   c.Hash,
				msgRaw: []byte(c.Message),
				sigRaw: []byte(meta.Signature),
			})
		}
	}

	sort.Slice(selected, func(i, j int) bool { return selected[i].hlc.Less(selected[j].hlc) })
	if len(selected) > req.MaxCommits {
		selected = selected[:req.MaxCommits]
	}

	head, err := db.GetLastCommit("main")
	if err != nil {
		return CommitBundle{}, err
	}
	var headHLC HLCTimestamp
	if meta, err := ParseCommitMetadata(head.Message); err == nil && meta != nil {
		headHLC = meta.HLC
	}

	out := CommitBundle{
		Header: BundleHeader{
			Base:             base,
			ProviderHeadHLC:  headHLC,
			ProviderHeadHash: head.Hash,
		},
	}

	// Provide negotiation checkpoints (newest -> oldest).
	sort.Slice(checkpoints, func(i, j int) bool { return checkpoints[i].HLC.Less(checkpoints[j].HLC) })
	const checkpointLimit = 64
	if len(checkpoints) > checkpointLimit {
		checkpoints = checkpoints[len(checkpoints)-checkpointLimit:]
	}
	for i := len(checkpoints) - 1; i >= 0; i-- {
		out.Header.ProviderCheckpoints = append(out.Header.ProviderCheckpoints, checkpoints[i])
	}

	if base.CommitHash != "" && !baseMatch {
		out.Header.BaseMismatch = true
		// Base is not compatible; return only header+checkpoints for negotiation.
		return out, nil
	}

	for _, s := range selected {
		out.Commits = append(out.Commits, BundledCommit{
			HLC:          s.hlc,
			CommitHash:   s.hash,
			MetadataJSON: s.msgRaw,
			MetadataSig:  s.sigRaw,
		})
	}

	csAny, err := db.GetChunkStore()
	if err != nil {
		return CommitBundle{}, err
	}
	cs, ok := csAny.(chunks.ChunkStore)
	if !ok {
		// db.GetChunkStore() returns chunks.ChunkStore today, but be defensive.
		return CommitBundle{}, fmt.Errorf("GetChunkStore returned %T (expected chunks.ChunkStore)", csAny)
	}

	seed := make([]hash.Hash, 0, len(out.Commits)+1)
	if base.CommitHash != "" {
		if h, ok := hash.MaybeParse(base.CommitHash); ok {
			seed = append(seed, h)
		}
	}
	for _, c := range out.Commits {
		if h, ok := hash.MaybeParse(c.CommitHash); ok {
			seed = append(seed, h)
		}
	}

	chunksOut, err := walkChunkClosure(ctx, cs, seed, req.MaxBytes, req.AllowPartial)
	if err != nil {
		return CommitBundle{}, err
	}
	out.Chunks = chunksOut

	return out, nil
}

func walkChunkClosure(ctx context.Context, cs chunks.ChunkStore, seeds []hash.Hash, maxBytes int64, allowPartial bool) ([]BundledChunk, error) {
	visited := make(map[hash.Hash]struct{})
	queue := make([]hash.Hash, 0, len(seeds))
	queue = append(queue, seeds...)

	var total int64
	out := make([]BundledChunk, 0)

	for len(queue) > 0 {
		h := queue[0]
		queue = queue[1:]
		if _, ok := visited[h]; ok {
			continue
		}
		visited[h] = struct{}{}

		ch, err := cs.Get(ctx, h)
		if err != nil {
			return nil, err
		}
		if ch.IsEmpty() {
			continue
		}

		data := ch.Data()
		if maxBytes > 0 && total+int64(len(data)) > maxBytes {
			if allowPartial {
				break
			}
			return nil, fmt.Errorf("bundle exceeds max_bytes (%d)", maxBytes)
		}
		total += int64(len(data))

		out = append(out, BundledChunk{
			Hash:  h[:],
			Data:  data,
			Codec: ChunkCodecRaw,
		})

		_ = types.SerialMessage(data).WalkAddrs(types.Format_Default, func(addr hash.Hash) error {
			if _, ok := visited[addr]; ok {
				return nil
			}
			queue = append(queue, addr)
			return nil
		})
	}

	return out, nil
}

// ImportBundle imports bundle chunks into the local chunk store without changing the working root.
//
// This is an initial implementation to support end-to-end testing of bundle transport. It does
// not yet update any local HLC index; higher layers should do that.
func (db *DB) ImportBundle(ctx context.Context, bundle CommitBundle) error {
	cs, err := db.GetChunkStore()
	if err != nil {
		return err
	}

	root, err := cs.Root(ctx)
	if err != nil {
		return err
	}

	noopGetAddrs := func(_ chunks.Chunk) chunks.GetAddrsCb {
		return func(context.Context, hash.HashSet, chunks.PendingRefExists) error { return nil }
	}

	for _, bc := range bundle.Chunks {
		if len(bc.Hash) != hash.ByteLen {
			return fmt.Errorf("invalid chunk hash length: %d", len(bc.Hash))
		}

		h := hash.New(bc.Hash)
		data := bc.Data

		// For now, codec handling is minimal; v1 uses raw chunk bytes.
		if bc.Codec != ChunkCodecRaw {
			return fmt.Errorf("unsupported chunk codec: %d", bc.Codec)
		}

		// Verify integrity.
		if hash.Of(data) != h {
			return fmt.Errorf("chunk hash mismatch for %s", h.String())
		}

		if err := cs.Put(ctx, chunks.NewChunkWithHash(h, data), noopGetAddrs); err != nil {
			return err
		}
	}

	// Persist new chunks without changing root.
	_, err = cs.Commit(ctx, root, root)
	return err
}
