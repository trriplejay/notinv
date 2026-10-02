package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func openTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dsn := "file:" + filepath.Join(t.TempDir(), "test.db")
	s, err := Open(t.Context(), dsn, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s, dsn
}

func TestRoundTripAndQueries(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := t.Context()
	if scripts, err := s.ListScripts(ctx); err != nil || len(scripts) != 0 {
		t.Fatalf("empty scripts = %v, err = %v", scripts, err)
	}
	failure := "request failed"
	empty := ""
	// Fixed, non-UTC input with sub-millisecond precision catches both unit
	// mistakes and accidental persistence of a time string instead of an integer.
	start := time.Date(2025, time.January, 2, 3, 4, 5, 123456789, time.FixedZone("test", 2*60*60))
	wantStart := time.Date(2025, time.January, 2, 1, 4, 5, 123000000, time.UTC)
	script := "script'; DROP TABLE runs; --"
	reqs := []Request{
		{Script: script, Method: "GET", URL: "https://example.com/one", StartedAt: start, DurationMs: 123, StatusCode: 200},
		{Script: script, Method: "POST", URL: "https://example.com/two", StartedAt: start.Add(time.Second), DurationMs: 456, StatusCode: 503, Err: &failure},
		{Script: script, Method: "HEAD", URL: "https://example.com/three", StartedAt: start.Add(2 * time.Second), DurationMs: 789, StatusCode: 204, Err: &empty},
		{Script: "request-only", Method: "GET", URL: "https://example.com", StartedAt: start},
	}
	if err := s.InsertRequests(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertRequests(ctx, reqs); err != nil {
		t.Fatal(err)
	}
	runs := []Run{
		{Script: script, StartedAt: start, DurationMs: 1001, OK: true},
		{Script: script, StartedAt: start.Add(time.Second), DurationMs: 1002, Err: &failure},
		{Script: script, StartedAt: start.Add(2 * time.Second), DurationMs: 1003, OK: true, Err: &empty},
		{Script: "run-only", StartedAt: start, DurationMs: 20, OK: true},
	}
	for _, run := range runs {
		if err := s.InsertRun(ctx, run); err != nil {
			t.Fatal(err)
		}
	}

	gotRequests, err := s.QueryRequests(ctx, script, wantStart, wantStart.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	wantRequests := append([]Request(nil), reqs[:3]...)
	for i := range wantRequests {
		wantRequests[i].StartedAt = wantStart.Add(time.Duration(i) * time.Second)
	}
	assertRequests(t, gotRequests, wantRequests)
	gotRuns, err := s.QueryRuns(ctx, script, wantStart, wantStart.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	wantRuns := append([]Run(nil), runs[:3]...)
	for i := range wantRuns {
		wantRuns[i].StartedAt = wantStart.Add(time.Duration(i) * time.Second)
	}
	assertRuns(t, gotRuns, wantRuns)

	// Equality at each endpoint is included; adjacent out-of-window rows and
	// other scripts are excluded. Millis conversion must happen on the bounds.
	middle := wantStart.Add(time.Second)
	windowRequests, err := s.QueryRequests(ctx, script, middle, middle)
	if err != nil {
		t.Fatal(err)
	}
	assertRequests(t, windowRequests, wantRequests[1:2])
	windowRuns, err := s.QueryRuns(ctx, script, middle, middle)
	if err != nil {
		t.Fatal(err)
	}
	assertRuns(t, windowRuns, wantRuns[1:2])
	latest, err := s.LatestRun(ctx, script)
	if err != nil || latest == nil {
		t.Fatalf("latest = %v, err = %v", latest, err)
	}
	assertRuns(t, []Run{*latest}, wantRuns[2:3])
	if missing, err := s.LatestRun(ctx, "missing"); err != nil || missing != nil {
		t.Fatalf("missing latest = %v, err = %v", missing, err)
	}
	if missing, err := s.QueryRequests(ctx, "missing", wantStart, middle); err != nil || len(missing) != 0 {
		t.Fatalf("missing requests = %v, err = %v", missing, err)
	}
	if missing, err := s.QueryRuns(ctx, script, middle, wantStart); err != nil || len(missing) != 0 {
		t.Fatalf("reversed window = %v, err = %v", missing, err)
	}
	scripts, err := s.ListScripts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"request-only", "run-only", script}; !reflect.DeepEqual(scripts, want) {
		t.Fatalf("scripts = %v, want %v", scripts, want)
	}

	var started int64
	var storedType string
	if err := s.db.QueryRowContext(ctx, `SELECT started_at, typeof(started_at) FROM requests WHERE script = ? ORDER BY started_at LIMIT 1`, script).Scan(&started, &storedType); err != nil {
		t.Fatal(err)
	}
	if started != 1735779845123 || storedType != "integer" {
		t.Fatalf("persisted request start = %d (%s)", started, storedType)
	}
	var ok int
	if err := s.db.QueryRowContext(ctx, `SELECT started_at, typeof(started_at), ok FROM runs WHERE script = ? ORDER BY started_at LIMIT 1`, script).Scan(&started, &storedType, &ok); err != nil {
		t.Fatal(err)
	}
	if started != 1735779845123 || storedType != "integer" || ok != 1 {
		t.Fatalf("persisted run = %d (%s), ok = %d", started, storedType, ok)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT ok FROM runs WHERE script = ? AND started_at = ?`, script, int64(1735779846123)).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	if ok != 0 {
		t.Fatalf("false persisted as %d", ok)
	}
}

func assertRequests(t *testing.T, got, want []Request) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("requests = %+v, want %+v", got, want)
	}
	for i, r := range got {
		w := want[i]
		if r.Script != w.Script || r.Method != w.Method || r.URL != w.URL ||
			!r.StartedAt.Equal(w.StartedAt) || r.DurationMs != w.DurationMs ||
			r.StatusCode != w.StatusCode || !reflect.DeepEqual(r.Err, w.Err) {
			t.Fatalf("request %d = %+v, want %+v", i, r, w)
		}
	}
}

func assertRuns(t *testing.T, got, want []Run) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("runs = %+v, want %+v", got, want)
	}
	for i, r := range got {
		w := want[i]
		if r.Script != w.Script || !r.StartedAt.Equal(w.StartedAt) ||
			r.DurationMs != w.DurationMs || r.OK != w.OK || !reflect.DeepEqual(r.Err, w.Err) {
			t.Fatalf("run %d = %+v, want %+v", i, r, w)
		}
	}
}

func TestMigrationIdempotency(t *testing.T) {
	s, dsn := openTestStore(t)
	ctx := t.Context()
	// A sentinel applied_at makes re-recording detectable without a clock race.
	if _, err := s.db.ExecContext(ctx, `UPDATE schema_migrations SET applied_at = ?`, 123); err != nil {
		t.Fatal(err)
	}
	var schemaVersion int
	if err := s.db.QueryRowContext(ctx, `PRAGMA schema_version`).Scan(&schemaVersion); err != nil {
		t.Fatal(err)
	}
	if err := s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	second, err := Open(ctx, dsn, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := second.Close(); err != nil {
			t.Error(err)
		}
	}()
	var after, count, version, applied int
	if err := second.db.QueryRowContext(ctx, `PRAGMA schema_version`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if err := second.db.QueryRowContext(ctx, `SELECT count(*), min(version), min(applied_at) FROM schema_migrations`).Scan(&count, &version, &applied); err != nil {
		t.Fatal(err)
	}
	if after != schemaVersion || count != 1 || version != 1 || applied != 123 {
		t.Fatalf("migration changed: schema %d -> %d, count=%d version=%d applied=%d", schemaVersion, after, count, version, applied)
	}
}

func TestBatchRollback(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := t.Context()
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER reject_request BEFORE INSERT ON requests
		WHEN NEW.method = 'FAIL' BEGIN SELECT RAISE(ABORT, 'rejected'); END`); err != nil {
		t.Fatal(err)
	}
	err := s.InsertRequests(ctx, []Request{{Script: "batch", Method: "GET"}, {Script: "batch", Method: "FAIL"}})
	if err == nil {
		t.Fatal("expected second insert to fail")
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM requests`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("partial batch committed: %d rows", count)
	}
}

func TestOpenFailure(t *testing.T) {
	const token = "private-token-should-not-appear"
	cases := []struct {
		name   string
		dsn    string
		scheme string
	}{
		{"missing parent", "file:" + filepath.Join(t.TempDir(), token, "missing", "test.db"), "file"},
		{"invalid URL", "libsql://example.invalid/%" + token, "unknown"},
		{"unsupported scheme", token + "://example.invalid", "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := Open(t.Context(), tc.dsn, token)
			if s != nil {
				if closeErr := s.Close(); closeErr != nil {
					t.Error(closeErr)
				}
				t.Fatal("failed open returned a store")
			}
			if err == nil || errors.Unwrap(err) == nil {
				t.Fatalf("expected wrapped error, got %v", err)
			}
			if !strings.Contains(err.Error(), tc.scheme) || strings.Contains(fmt.Sprintf("%+v", err), token) {
				t.Fatalf("unsafe or context-free error: %v", err)
			}
		})
	}
}

func TestCanceledOpen(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	s, err := Open(ctx, "file:"+filepath.Join(t.TempDir(), "test.db"), "")
	if s != nil {
		if closeErr := s.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal("canceled open returned a store")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation cause lost: %v", err)
	}
}

func TestMigrationFailureRollsBack(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("libsql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	// Conflict with the last table creation: earlier requests DDL must roll back.
	if _, err := db.ExecContext(t.Context(), `CREATE TABLE runs (existing TEXT)`); err != nil {
		t.Fatal(err)
	}
	s, err := Open(t.Context(), dsn, "")
	if s != nil {
		if closeErr := s.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal("migration failure returned a store")
	}
	if err == nil || errors.Unwrap(err) == nil || !strings.Contains(err.Error(), "migrate") {
		t.Fatalf("expected wrapped migration error: %v", err)
	}
	var count int
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM sqlite_master WHERE name IN ('requests', 'schema_migrations')`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("failed migration left %d schema objects", count)
	}
}

func TestDatabaseDSN(t *testing.T) {
	const token = "secret +&/?=" //nolint:gosec // G101 false positive: fake auth token test fixture, not a real credential
	local := "file:./test.db?mode=rwc"
	if dsn, scheme, err := databaseDSN(local, token); err != nil || dsn != local || scheme != "file" {
		t.Fatalf("local DSN = %q, %q, %v", dsn, scheme, err)
	}
	dsn, scheme, err := databaseDSN("libsql://example.invalid?other=value&authToken=old", token)
	if err != nil || scheme != "libsql" {
		t.Fatalf("remote scheme = %q, err = %v", scheme, err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if u.Query().Get("authToken") != token || u.Query().Get("other") != "value" || len(u.Query()["authToken"]) != 1 {
		t.Fatal("auth token not encoded or existing query not preserved")
	}
	if dsn, _, err := databaseDSN("libsql://example.invalid?other=value", ""); err != nil || dsn != "libsql://example.invalid?other=value" {
		t.Fatal("empty token changed DSN")
	}
}

func TestPrivateErrorsPreserveCause(t *testing.T) {
	cause := &url.Error{Op: "connect", URL: "libsql://host?authToken=private", Err: context.DeadlineExceeded}
	err := databaseError("query", "libsql", cause)
	if strings.Contains(err.Error(), "private") || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("redaction or cause preservation failed: %v", err)
	}
	var urlErr *url.Error
	if !errors.As(err, &urlErr) || urlErr != cause {
		t.Fatal("typed cause lost")
	}
}
