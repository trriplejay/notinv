package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// retentionBatchSize bounds each DELETE so remote (libSQL) transactions stay
// small; it is a compile-time constant, not configurable via the environment.
const retentionBatchSize = 500

// DeleteOlderThan removes every row in requests and runs whose started_at is
// strictly older than cutoff (measured in Unix milliseconds). Deletes run in
// bounded batches so no single transaction spans the whole backlog. On a local
// (file:) store it then issues VACUUM to reclaim space; on a remote (libsql:)
// store VACUUM is skipped — only the row deletion runs. Rows at or newer than
// cutoff are never removed.
func (s *Store) DeleteOlderThan(ctx context.Context, cutoff time.Time) error {
	cutoffMs := cutoff.UnixMilli()
	for _, statement := range []string{
		`DELETE FROM requests WHERE rowid IN (SELECT rowid FROM requests WHERE started_at < ? LIMIT ?)`,
		`DELETE FROM runs WHERE rowid IN (SELECT rowid FROM runs WHERE started_at < ? LIMIT ?)`,
	} {
		for {
			var deleted int64
			err := s.withRetry(ctx, func() error {
				result, err := s.db.ExecContext(ctx, statement, cutoffMs, retentionBatchSize)
				if err != nil {
					return err
				}
				deleted, err = result.RowsAffected()
				return err
			})
			if err != nil {
				return s.wrap("delete older than", err)
			}
			if deleted == 0 {
				break
			}
		}
	}
	if !s.remote {
		if _, err := s.db.ExecContext(ctx, "VACUUM"); err != nil {
			return s.wrap("delete older than", err)
		}
	}
	return nil
}

// QueryRequests returns a script's requests in ascending start-time order.
// Both bounds are inclusive and compared at Unix-millisecond precision.
func (s *Store) QueryRequests(ctx context.Context, script string, since, until time.Time) ([]Request, error) {
	var result []Request
	err := s.withRetry(ctx, func() (err error) {
		result = nil // Discard partial results before retrying the whole read.
		rows, err := s.db.QueryContext(ctx, `SELECT script, method, url, started_at, duration_ms, status_code, error
			FROM requests WHERE script = ? AND started_at >= ? AND started_at <= ? ORDER BY started_at`,
			script, since.UnixMilli(), until.UnixMilli())
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, rows.Close()) }()
		for rows.Next() {
			var r Request
			var started int64
			if err := rows.Scan(&r.Script, &r.Method, &r.URL, &started, &r.DurationMs, &r.StatusCode, &r.Err); err != nil {
				return err
			}
			r.StartedAt = time.UnixMilli(started)
			result = append(result, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, s.wrap("query requests", err)
	}
	return result, nil
}

// QueryRuns returns a script's runs in ascending start-time order.
// Both bounds are inclusive and compared at Unix-millisecond precision.
func (s *Store) QueryRuns(ctx context.Context, script string, since, until time.Time) ([]Run, error) {
	var result []Run
	err := s.withRetry(ctx, func() (err error) {
		result = nil
		rows, err := s.db.QueryContext(ctx, `SELECT script, started_at, duration_ms, ok, error
			FROM runs WHERE script = ? AND started_at >= ? AND started_at <= ? ORDER BY started_at`,
			script, since.UnixMilli(), until.UnixMilli())
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, rows.Close()) }()
		for rows.Next() {
			r, err := scanRun(rows)
			if err != nil {
				return err
			}
			result = append(result, *r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, s.wrap("query runs", err)
	}
	return result, nil
}

// LatestRun returns the run with the greatest started_at for script, or
// (nil, nil) if the script has no runs. Ties have no defined ordering.
func (s *Store) LatestRun(ctx context.Context, script string) (*Run, error) {
	var result *Run
	err := s.withRetry(ctx, func() error {
		var err error
		result, err = scanRun(s.db.QueryRowContext(ctx, `SELECT script, started_at, duration_ms, ok, error
			FROM runs WHERE script = ? ORDER BY started_at DESC LIMIT 1`, script))
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	})
	if err != nil {
		return nil, s.wrap("latest run", err)
	}
	return result, nil
}

func scanRun(row interface{ Scan(...any) error }) (*Run, error) {
	var r Run
	var started int64
	var ok int
	if err := row.Scan(&r.Script, &started, &r.DurationMs, &ok, &r.Err); err != nil {
		return nil, err
	}
	r.StartedAt = time.UnixMilli(started)
	r.OK = ok == 1
	return &r, nil
}

// ListScripts returns the sorted, distinct union of scripts in requests and runs.
func (s *Store) ListScripts(ctx context.Context) ([]string, error) {
	var result []string
	err := s.withRetry(ctx, func() (err error) {
		result = nil
		rows, err := s.db.QueryContext(ctx, `SELECT script FROM requests UNION SELECT script FROM runs ORDER BY script`)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, rows.Close()) }()
		for rows.Next() {
			var script string
			if err := rows.Scan(&script); err != nil {
				return err
			}
			result = append(result, script)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, s.wrap("list scripts", err)
	}
	return result, nil
}
