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

func NewCustomFactory(dbName string, db *DB, logger *logrus.Entry) CustomFactory {
	return CustomFactory{dbName, db, logger}
}

type CustomFactory struct {
	dbName string
	db     *DB
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

	client, err := fact.db.GetClient(peerID)
	if err != nil {
		return nil, fmt.Errorf("could not get client for '%s': %w", peerID, err)
	}

	cs, err := NewRemoteChunkStore(client, peerID, fact.dbName, nbfVersion, fact.logger)
	if err != nil {
		return nil, fmt.Errorf("could not create remote cs for '%s': %w", peerID, err)
	}
	return cs, err
}
