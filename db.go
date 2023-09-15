package db

import (
	"context"
	"database/sql"
	"fmt"
	"io"
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
	"github.com/nustiueudinastea/doltswarm/client"
	"github.com/nustiueudinastea/doltswarm/proto"
	"github.com/nustiueudinastea/doltswarm/server"
	"github.com/segmentio/ksuid"
	"github.com/sirupsen/logrus"
	"go.uber.org/multierr"
)

const (
	FactorySwarm = "swarm"
)

type Commit struct {
	Hash         string
	Table        string
	Committer    string
	Email        string
	Date         time.Time
	Message      string
	DataChange   bool
	SchemaChange bool
}

var tableName = "testtable"

type DB struct {
	name            string
	stoppers        []func() error
	commitListChan  chan []Commit
	dbEnvInit       *env.DoltEnv
	mrEnv           *env.MultiRepoEnv
	sqle            *engine.SqlEngine
	sqld            *sql.DB
	sqlCtx          *doltSQL.Context
	workingDir      string
	peerRegistrator PeerHandlerRegistrator
	grpcRetriever   GRPCServerRetriever
	clientRetriever client.ClientRetriever
	log             *logrus.Logger
}

func New(dir string, name string, commitListChan chan []Commit, peerRegistrator PeerHandlerRegistrator, grpcRetriever GRPCServerRetriever, clientRetriever client.ClientRetriever, logger *logrus.Logger) (*DB, error) {
	workingDir, err := filesys.LocalFS.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to get absolute path for %s: %v", workingDir, err)
	}

	db := &DB{
		name:            name,
		workingDir:      workingDir,
		peerRegistrator: peerRegistrator,
		grpcRetriever:   grpcRetriever,
		clientRetriever: clientRetriever,
		log:             logger,
		commitListChan:  commitListChan,
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
		Autocommit:              true,
		DoltTransactionCommit:   true,
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
	} else {
		err = db.p2pSetup(false)
		if err != nil {
			return fmt.Errorf("failed to do p2p setup: %w", err)
		}
	}

	return nil
}

func (db *DB) p2pSetup(withGRPCservers bool) error {
	db.log.Info("Doing p2p setup")
	db.peerRegistrator.RegisterPeerHandler(db)
	// register new factory
	dbfactory.RegisterFactory(FactorySwarm, client.NewCustomFactory(db.name, db.clientRetriever, logrus.NewEntry(db.log)))

	if withGRPCservers {
		// prepare dolt chunk store server
		cs, err := db.GetChunkStore()
		if err != nil {
			return fmt.Errorf("error getting chunk store: %s", err.Error())
		}
		chunkStoreCache := server.NewCSCache(cs.(remotesrv.RemoteSrvStore))
		chunkStoreServer := server.NewServerChunkStore(logrus.NewEntry(db.log), chunkStoreCache, db.GetFilePath())
		eventQueue := make(chan server.Event, 100)
		syncerServer := server.NewServerSyncer(logrus.NewEntry(db.log), eventQueue)

		// register grpc servers
		grpcServer := db.grpcRetriever.GetGRPCServer()
		proto.RegisterDownloaderServer(grpcServer, chunkStoreServer)
		remotesapi.RegisterChunkStoreServiceServer(grpcServer, chunkStoreServer)
		proto.RegisterDBSyncerServer(grpcServer, syncerServer)

		// start event processor
		db.stoppers = append(db.stoppers, db.remoteEventProcessor(eventQueue))
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
	err := db.Query(fmt.Sprintf("CREATE DATABASE %s;", db.name), true)
	if err != nil {
		return fmt.Errorf("failed to create db: %w", err)
	}

	err = db.Query(fmt.Sprintf("USE %s;", db.name), false)
	if err != nil {
		return fmt.Errorf("failed to use db: %w", err)
	}

	// create table
	err = db.Query(fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s(
		  id varchar(256) PRIMARY KEY,
		  name varchar(512)
	    );`, tableName), true)
	if err != nil {
		return fmt.Errorf("failed to create table: %w", err)
	}

	return nil
}

func (db *DB) StartUpdater() {
	db.stoppers = append(db.stoppers, db.commitUpdater())
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

func (db *DB) AddPeer(peerID string) error {

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

	return nil
}

func (db *DB) RemovePeer(peerID string) error {

	dbEnv := db.mrEnv.GetEnv(db.name)
	if dbEnv == nil {
		return nil
	}

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

func (db *DB) InitFromPeer(peerID string) error {
	db.log.Infof("Initializing from peer %s", peerID)

	tries := 0
	for tries < 10 {
		query := fmt.Sprintf("CALL DOLT_CLONE('%s://%s/%s');", FactorySwarm, peerID, db.name)
		err := db.Query(query, true)
		if err != nil {
			if strings.Contains(err.Error(), "could not get client") {
				db.log.Warnf("Peer %s not available yet. Retrying...", peerID)
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
	err := db.Query(fmt.Sprintf("USE %s;", db.name), false)
	if err != nil {
		return fmt.Errorf("failed to use db: %w", err)
	}

	err = db.Query(fmt.Sprintf("CALL DOLT_CHECKOUT('%s');", peerID), false)
	if err != nil {
		if strings.Contains(err.Error(), fmt.Sprintf("could not find %s", peerID)) {
			// peer branch not found, we create it
			err = db.Query(fmt.Sprintf("CALL DOLT_CHECKOUT('-b', '%s');", peerID), false)
			if err != nil {
				return fmt.Errorf("failed to checkout branch for peer %s: %w", peerID, err)
			}
		} else {
			return fmt.Errorf("failed to checkout branch for peer %s: %w", peerID, err)
		}
	}

	err = db.Query(fmt.Sprintf("CALL DOLT_PULL('%s', 'main');", peerID), false)
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

func (db *DB) Query(query string, printResult bool) (err error) {
	schema, rows, err := db.sqle.Query(db.sqlCtx, query)
	if err != nil {
		return err
	}

	if printResult {
		engine.PrettyPrintResults(db.sqlCtx, engine.FormatTabular, schema, rows)
	} else {
		for {
			_, err := rows.Next(db.sqlCtx)
			if err == io.EOF {
				break
			} else if err != nil {
				return err
			}
		}
		rows.Close(db.sqlCtx)
	}

	return nil
}

func (db *DB) Insert(data string) error {
	uid, err := ksuid.NewRandom()
	if err != nil {
		return fmt.Errorf("failed to create uid: %w", err)
	}

	err = db.Query(fmt.Sprintf("USE %s;", db.name), false)
	if err != nil {
		return fmt.Errorf("failed to use db: %w", err)
	}

	queryString := fmt.Sprintf("INSERT INTO %s (id, name) VALUES ('%s', '%s');", tableName, uid.String(), data)
	err = db.Query(queryString, false)
	if err != nil {
		return fmt.Errorf("failed to save record: %w", err)
	}

	// async advertise new head
	go db.AdvertiseHead()

	return nil
}

func (db *DB) GetLastCommit() (Commit, error) {

	query := fmt.Sprintf("SELECT {*} FROM `%s/main`.dolt_diff ORDER BY date DESC;", db.name)
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

func (db *DB) GetAllCommits() ([]Commit, error) {
	query := fmt.Sprintf("SELECT {*} FROM `%s/main`.dolt_diff ORDER BY date DESC;", db.name)
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
	err := db.Query(query, true)
	if err != nil {
		return fmt.Errorf("failed to retrieve commits: %w", err)
	}

	return nil
}

func (db *DB) PrintAllData() error {
	err := db.Query(fmt.Sprintf("SELECT * FROM `%s/main`.%s;", db.name, tableName), true)
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

func (db *DB) commitUpdater() func() error {
	db.log.Info("Starting commit updater")
	updateTimer := time.NewTicker(1 * time.Second)
	commitTimmer := time.NewTicker(15 * time.Second)
	stopSignal := make(chan struct{})
	go func() {
		for {
			select {
			case <-updateTimer.C:
				commits, err := db.GetAllCommits()
				if err != nil {
					db.log.Errorf("failed to retrieve all commits: %s", err.Error())
					continue
				}
				db.commitListChan <- commits
			case timer := <-commitTimmer.C:
				err := db.Insert(timer.String())
				if err != nil {
					db.log.Errorf("Failed to insert time: %s", err.Error())
					continue
				}
				db.log.Infof("Inserted time '%s' into db", timer.String())
			case <-stopSignal:
				db.log.Info("Stopping commit updater")
				return
			}
		}
	}()
	stopper := func() error {
		stopSignal <- struct{}{}
		return nil
	}
	return stopper
}
