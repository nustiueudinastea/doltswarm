package doltswarm

import (
	"context"
	"fmt"
	"net/url"

	"github.com/dolthub/dolt/go/store/chunks"
	"github.com/dolthub/dolt/go/store/datas"
	"github.com/dolthub/dolt/go/store/prolly/tree"
	"github.com/dolthub/dolt/go/store/types"
	"github.com/sirupsen/logrus"
)

type ClientRetriever interface {
	GetClient(peerID string) (*DBClient, error)
}

func NewDoltSwarmFactory(dbName string, clientRetriever ClientRetriever, logger *logrus.Entry) DoltSwarmFactory {
	return DoltSwarmFactory{dbName, clientRetriever, logger}
}

type DoltSwarmFactory struct {
	dbName          string
	clientRetriever ClientRetriever
	logger          *logrus.Entry
}

func (fact DoltSwarmFactory) PrepareDB(ctx context.Context, nbf *types.NomsBinFormat, urlObj *url.URL, params map[string]interface{}) error {
	return nil
}

func (fact DoltSwarmFactory) CreateDB(ctx context.Context, nbf *types.NomsBinFormat, urlObj *url.URL, params map[string]interface{}) (datas.Database, types.ValueReadWriter, tree.NodeStore, error) {
	cs, err := fact.newChunkStore(urlObj.Host, nbf.VersionString())
	if err != nil {
		return nil, nil, nil, err
	}

	vrw := types.NewValueStore(cs)
	ns := tree.NewNodeStore(cs)
	typesDB := datas.NewTypesDatabase(vrw, ns)

	return typesDB, vrw, ns, nil
}

func (fact DoltSwarmFactory) newChunkStore(peerID string, nbfVersion string) (chunks.ChunkStore, error) {

	client, err := fact.clientRetriever.GetClient(peerID)
	if err != nil {
		return nil, fmt.Errorf("could not get client for '%s': %w", peerID, err)
	}

	cs, err := NewRemoteChunkStore(client, peerID, fact.dbName, nbfVersion, fact.logger)
	if err != nil {
		return nil, fmt.Errorf("could not create remote cs for '%s': %w", peerID, err)
	}
	return cs, err
}
