package core

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bokwoon95/sq"
	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/env"
	"github.com/dolthub/dolt/go/libraries/utils/concurrentmap"
	"github.com/dolthub/dolt/go/libraries/utils/filesys"
	"github.com/dolthub/dolt/go/store/chunks"
	"github.com/dolthub/dolt/go/store/datas"
	"github.com/rs/xid"
	"github.com/sirupsen/logrus"
	"go.uber.org/multierr"

	_ "github.com/dolthub/driver"
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

type Notifier interface {
	Notify()
}

type DB struct {
	name                 string
	initialized          atomic.Bool
	stoppers             *concurrentmap.Map[string, func() error]
	tableChangeCallbacks *concurrentmap.Map[string, Notifier]
	sqldb                *sql.DB
	sqlMu                sync.RWMutex // Protects sqldb during close/reopen operations
	workingDir           string
	log                  *logrus.Entry
	signer               Signer
	shutdownWg           sync.WaitGroup // Track active operations for graceful shutdown
	ctx                  context.Context
	cancel               context.CancelFunc
	reconciler           *Reconciler // HLC-based commit reconciler
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

	ctx, cancel := context.WithCancel(context.Background())

	db := &DB{
		name:                 name,
		workingDir:           workingDir,
		log:                  logger,
		stoppers:             concurrentmap.New[string, func() error](),
		tableChangeCallbacks: concurrentmap.New[string, Notifier](),
		signer:               signer,
		ctx:                  ctx,
		cancel:               cancel,
	}

	// Initialize reconciler
	db.reconciler = NewReconciler(db, signer.GetID(), logger)

	err = ensureDir(db.workingDir)
	if err != nil {
		return nil, fmt.Errorf("failed to open db: %w", err)
	}

	db.sqldb, err = openDB(db.workingDir, db.signer.GetID(), "")
	if err != nil {
		return nil, fmt.Errorf("failed to open db: %w", err)
	}

	foundDB, err := db.DatabaseExists(db.name)
	if err == nil && foundDB {

		db.sqlMu.Lock()
		err = db.sqldb.Close()
		if err != nil {
			db.sqlMu.Unlock()
			return nil, fmt.Errorf("failed to close db connection after init local: %w", err)
		}

		db.initialized.Store(true)

		// re-open the db connection with db name included
		db.sqldb, err = openDB(db.workingDir, db.signer.GetID(), db.name)
		db.sqlMu.Unlock()
		if err != nil {
			return nil, fmt.Errorf("failed to re-open db: %w", err)
		}

		_, err = db.sqldb.ExecContext(context.Background(), fmt.Sprintf("USE %s;", db.name))
		if err != nil {
			return nil, fmt.Errorf("failed to use db: %w", err)
		}
		_, err = db.sqldb.ExecContext(context.Background(), "CALL DOLT_REMOTE('remove','origin');")
		if err != nil && !strings.Contains(err.Error(), "unknown remote") {
			return nil, fmt.Errorf("failed to remove origin remote during init: %w", err)
		}
	}
	return db, nil
}

// re-open the db connection with the new db param
func openDB(workingDir, signerID, dbName string) (*sql.DB, error) {

	if workingDir == "" || signerID == "" {
		return nil, fmt.Errorf("workingDir and signerID cannot be empty")
	}

	dbConnString := fmt.Sprintf("file://%s?commitname=%s&commitemail=%s@doltswarm&multistatements=true", workingDir, signerID, signerID)
	if dbName != "" {
		dbConnString = fmt.Sprintf(dbConnString+"&database=%s", dbName)
	}

	sqldb, err := sql.Open("dolt", dbConnString)
	if err != nil {
		return nil, fmt.Errorf("failed to open db: %w", err)
	}

	if sqldb.PingContext(context.Background()) != nil {
		return nil, fmt.Errorf("failed to ping db: %w", err)
	}

	return sqldb, nil
}

func (db *DB) triggerTableChangeCallbacks(tableName string) {
	notifiers := db.tableChangeCallbacks.Snapshot()
	for table, n := range notifiers {
		if strings.Contains(table, tableName) {
			db.log.Debugf("triggering table change notification for table '%s'", tableName)
			go n.Notify()
		}
	}
}

func (db *DB) RegisterTableChangeCallback(tableName string, n Notifier) {
	guid := xid.New()
	table := tableName + "_" + guid.String()
	db.tableChangeCallbacks.Set(table, n)
}

func (db *DB) Close() error {
	db.log.Info("Starting graceful shutdown...")

	// Signal shutdown to all operations
	db.cancel()

	// Stop reconciler
	if db.reconciler != nil {
		db.reconciler.Stop()
	}

	// Stop all stoppers first (this includes the event processor)
	var finalerr error
	db.stoppers.Iter(func(key string, stopper func() error) bool {
		err := stopper()
		if err != nil {
			finalerr = multierr.Append(finalerr, err)
		}
		db.log.Infof("Stopped %s", key)
		return true
	})

	// Wait for in-flight operations to complete (with timeout)
	done := make(chan struct{})
	go func() {
		db.shutdownWg.Wait()
		close(done)
	}()

	select {
	case <-done:
		db.log.Info("All in-flight operations completed")
	case <-time.After(30 * time.Second):
		db.log.Warn("Shutdown timeout waiting for in-flight operations")
	}

	// Close SQL connection last
	db.sqlMu.Lock()
	if db.sqldb != nil {
		if err := db.sqldb.Close(); err != nil {
			finalerr = multierr.Append(finalerr, err)
		}
	}
	db.sqlMu.Unlock()

	db.log.Info("Shutdown complete")
	return finalerr
}

func (db *DB) GetFilePath() string {
	return db.workingDir + "/" + db.name
}

func (db *DB) GetChunkStore() (chunks.ChunkStore, error) {

	var mrEnv *env.MultiRepoEnv
	conn, err := db.sqldb.Conn(context.Background())
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve db connection: %w", err)
	}
	err = conn.Raw(func(driverConn any) error {
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

	doltDB := env.DoltDB(context.Background())
	dbd := doltdb.HackDatasDatabaseFromDoltDB(doltDB)
	return datas.ChunkStoreFromDatabase(dbd), nil
}

func (db *DB) InitLocal() error {
	db.log.Infof("Initializing local db %s", db.name)

	ctx := context.Background()

	sqlCmd := fmt.Sprintf("CREATE DATABASE %s;", db.name)
	_, err := db.sqldb.ExecContext(ctx, sqlCmd)
	if err != nil {
		return fmt.Errorf("failed to exec '%s': %w", sqlCmd, err)
	}

	_, err = db.sqldb.ExecContext(ctx, "commit;")
	if err != nil {
		return fmt.Errorf("failed to commit db creation: %w", err)
	}

	db.sqlMu.Lock()
	err = db.sqldb.Close()
	if err != nil {
		db.sqlMu.Unlock()
		return fmt.Errorf("failed to close db connection after init local: %w", err)
	}

	// re-open the db connection with db name included
	db.sqldb, err = openDB(db.workingDir, db.signer.GetID(), db.name)
	db.sqlMu.Unlock()
	if err != nil {
		return fmt.Errorf("failed to re-open db after local init: %w", err)
	}

	_, err = db.sqldb.ExecContext(context.Background(), fmt.Sprintf("USE %s;", db.name))
	if err != nil {
		return fmt.Errorf("failed to use db after init local: %w", err)
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
	_, err = db.sqldb.ExecContext(ctx, tagcmd)
	if err != nil {
		return fmt.Errorf("failed to create signature tag (%s) for init commit (%s): %w", signature, commit.Hash, err)
	}

	db.initialized.Store(true)

	return nil
}

func (db *DB) GetLastCommit(branch string) (Commit, error) {

	// NOTE: Prefer the dolt_log() table function over the legacy `db/branch`.dolt_log
	// schema form. The latter has shown intermittent "branch not found" errors in the
	// integration Docker environment.
	query := fmt.Sprintf("SELECT commit_hash, committer, email, date, message FROM dolt_log('%s') LIMIT 1;", escapeSQL(branch))
	commits, err := sq.FetchAll(db.sqldb, sq.
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

func (db *DB) DatabaseExists(name string) (bool, error) {
	dbName := ""
	err := db.sqldb.QueryRowContext(context.Background(), fmt.Sprintf("SHOW DATABASES LIKE '%s'", db.name)).Scan(&dbName)
	return "" != dbName, err
}

func (db *DB) GetFirstCommit() (Commit, error) {

	// Oldest commit on main (used to tag/sign the init commit on InitLocal()).
	query := "SELECT commit_hash, committer, email, date, message FROM dolt_log('main') ORDER BY date ASC LIMIT 1;"
	commits, err := sq.FetchAll(db.sqldb, sq.
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
	query := fmt.Sprintf("SELECT commit_hash, committer, email, date, message FROM dolt_log('main') WHERE commit_hash = '%s' LIMIT 1;", escapeSQL(commitHash))
	commit, err := sq.FetchOne(db.sqldb, sq.
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
	query := "SELECT commit_hash, committer, email, date, message FROM dolt_log('main');"
	commits, err := sq.FetchAll(db.sqldb, sq.
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
	return db.sqldb
}

// EnsureSwarmRemote ensures a `swarm` remote exists pointing at swarm://<repo>.
// This is idempotent and safe to call repeatedly.
func (db *DB) EnsureSwarmRemote(ctx context.Context, repo RepoID) error {
	if repo.RepoName == "" {
		return fmt.Errorf("repo name is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	url := fmt.Sprintf("%s://%s", SwarmDBFactoryScheme, repo.RepoName)
	if repo.Org != "" {
		url = fmt.Sprintf("%s://%s/%s", SwarmDBFactoryScheme, repo.Org, repo.RepoName)
	}

	// Fast path: if the remote already exists with the desired URL, do nothing.
	// This avoids churning dolt_remotes and triggering extra work under load.
	var cur sql.NullString
	qerr := db.sqldb.QueryRowContext(ctx, "SELECT url FROM dolt_remotes WHERE name = 'swarm' LIMIT 1;").Scan(&cur)
	if qerr == nil && cur.Valid && cur.String == url {
		return nil
	}

	// If the system table doesn't exist (older Dolt) or schema differs, fall back to remove+add.
	// Otherwise, only rewrite when missing/mismatched.
	needRewrite := false
	switch {
	case qerr == nil:
		needRewrite = !(cur.Valid && cur.String == url)
	case qerr == sql.ErrNoRows || strings.Contains(qerr.Error(), "no rows"):
		needRewrite = false
	case strings.Contains(strings.ToLower(qerr.Error()), "doesn't exist") ||
		strings.Contains(strings.ToLower(qerr.Error()), "unknown table") ||
		strings.Contains(strings.ToLower(qerr.Error()), "dolt_remotes"):
		needRewrite = true
	default:
		// Unknown error: be conservative and try rewriting.
		needRewrite = true
	}

	if needRewrite {
		_, err := db.sqldb.ExecContext(ctx, "CALL DOLT_REMOTE('remove','swarm');")
		if err != nil && !strings.Contains(err.Error(), "unknown remote") {
			return fmt.Errorf("failed to remove swarm remote: %w", err)
		}
	}

	_, err := db.sqldb.ExecContext(ctx, fmt.Sprintf("CALL DOLT_REMOTE('add','swarm','%s');", escapeSQL(url)))
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		return fmt.Errorf("failed to add swarm remote: %w", err)
	}
	return nil
}

// FetchSwarm performs a best-effort fetch from the `swarm` remote, updating remote refs and pulling objects.
func (db *DB) FetchSwarm(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	_, err := db.sqldb.ExecContext(ctx, "CALL DOLT_FETCH('swarm');")
	return err
}

// GetBranchHead resolves the head commit hash for the given ref name.
// It searches both local and remote branch tables.
func (db *DB) GetBranchHead(ctx context.Context, refName string) (hash string, ok bool, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var out sql.NullString
	err = db.sqldb.QueryRowContext(ctx, "SELECT hash FROM dolt_branches WHERE name = ? LIMIT 1;", refName).Scan(&out)
	if err == nil {
		return out.String, out.Valid && out.String != "", nil
	}
	if err != nil && !strings.Contains(err.Error(), "no rows") && err != sql.ErrNoRows {
		return "", false, err
	}

	out = sql.NullString{}
	err = db.sqldb.QueryRowContext(ctx, "SELECT hash FROM dolt_remote_branches WHERE name = ? LIMIT 1;", refName).Scan(&out)
	if err == nil {
		return out.String, out.Valid && out.String != "", nil
	}
	if err == sql.ErrNoRows || strings.Contains(err.Error(), "no rows") {
		return "", false, nil
	}
	return "", false, err
}

// MergeBase returns the merge base commit hash between two refs, or "" if none.
func (db *DB) MergeBase(ctx context.Context, leftRef string, rightRef string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var out sql.NullString
	if err := db.sqldb.QueryRowContext(ctx, "SELECT DOLT_MERGE_BASE(?, ?);", leftRef, rightRef).Scan(&out); err != nil {
		if err == sql.ErrNoRows || strings.Contains(err.Error(), "no rows") {
			return "", nil
		}
		return "", err
	}
	if !out.Valid {
		return "", nil
	}
	return out.String, nil
}

func (db *DB) Initialized() bool {
	return db.initialized.Load()
}

// Context returns the DB's context for use by operations that need cancellation support
func (db *DB) Context() context.Context {
	return db.ctx
}

// GetReconciler returns the reconciler
func (db *DB) GetReconciler() *Reconciler {
	return db.reconciler
}

// SetCommitRejectedCallback sets the callback for rejected commits
func (db *DB) SetCommitRejectedCallback(cb CommitRejectedCallback) {
	db.reconciler.SetCommitRejectedCallback(cb)
}
