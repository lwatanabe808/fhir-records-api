// Package store opens the SQLite database and creates the tables.
package store

import (
	"database/sql"
	_ "embed"
	"fmt"
	"os"

	_ "modernc.org/sqlite" // pure Go SQLite driver, so the Docker build needs no C compiler
)

//go:embed schema.sql
var schemaSQL string

// Open deletes any old database file and creates a fresh one.
// The data file is the source of truth, so rebuilding on every start keeps
// results repeatable: the same file always gives the same tables.
func Open(path string) (*sql.DB, error) {
	if path != ":memory:" {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
				return nil, fmt.Errorf("remove old database: %w", err)
			}
		}
	}
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(0)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	if path == ":memory:" {
		// Each new connection to :memory: would be a separate, empty database.
		db.SetMaxOpenConns(1)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("create tables: %w", err)
	}
	return db, nil
}

// Nullable turns "" into SQL NULL, so clean tables never store empty strings.
func Nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
