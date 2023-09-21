package doltswarm

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/bokwoon95/sq"
	"github.com/dolthub/dolt/go/cmd/dolt/commands/engine"
	remotesapi "github.com/dolthub/dolt/go/gen/proto/dolt/services/remotesapi/v1alpha1"
	"github.com/dolthub/dolt/go/libraries/doltcore/dbfactory"
	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/env"
	"github.com/dolthub/dolt/go/libraries/doltcore/remotesrv"
	"github.com/dolthub/dolt/go/libraries/doltcore/sqle/binlogreplication"
	"github.com/dolthub/dolt/go/libraries/utils/filesys"
	"github.com/dolthub/dolt/go/store/chunks"
	"github.com/dolthub/dolt/go/store/datas"
	dd "github.com/dolthub/driver"
	doltSQL "github.com/dolthub/go-mysql-server/sql"
	"github.com/nustiueudinastea/doltswarm/proto"
	"github.com/sirupsen/logrus"
	"go.uber.org/multierr"
	"google.golang.org/grpc"
)

const (
	FactorySwarm = "swarm"
)

type Commit struct {
	Hash      string
	Committer string
	Email     string
	Date      time.Time
	Message   string
}

var tableName = "testtable"

type DB struct {
	name       string
	stoppers   []func() error
	dbEnvInit  *env.DoltEnv
	mrEnv      *env.MultiRepoEnv
	sqle       *engine.SqlEngine
	sqld       *sql.DB
	sqlCtx     *doltSQL.Context
	eventQueue chan Event
	workingDir string
	grpcServer *grpc.Server
	log        *logrus.Logger
	dbClients  map[string]*DBClient
}

func New(dir string, name string, logger *logrus.Logger) (*DB, error) {
	workingDir, err := filesys.LocalFS.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to get absolute path for %s: %v", workingDir, err)
	}

	db := &DB{
		name:       name,
		workingDir: workingDir,
		log:        logger,
		eventQueue: make(chan Event, 300),
		dbClients:  map[string]*DBClient{},
	}

	return db, nil
}

func (db *DB) Open() error {

	workingDirFS, err := filesys.LocalFS.WithWorkingDir(db.workingDir)
	if err != nil {
		return fmt.Errorf("failed to open db: %w", err)
	}

	err = ensureDir(db.workingDir)
	if err != nil {
		return fmt.Errorf("failed to open db: %w", err)
	}

	ctx := context.Background()
	dbEnv := env.Load(ctx, env.GetCurrentUserHomeDir, workingDirFS, "file://"+db.workingDir+"/"+db.name, "1.6.1")
	db.dbEnvInit = dbEnv
	err = dbEnv.Config.WriteableConfig().SetStrings(map[string]string{
		env.UserEmailKey: "alex@giurgiu.io",
		env.UserNameKey:  "Alex Giurgiu",
	})
	if err != nil {
		return fmt.Errorf("failed to set config : %w", err)
	}

	db.mrEnv, err = env.MultiEnvForDirectory(ctx, dbEnv.Config.WriteableConfig(), workingDirFS, dbEnv.Version, dbEnv.IgnoreLockFile, dbEnv)
	if err != nil {
		return fmt.Errorf("failed to load database names: %v", err)
	}

	sqleConfig := &engine.SqlEngineConfig{
		IsReadOnly:              false,
		PrivFilePath:            ".doltcfg/privileges.db",
		BranchCtrlFilePath:      ".doltcfg/branch_control.db",
		DoltCfgDirPath:          ".doltcfg",
		ServerUser:              "root",
		ServerPass:              "",
		ServerHost:              "localhost",
		Autocommit:              false,
		DoltTransactionCommit:   false,
		JwksConfig:              []engine.JwksConfig{},
		ClusterController:       nil,
		BinlogReplicaController: binlogreplication.DoltBinlogReplicaController,
	}

	db.sqle, err = engine.NewSqlEngine(ctx, db.mrEnv, sqleConfig)
	if err != nil {
		return fmt.Errorf("failed to create sql engine: %w", err)
	}

	db.sqlCtx, err = db.sqle.NewLocalContext(ctx)
	if err != nil {
		return err
	}

	db.sqld = sql.OpenDB(&Connector{driver: &doltDriver{conn: &dd.DoltConn{DataSource: &dd.DoltDataSource{}, SE: db.sqle, GmsCtx: db.sqlCtx}}})

	env := db.mrEnv.GetEnv(db.name)
	if env != nil {
		err = db.p2pSetup(true)
		if err != nil {
			return fmt.Errorf("failed to do p2p setup: %w", err)
		}

		_, err = db.sqld.Exec(fmt.Sprintf("USE %s;", db.name))
		if err != nil {
			return fmt.Errorf("failed to use db: %w", err)
		}
	} else {
		err = db.p2pSetup(false)
		if err != nil {
			return fmt.Errorf("failed to do p2p setup: %w", err)
		}
	}

	return nil
}

func (db *DB) p2pSetup(withGRPCservers bool) error {
	db.log.Infof("Doing p2p setup (grpc: %v)", withGRPCservers)
	// register new factory
	dbfactory.RegisterFactory(FactorySwarm, NewCustomFactory(db.name, db, logrus.NewEntry(db.log)))

	if withGRPCservers {
		// prepare dolt chunk store server
		cs, err := db.GetChunkStore()
		if err != nil {
			return fmt.Errorf("error getting chunk store: %s", err.Error())
		}
		chunkStoreCache := NewCSCache(cs.(remotesrv.RemoteSrvStore))
		chunkStoreServer := NewServerChunkStore(logrus.NewEntry(db.log), chunkStoreCache, db.GetFilePath())
		syncerServer := NewServerSyncer(logrus.NewEntry(db.log), db)

		// register grpc servers
		if db.grpcServer == nil {
			return fmt.Errorf("grpc server not initialized")
		}
		proto.RegisterDownloaderServer(db.grpcServer, chunkStoreServer)
		remotesapi.RegisterChunkStoreServiceServer(db.grpcServer, chunkStoreServer)
		proto.RegisterDBSyncerServer(db.grpcServer, syncerServer)

		// start event processor
		db.stoppers = append(db.stoppers, db.remoteEventProcessor())
	}

	return nil
}

func (db *DB) Close() error {
	if db.mrEnv != nil {
		dbEnv := db.mrEnv.GetEnv(db.name)
		if dbEnv != nil {
			remotes, err := dbEnv.GetRemotes()
			if err == nil {
				for r := range remotes {
					err = dbEnv.RemoveRemote(context.TODO(), r)
					if err != nil {
						return err
					}
				}
			}
		}
	}

	err := db.sqle.Close()
	if err != context.Canceled {
		return err
	}

	var finalerr error
	for _, stopper := range db.stoppers {
		err = stopper()
		if err != nil {
			finalerr = multierr.Append(finalerr, err)
		}
	}
	return finalerr
}

func (db *DB) InitLocal() error {

	_, err := db.sqld.Exec(fmt.Sprintf("CREATE DATABASE %s;", db.name))
	if err != nil {
		return fmt.Errorf("failed to create db: %w", err)
	}

	_, err = db.sqld.Exec(fmt.Sprintf("USE %s;", db.name))
	if err != nil {
		return fmt.Errorf("failed to use db: %w", err)
	}

	return nil
}

func (db *DB) GetFilePath() string {
	return db.workingDir + "/" + db.name
}

func (db *DB) GetChunkStore() (chunks.ChunkStore, error) {
	env := db.mrEnv.GetEnv(db.name)
	if env == nil {
		return nil, fmt.Errorf("failed to retrieve db env")
	}

	dbd := doltdb.HackDatasDatabaseFromDoltDB(env.DoltDB)
	return datas.ChunkStoreFromDatabase(dbd), nil
}

func (db *DB) AddGRPCServer(server *grpc.Server) {
	db.grpcServer = server
}

func (db *DB) AddPeer(peerID string, conn *grpc.ClientConn) error {
	db.log.Infof("Adding peer %s", peerID)

	if _, ok := db.dbClients[peerID]; !ok {
		db.dbClients[peerID] = &DBClient{
			id:                      peerID,
			DBSyncerClient:          proto.NewDBSyncerClient(conn),
			ChunkStoreServiceClient: remotesapi.NewChunkStoreServiceClient(conn),
			DownloaderClient:        proto.NewDownloaderClient(conn),
		}
	} else {
		db.log.Infof("Client for peer %s already exists", peerID)
	}

	// this part only continues if the db is already initialized (non nil env)
	// so that the client is still available when doing initialization from another peer
	dbEnv := db.mrEnv.GetEnv(db.name)
	if dbEnv == nil {
		return nil
	}

	remotes, err := dbEnv.GetRemotes()
	if err != nil {
		return fmt.Errorf("failed to get remotes: %v", err)
	}

	if _, ok := remotes[peerID]; !ok {
		r := env.NewRemote(peerID, fmt.Sprintf("%s://%s", FactorySwarm, peerID), map[string]string{})
		err := dbEnv.AddRemote(r)
		if err != nil {
			return fmt.Errorf("failed to add remote: %w", err)
		}
	} else {
		db.log.Infof("Remote for peer %s already exists", peerID)
	}

	db.log.Infof("Added remote for peer %s", peerID)

	err = db.RequestHeadFromPeer(peerID)
	if err != nil {
		db.log.Errorf("failed to request head from peer %s: %v", peerID, err)
	}

	return nil
}

func (db *DB) RemovePeer(peerID string) error {

	dbEnv := db.mrEnv.GetEnv(db.name)
	if dbEnv == nil {
		return nil
	}

	delete(db.dbClients, peerID)

	err := dbEnv.RemoveRemote(context.Background(), peerID)
	if err != nil {
		if strings.Contains(err.Error(), "remote not found") {
			return nil
		}
		return fmt.Errorf("failed to remove remote: %w", err)
	}
	db.log.Infof("Removed remote for peer %s", peerID)

	return nil
}

func (db *DB) GetClient(peerID string) (*DBClient, error) {
	if client, ok := db.dbClients[peerID]; ok {
		return client, nil
	}
	return nil, fmt.Errorf("client for peer %s not found", peerID)
}

func (db *DB) GetClients() map[string]*DBClient {
	return db.dbClients
}

func (db *DB) InitFromPeer(peerID string) error {
	db.log.Infof("Initializing from peer %s", peerID)

	tries := 0
	for tries < 10 {
		query := fmt.Sprintf("CALL DOLT_CLONE('%s://%s/%s');", FactorySwarm, peerID, db.name)
		_, err := db.Exec(query)
		if err != nil {
			if strings.Contains(err.Error(), "could not get client") {
				db.log.Infof("Peer %s not available yet. Retrying...", peerID)
				tries++
				time.Sleep(2 * time.Second)
				continue
			}
			return fmt.Errorf("failed to clone db: %w", err)
		}
		db.log.Infof("Successfully cloned db from peer %s", peerID)
		return nil
	}

	return fmt.Errorf("failed to clone db from peer %s. Peer not found", peerID)
}

func (db *DB) Pull(peerID string) error {
	_, err := db.Exec(fmt.Sprintf("CALL DOLT_CHECKOUT('%s');", peerID))
	if err != nil {
		if strings.Contains(err.Error(), fmt.Sprintf("could not find %s", peerID)) {
			// peer branch not found, we create it
			_, err = db.Exec(fmt.Sprintf("CALL DOLT_CHECKOUT('-b', '%s');", peerID))
			if err != nil {
				return fmt.Errorf("failed to checkout branch for peer %s: %w", peerID, err)
			}
		} else {
			return fmt.Errorf("failed to checkout branch for peer %s: %w", peerID, err)
		}
	}

	_, err = db.Exec(fmt.Sprintf("CALL DOLT_PULL('%s', 'main');", peerID))
	if err != nil {
		return fmt.Errorf("failed to pull db from peer %s: %w", peerID, err)
	}
	return nil
}

func (db *DB) Merge(peerID string) error {

	txn, err := db.sqld.Begin()
	if err != nil {
		return fmt.Errorf("failed to start transaction: %w", err)
	}
	defer txn.Rollback()

	_, err = txn.Exec("CALL DOLT_CHECKOUT('main');")
	if err != nil {
		return fmt.Errorf("failed to checkout main branch: %w", err)
	}

	_, err = txn.Exec(fmt.Sprintf("CALL DOLT_MERGE('%s');", peerID))
	if err != nil {
		return fmt.Errorf("failed to merge branch for peer '%s': %w", peerID, err)
	}

	err = txn.Commit()
	if err != nil {
		return fmt.Errorf("failed to commit merge transaction: %w", err)
	}

	return nil
}

func (db *DB) Exec(query string, args ...any) (sql.Result, error) {
	return db.sqld.Exec(query)
}

func (db *DB) Query(query string, args ...any) (*sql.Rows, error) {
	return db.sqld.Query(query)
}

func (db *DB) Begin() (*sql.Tx, error) {
	return db.sqld.Begin()
}

func (db *DB) GetLastCommit() (Commit, error) {

	query := `SELECT {*} FROM dolt_commits ORDER BY date DESC LIMIT 1;`
	commits, err := sq.FetchAll(db.sqld, sq.
		Queryf(query).
		SetDialect(sq.DialectMySQL),
		commitMapper,
	)
	if err != nil {
		return Commit{}, fmt.Errorf("failed to retrieve last commit hash: %w", err)
	}

	if len(commits) == 0 {
		return Commit{}, fmt.Errorf("no commits found")
	}

	return commits[0], nil
}

func (db *DB) CheckIfCommitPresent(commitHash string) (bool, error) {
	query := fmt.Sprintf("SELECT {*} FROM dolt_commits WHERE commit_hash = '%s' LIMIT 1;", commitHash)
	commit, err := sq.FetchOne(db.sqld, sq.
		Queryf(query).
		SetDialect(sq.DialectMySQL),
		commitMapper,
	)
	if err != nil {
		if strings.Contains(err.Error(), "no rows in result set") {
			return false, nil
		}
		return false, fmt.Errorf("failed to look up commit hash: %w", err)
	}

	if commit.Hash == commitHash {
		return true, nil
	}

	return false, nil
}

func (db *DB) GetAllCommits() ([]Commit, error) {
	query := `SELECT {*} FROM dolt_commits ORDER BY date;`
	commits, err := sq.FetchAll(db.sqld, sq.
		Queryf(query).
		SetDialect(sq.DialectMySQL),
		commitMapper,
	)

	if err != nil {
		return commits, fmt.Errorf("failed to retrieve last commit hash: %w", err)
	}

	return commits, nil
}

func (db *DB) PrintAllCommits() error {
	query := fmt.Sprintf("SELECT * FROM `%s/main`.dolt_diff ORDER BY date;", db.name)
	_, err := db.Exec(query)
	if err != nil {
		return fmt.Errorf("failed to retrieve commits: %w", err)
	}

	return nil
}

func (db *DB) PrintAllData() error {
	_, err := db.Exec(fmt.Sprintf("SELECT * FROM `%s/main`.%s;", db.name, tableName))
	if err != nil {
		return fmt.Errorf("failed to retrieve commits: %w", err)
	}

	return nil
}

func (db *DB) PrintBranches() error {
	dbEnv := db.mrEnv.GetEnv(db.name)
	if dbEnv == nil {
		return fmt.Errorf("db '%s' not found", db.name)
	}

	ctx := context.Background()
	headRefs, err := dbEnv.DoltDB.GetHeadRefs(ctx)
	if err != nil {
		log.Fatalf("failed to retrieve head refs: %s", err.Error())
	}
	fmt.Println(headRefs)
	return nil
}
