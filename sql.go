package doltswarm

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/bokwoon95/sq"
)

type Queryer interface {
	Query(query string, args ...interface{}) (*sql.Rows, error)
}

type TAG struct {
	sq.TableStruct `sq:"dolt_tags"`
	TAG_NAME       sq.StringField
	TAG_HASH       sq.StringField
	TAGGER         sq.StringField
	EMAIL          sq.StringField
	DATE           sq.TimeField
	MESSAGE        sq.StringField
}

type Tag struct {
	Name    string
	Hash    string
	Tagger  string
	Email   string
	Date    time.Time
	Message string
}

func tagMapper(row *sq.Row) Tag {
	tag := Tag{
		Name:    row.String("tag_name"),
		Hash:    row.String("tag_hash"),
		Tagger:  row.String("tagger"),
		Email:   row.String("email"),
		Date:    row.Time("date"),
		Message: row.String("message"),
	}
	return tag
}

type Commit struct {
	Hash      string
	Committer string
	Email     string
	Date      time.Time
	Message   string
}

type ExecFunc func(*sql.Tx) error

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

func doCommit(tx *sql.Tx, msg string, signer Signer) (string, error) {

	if signer == nil {
		return "", fmt.Errorf("no signer available")
	}

	// commit
	var commitHash string
	err := tx.QueryRow(fmt.Sprintf("CALL DOLT_COMMIT('-A', '-m', '%s', '--author', '%s <%s@doltswarm>', '--date', '%s');", msg, signer.GetID(), signer.GetID(), time.Now().Format(time.RFC3339Nano))).Scan(&commitHash)
	if err != nil {
		return "", fmt.Errorf("failed to run commit procedure: %w", err)
	}

	// create commit signature and add it to a tag
	signature, err := signer.Sign(commitHash)
	if err != nil {
		return "", fmt.Errorf("failed to sign commit '%s': %w", commitHash, err)
	}
	tagcmd := fmt.Sprintf("CALL DOLT_TAG('-m', '%s', '--author', '%s <%s@doltswarm>', '%s', '%s');", signer.PublicKey(), signer.GetID(), signer.GetID(), signature, commitHash)
	_, err = tx.Exec(tagcmd)
	if err != nil {
		return "", fmt.Errorf("failed to create signature tag (%s) : %w", signature, err)
	}

	return commitHash, nil
}

func getChangedTables(q Queryer, from string, to string) ([]string, error) {
	query := fmt.Sprintf("SELECT to_table_name FROM DOLT_DIFF_SUMMARY('%s', '%s')", from, to)
	rows, err := q.Query(query)
	if err != nil {
		return nil, fmt.Errorf("failed to query changed tables: %w", err)
	}
	defer rows.Close()

	var changedTables []string
	for rows.Next() {
		var tableName string
		err := rows.Scan(&tableName)
		if err != nil {
			return nil, fmt.Errorf("failed to scan table name: %w", err)
		}
		changedTables = append(changedTables, tableName)
	}

	return changedTables, nil
}

func (db *DB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return db.conn.ExecContext(ctx, query, args...)
}

func (db *DB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return db.conn.QueryContext(ctx, query, args...)
}

func (db *DB) PrepareContext(ctx context.Context, query string) (*sql.Stmt, error) {
	return db.conn.PrepareContext(ctx, query)
}

func (db *DB) BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
	return db.conn.BeginTx(ctx, opts)
}

// Commit
func (db *DB) Commit(commitMsg string) (string, error) {
	tx, err := db.BeginTx(context.TODO(), nil)
	if err != nil {
		return "", fmt.Errorf("failed to start transaction: %w", err)
	}
	defer tx.Rollback()

	commitHash, err := doCommit(tx, commitMsg, db.signer)
	if err != nil {
		return "", fmt.Errorf("failed to commit: %w", err)
	}

	// find out which tables changed
	changedTables, err := getChangedTables(tx, "HEAD^", "HEAD")
	if err != nil {
		return "", fmt.Errorf("failed to get changed tables: %w", err)
	}

	err = tx.Commit()
	if err != nil {
		return "", fmt.Errorf("failed to commit transaction: %w", err)
	}

	// trigger table change callbacks after successful commit
	for _, tableName := range changedTables {
		db.triggerTableChangeCallbacks(tableName)
	}

	// advertise new head
	db.AdvertiseHead()

	return commitHash, nil

}

func (db *DB) ExecAndCommit(execFunc ExecFunc, commitMsg string) (string, error) {
	tx, err := db.BeginTx(context.TODO(), nil)
	if err != nil {
		return "", fmt.Errorf("failed to start transaction: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.Exec("CALL DOLT_CHECKOUT('main');")
	if err != nil {
		return "", fmt.Errorf("failed to checkout main branch: %w", err)
	}

	// exec the sql func
	err = execFunc(tx)
	if err != nil {
		return "", fmt.Errorf("failed to run exec function: %w", err)
	}

	// find out which tables changed
	changedTables, err := getChangedTables(tx, "WORKING", "HEAD")
	if err != nil {
		return "", err
	}

	// dolt commit
	commitHash, err := doCommit(tx, commitMsg, db.signer)
	if err != nil {
		return "", fmt.Errorf("failed to commit: %w", err)
	}

	// finish sql transaction
	err = tx.Commit()
	if err != nil {
		return "", fmt.Errorf("failed to commit transaction: %w", err)
	}

	// trigger table change callbacks after successful commit
	for _, tableName := range changedTables {
		db.triggerTableChangeCallbacks(tableName)
	}

	// advertise new head
	db.AdvertiseHead()

	return commitHash, nil
}
