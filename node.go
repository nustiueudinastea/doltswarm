package doltswarm

import (
	"context"
	"fmt"
	"time"

	"github.com/sirupsen/logrus"
)

type IdentityResolver interface {
	PublicKeyForPeerID(ctx context.Context, peerID string) ([]byte, error)
}

type NodeConfig struct {
	Dir       string
	Repo      RepoID
	Signer    Signer
	Transport Transport
	Log       *logrus.Entry

	MaxClockSkew time.Duration
	RepairInterval time.Duration

	// Bundle request caps (best-effort).
	BundleMaxCommits int
	BundleMaxBytes   int64

	Identity IdentityResolver
}

type Node struct {
	cfg NodeConfig
	db  *DB
	idx CommitIndex
}

func OpenNode(cfg NodeConfig) (*Node, error) {
	if cfg.Dir == "" {
		return nil, fmt.Errorf("Dir is required")
	}
	if cfg.Repo.RepoName == "" {
		return nil, fmt.Errorf("Repo is required")
	}
	if cfg.Signer == nil {
		return nil, fmt.Errorf("Signer is required")
	}
	if cfg.Transport == nil {
		return nil, fmt.Errorf("Transport is required")
	}
	if cfg.Log == nil {
		cfg.Log = logrus.NewEntry(logrus.StandardLogger())
	}
	if cfg.MaxClockSkew == 0 {
		cfg.MaxClockSkew = MaxClockSkew
	}
	if cfg.RepairInterval == 0 {
		cfg.RepairInterval = 30 * time.Second
	}
	if cfg.BundleMaxCommits == 0 {
		cfg.BundleMaxCommits = 256
	}
	if cfg.BundleMaxBytes == 0 {
		cfg.BundleMaxBytes = 32 << 20
	}

	db, err := Open(cfg.Dir, cfg.Repo.RepoName, cfg.Log, cfg.Signer)
	if err != nil {
		return nil, err
	}

	return &Node{cfg: cfg, db: db, idx: NewMemoryCommitIndex()}, nil
}

func (n *Node) Close() error {
	if n.db == nil {
		return nil
	}
	if n.idx != nil {
		_ = n.idx.Close()
	}
	return n.db.Close()
}

func (n *Node) Commit(msg string) (string, error) {
	return n.db.Commit(msg)
}

func (n *Node) ExecAndCommit(exec ExecFunc, msg string) (string, error) {
	return n.db.ExecAndCommit(exec, msg)
}

// Run starts the gossip-first synchronization loops.
//
// Initial implementation is a scaffold: it wires lifecycle and config and will be
// extended to publish/ingest commit ads, exchange bundles, and apply via reconciler.
func (n *Node) Run(ctx context.Context) error {
	// TODO(PLAN2): implement:
	// - subscribe to commit ads/digests via n.cfg.Transport.Gossip()
	// - maintain local HLC index and request bundles via n.cfg.Transport.Exchange()
	// - drive reconciler purely from imported bundles
	<-ctx.Done()
	return ctx.Err()
}
