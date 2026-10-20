package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// migrationLockKey is the pg_advisory_xact_lock key that serialises
// concurrent migrate calls (for example two processes starting at once against
// an empty database). The value is arbitrary but must never change.
const migrationLockKey int64 = 0x6e6f74696e76 // "notinv"

// migrations holds Postgres DDL. Timestamps are BIGINT Unix milliseconds so
// they do not overflow in 2038, and each table has an identity primary key.
var migrations = []struct {
	version int
	sql     []string
}{
	{1, []string{
		`CREATE TABLE IF NOT EXISTS requests (
			id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			script TEXT NOT NULL,
			method TEXT NOT NULL,
			url TEXT NOT NULL,
			started_at BIGINT NOT NULL,
			duration_ms BIGINT NOT NULL,
			status_code INTEGER NOT NULL,
			error TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_requests_script_started ON requests(script, started_at)`,
		`CREATE TABLE IF NOT EXISTS runs (
			id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			script TEXT NOT NULL,
			started_at BIGINT NOT NULL,
			duration_ms BIGINT NOT NULL,
			ok BOOLEAN NOT NULL,
			error TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_runs_script_started ON runs(script, started_at)`,
	}},
}

// migrate applies outstanding migrations in one transaction. It first takes a
// transaction-scoped advisory lock, so concurrent callers run one after another
// and each version is applied exactly once.
func (s *Store) migrate(ctx context.Context) error {
	return inTransaction(ctx, s.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLockKey); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY, applied_at BIGINT NOT NULL
		)`); err != nil {
			return err
		}
		for _, migration := range migrations {
			var applied int
			err := tx.QueryRowContext(ctx, `SELECT version FROM schema_migrations WHERE version = $1`, migration.version).Scan(&applied)
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
			if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES ($1, $2)`,
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
