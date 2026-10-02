package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// This test uses the actual C-backed libsql remote client and a real loopback
// HTTP server returning transport failures, not a substituted SQL driver.
// HTTP is the driver's unencrypted loopback transport for the libSQL protocol;
// production Open only accepts file: and libsql: URLs.
func TestRemoteOperationsRetryDriverFailures(t *testing.T) {
	operations := []struct {
		name string
		call func(context.Context, *Store) error
	}{
		{"insert requests", func(ctx context.Context, s *Store) error {
			return s.InsertRequests(ctx, []Request{{Script: "script", Method: "GET"}})
		}},
		{"insert run", func(ctx context.Context, s *Store) error {
			return s.InsertRun(ctx, Run{Script: "script"})
		}},
		{"query requests", func(ctx context.Context, s *Store) error {
			_, err := s.QueryRequests(ctx, "script", time.UnixMilli(0), time.UnixMilli(1000))
			return err
		}},
		{"query runs", func(ctx context.Context, s *Store) error {
			_, err := s.QueryRuns(ctx, "script", time.UnixMilli(0), time.UnixMilli(1000))
			return err
		}},
		{"latest run", func(ctx context.Context, s *Store) error {
			_, err := s.LatestRun(ctx, "script")
			return err
		}},
		{"list scripts", func(ctx context.Context, s *Store) error {
			_, err := s.ListScripts(ctx)
			return err
		}},
	}
	for _, operation := range operations {
		for _, tc := range []struct {
			name     string
			status   int
			attempts int
		}{
			{"transient", http.StatusServiceUnavailable, 4},
			{"permanent", http.StatusUnauthorized, 1},
		} {
			t.Run(operation.name+"/"+tc.name, func(t *testing.T) {
				const token = "integration-secret"
				var mu sync.Mutex
				var arrivals []time.Time
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					arrivals = append(arrivals, time.Now())
					mu.Unlock()
					if !strings.Contains(r.Header.Get("Authorization"), token) {
						t.Error("driver did not send the configured token")
					}
					// Echo a secret deliberately: driver error text must not escape.
					http.Error(w, http.StatusText(tc.status)+" "+token, tc.status)
				}))
				t.Cleanup(server.Close)
				db, err := sql.Open("libsql", server.URL+"?authToken="+url.QueryEscape(token))
				if err != nil {
					t.Fatal(err)
				}
				s := &Store{db: db, remote: true}
				t.Cleanup(func() {
					if err := s.Close(); err != nil {
						t.Error(err)
					}
				})
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				err = operation.call(ctx, s)
				if err == nil || strings.Contains(err.Error(), token) {
					t.Fatalf("expected safe driver error, got %v", err)
				}
				mu.Lock()
				times := append([]time.Time(nil), arrivals...)
				mu.Unlock()
				if len(times) != tc.attempts {
					t.Fatalf("driver requests = %d, want %d; error = %v", len(times), tc.attempts, err)
				}
				// Lower bounds avoid scheduler-sensitive upper-bound assertions.
				for i, minimum := range []time.Duration{50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond} {
					if i+1 < len(times) && times[i+1].Sub(times[i]) < minimum {
						t.Errorf("retry %d did not back off for %v", i+1, minimum)
					}
				}
			})
		}
	}
}

func TestLocalDoesNotRetryDriverFailure(t *testing.T) {
	s, _ := openTestStore(t)
	if s.remote {
		t.Fatal("file store marked remote")
	}
	// Use a real SQLite failure whose message matches the transient heuristic.
	if _, err := s.db.ExecContext(t.Context(), `CREATE TRIGGER fail_run BEFORE INSERT ON runs
		BEGIN SELECT RAISE(ABORT, 'connection reset'); END`); err != nil {
		t.Fatal(err)
	}
	attempts := 0
	err := s.withRetry(t.Context(), func() error {
		attempts++
		_, err := s.db.ExecContext(t.Context(), `INSERT INTO runs (script, started_at, duration_ms, ok) VALUES (?, ?, ?, ?)`, "script", 0, 0, 0)
		return err
	})
	if err == nil || !isTransient(err) || attempts != 1 {
		t.Fatalf("local attempts=%d, err=%v", attempts, err)
	}
}

func TestRetryCancellationAndRecovery(t *testing.T) {
	s := &Store{remote: true}
	transient := errors.New("connection reset")
	t.Run("recovery", func(t *testing.T) {
		attempts := 0
		err := s.withRetry(t.Context(), func() error {
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
		err := s.withRetry(t.Context(), func() error {
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
		err := s.withRetry(ctx, func() error {
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
		err := s.withRetry(ctx, func() error {
			attempts++
			return nil
		})
		if !errors.Is(err, context.Canceled) || attempts != 0 {
			t.Fatalf("attempts=%d, err=%v", attempts, err)
		}
	})
}

func TestTransientClassification(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("constraint failed"), false},
		{errors.New("UNAVAILABLE"), true},
		{errors.New("connection refused"), true},
		{errors.New("connection reset"), true},
		{errors.New("timeout"), true},
		{fmt.Errorf("wrapped: %w", &net.DNSError{IsTimeout: true}), true},
		{fmt.Errorf("timeout: %w", context.Canceled), false},
		{context.DeadlineExceeded, false},
	} {
		if got := isTransient(tc.err); got != tc.want {
			t.Errorf("isTransient(%v)=%v, want %v", tc.err, got, tc.want)
		}
	}
}
