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

const CommitKindUser = protocol.CommitKindUser

type CommitAdV1 = protocol.CommitAdV1
type HeartbeatV1 = protocol.HeartbeatV1

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

type UsedProviderTracker = transport.UsedProviderTracker

func WithUsedProviderTracker(ctx context.Context, tracker *UsedProviderTracker) context.Context {
	return transport.WithUsedProviderTracker(ctx, tracker)
}

func UsedProviderTrackerFromContext(ctx context.Context) *UsedProviderTracker {
	return transport.UsedProviderTrackerFromContext(ctx)
}
