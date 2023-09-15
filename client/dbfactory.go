package client

import (
	"context"
	"fmt"
	"net/url"

	remotesapi "github.com/dolthub/dolt/go/gen/proto/dolt/services/remotesapi/v1alpha1"
	"github.com/dolthub/dolt/go/store/chunks"
	"github.com/dolthub/dolt/go/store/datas"
	"github.com/dolthub/dolt/go/store/prolly/tree"
	"github.com/dolthub/dolt/go/store/types"
	"github.com/nustiueudinastea/doltswarm/proto"
	"github.com/sirupsen/logrus"
)

type Client interface {
	proto.DBSyncerClient
	remotesapi.ChunkStoreServiceClient
	proto.DownloaderClient

	GetID() string
}

type ClientRetriever interface {
	GetClient(peerID string) (Client, error)
	GetClients() []Client
}

func NewCustomFactory(dbName string, cr ClientRetriever, logger *logrus.Entry) CustomFactory {
	return CustomFactory{dbName, cr, logger}
}

type CustomFactory struct {
	dbName string
	cr     ClientRetriever
	logger *logrus.Entry
}

func (fact CustomFactory) PrepareDB(ctx context.Context, nbf *types.NomsBinFormat, urlObj *url.URL, params map[string]interface{}) error {
	return nil
}

func (fact CustomFactory) CreateDB(ctx context.Context, nbf *types.NomsBinFormat, urlObj *url.URL, params map[string]interface{}) (datas.Database, types.ValueReadWriter, tree.NodeStore, error) {
	var db datas.Database

	cs, err := fact.newChunkStore(urlObj.Host, nbf.VersionString())
	if err != nil {
		return nil, nil, nil, err
	}

	vrw := types.NewValueStore(cs)
	ns := tree.NewNodeStore(cs)
	db = datas.NewTypesDatabase(vrw, ns)

	return db, vrw, ns, nil
}

func (fact CustomFactory) newChunkStore(peerID string, nbfVersion string) (chunks.ChunkStore, error) {

	client, err := fact.cr.GetClient(peerID)
	if err != nil {
		return nil, fmt.Errorf("could not get client for '%s': %w", peerID, err)
	}

	cs, err := NewRemoteChunkStore(client, peerID, fact.dbName, nbfVersion, fact.logger)
	if err != nil {
		return nil, fmt.Errorf("could not create remote cs for '%s': %w", peerID, err)
	}
	return cs, err
}
