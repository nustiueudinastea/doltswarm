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

	db.sqld = sql.OpenDB(&Connector{driver: &doltDriver{conn: dd.CreateCustomConnection(&dd.DoltDataSource{}, db.sqle, db.sqlCtx)}})

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

		_, err = db.sqld.Exec("CALL DOLT_REMOTE('remove','origin');")
		if err != nil && !strings.Contains(err.Error(), "unknown remote") {
			return fmt.Errorf("failed to remove origin remote during init: %w", err)
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
	db.log.Infof("doing p2p setup (grpc: %v)", withGRPCservers)
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

	db.log.Info("p2p setup done")

	return nil
}

func (db *DB) Close() error {
	for _, client := range db.dbClients {
		err := db.RemovePeer(client.GetID())
		if err != nil {
			db.log.Warnf("failed to remove peer %s: %v", client.GetID(), err)
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

	dbEnv := db.mrEnv.GetEnv(db.name)
	if dbEnv == nil {
		return nil
	}

	// this part only continues if the db is already initialized (non nil env)
	// so that the client is still available when doing initialization from another peer

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
		if !strings.Contains(err.Error(), "unknown service proto.DBSyncer") {
			db.log.Errorf("failed to request head from peer %s: %v", peerID, err)
		}
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

func (db *DB) InitFromPeer(peerID string) error {
	db.log.Infof("Initializing from peer %s", peerID)

	tries := 0
	for tries < 10 {
		query := fmt.Sprintf("CALL DOLT_CLONE('-b', 'main','%s://%s/%s');", FactorySwarm, peerID, db.name)
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
	db.log.Infof("Pulling from peer %s", peerID)
	txn, err := db.Begin()
	if err != nil {
		return fmt.Errorf("failed to start transaction: %w", err)
	}
	defer txn.Rollback()

	_, err = txn.Exec(fmt.Sprintf("CALL DOLT_FETCH('%s', 'main');", peerID))
	if err != nil {
		return fmt.Errorf("failed to pull db from peer %s: %w", peerID, err)
	}

	err = txn.Commit()
	if err != nil {
		return fmt.Errorf("failed to commit merge transaction: %w", err)
	}

	return nil
}

func (db *DB) Merge(peerID string) error {
	db.log.Infof("Merging from peer %s", peerID)

	commit, err := db.GetFirstCommit()
	if err != nil {
		return fmt.Errorf("failed to get first commit while adding peer: %w", err)
	}

	txn, err := db.sqld.Begin()
	if err != nil {
		return fmt.Errorf("failed to start transaction: %w", err)
	}
	defer txn.Rollback()

	tempPeerBranch := randSeq(6)
	tempMainBranch := randSeq(6)

	_, err = db.Exec(fmt.Sprintf("CALL DOLT_CHECKOUT('-b', '%s', '%s');", tempPeerBranch, commit.Hash))
	if err != nil {
		return fmt.Errorf("failed to checkout branch for peer %s: %w", peerID, err)
	}
	defer func() {
		// delete temp branch
		_, err = db.Exec(fmt.Sprintf("CALL DOLT_BRANCH('-d', '%s');", tempPeerBranch))
		if err != nil {
			db.log.Errorf("failed to delete temp branch: %v", err)
		}
	}()

	_, err = db.Exec(fmt.Sprintf("CALL DOLT_MERGE('%s/main');", peerID))
	if err != nil {
		return fmt.Errorf("failed to checkout branch for peer %s: %w", peerID, err)
	}

	_, err = txn.Exec(fmt.Sprintf("CALL DOLT_BRANCH('-c', 'main', '%s');", tempMainBranch))
	if err != nil {
		return fmt.Errorf("failed to copy source branch: %w", err)
	}
	defer func() {
		// delete temp branch
		_, err = db.Exec(fmt.Sprintf("CALL DOLT_BRANCH('-d', '%s');", tempMainBranch))
		if err != nil {
			db.log.Errorf("failed to delete temp branch: %v", err)
		}
	}()

	var hashMain sql.NullString
	err = txn.QueryRow(fmt.Sprintf("select commit_hash from `%s/%s`.dolt_log LIMIT 1", db.name, tempMainBranch)).Scan(&hashMain)
	if err != nil || !hashMain.Valid {
		return fmt.Errorf("failed to retrieve main hash: %w", err)
	}

	var hashPeer sql.NullString
	err = txn.QueryRow(fmt.Sprintf("select commit_hash from `%s/%s`.dolt_log LIMIT 1", db.name, tempPeerBranch)).Scan(&hashPeer)
	if err != nil || !hashPeer.Valid {
		return fmt.Errorf("failed to retrieve peer hash: %w", err)
	}

	// establish deterministic merge direction based on simple string comparison
	sourceBranch := tempPeerBranch
	targetBranch := tempMainBranch
	if hashMain.String > hashPeer.String {
		sourceBranch = tempMainBranch
		targetBranch = tempPeerBranch
	}

	_, err = txn.Exec(fmt.Sprintf("CALL DOLT_CHECKOUT('%s');", targetBranch))
	if err != nil {
		return fmt.Errorf("failed to checkout main branch: %w", err)
	}

	rows, err := txn.Query(fmt.Sprintf("CALL DOLT_MERGE('--no-commit', '%s');", sourceBranch))
	if err != nil {
		return fmt.Errorf("failed to merge branch for peer '%s': %w", peerID, err)
	}

	defer rows.Close()
	if !rows.Next() {
		err = rows.Err()
		if err != nil {
			return fmt.Errorf("no query result after performing merge for '%s': %w", peerID, err)
		}
	}

	var hash string
	var fastForwards int
	var conflicts int
	err = rows.Scan(&hash, &fastForwards, &conflicts)
	if err != nil {
		return fmt.Errorf("failed to scan rows while merging branch for peer '%s': %w", peerID, err)
	}

	if conflicts > 0 {
		return fmt.Errorf("conflicts found while merging branch for peer '%s'", peerID)
	}

	// if no conflict and a ff happend, we commit the transaction and we return because we don't need a dolt commit
	if fastForwards > 0 {
		//
		// fast forward merge (no commit needed)
		//

		_, err = txn.Exec("CALL DOLT_CHECKOUT('main');")
		if err != nil {
			return fmt.Errorf("failed to checkout main branch: %w", err)
		}

		_, err = txn.Exec(fmt.Sprintf("CALL DOLT_MERGE('%s');", targetBranch))
		if err != nil {
			return fmt.Errorf("failed to merge target to main branch: %w", err)
		}

	} else {
		//
		// commit merge
		//

		// get last commit date and use it for our auto merge commit
		query := fmt.Sprintf("SELECT {*} FROM `%s/%s`.dolt_log LIMIT 1;", db.name, targetBranch)
		commits, err := sq.FetchAll(db.sqld, sq.
			Queryf(query).
			SetDialect(sq.DialectMySQL),
			commitMapper,
		)
		if err != nil && len(commits) == 0 {
			return fmt.Errorf("failed to retrieve last commit hash: %w", err)
		}

		_, err = txn.Exec(fmt.Sprintf("CALL DOLT_COMMIT('-A', '--author', 'merge <merge@merge.com>', '--date', '%s', '-m', 'auto merge');", commits[0].Date.Format(time.RFC3339Nano)))
		if err != nil {
			fmt.Println(err)
			if strings.Contains(err.Error(), "nothing to commit") {
				return nil
			}
			return fmt.Errorf("failed to commit merge: %w", err)
		}

		_, err = txn.Exec("CALL DOLT_CHECKOUT('main');")
		if err != nil {
			return fmt.Errorf("failed to checkout main branch: %w", err)
		}

		_, err = txn.Exec(fmt.Sprintf("CALL DOLT_MERGE('%s');", targetBranch))
		if err != nil {
			return fmt.Errorf("failed to merge target to main branch: %w", err)
		}
	}

	err = txn.Commit()
	if err != nil {
		return fmt.Errorf("failed to commit merge transaction: %w", err)
	}

	// advertise new head
	db.AdvertiseHead()

	db.log.Infof("Finished merging from peer %s", peerID)
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

func (db *DB) ExecAndCommit(query string, commitMsg string) (string, error) {
	tx, err := db.Begin()
	if err != nil {
		return "", fmt.Errorf("failed to start transaction: %w", err)
	}

	defer tx.Rollback()

	_, err = tx.Exec("CALL DOLT_CHECKOUT('main');")
	if err != nil {
		return "", fmt.Errorf("failed to checkout main branch: %w", err)
	}

	_, err = tx.Exec(query)
	if err != nil {
		return "", fmt.Errorf("failed to save record: %w", err)
	}

	// commit
	var commitHash string
	err = tx.QueryRow(fmt.Sprintf("CALL DOLT_COMMIT('-a', '-m', '%s', '--author', 'Alex Giurgiu <alex@giurgiu.io>', '--date', '%s');", commitMsg, time.Now().Format(time.RFC3339Nano))).Scan(&commitHash)
	if err != nil {
		return "", fmt.Errorf("failed to commit table: %w", err)
	}

	err = tx.Commit()
	if err != nil {
		return "", fmt.Errorf("failed to commit insert transaction: %w", err)
	}

	// advertise new head
	db.AdvertiseHead()

	return commitHash, nil
}

func (db *DB) GetLastCommit(branch string) (Commit, error) {

	query := fmt.Sprintf("SELECT {*} FROM `%s/%s`.dolt_log LIMIT 1;", db.name, branch)
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

func (db *DB) GetFirstCommit() (Commit, error) {

	query := fmt.Sprintf("SELECT {*} FROM `%s/main`.dolt_log ORDER BY date LIMIT 1;", db.name)
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
	query := fmt.Sprintf("SELECT {*} FROM `%s/main`.dolt_log WHERE commit_hash = '%s' LIMIT 1;", db.name, commitHash)
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
	query := fmt.Sprintf("SELECT {*} FROM `%s/main`.dolt_log;", db.name)
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

// func (db *DB) PrintAllCommits() error {
// 	query := fmt.Sprintf("SELECT * FROM `%s/main`.dolt_diff ORDER BY date;", db.name)
// 	_, err := db.Exec(query)
// 	if err != nil {
// 		return fmt.Errorf("failed to retrieve commits: %w", err)
// 	}

// 	return nil
// }

// func (db *DB) PrintAllData() error {
// 	_, err := db.Exec(fmt.Sprintf("SELECT * FROM `%s/main`.%s;", db.name, tableName))
// 	if err != nil {
// 		return fmt.Errorf("failed to retrieve commits: %w", err)
// 	}

// 	return nil
// }

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
