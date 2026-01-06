package grpcswarm

import (
	"context"
	"fmt"

	"github.com/dolthub/dolt/go/libraries/doltcore/remotesrv"
	"github.com/dolthub/dolt/go/store/chunks"
	"github.com/nustiueudinastea/doltswarm"
	"github.com/nustiueudinastea/doltswarm/integration/proto"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"

	remotesapi "github.com/dolthub/dolt/go/gen/proto/dolt/services/remotesapi/v1alpha1"
)

// SwarmDB is the transport-facing surface required to expose DoltSwarm services over gRPC.
// Implemented by *doltswarm.DB and can be implemented by custom DB wrappers.
type SwarmDB interface {
	GetChunkStore() (chunks.ChunkStore, error)
	GetFilePath() string

	HandleAdvertiseCommit(ctx context.Context, fromPeerID string, ad *doltswarm.CommitAd) (accepted bool, err error)
	HandleRequestCommitsSince(ctx context.Context, since doltswarm.HLCTimestamp) ([]*doltswarm.CommitAd, error)
}

// BundleProvider is an optional interface a DB can implement to expose commit bundles over gRPC.
// The core library is transport-agnostic; integration uses gRPC as one possible transport.
type BundleProvider interface {
	BuildBundleSince(ctx context.Context, base doltswarm.Checkpoint, req doltswarm.BundleRequest) (doltswarm.CommitBundle, error)
}

type RegisterOptions struct {
	// GossipSink is optional; when set, incoming AdvertiseCommit calls will also be emitted as
	// gossip events (CommitAdV1) into the sink.
	GossipSink GossipSink
}

func Register(server *grpc.Server, db SwarmDB, logger *logrus.Entry) error {
	return RegisterWithOptions(server, db, logger, RegisterOptions{})
}

func RegisterWithOptions(server *grpc.Server, db SwarmDB, logger *logrus.Entry, opts RegisterOptions) error {
	csAny, err := db.GetChunkStore()
	if err != nil {
		return fmt.Errorf("get chunk store: %w", err)
	}
	cs, ok := csAny.(remotesrv.RemoteSrvStore)
	if !ok {
		return fmt.Errorf("chunk store does not implement remotesrv.RemoteSrvStore (got %T)", csAny)
	}

	chunkStoreCache := doltswarm.NewCSCache(cs)
	chunkStoreServer := NewServerChunkStore(logger, chunkStoreCache, db.GetFilePath())
	proto.RegisterDownloaderServer(server, chunkStoreServer)
	remotesapi.RegisterChunkStoreServiceServer(server, chunkStoreServer)

	syncerServer := NewServerSyncer(logger, db, opts.GossipSink)
	proto.RegisterDBSyncerServer(server, syncerServer)

	// BundleExchange is optional for now; it is registered when the DB supports bundle building.
	if bundleDB, ok := any(db).(BundleProvider); ok {
		proto.RegisterBundleExchangeServer(server, NewBundleExchangeServer(logger, bundleDB))
	}

	return nil
}
