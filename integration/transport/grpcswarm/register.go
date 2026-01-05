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

func Register(server *grpc.Server, db SwarmDB, logger *logrus.Entry) error {
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

	syncerServer := NewServerSyncer(logger, db)
	proto.RegisterDBSyncerServer(server, syncerServer)

	return nil
}
