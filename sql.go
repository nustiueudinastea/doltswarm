package doltswarm

import (
	"fmt"
	"time"

	"github.com/bokwoon95/sq"
	"github.com/segmentio/ksuid"
)

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
	err = tx.QueryRow(fmt.Sprintf("CALL DOLT_COMMIT('-a', '-m', '%s', '--author', '%s <%s@%s>', '--date', '%s');", commitMsg, db.signer.GetID(), db.signer.GetID(), db.domain, time.Now().Format(time.RFC3339Nano))).Scan(&commitHash)
	if err != nil {
		return "", fmt.Errorf("failed to commit table: %w", err)
	}

	if db.signer == nil {
		return "", fmt.Errorf("no signer available")
	}
	// create commit signature and add it to a tag
	signature, err := db.signer.Sign(commitHash)
	if err != nil {
		return "", fmt.Errorf("failed to sign commit '%s': %w", commitHash, err)
	}
	tagcmd := fmt.Sprintf("CALL DOLT_TAG('-m', '%s', '--author', '%s <%s@%s>', '%s', '%s');", db.signer.PublicKey(), db.signer.GetID(), db.signer.GetID(), db.domain, signature, commitHash)
	_, err = tx.Exec(tagcmd)
	if err != nil {
		return "", fmt.Errorf("failed to create signature tag (%s) : %w", signature, err)
	}

	err = tx.Commit()
	if err != nil {
		return "", fmt.Errorf("failed to commit transaction: %w", err)
	}

	// advertise new head
	db.AdvertiseHead()

	return commitHash, nil
}

func (db *DB) Insert(table string, data string) error {

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("failed to start transaction: %w", err)
	}

	defer tx.Rollback()

	_, err = tx.Exec("CALL DOLT_CHECKOUT('main');")
	if err != nil {
		return fmt.Errorf("failed to checkout main branch: %w", err)
	}

	uid, err := ksuid.NewRandom()
	if err != nil {
		return fmt.Errorf("failed to create uid: %w", err)
	}
	queryString := fmt.Sprintf("INSERT INTO %s (id, name) VALUES ('%s', '%s');", table, uid.String(), data)
	_, err = tx.Exec(queryString)
	if err != nil {
		return fmt.Errorf("failed to save record: %w", err)
	}

	// commit
	var commitHash string
	err = tx.QueryRow(fmt.Sprintf("CALL DOLT_COMMIT('-a', '-m', '%s', '--author', '%s <%s@%s>', '--date', '%s');", data, db.signer.GetID(), db.signer.GetID(), db.domain, time.Now().Format(time.RFC3339Nano))).Scan(&commitHash)
	if err != nil {
		return fmt.Errorf("failed to commit: %w", err)
	}

	if db.signer == nil {
		return fmt.Errorf("no signer available")
	}
	// create commit signature and add it to a tag
	signature, err := db.signer.Sign(commitHash)
	if err != nil {
		return fmt.Errorf("failed to sign commit '%s': %w", commitHash, err)
	}
	tagcmd := fmt.Sprintf("CALL DOLT_TAG('-m', '%s', '--author', '%s <%s@%s>', '%s', '%s');", db.signer.PublicKey(), db.signer.GetID(), db.signer.GetID(), db.domain, signature, commitHash)
	_, err = tx.Exec(tagcmd)
	if err != nil {
		return fmt.Errorf("failed to create signature tag (%s) : %w", signature, err)
	}

	err = tx.Commit()
	if err != nil {
		return fmt.Errorf("failed to commit insert transaction: %w", err)
	}

	// advertise new head
	db.AdvertiseHead()

	return nil
}
