package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"time"

	// Register the pgx database/sql driver ("pgx") for postgres:// URLs.
	_ "github.com/jackc/pgx/v5/stdlib"
)

// RandomKey returns a cryptographically random 16-byte hex record key.
func RandomKey() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// Store persists check results in a Postgres database.
type Store struct {
	db *sql.DB
	// callTimeout overrides defaultCallTimeout when non-zero; tests shorten it.
	callTimeout time.Duration
	// onAttempt, when set, is called before each withRetry attempt; tests count attempts.
	onAttempt func(attempt int)
}

// Request records the result of one HTTP request. Err is nil on success.
type Request struct {
	Key        string
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
	Key        string
	Script     string
	StartedAt  time.Time
	DurationMs int64
	OK         bool
	Err        *string
}

var (
	openWaitBudget     = 60 * time.Second
	openInitialBackoff = 50 * time.Millisecond
	openMaxBackoff     = time.Second
	openPingTimeout    = 3 * time.Second
	// openMigrateTimeout bounds migrate inside Open, including the wait for
	// the migration advisory lock, so a database that accepts the connection
	// but stalls cannot hang startup.
	openMigrateTimeout = 30 * time.Second
)

// Open opens and checks the database, then applies any outstanding migrations.
// postgres: and postgresql: URLs use the pgx driver. Credentials are never
// printed in returned errors; the original cause remains available via Unwrap.
func Open(ctx context.Context, databaseURL string) (*Store, error) {
	dsn, scheme, err := databaseDSN(databaseURL)
	if err != nil {
		return nil, databaseError("open", scheme, err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		if db != nil {
			err = errors.Join(err, db.Close())
		}
		return nil, databaseError("open", scheme, err)
	}
	s := &Store{db: db}

	start := time.Now()
	delay := openInitialBackoff
	for {
		if err := ctx.Err(); err != nil {
			_ = db.Close()
			return nil, databaseError("ping", scheme, err)
		}

		pingCtx, pingCancel := context.WithTimeout(ctx, openPingTimeout)
		err := db.PingContext(pingCtx)
		attemptTimedOut := errors.Is(pingCtx.Err(), context.DeadlineExceeded)
		pingCancel()
		if err == nil {
			break
		}

		if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			_ = db.Close()
			return nil, databaseError("ping", scheme, context.Canceled)
		}

		// A ping attempt that hit its own deadline while the caller's context
		// is still live means the server accepted the connection but did not
		// answer: keep waiting within the budget. This applies to Open's wait
		// only; ordinary Store calls still treat a deadline as final.
		stalled := ctx.Err() == nil && (attemptTimedOut || errors.Is(err, context.DeadlineExceeded))

		// Permanent failure or non-transient error: fail immediately on first attempt.
		if !stalled && !isTransient(err) {
			_ = db.Close()
			return nil, databaseError("ping", scheme, err)
		}

		// Check if wait budget expired
		if time.Since(start) >= openWaitBudget {
			_ = db.Close()
			return nil, fmt.Errorf("database could not be reached within the wait (%v): %w", openWaitBudget, databaseError("ping", scheme, err))
		}

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = db.Close()
			return nil, databaseError("ping", scheme, ctx.Err())
		case <-timer.C:
		}
		delay = min(delay*2, openMaxBackoff)
	}

	migrateCtx, migrateCancel := context.WithTimeout(ctx, openMigrateTimeout)
	err = s.migrate(migrateCtx)
	if err != nil && errors.Is(migrateCtx.Err(), context.DeadlineExceeded) && !errors.Is(err, context.DeadlineExceeded) {
		err = fmt.Errorf("%w: %w", context.DeadlineExceeded, err)
	}
	migrateCancel()
	if err != nil {
		return nil, databaseError("migrate", scheme, errors.Join(err, db.Close()))
	}
	return s, nil
}

func databaseDSN(databaseURL string) (string, string, error) {
	u, err := url.Parse(databaseURL)
	if err != nil {
		return "", "unknown", err
	}
	switch u.Scheme {
	case "postgres", "postgresql":
		// Postgres carries credentials in the URL itself.
		return databaseURL, "postgres", nil
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
	return databaseError(operation, "postgres", err)
}

// InsertRequests inserts the entire batch in one transaction. An empty batch
// is a no-op. Each record has its key filled if empty before withRetry, and
// rows are inserted with ON CONFLICT (key) DO NOTHING.
func (s *Store) InsertRequests(ctx context.Context, reqs []Request) error {
	if len(reqs) == 0 {
		return nil
	}
	for i := range reqs {
		if reqs[i].Key == "" {
			reqs[i].Key = RandomKey()
		}
	}
	err := s.withRetry(ctx, func(ctx context.Context) error {
		return inTransaction(ctx, s.db, func(tx *sql.Tx) (err error) {
			stmt, err := tx.PrepareContext(ctx, `INSERT INTO requests
				(key, script, method, url, started_at, duration_ms, status_code, error)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
				ON CONFLICT (key) DO NOTHING`)
			if err != nil {
				return err
			}
			defer func() { err = errors.Join(err, stmt.Close()) }()
			for _, r := range reqs {
				if _, err := stmt.ExecContext(ctx, r.Key, r.Script, r.Method, r.URL,
					r.StartedAt.UnixMilli(), r.DurationMs, r.StatusCode, r.Err); err != nil {
					return err
				}
			}
			return nil
		})
	})
	return s.wrap("insert requests", err)
}

// InsertRun inserts one run, storing OK as a boolean and StartedAt as 64-bit
// Unix millis. An empty key is filled before withRetry, and rows are inserted
// with ON CONFLICT (key) DO NOTHING.
func (s *Store) InsertRun(ctx context.Context, run Run) error {
	if run.Key == "" {
		run.Key = RandomKey()
	}
	err := s.withRetry(ctx, func(ctx context.Context) error {
		_, err := s.db.ExecContext(ctx, `INSERT INTO runs
			(key, script, started_at, duration_ms, ok, error) VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (key) DO NOTHING`,
			run.Key, run.Script, run.StartedAt.UnixMilli(), run.DurationMs, run.OK, run.Err)
		return err
	})
	return s.wrap("insert run", err)
}
