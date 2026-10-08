package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"time"

	// Register the same driver for local files and remote libSQL databases.
	_ "github.com/tursodatabase/go-libsql"
)

// Store persists check results in a local or remote libSQL database.
type Store struct {
	db     *sql.DB
	remote bool
}

// Request records the result of one HTTP request. Err is nil on success.
type Request struct {
	Script     string
	Method     string
	URL        string
	StartedAt  time.Time
	DurationMs int64
	StatusCode int
	Err        *string
}

// Run records the result of one script execution. Err may be nil.
type Run struct {
	Script     string
	StartedAt  time.Time
	DurationMs int64
	OK         bool
	Err        *string
}

// Open opens and checks the database, then applies any outstanding migrations.
// Both file: and libsql: URLs use the libsql driver. Credentials are never
// printed in returned errors; the original cause remains available via Unwrap.
func Open(ctx context.Context, databaseURL, authToken string) (*Store, error) {
	dsn, scheme, err := databaseDSN(databaseURL, authToken)
	if err != nil {
		return nil, databaseError("open", scheme, err)
	}
	db, err := sql.Open("libsql", dsn)
	if err != nil {
		if db != nil {
			err = errors.Join(err, db.Close())
		}
		return nil, databaseError("open", scheme, err)
	}
	s := &Store{db: db, remote: scheme == "libsql"}
	if !s.remote {
		// SQLite allows one writer at a time, and the driver does not wait on a
		// held lock, so concurrent writers (script runs and the retention
		// sweep) fail with "database is locked". A single connection serializes
		// access in-process instead. No store method nests queries, so this
		// cannot deadlock.
		db.SetMaxOpenConns(1)
	}
	if err := db.PingContext(ctx); err != nil {
		return nil, databaseError("ping", scheme, errors.Join(err, db.Close()))
	}
	if err := s.migrate(ctx); err != nil {
		return nil, databaseError("migrate", scheme, errors.Join(err, db.Close()))
	}
	return s, nil
}

func databaseDSN(databaseURL, authToken string) (string, string, error) {
	u, err := url.Parse(databaseURL)
	if err != nil {
		return "", "unknown", err
	}
	switch u.Scheme {
	case "file":
		return databaseURL, u.Scheme, nil
	case "libsql":
		if authToken != "" {
			q := u.Query()
			q.Set("authToken", authToken)
			u.RawQuery = q.Encode()
		}
		return u.String(), u.Scheme, nil
	default:
		return "", "unknown", errors.New("unsupported database scheme")
	}
}

// Ping checks database reachability using the caller's context.
func (s *Store) Ping(ctx context.Context) error {
	return s.wrap("ping", s.db.PingContext(ctx))
}

// Close releases the database handle.
func (s *Store) Close() error {
	return s.wrap("close", s.db.Close())
}

// privateCause intentionally hides all driver text, not just known token
// spellings: URLs and server messages may include escaped or echoed secrets.
// Unwrap preserves errors.Is/errors.As without rendering the unsafe text.
type privateCause struct {
	cause error
}

func (e privateCause) Error() string {
	return "database failure (details redacted)"
}

func (e privateCause) Unwrap() error {
	return e.cause
}

func databaseError(operation, scheme string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s store (scheme %q): %w", operation, scheme, privateCause{cause: err})
}

func (s *Store) wrap(operation string, err error) error {
	scheme := "file"
	if s.remote {
		scheme = "libsql"
	}
	return databaseError(operation, scheme, err)
}

// InsertRequests inserts the entire batch in one transaction. An empty batch
// is a no-op. Remote retries replay the batch; an ambiguous commit response can
// therefore duplicate records (the schema does not contain idempotency keys).
func (s *Store) InsertRequests(ctx context.Context, reqs []Request) error {
	if len(reqs) == 0 {
		return nil
	}
	err := s.withRetry(ctx, func() error {
		return inTransaction(ctx, s.db, func(tx *sql.Tx) (err error) {
			stmt, err := tx.PrepareContext(ctx, `INSERT INTO requests
				(script, method, url, started_at, duration_ms, status_code, error)
				VALUES (?, ?, ?, ?, ?, ?, ?)`)
			if err != nil {
				return err
			}
			defer func() { err = errors.Join(err, stmt.Close()) }()
			for _, r := range reqs {
				if _, err := stmt.ExecContext(ctx, r.Script, r.Method, r.URL,
					r.StartedAt.UnixMilli(), r.DurationMs, r.StatusCode, r.Err); err != nil {
					return err
				}
			}
			return nil
		})
	})
	return s.wrap("insert requests", err)
}

// InsertRun inserts one run, storing OK as 0 or 1 and StartedAt as Unix millis.
// A remote retry after an ambiguous response can duplicate the record.
func (s *Store) InsertRun(ctx context.Context, run Run) error {
	ok := 0
	if run.OK {
		ok = 1
	}
	err := s.withRetry(ctx, func() error {
		_, err := s.db.ExecContext(ctx, `INSERT INTO runs
			(script, started_at, duration_ms, ok, error) VALUES (?, ?, ?, ?, ?)`,
			run.Script, run.StartedAt.UnixMilli(), run.DurationMs, ok, run.Err)
		return err
	})
	return s.wrap("insert run", err)
}
