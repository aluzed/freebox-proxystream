package db

import (
	"database/sql"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

func Open(path string) (*sql.DB, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}

	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	sdb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// Single user, and SQLite writers serialise anyway. Keeping the pool small
	// avoids spurious SQLITE_BUSY between the scanner and the HTTP handlers.
	sdb.SetMaxOpenConns(4)
	sdb.SetMaxIdleConns(4)

	if err := sdb.Ping(); err != nil {
		sdb.Close()
		return nil, err
	}
	if _, err := sdb.Exec(schema); err != nil {
		sdb.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := migrate(sdb); err != nil {
		sdb.Close()
		return nil, err
	}
	return sdb, nil
}
