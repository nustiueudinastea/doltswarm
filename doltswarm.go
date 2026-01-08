package doltswarm

import (
	"context"

	"github.com/nustiueudinastea/doltswarm/core"
	"github.com/nustiueudinastea/doltswarm/protocol"
	"github.com/nustiueudinastea/doltswarm/transport"
	"github.com/sirupsen/logrus"
)

// --- Protocol types ---

type RepoID = protocol.RepoID
type HLCTimestamp = protocol.HLCTimestamp
type HLC = protocol.HLC

const MaxClockSkew = protocol.MaxClockSkew

type CommitMetadata = protocol.CommitMetadata

type CommitAdV1 = protocol.CommitAdV1
type Checkpoint = protocol.Checkpoint
type DigestV1 = protocol.DigestV1

type BundleRequest = protocol.BundleRequest
type BundleHeader = protocol.BundleHeader
type BundledCommit = protocol.BundledCommit
type BundledChunk = protocol.BundledChunk
type ChunkCodec = protocol.ChunkCodec

const (
	ChunkCodecRaw           = protocol.ChunkCodecRaw
	ChunkCodecNBSCompressed = protocol.ChunkCodecNBSCompressed
)

type CommitBundle = protocol.CommitBundle

type Signer = protocol.Signer

// --- Transport boundary types ---

type Transport = transport.Transport
type Gossip = transport.Gossip
type GossipSubscription = transport.GossipSubscription
type GossipEvent = transport.GossipEvent

type ProviderPicker = transport.ProviderPicker
type Provider = transport.Provider
type ChunkStoreClient = transport.ChunkStoreClient
type DownloaderClient = transport.DownloaderClient

type ClientRepoFormat = transport.ClientRepoFormat
type RepoMetadata = transport.RepoMetadata
type TableFileInfo = transport.TableFileInfo

var ErrUnimplemented = transport.ErrUnimplemented

func WithPreferredProvider(ctx context.Context, providerID string) context.Context {
	return transport.WithPreferredProvider(ctx, providerID)
}

func PreferredProviderFromContext(ctx context.Context) (string, bool) {
	return transport.PreferredProviderFromContext(ctx)
}

// --- Core engine types ---

type DB = core.DB
type Node = core.Node
type NodeConfig = core.NodeConfig

type Commit = core.Commit
type ExecFunc = core.ExecFunc

type RejectionReason = core.RejectionReason

const (
	NotRejected              = core.NotRejected
	RejectedConflict         = core.RejectedConflict
	RejectedInvalidSignature = core.RejectedInvalidSignature
	RejectedFetchFailed      = core.RejectedFetchFailed
	RejectedClockSkew        = core.RejectedClockSkew
)

type CommitAd = core.CommitAd
type CommitRejectedCallback = core.CommitRejectedCallback

const SwarmDBFactoryScheme = core.SwarmDBFactoryScheme

func RegisterSwarmProviders(repo RepoID, picker ProviderPicker) {
	core.RegisterSwarmProviders(repo, picker)
}

func UnregisterSwarmProviders(repo RepoID) {
	core.UnregisterSwarmProviders(repo)
}

func Open(dir string, name string, logger *logrus.Entry, signer Signer) (*DB, error) {
	return core.Open(dir, name, logger, signer)
}

func OpenNode(cfg NodeConfig) (*Node, error) {
	return core.OpenNode(cfg)
}
