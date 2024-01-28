package doltswarm

import (
	"fmt"
	"time"

	"github.com/segmentio/ksuid"
)

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
	_, err = tx.Exec(fmt.Sprintf("CALL DOLT_COMMIT('-a', '-m', '%s', '--author', 'Alex Giurgiu <alex@giurgiu.io>', '--date', '%s');", data, time.Now().Format(time.RFC3339Nano)))
	if err != nil {
		return fmt.Errorf("failed to commit table: %w", err)
	}

	err = tx.Commit()
	if err != nil {
		return fmt.Errorf("failed to commit insert transaction: %w", err)
	}

	// advertise new head
	db.AdvertiseHead()

	return nil
}
