package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/trriplejay/notinv/internal/storetest"
)

func openTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dsn := storetest.DatabaseURL(t)
	s, err := Open(t.Context(), dsn)
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

func TestDeleteOlderThan(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := t.Context()
	cutoff := time.Date(2025, time.January, 2, 3, 4, 5, 123000000, time.UTC)
	starts := []time.Time{
		cutoff.Add(-48 * time.Hour),
		cutoff.Add(-time.Millisecond),
		cutoff,
		cutoff.Add(time.Millisecond),
		cutoff.Add(48 * time.Hour),
	}
	var requests []Request
	for _, start := range starts {
		requests = append(requests, Request{Script: "retention", Method: "GET", URL: "https://example.com", StartedAt: start})
		if err := s.InsertRun(ctx, Run{Script: "retention", StartedAt: start, OK: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.InsertRequests(ctx, requests); err != nil {
		t.Fatal(err)
	}
	// A second call also exercises an already-drained store.
	for range 2 {
		if err := s.DeleteOlderThan(ctx, cutoff); err != nil {
			t.Fatal(err)
		}
		for _, query := range []struct {
			statement string
			want      int
		}{
			{`SELECT count(*) FROM requests WHERE started_at < $1`, 0},
			{`SELECT count(*) FROM runs WHERE started_at < $1`, 0},
			{`SELECT count(*) FROM requests WHERE started_at = $1`, 1},
			{`SELECT count(*) FROM runs WHERE started_at = $1`, 1},
			{`SELECT count(*) FROM requests WHERE started_at > $1`, 2},
			{`SELECT count(*) FROM runs WHERE started_at > $1`, 2},
		} {
			var count int
			if err := s.db.QueryRowContext(ctx, query.statement, cutoff.UnixMilli()).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != query.want {
				t.Errorf("%s: count = %d, want %d", query.statement, count, query.want)
			}
		}
	}
}

func TestDeleteOlderThanMultipleBatches(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := t.Context()
	cutoff := time.Date(2025, time.January, 2, 0, 0, 0, 0, time.UTC)
	requests := make([]Request, retentionBatchSize+100)
	for i := range requests {
		requests[i] = Request{Script: "backlog", Method: "GET", URL: "https://example.com", StartedAt: cutoff.Add(-time.Hour)}
	}
	if err := s.InsertRequests(ctx, requests); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteOlderThan(ctx, cutoff); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM requests`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("backlog retained %d of %d old rows", count, len(requests))
	}
}

func TestDeleteOlderThanCanceled(t *testing.T) {
	s, _ := openTestStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.DeleteOlderThan(ctx, time.UnixMilli(0)); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected wrapped cancellation, got %v", err)
	}
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
	if err := s.db.QueryRowContext(ctx, `SELECT started_at, pg_typeof(started_at)::text FROM requests WHERE script = $1 ORDER BY started_at LIMIT 1`, script).Scan(&started, &storedType); err != nil {
		t.Fatal(err)
	}
	if started != 1735779845123 || storedType != "bigint" {
		t.Fatalf("persisted request start = %d (%s)", started, storedType)
	}
	var ok bool
	if err := s.db.QueryRowContext(ctx, `SELECT started_at, pg_typeof(started_at)::text, ok FROM runs WHERE script = $1 ORDER BY started_at LIMIT 1`, script).Scan(&started, &storedType, &ok); err != nil {
		t.Fatal(err)
	}
	if started != 1735779845123 || storedType != "bigint" || !ok {
		t.Fatalf("persisted run = %d (%s), ok = %t", started, storedType, ok)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT ok FROM runs WHERE script = $1 AND started_at = $2`, script, int64(1735779846123)).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("false persisted as true")
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
	if _, err := s.db.ExecContext(ctx, `UPDATE schema_migrations SET applied_at = $1`, 123); err != nil {
		t.Fatal(err)
	}

	type catalogColumn struct {
		tableName  string
		columnName string
		dataType   string
		isNullable string
	}
	type catalogIndex struct {
		indexName string
		indexDef  string
	}
	type catalogSnapshot struct {
		columns []catalogColumn
		indexes []catalogIndex
	}

	takeSnapshot := func(db *sql.DB) catalogSnapshot {
		var snap catalogSnapshot
		colRows, err := db.QueryContext(ctx, `
			SELECT table_name, column_name, data_type, is_nullable
			FROM information_schema.columns
			WHERE table_schema = current_schema()
			ORDER BY table_name, ordinal_position`)
		if err != nil {
			t.Fatalf("catalog snapshot columns: %v", err)
		}
		defer colRows.Close()
		for colRows.Next() {
			var col catalogColumn
			if err := colRows.Scan(&col.tableName, &col.columnName, &col.dataType, &col.isNullable); err != nil {
				t.Fatalf("scanning catalog column: %v", err)
			}
			snap.columns = append(snap.columns, col)
		}
		if err := colRows.Err(); err != nil {
			t.Fatalf("catalog column rows: %v", err)
		}

		idxRows, err := db.QueryContext(ctx, `
			SELECT indexname, indexdef
			FROM pg_indexes
			WHERE schemaname = current_schema()
			ORDER BY indexname`)
		if err != nil {
			t.Fatalf("catalog snapshot indexes: %v", err)
		}
		defer idxRows.Close()
		for idxRows.Next() {
			var idx catalogIndex
			if err := idxRows.Scan(&idx.indexName, &idx.indexDef); err != nil {
				t.Fatalf("scanning catalog index: %v", err)
			}
			snap.indexes = append(snap.indexes, idx)
		}
		if err := idxRows.Err(); err != nil {
			t.Fatalf("catalog index rows: %v", err)
		}
		return snap
	}

	before := takeSnapshot(s.db)

	if err := s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	second, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := second.Close(); err != nil {
			t.Error(err)
		}
	}()

	after := takeSnapshot(second.db)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("catalog snapshot changed across migrate / reopen:\nbefore: %+v\nafter:  %+v", before, after)
	}

	var count, version, applied int
	if err := second.db.QueryRowContext(ctx, `SELECT count(*), min(version), min(applied_at) FROM schema_migrations`).Scan(&count, &version, &applied); err != nil {
		t.Fatal(err)
	}
	if count != len(migrations) || version != 1 || applied != 123 {
		t.Fatalf("schema_migrations changed: count=%d (want %d) version=%d (want 1) applied=%d (want 123)", count, len(migrations), version, applied)
	}
}

func TestMigrationV1ToV2(t *testing.T) {
	ctx := t.Context()
	dsn := storetest.DatabaseURL(t)

	rawDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rawDB.ExecContext(ctx, `CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at BIGINT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := rawDB.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (1, 1000)`); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range migrations[0].sql {
		if _, err := rawDB.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().Truncate(time.Millisecond)
	errText := "some error"
	seedRuns := []Run{
		{Key: "seed-run-1", Script: "test-script-1", StartedAt: now, DurationMs: 120, OK: true},
		{Key: "seed-run-2", Script: "test-script-2", StartedAt: now.Add(-time.Hour), DurationMs: 450, OK: false, Err: &errText},
	}
	for _, r := range seedRuns {
		if _, err := rawDB.ExecContext(ctx, `INSERT INTO runs (key, script, started_at, duration_ms, ok, error) VALUES ($1, $2, $3, $4, $5, $6)`,
			r.Key, r.Script, r.StartedAt.UnixMilli(), r.DurationMs, r.OK, r.Err); err != nil {
			t.Fatal(err)
		}
	}
	if err := rawDB.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("first Open failed: %v", err)
	}

	var hasV2 int
	if err := st.db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations WHERE version = 2`).Scan(&hasV2); err != nil {
		t.Fatal(err)
	}
	if hasV2 != 1 {
		t.Fatalf("schema_migrations missing version 2: count=%d", hasV2)
	}

	var stateTableCount int
	if err := st.db.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema = current_schema() AND table_name = 'script_state'`).Scan(&stateTableCount); err != nil {
		t.Fatal(err)
	}
	if stateTableCount != 1 {
		t.Fatalf("script_state table does not exist: count=%d", stateTableCount)
	}

	runs, err := st.QueryRuns(ctx, "test-script-1", now.Add(-time.Minute), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Script != seedRuns[0].Script || runs[0].DurationMs != seedRuns[0].DurationMs || !runs[0].OK {
		t.Fatalf("runs rows changed or missing: got %+v", runs)
	}

	failingRuns, err := st.QueryRuns(ctx, "test-script-2", now.Add(-2*time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(failingRuns) != 1 || failingRuns[0].Script != seedRuns[1].Script || failingRuns[0].OK || failingRuns[0].Err == nil || *failingRuns[0].Err != "some error" {
		t.Fatalf("failing runs rows changed or missing: got %+v", failingRuns)
	}

	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("second Open failed: %v", err)
	}
	defer func() {
		if err := second.Close(); err != nil {
			t.Error(err)
		}
	}()
	var migrationCount int
	if err := second.db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&migrationCount); err != nil {
		t.Fatal(err)
	}
	if migrationCount != len(migrations) {
		t.Fatalf("schema_migrations count after reopen = %d, want %d", migrationCount, len(migrations))
	}
}

func TestBatchRollback(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := t.Context()
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE requests ADD CONSTRAINT test_check_method CHECK (method <> 'FAIL')`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = s.db.ExecContext(context.Background(), `ALTER TABLE requests DROP CONSTRAINT IF EXISTS test_check_method`)
	})
	err := s.InsertRequests(ctx, []Request{{Script: "batch", Method: "GET"}, {Script: "batch", Method: "FAIL"}})
	if err == nil {
		t.Fatal("expected second insert to fail")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("expected 23514 check_violation, got %v", err)
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
		{"invalid URL", "libsql://example.invalid/%" + token, "unknown"},
		{"unsupported scheme", token + "://example.invalid", "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := Open(t.Context(), tc.dsn)
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
	s, err := Open(ctx, storetest.DatabaseURL(t))
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
	dsn := storetest.DatabaseURL(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	// CREATE TABLE IF NOT EXISTS runs accepts this existing table, but the
	// later CREATE UNIQUE INDEX idx_runs_key ON runs(key) fails because it has
	// no key column. The requests DDL that ran before it must roll back.
	if _, err := db.ExecContext(t.Context(), `CREATE TABLE runs (existing TEXT)`); err != nil {
		t.Fatal(err)
	}
	s, err := Open(t.Context(), dsn)
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
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_name IN ('requests', 'schema_migrations')`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("failed migration left %d schema objects", count)
	}
}

func TestDatabaseDSN(t *testing.T) {
	for _, bad := range []string{
		"file:./test.db?mode=rwc",
		"libsql://example.invalid?other=value",
		"sqlite://./test.db",
		"mysql://u:p@localhost/db",
	} {
		if dsn, scheme, err := databaseDSN(bad); err == nil {
			t.Fatalf("databaseDSN(%q) accepted, got dsn=%q, scheme=%q", bad, dsn, scheme)
		}
	}
	for _, pg := range []string{
		"postgres://u:p@localhost:5432/notinv?sslmode=disable",
		"postgresql://u:p@localhost:5432/notinv?sslmode=disable",
	} {
		if dsn, scheme, err := databaseDSN(pg); err != nil || dsn != pg || scheme != "postgres" {
			t.Fatalf("postgres DSN = %q, %q, %v", dsn, scheme, err)
		}
	}
}

func TestPrivateErrorsPreserveCause(t *testing.T) {
	cause := &url.Error{Op: "connect", URL: "postgres://u:private@host", Err: context.DeadlineExceeded}
	err := databaseError("query", "postgres", cause)
	if strings.Contains(err.Error(), "private") || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("redaction or cause preservation failed: %v", err)
	}
	var urlErr *url.Error
	if !errors.As(err, &urlErr) || urlErr != cause {
		t.Fatal("typed cause lost")
	}
}

func TestScriptEnabled(t *testing.T) {
	ctx := t.Context()
	dsn := storetest.DatabaseURL(t)
	s, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	at := time.UnixMilli(1_700_000_000_000)
	if err := s.InsertRun(ctx, Run{Script: "a", StartedAt: at, DurationMs: 5, OK: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertRequests(ctx, []Request{{Script: "a", Method: "GET", URL: "https://example.test/", StartedAt: at, DurationMs: 3, StatusCode: 200}}); err != nil {
		t.Fatal(err)
	}
	counts := func(st *Store) (int, int) {
		t.Helper()
		var runs, reqs int
		if err := st.db.QueryRowContext(ctx, `SELECT count(*) FROM runs WHERE script = 'a'`).Scan(&runs); err != nil {
			t.Fatal(err)
		}
		if err := st.db.QueryRowContext(ctx, `SELECT count(*) FROM requests WHERE script = 'a'`).Scan(&reqs); err != nil {
			t.Fatal(err)
		}
		return runs, reqs
	}
	wantRuns, wantReqs := counts(s)
	if wantRuns != 1 || wantReqs != 1 {
		t.Fatalf("setup counts runs=%d requests=%d", wantRuns, wantReqs)
	}
	expect := func(st *Store, script string, want bool, when string) {
		t.Helper()
		got, err := st.ScriptEnabled(ctx, script)
		if err != nil {
			t.Fatalf("%s: %v", when, err)
		}
		if got != want {
			t.Fatalf("%s: %s enabled=%v, want %v", when, script, got, want)
		}
	}
	expect(s, "a", true, "never set")

	if err := s.SetScriptEnabled(ctx, "a", false); err != nil {
		t.Fatal(err)
	}
	expect(s, "a", false, "after disable")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	expect(s, "a", false, "after reopen")
	expect(s, "b", true, "other script unaffected")

	if err := s.SetScriptEnabled(ctx, "a", true); err != nil {
		t.Fatal(err)
	}
	expect(s, "a", true, "after re-enable")

	if runs, reqs := counts(s); runs != wantRuns || reqs != wantReqs {
		t.Fatalf("counts changed: runs=%d requests=%d, want %d and %d", runs, reqs, wantRuns, wantReqs)
	}
}
