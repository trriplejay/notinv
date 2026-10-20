package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// CLM-1: An integration test that terminates the store's backends with
// pg_terminate_backend before InsertRun. The run row must still be written once,
// and a CHECK-constraint failure must be attempted exactly once. The test goes
// red when the !s.remote short-circuit is restored.
func TestRemoteOperationsRetryDriverFailures(t *testing.T) {
	dbURL := postgresTestURL(t)
	s := openPostgresStore(t, dbURL)
	ctx := t.Context()

	base := os.Getenv(testDatabaseURLEnv)
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })

	t.Run("retry terminated backend", func(t *testing.T) {
		// Warm up a connection in s.db so it has a backend pid.
		var pid int
		if err := s.db.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
			t.Fatalf("get backend pid: %v", err)
		}

		// Terminate that backend from admin connection.
		if _, err := admin.ExecContext(ctx, `SELECT pg_terminate_backend($1)`, pid); err != nil {
			t.Fatalf("terminate backend %d: %v", pid, err)
		}

		// Give Postgres a moment to terminate the process.
		time.Sleep(50 * time.Millisecond)

		var attempts int
		onAttempt = func(attempt int) {
			attempts++
		}
		defer func() { onAttempt = nil }()

		// InsertRun must retry through transient termination and write the row once.
		run := Run{
			Script:     "retry_script",
			StartedAt:  time.Now(),
			DurationMs: 120,
			OK:         true,
		}
		if err := s.InsertRun(ctx, run); err != nil {
			t.Fatalf("InsertRun failed after backend termination: %v", err)
		}

		if attempts != 2 {
			t.Errorf("attempts = %d, want 2", attempts)
		}

		var count int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM runs WHERE script = $1`, "retry_script").Scan(&count); err != nil {
			t.Fatalf("count runs: %v", err)
		}
		if count != 1 {
			t.Errorf("runs count = %d, want 1", count)
		}
	})

	t.Run("check constraint attempted once", func(t *testing.T) {
		// Add a CHECK constraint to runs table.
		if _, err := s.db.ExecContext(ctx, `ALTER TABLE runs ADD CONSTRAINT test_check_duration CHECK (duration_ms >= 0)`); err != nil {
			t.Fatalf("add check constraint: %v", err)
		}
		t.Cleanup(func() {
			_, _ = s.db.ExecContext(context.Background(), `ALTER TABLE runs DROP CONSTRAINT IF EXISTS test_check_duration`)
		})

		var attempts int
		onAttempt = func(attempt int) {
			attempts++
		}
		defer func() { onAttempt = nil }()

		run := Run{
			Script:     "check_script",
			StartedAt:  time.Now(),
			DurationMs: -50,
			OK:         false,
		}
		err := s.InsertRun(ctx, run)

		if err == nil {
			t.Fatal("InsertRun with invalid duration succeeded, want check constraint error")
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Errorf("expected 23514 check_violation, got %v", err)
		}
		// Attempted exactly once: asserted by count, not timing.
		if attempts != 1 {
			t.Errorf("check constraint failure attempts = %d, want exactly 1 without retry", attempts)
		}
	})
}

func TestRetryCancellationAndRecovery(t *testing.T) {
	s := &Store{}
	transient := errors.New("connection reset")
	t.Run("recovery", func(t *testing.T) {
		attempts := 0
		err := s.withRetry(t.Context(), func(ctx context.Context) error {
			attempts++
			if attempts < 3 {
				return transient
			}
			return nil
		})
		if err != nil || attempts != 3 {
			t.Fatalf("attempts=%d, err=%v", attempts, err)
		}
	})
	t.Run("last cause", func(t *testing.T) {
		attempts := 0
		last := errors.New("last connection reset")
		err := s.withRetry(t.Context(), func(ctx context.Context) error {
			attempts++
			if attempts == 4 {
				return last
			}
			return transient
		})
		if !errors.Is(err, last) || attempts != 4 {
			t.Fatalf("last cause lost: attempts=%d, err=%v", attempts, err)
		}
	})
	t.Run("cancel during backoff", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		attempts := 0
		err := s.withRetry(ctx, func(ctx context.Context) error {
			attempts++
			cancel()
			return transient
		})
		if !errors.Is(err, context.Canceled) || attempts != 1 {
			t.Fatalf("attempts=%d, err=%v", attempts, err)
		}
	})
	t.Run("already canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		attempts := 0
		err := s.withRetry(ctx, func(ctx context.Context) error {
			attempts++
			return nil
		})
		if !errors.Is(err, context.Canceled) || attempts != 0 {
			t.Fatalf("attempts=%d, err=%v", attempts, err)
		}
	})
	t.Run("per-attempt deadline exceeded not retried", func(t *testing.T) {
		orig := perCallTimeout
		perCallTimeout = 20 * time.Millisecond
		defer func() { perCallTimeout = orig }()

		attempts := 0
		err := s.withRetry(t.Context(), func(ctx context.Context) error {
			attempts++
			<-ctx.Done()
			return ctx.Err()
		})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected DeadlineExceeded, got %v", err)
		}
		if attempts != 1 {
			t.Fatalf("attempts = %d, want 1", attempts)
		}
	})
}

// CLM-1: A table-driven unit test of isTransient:
// pgconn.PgError class 08, 57P01, 40001 and 40P01, *pgconn.ConnectError, and
// ECONNREFUSED/ECONNRESET are true;
// 23505, 42601, 28P01, 3D000, context.Canceled and context.DeadlineExceeded are false.
func TestTransientClassification(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"generic constraint failed", errors.New("constraint failed"), false},
		{"UNAVAILABLE", errors.New("UNAVAILABLE"), true},
		{"connection refused string", errors.New("connection refused"), true},
		{"connection reset string", errors.New("connection reset"), true},
		{"timeout string", errors.New("timeout"), true},
		{"net.DNSError timeout", fmt.Errorf("wrapped: %w", &net.DNSError{IsTimeout: true}), true},
		{"timeout string with Canceled", fmt.Errorf("timeout: %w", context.Canceled), false},
		{"context.Canceled", context.Canceled, false},
		{"context.DeadlineExceeded", context.DeadlineExceeded, false},
		// pgconn.PgError transient classes/codes
		{"pgconn class 08000", &pgconn.PgError{Code: "08000"}, true},
		{"pgconn class 08006", &pgconn.PgError{Code: "08006"}, true},
		{"pgconn 57P01 admin shutdown", &pgconn.PgError{Code: "57P01"}, true},
		{"pgconn 57P03 cannot_connect_now", &pgconn.PgError{Code: "57P03"}, true},
		{"pgconn 40001 serialization failure", &pgconn.PgError{Code: "40001"}, true},
		{"pgconn 40P01 deadlock detected", &pgconn.PgError{Code: "40P01"}, true},
		// *pgconn.ConnectError
		{"pgconn.ConnectError", &pgconn.ConnectError{}, true},
		// ECONNREFUSED and ECONNRESET
		{"ECONNREFUSED", syscall.ECONNREFUSED, true},
		{"ECONNRESET", syscall.ECONNRESET, true},
		{"wrapped ECONNREFUSED", fmt.Errorf("connect: %w", syscall.ECONNREFUSED), true},
		{"wrapped ECONNRESET", fmt.Errorf("read: %w", syscall.ECONNRESET), true},
		// Permanent errors
		{"pgconn 23505 unique violation", &pgconn.PgError{Code: "23505"}, false},
		{"pgconn 42601 syntax error", &pgconn.PgError{Code: "42601"}, false},
		{"pgconn 28P01 invalid password", &pgconn.PgError{Code: "28P01"}, false},
		{"pgconn 28000 invalid authorization", &pgconn.PgError{Code: "28000"}, false},
		{"pgconn 3D000 invalid database", &pgconn.PgError{Code: "3D000"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTransient(tc.err); got != tc.want {
				t.Errorf("isTransient(%v)=%v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
