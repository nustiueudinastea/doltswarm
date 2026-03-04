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
type CommitMetadataForSigning = protocol.CommitMetadataForSigning

const CommitKindUser = protocol.CommitKindUser

type CommitAdV1 = protocol.CommitAdV1
type HeartbeatV1 = protocol.HeartbeatV1

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

type UsedProviderTracker = transport.UsedProviderTracker

func WithUsedProviderTracker(ctx context.Context, tracker *UsedProviderTracker) context.Context {
	return transport.WithUsedProviderTracker(ctx, tracker)
}

func UsedProviderTrackerFromContext(ctx context.Context) *UsedProviderTracker {
	return transport.UsedProviderTrackerFromContext(ctx)
}

// --- Protocol helpers ---

func ParseCommitMetadata(commitMessage string) (*CommitMetadata, error) {
	return protocol.ParseCommitMetadata(commitMessage)
}

func IsMetadataCommit(commitMessage string) bool {
	return protocol.IsMetadataCommit(commitMessage)
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
