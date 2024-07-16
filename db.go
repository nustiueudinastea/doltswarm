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

type Signer interface {
	Sign(commit string) (string, error)
	Verify(commit string, signature string, publicKey string) error
	PublicKey() string
	GetID() string
}

type DB struct {
	name        string
	initialized bool
	stoppers    *concurrentmap.Map[string, func() error]
	conn        *sql.Conn
	db          *sql.DB
	eventQueue  chan Event
	workingDir  string
	grpcServer  *grpc.Server
	log         *logrus.Entry
	dbClients   *concurrentmap.Map[string, *DBClient]
	signer      Signer
}

func Open(dir string, name string, logger *logrus.Entry, signer Signer) (*DB, error) {

	if logger == nil {
		return nil, fmt.Errorf("logger is nil")
	}

	if signer == nil {
		return nil, fmt.Errorf("signer is nil")
	}

	workingDir, err := filesys.LocalFS.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to get absolute path for %s: %v", workingDir, err)
	}

	db := &DB{
		name:       name,
		workingDir: workingDir,
		log:        logger,
		eventQueue: make(chan Event, 300),
		dbClients:  concurrentmap.New[string, *DBClient](),
		stoppers:   concurrentmap.New[string, func() error](),
		signer:     signer,
	}

	err = ensureDir(db.workingDir)
	if err != nil {
		return nil, fmt.Errorf("failed to open db: %w", err)
	}

	dbConn := fmt.Sprintf("file://%s?commitname=%s&commitemail=%s@doltswarm&multistatements=true", db.workingDir, db.signer.GetID(), db.signer.GetID())
	db.db, err = sql.Open("dolt", dbConn)
	if err != nil {
		return nil, fmt.Errorf("failed to open db: %w", err)
	}

	db.conn, err = db.db.Conn(context.Background())
	if err != nil {
		return nil, fmt.Errorf("failed to open db connection: %w", err)
	}

	if db.conn.PingContext(context.Background()) != nil {
		return nil, fmt.Errorf("failed to ping db: %w", err)
	}

	dbs, err := db.GetDatabase(db.name)
	if err == nil && dbs == db.name {
		db.initialized = true
		_, err = db.conn.ExecContext(context.Background(), fmt.Sprintf("USE %s;", db.name))
		if err != nil {
			return nil, fmt.Errorf("failed to use db: %w", err)
		}
		_, err = db.conn.ExecContext(context.Background(), "CALL DOLT_REMOTE('remove','origin');")
		if err != nil && !strings.Contains(err.Error(), "unknown remote") {
			return nil, fmt.Errorf("failed to remove origin remote during init: %w", err)
		}
	}
	err = db.p2pSetup()
	if err != nil {
		return nil, fmt.Errorf("failed to do p2p setup: %w", err)
	}

	return db, nil
}

func (db *DB) p2pSetup() error {
	db.log.Info("doing p2p setup")
	// register new factory
	dbfactory.DBFactories[FactorySwarm] = NewDoltSwarmFactory(db.name, db, db.log)
	dbfactory.RegisterFactory(FactorySwarm, NewDoltSwarmFactory(db.name, db, db.log))

	db.log.Info("p2p setup done")

	return nil
}

func (db *DB) EnableGRPCServers() error {

	db.log.Debug("enabling grpc servers")

	// prepare dolt chunk store server
	cs, err := db.GetChunkStore()
	if err != nil {
		return fmt.Errorf("error getting chunk store: %s", err.Error())
	}
	chunkStoreCache := NewCSCache(cs.(remotesrv.RemoteSrvStore))
	chunkStoreServer := NewServerChunkStore(db.log, chunkStoreCache, db.GetFilePath())
	syncerServer := NewServerSyncer(db.log, db)

	// register grpc servers
	if db.grpcServer == nil {
		return fmt.Errorf("grpc server not initialized")
	}
	proto.RegisterDownloaderServer(db.grpcServer, chunkStoreServer)
	remotesapi.RegisterChunkStoreServiceServer(db.grpcServer, chunkStoreServer)
	proto.RegisterDBSyncerServer(db.grpcServer, syncerServer)

	// start event processor
	db.stoppers.Set("dbEventProcessor", db.startRemoteEventProcessor())

	return nil
}

func (db *DB) Close() error {
	if db.conn != nil {
		defer db.conn.Close()
	}
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
		db.log.Debugf("added swarm client for peer %s", peerID)
	} else {
		db.log.Debugf("swarm client for peer %s already exists", peerID)
	}

	if db.initialized {

		// TODO: change this to a check if the remote exists
		_, err := db.ExecContext(context.TODO(), fmt.Sprintf("CALL DOLT_REMOTE('remove','%s');", peerID))
		if err != nil {
			if !strings.Contains(err.Error(), "remote not found") && !strings.Contains(err.Error(), "unknown remote") {
				return fmt.Errorf("failed to remove remote for peer %s: %w", peerID, err)
			}
		}

		// this part is executed if the db is already initialized
		// so that the client is still available when doing initialization from another peer
		_, err = db.ExecContext(context.TODO(), fmt.Sprintf("CALL DOLT_REMOTE('add','%s','%s://%s');", peerID, FactorySwarm, peerID))
		if err != nil {
			return fmt.Errorf("failed to add remote for peer %s: %w", peerID, err)
		}

		db.log.Debugf("added swarm remote for peer %s", peerID)

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
	db.log.Debugf("deleting swarm client for peer %s", peerID)
	db.dbClients.Delete(peerID)

	if db.initialized {
		db.log.Debugf("deleting swarm remote for peer %s", peerID)
		_, err := db.ExecContext(context.TODO(), fmt.Sprintf("CALL DOLT_REMOTE('remove','%s');", peerID))
		if err != nil {
			if !strings.Contains(err.Error(), "remote not found") && !strings.Contains(err.Error(), "unknown remote") {
				return fmt.Errorf("failed to remove remote for peer %s: %w", peerID, err)
			}
		}
	}

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

	sqlCmd := fmt.Sprintf("CREATE DATABASE %s;", db.name)
	_, err := db.conn.ExecContext(ctx, sqlCmd)
	if err != nil {
		return fmt.Errorf("failed to exec '%s': %w", sqlCmd, err)
	}

	_, err = db.conn.ExecContext(ctx, "commit;")
	if err != nil {
		return fmt.Errorf("failed to commit db creation: %w", err)
	}

	_, err = db.conn.ExecContext(ctx, fmt.Sprintf("USE %s;", db.name))
	if err != nil {
		return fmt.Errorf("failed to use db: %w", err)
	}

	commit, err := db.GetFirstCommit()
	if err != nil {
		return fmt.Errorf("failed to retrieve init commit: %w", err)
	}

	// create commit signature and add it to a tag
	signature, err := db.signer.Sign(commit.Hash)
	if err != nil {
		return fmt.Errorf("failed to sign init commit '%s': %w", commit.Hash, err)
	}
	tagcmd := fmt.Sprintf("CALL DOLT_TAG('-m', '%s', '--author', '%s <%s@doltswarm>', '%s', '%s');", db.signer.PublicKey(), db.signer.GetID(), db.signer.GetID(), signature, commit.Hash)
	_, err = db.conn.ExecContext(ctx, tagcmd)
	if err != nil {
		return fmt.Errorf("failed to create signature tag (%s) for init commit (%s): %w", signature, commit.Hash, err)
	}

	db.initialized = true

	return nil
}

func (db *DB) InitFromPeer(peerID string) error {
	db.log.Infof("Initializing from peer %s", peerID)

	// time.Sleep(3 * time.Second)

	tries := 0
	for tries < 10 {
		query := fmt.Sprintf("CALL DOLT_CLONE('-b', 'main', '%s://%s/%s', '%s');", FactorySwarm, peerID, db.name, db.name)
		_, err := db.ExecContext(context.TODO(), query)
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

	db.initialized = true

	return fmt.Errorf("failed to clone db from peer %s. Peer not found", peerID)
}

func (db *DB) Pull(peerID string) error {
	db.log.Infof("Pulling from peer %s", peerID)
	txn, err := db.BeginTx(context.TODO(), nil)
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
		return fmt.Errorf("failed to commit sql pull transaction: %w", err)
	}

	return nil
}

func (db *DB) VerifySignatures(peerID string) error {

	// retrieve commits specific to peer branch
	query := fmt.Sprintf("SELECT {*} FROM dolt_log('main..%s/main');", peerID)
	commits, err := sq.FetchAll(db.conn, sq.
		Queryf(query).
		SetDialect(sq.DialectMySQL),
		commitMapper,
	)
	if err != nil {
		return fmt.Errorf("failed to retrieve commits for peer %s: %w", peerID, err)
	}

	// build the list of commit hashes
	commitHashes := make(sq.RowValue, len(commits))
	for _, commit := range commits {
		commitHashes = append(commitHashes, commit.Hash)
	}

	// retrieve signatures for peer commits (from tags)
	t := sq.New[TAG]("")
	tags, err := sq.FetchAll(db.conn, sq.
		From(t).
		Where(
			t.TAG_HASH.In(commitHashes),
			t.TAGGER.EqString(peerID),
		).
		SetDialect(sq.DialectMySQL),
		tagMapper,
	)
	if err != nil {
		return fmt.Errorf("failed to retrieve tags for peer %s: %w", peerID, err)
	}

	if len(tags) != len(commits) {
		db.log.Warnf("cannot find all signatures for peer %s", peerID)
	}

	for _, tag := range tags {
		err = db.signer.Verify(tag.Hash, tag.Name, tag.Message)
		if err != nil {
			return fmt.Errorf("failed to verify signature for commit %s: %w", tag.Hash, err)
		}
	}

	return nil
}

func (db *DB) Merge(peerID string) error {
	db.log.Infof("Merging from peer %s", peerID)

	txn, err := db.conn.BeginTx(context.Background(), nil)
	if err != nil {
		return fmt.Errorf("failed to start transaction: %w", err)
	}
	defer txn.Rollback()

	// retrieve mergeBase between peer branch and main
	var mergeBase string
	err = txn.QueryRow(fmt.Sprintf("SELECT DOLT_MERGE_BASE('%s/main', 'main');", peerID)).Scan(&mergeBase)
	if err != nil {
		return fmt.Errorf("failed to retrieve merge base for peer branch %s: %w", peerID, err)
	}

	tempPeerBranch := randSeq(6)
	tempMainBranch := randSeq(6)

	_, err = txn.Exec(fmt.Sprintf("USE %s;", db.name))
	if err != nil {
		return fmt.Errorf("failed to use db: %w", err)
	}

	// create temp peer branch from old commit (this is needed to avoid conflicts) which will be used for storing the peer changes
	_, err = txn.Exec(fmt.Sprintf("CALL DOLT_CHECKOUT('-b', '%s', '%s');", tempPeerBranch, mergeBase))
	if err != nil {
		return fmt.Errorf("failed to checkout branch for peer %s: %w", peerID, err)
	}
	defer func() {
		// delete temp branch
		_, err = db.ExecContext(context.TODO(), fmt.Sprintf("CALL DOLT_BRANCH('-d', '%s');", tempPeerBranch))
		if err != nil {
			db.log.Errorf("failed to delete temp branch: %v", err)
		}
	}()

	// merge peer main branch into temp peer branch
	_, err = txn.Exec(fmt.Sprintf("CALL DOLT_MERGE('%s/main');", peerID))
	if err != nil {
		return fmt.Errorf("failed to merge peer branch %s: %w", peerID, err)
	}

	// create temp main branch from main
	_, err = txn.Exec(fmt.Sprintf("CALL DOLT_BRANCH('-c', 'main', '%s');", tempMainBranch))
	if err != nil {
		return fmt.Errorf("failed to copy source branch: %w", err)
	}
	defer func() {
		// delete temp branch
		_, err = db.ExecContext(context.TODO(), fmt.Sprintf("CALL DOLT_BRANCH('-d', '%s');", tempMainBranch))
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
	var message string
	err = rows.Scan(&hash, &fastForwards, &conflicts, &message)
	if err != nil {
		return fmt.Errorf("failed to scan rows while merging branch for peer '%s': %w", peerID, err)
	}

	if conflicts > 0 {
		return fmt.Errorf("conflicts found while merging branch for peer '%s'", peerID)
	}

	if fastForwards > 0 {
		//
		// fast forward merge (no dolt commit needed)
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

func (db *DB) GetSqlDB() *sql.DB {
	return db.db
}

func (db *DB) Initialized() bool {
	return db.initialized
}
