package db

import (
	"errors"
	"os"

	"github.com/bokwoon95/sq"
)

func commitMapper(row *sq.Row) (Commit, error) {
	commit := Commit{
		Hash:         row.String("commit_hash"),
		Table:        row.String("table_name"),
		Committer:    row.String("committer"),
		Email:        row.String("email"),
		Date:         row.Time("date"),
		Message:      row.String("message"),
		DataChange:   row.Bool("data_change"),
		SchemaChange: row.Bool("schema_change"),
	}
	return commit, nil
}

func ensureDir(dirName string) error {
	err := os.Mkdir(dirName, os.ModePerm)
	if err == nil {
		return nil
	}
	if os.IsExist(err) {
		info, err := os.Stat(dirName)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return errors.New("path exists but is not a directory")
		}
		return nil
	}
	return err
}
