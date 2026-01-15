package core

import (
	"context"

	"github.com/nustiueudinastea/doltswarm/protocol"
	"github.com/nustiueudinastea/doltswarm/transport"
)

// Core re-exports of shared protocol types.
type RepoID = protocol.RepoID
type HLCTimestamp = protocol.HLCTimestamp
type HLC = protocol.HLC

const MaxClockSkew = protocol.MaxClockSkew

type CommitMetadata = protocol.CommitMetadata
type CommitMetadataForSigning = protocol.CommitMetadataForSigning
type OriginEnvelope = protocol.OriginEnvelope

// Re-export commit kind constants
const (
	CommitKindNormal       = protocol.CommitKindNormal
	CommitKindResubmission = protocol.CommitKindResubmission
)

// Re-export metadata helper functions
var ComputeOriginEventID = protocol.ComputeOriginEventID
var NewResubmissionMetadata = protocol.NewResubmissionMetadata

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

// Core re-exports of transport boundary types.
type Transport = transport.Transport
type Gossip = transport.Gossip
type GossipSubscription = transport.GossipSubscription
type GossipEvent = transport.GossipEvent

type ProviderPicker = transport.ProviderPicker
type LastUsedProviderGetter = transport.LastUsedProviderGetter
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

func WithExcludedProviders(ctx context.Context, providerIDs []string) context.Context {
	return transport.WithExcludedProviders(ctx, providerIDs)
}

func ExcludedProvidersFromContext(ctx context.Context) []string {
	return transport.ExcludedProvidersFromContext(ctx)
}

type UsedProviderTracker = transport.UsedProviderTracker

func WithUsedProviderTracker(ctx context.Context, tracker *UsedProviderTracker) context.Context {
	return transport.WithUsedProviderTracker(ctx, tracker)
}

func UsedProviderTrackerFromContext(ctx context.Context) *UsedProviderTracker {
	return transport.UsedProviderTrackerFromContext(ctx)
}
