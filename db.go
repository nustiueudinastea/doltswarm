package doltswarm

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/bokwoon95/sq"
	remotesapi "github.com/dolthub/dolt/go/gen/proto/dolt/services/remotesapi/v1alpha1"
	"github.com/dolthub/dolt/go/libraries/doltcore/dbfactory"
	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/env"
	"github.com/dolthub/dolt/go/libraries/doltcore/remotesrv"
	"github.com/dolthub/dolt/go/libraries/utils/concurrentmap"
	"github.com/dolthub/dolt/go/libraries/utils/filesys"
	"github.com/dolthub/dolt/go/store/chunks"
	"github.com/dolthub/dolt/go/store/datas"
	"github.com/nustiueudinastea/doltswarm/proto"
	"github.com/sirupsen/logrus"
	"go.uber.org/multierr"
	"google.golang.org/grpc"

	_ "github.com/dolthub/driver"
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

type Remote struct {
	Name       string
	URL        string
	FetchSpecs string
	Params     string
}

type Database struct {
	Name string
}

type DoltMREnvRetriever interface {
	GetMultiRepoEnv() *env.MultiRepoEnv
}

func commitMapper(row *sq.Row) Commit {
	commit := Commit{
		Hash:      row.String("commit_hash"),
		Committer: row.String("committer"),
		Email:     row.String("email"),
		Date:      row.Time("date"),
		Message:   row.String("message"),
	}
	return commit
}

type DB struct {
	init     bool
	name     string
	stoppers *concurrentmap.Map[string, func() error]
	// mrEnv      *env.MultiRepoEnv
	conn       *sql.Conn
	eventQueue chan Event
	workingDir string
	grpcServer *grpc.Server
	log        *logrus.Logger
	dbClients  *concurrentmap.Map[string, *DBClient]
}

func New(dir string, name string, logger *logrus.Logger, init bool) (*DB, error) {
	workingDir, err := filesys.LocalFS.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to get absolute path for %s: %v", workingDir, err)
	}

	db := &DB{
		init:       init,
		name:       name,
		workingDir: workingDir,
		log:        logger,
		eventQueue: make(chan Event, 300),
		dbClients:  concurrentmap.New[string, *DBClient](),
		stoppers:   concurrentmap.New[string, func() error](),
	}

	return db, nil
}

func (db *DB) Open() error {

	err := ensureDir(db.workingDir)
	if err != nil {
		return fmt.Errorf("failed to open db: %w", err)
	}

	var dbConn string
	if db.init {
		dbConn = "file://" + db.workingDir + "?commitname=Alex&commitemail=alex@giurgiu.io&multistatements=true"
	} else {
		dbConn = "file://" + db.workingDir + "?commitname=Alex&commitemail=alex@giurgiu.io&database=" + db.name + "&multistatements=true"
	}

	sqld, err := sql.Open("dolt", dbConn)
	if err != nil {
		return fmt.Errorf("failed to open db: %w", err)
	}

	db.conn, err = sqld.Conn(context.Background())
	if err != nil {
		return fmt.Errorf("failed to open db connection: %w", err)
	}

	if db.conn.PingContext(context.Background()) != nil {
		return fmt.Errorf("failed to ping db: %w", err)
	}

	dbs, err := db.GetDatabase(db.name)
	if err == nil && dbs == db.name {
		err = db.p2pSetup(true)
		if err != nil {
			return fmt.Errorf("failed to do p2p setup: %w", err)
		}

		_, err = db.conn.ExecContext(context.Background(), "CALL DOLT_REMOTE('remove','origin');")
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
	dbfactory.DBFactories[FactorySwarm] = NewDoltSwarmFactory(db.name, db, logrus.NewEntry(db.log))
	dbfactory.RegisterFactory(FactorySwarm, NewDoltSwarmFactory(db.name, db, logrus.NewEntry(db.log)))

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
		db.stoppers.Set("dbEventProcessor", db.startRemoteEventProcessor())
	}

	db.log.Info("p2p setup done")

	return nil
}

func (db *DB) Close() error {
	defer db.conn.Close()
	db.dbClients.Iter(func(key string, client *DBClient) bool {
		err := db.RemovePeer(client.GetID())
		if err != nil {
			db.log.Warnf("failed to remove peer %s: %v", client.GetID(), err)
		}
		return true
	})

	var finalerr error
	db.stoppers.Iter(func(key string, stopper func() error) bool {
		err := stopper()
		if err != nil {
			finalerr = multierr.Append(finalerr, err)
		}
		db.log.Infof("Stopped %s", key)
		return true
	})
	return finalerr
}

func (db *DB) GetFilePath() string {
	return db.workingDir + "/" + db.name
}

func (db *DB) GetChunkStore() (chunks.ChunkStore, error) {

	var mrEnv *env.MultiRepoEnv
	err := db.conn.Raw(func(driverConn any) error {
		r, ok := driverConn.(DoltMREnvRetriever)
		if !ok {
			return fmt.Errorf("connection is not of dolt type")
		}
		mrEnv = r.GetMultiRepoEnv()
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve multi repo env: %w", err)
	}
	if mrEnv == nil {
		return nil, fmt.Errorf("multi repo env not found")
	}

	env := mrEnv.GetEnv(db.name)
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
	if _, ok := db.dbClients.Get(peerID); !ok {
		db.dbClients.Set(peerID, &DBClient{
			id:                      peerID,
			DBSyncerClient:          proto.NewDBSyncerClient(conn),
			ChunkStoreServiceClient: remotesapi.NewChunkStoreServiceClient(conn),
			DownloaderClient:        proto.NewDownloaderClient(conn),
		})
		db.log.Infof("Added client for peer %s", peerID)
	} else {
		db.log.Infof("Client for peer %s already exists", peerID)
	}

	if !db.init {
		// this part is executed if the db is already initialized
		// so that the client is still available when doing initialization from another peer
		_, err := db.Exec(fmt.Sprintf("CALL DOLT_REMOTE('add','%s','%s://%s');", peerID, FactorySwarm, peerID))
		if err != nil {
			return fmt.Errorf("failed to add remote for peer %s: %w", peerID, err)
		}

		db.log.Infof("Added remote for peer %s", peerID)

		err = db.RequestHeadFromPeer(peerID)
		if err != nil {
			if !strings.Contains(err.Error(), "unknown service proto.DBSyncer") {
				db.log.Errorf("failed to request head from peer %s: %v", peerID, err)
			}
		}
	}

	return nil
}

func (db *DB) RemovePeer(peerID string) error {
	db.dbClients.Delete(peerID)

	if !db.init {
		_, err := db.Exec(fmt.Sprintf("CALL DOLT_REMOTE('remove','%s');", peerID))
		if err != nil {
			if !strings.Contains(err.Error(), "remote not found") && !strings.Contains(err.Error(), "unknown remote") {
				return fmt.Errorf("failed to remove remote for peer %s: %w", peerID, err)
			}
		}
	}

	db.log.Infof("Removed remote for peer %s", peerID)

	return nil
}

func (db *DB) GetClient(peerID string) (*DBClient, error) {
	if client, ok := db.dbClients.Get(peerID); ok {
		return client, nil
	}
	return nil, fmt.Errorf("client for peer %s not found", peerID)
}

func (db *DB) GetClients() map[string]*DBClient {
	return db.dbClients.Snapshot()
}

func (db *DB) InitLocal() error {
	db.log.Infof("Initializing local db %s", db.name)

	ctx := context.Background()

	_, err := db.conn.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s;", db.name))
	if err != nil {
		return fmt.Errorf("failed to create db: %w", err)
	}

	_, err = db.conn.ExecContext(ctx, "commit;")
	if err != nil {
		return fmt.Errorf("failed to commit db creation: %w", err)
	}

	return nil
}

func (db *DB) InitFromPeer(peerID string) error {
	db.log.Infof("Initializing from peer %s", peerID)

	// time.Sleep(3 * time.Second)

	tries := 0
	for tries < 10 {
		query := fmt.Sprintf("CALL DOLT_CLONE('-b', 'main', '%s://%s/%s', '%s');", FactorySwarm, peerID, db.name, db.name)
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

	txn, err := db.conn.BeginTx(context.Background(), nil)
	if err != nil {
		return fmt.Errorf("failed to start transaction: %w", err)
	}
	defer txn.Rollback()

	tempPeerBranch := randSeq(6)
	tempMainBranch := randSeq(6)

	_, err = txn.Exec(fmt.Sprintf("USE %s;", db.name))
	if err != nil {
		return fmt.Errorf("failed to use db: %w", err)
	}

	_, err = txn.Exec(fmt.Sprintf("CALL DOLT_CHECKOUT('-b', '%s', '%s');", tempPeerBranch, commit.Hash))
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

	_, err = txn.Exec(fmt.Sprintf("CALL DOLT_MERGE('%s/main');", peerID))
	if err != nil {
		return fmt.Errorf("failed to merge peer branch %s: %w", peerID, err)
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
		commits, err := sq.FetchAll(db.conn, sq.
			Queryf(query).
			SetDialect(sq.DialectMySQL),
			commitMapper,
		)
		if err != nil && len(commits) == 0 {
			return fmt.Errorf("failed to retrieve last commit hash: %w", err)
		}

		_, err = txn.Exec(fmt.Sprintf("CALL DOLT_COMMIT('-A', '--author', 'merge <merge@merge.com>', '--date', '%s', '-m', 'auto merge');", commits[0].Date.Format(time.RFC3339Nano)))
		if err != nil {
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
	return db.conn.ExecContext(context.Background(), query)
}

func (db *DB) Query(query string, args ...any) (*sql.Rows, error) {
	return db.conn.QueryContext(context.Background(), query)
}

func (db *DB) Begin() (*sql.Tx, error) {
	return db.conn.BeginTx(context.Background(), nil)
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
	commits, err := sq.FetchAll(db.conn, sq.
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

func (db *DB) GetRemote(name string) (Remote, error) {
	return sq.FetchOne(db.conn, sq.
		Queryf("SELECT {*} FROM remotes WHERE name = {}", name).
		SetDialect(sq.DialectMySQL),
		func(row *sq.Row) Remote {
			return Remote{
				Name:       row.String("name"),
				URL:        row.String("url"),
				FetchSpecs: row.String("fetch_specs"),
				Params:     row.String("params"),
			}
		},
	)
}

func (db *DB) GetDatabase(name string) (string, error) {
	dbName := ""
	err := db.conn.QueryRowContext(context.Background(), fmt.Sprintf("SHOW DATABASES LIKE '%s'", db.name)).Scan(&dbName)
	return dbName, err
}

func (db *DB) GetFirstCommit() (Commit, error) {

	query := fmt.Sprintf("SELECT {*} FROM `%s/main`.dolt_log ORDER BY date LIMIT 1;", db.name)
	commits, err := sq.FetchAll(db.conn, sq.
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
	commit, err := sq.FetchOne(db.conn, sq.
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
	commits, err := sq.FetchAll(db.conn, sq.
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

// func (db *DB) PrintBranches() error {
// 	dbEnv := db.mrEnv.GetEnv(db.name)
// 	if dbEnv == nil {
// 		return fmt.Errorf("db '%s' not found", db.name)
// 	}

// 	ctx := context.Background()
// 	headRefs, err := dbEnv.DoltDB.GetHeadRefs(ctx)
// 	if err != nil {
// 		log.Fatalf("failed to retrieve head refs: %s", err.Error())
// 	}
// 	fmt.Println(headRefs)
// 	return nil
// }
