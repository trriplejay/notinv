package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

var migrations = []struct {
	version int
	sql     []string
}{
	{1, []string{
		`CREATE TABLE requests (
			script TEXT NOT NULL,
			method TEXT NOT NULL,
			url TEXT NOT NULL,
			started_at INTEGER NOT NULL,
			duration_ms INTEGER NOT NULL,
			status_code INTEGER NOT NULL,
			error TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_requests_script_started ON requests(script, started_at)`,
		`CREATE TABLE runs (
			script TEXT NOT NULL,
			started_at INTEGER NOT NULL,
			duration_ms INTEGER NOT NULL,
			ok INTEGER NOT NULL CHECK (ok IN (0, 1)),
			error TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_runs_script_started ON runs(script, started_at)`,
	}},
}

func (s *Store) migrate(ctx context.Context) error {
	return inTransaction(ctx, s.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL
		)`); err != nil {
			return err
		}
		for _, migration := range migrations {
			var applied int
			err := tx.QueryRowContext(ctx, `SELECT version FROM schema_migrations WHERE version = ?`, migration.version).Scan(&applied)
			if err == nil {
				continue
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			for _, statement := range migration.sql {
				if _, err := tx.ExecContext(ctx, statement); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
				migration.version, time.Now().UnixMilli()); err != nil {
				return err
			}
		}
		return nil
	})
}

func inTransaction(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) (err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, rollbackErr)
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
