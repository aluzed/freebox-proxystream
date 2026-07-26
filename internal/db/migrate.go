package db

import (
	"database/sql"
	"fmt"
)

// migration is an idempotent step applied after the base schema. The schema
// itself is all CREATE TABLE IF NOT EXISTS, which silently does nothing to an
// existing table, so a new column needs its own step.
type migration struct {
	name string
	// applied reports whether the step is already in place, so a step can be
	// skipped without relying on a version counter that a hand-edited
	// database would get wrong.
	applied func(*sql.DB) (bool, error)
	apply   string
}

var migrations = []migration{
	{
		name:    "media.probe_attempts",
		applied: hasColumn("media", "probe_attempts"),
		apply:   `ALTER TABLE media ADD COLUMN probe_attempts INTEGER NOT NULL DEFAULT 0`,
	},
	{
		name:    "media.next_attempt_at",
		applied: hasColumn("media", "next_attempt_at"),
		apply:   `ALTER TABLE media ADD COLUMN next_attempt_at DATETIME`,
	},
	{
		// Set when someone edits the title or synopsis by hand, so a refresh
		// that re-derives names from filenames never overwrites their words.
		name:    "media.user_edited",
		applied: hasColumn("media", "user_edited"),
		apply:   `ALTER TABLE media ADD COLUMN user_edited INTEGER NOT NULL DEFAULT 0`,
	},
	{
		// TMDB is gone; its negative cache has no meaning any more.
		name:    "drop tmdb_misses",
		applied: tableAbsent("tmdb_misses"),
		apply:   `DROP TABLE IF EXISTS tmdb_misses`,
	},
	{
		name:    "media.error_permanent",
		applied: hasColumn("media", "error_permanent"),
		apply:   `ALTER TABLE media ADD COLUMN error_permanent INTEGER NOT NULL DEFAULT 0`,
	},
}

func hasColumn(table, column string) func(*sql.DB) (bool, error) {
	return func(db *sql.DB) (bool, error) {
		rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
		if err != nil {
			return false, err
		}
		defer rows.Close()
		for rows.Next() {
			var cid int
			var name, typ string
			var notNull, pk int
			var dflt any
			if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
				return false, err
			}
			if name == column {
				return true, nil
			}
		}
		return false, rows.Err()
	}
}

func tableAbsent(table string) func(*sql.DB) (bool, error) {
	return func(db *sql.DB) (bool, error) {
		var n int
		err := db.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`,
			table).Scan(&n)
		return n == 0, err
	}
}

func migrate(db *sql.DB) error {
	for _, m := range migrations {
		done, err := m.applied(db)
		if err != nil {
			return fmt.Errorf("check migration %s: %w", m.name, err)
		}
		if done {
			continue
		}
		if _, err := db.Exec(m.apply); err != nil {
			return fmt.Errorf("apply migration %s: %w", m.name, err)
		}
	}
	return nil
}
