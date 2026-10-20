package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testDatabaseURLEnv names the Postgres URL the integration tests run against.
// The tests fail, never skip, when it is unset: they are the proof that the
// store works on Postgres.
const testDatabaseURLEnv = "NOTINV_TEST_DATABASE_URL"

var schemaCounter atomic.Int64

// postgresTestURL creates a fresh, uniquely named schema in the test database
// and returns a postgres URL whose search_path points at it. The schema is
// dropped when the test ends.
func postgresTestURL(t *testing.T) string {
	t.Helper()
	base := os.Getenv(testDatabaseURLEnv)
	if base == "" {
		t.Fatalf("%s is not set; these integration tests need a Postgres database URL", testDatabaseURLEnv)
	}
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })

	schema := fmt.Sprintf("notinv_test_%d_%d", time.Now().UnixNano(), schemaCounter.Add(1))
	if _, err := admin.ExecContext(t.Context(), `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`); err != nil {
			t.Errorf("drop schema %s: %v", schema, err)
		}
	})

	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse %s: %v", testDatabaseURLEnv, err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}

func openPostgresStore(t *testing.T, databaseURL string) *Store {
	t.Helper()
	s, err := Open(t.Context(), databaseURL, "")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func strPtr(s string) *string { return &s }

// CLM-2: migration 1 creates Postgres DDL with 64-bit timestamps, a boolean
// ok, id primary keys and the two (script, started_at) indexes.
func TestPostgresSchema(t *testing.T) {
	dbURL := postgresTestURL(t)
	s := openPostgresStore(t, dbURL)
	ctx := t.Context()

	columnType := func(table, column string) string {
		t.Helper()
		var dataType string
		err := s.db.QueryRowContext(ctx, `SELECT data_type FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = $1 AND column_name = $2`,
			table, column).Scan(&dataType)
		if err != nil {
			t.Fatalf("column %s.%s: %v", table, column, err)
		}
		return dataType
	}
	for _, c := range []struct{ table, column, want string }{
		{"runs", "ok", "boolean"},
		{"requests", "started_at", "bigint"},
		{"runs", "started_at", "bigint"},
		{"schema_migrations", "applied_at", "bigint"},
		{"requests", "id", "bigint"},
		{"runs", "id", "bigint"},
	} {
		if got := columnType(c.table, c.column); got != c.want {
			t.Errorf("%s.%s data_type = %q, want %q", c.table, c.column, got, c.want)
		}
	}

	var nullable string
	if err := s.db.QueryRowContext(ctx, `SELECT is_nullable FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'runs' AND column_name = 'ok'`).Scan(&nullable); err != nil {
		t.Fatal(err)
	}
	if nullable != "NO" {
		t.Errorf("runs.ok is_nullable = %q, want NO", nullable)
	}

	for _, table := range []string{"requests", "runs"} {
		var pkColumns string
		err := s.db.QueryRowContext(ctx, `SELECT string_agg(kcu.column_name, ',' ORDER BY kcu.ordinal_position)
			FROM information_schema.table_constraints tc
			JOIN information_schema.key_column_usage kcu
			  ON kcu.constraint_name = tc.constraint_name AND kcu.table_schema = tc.table_schema
			WHERE tc.table_schema = current_schema() AND tc.table_name = $1 AND tc.constraint_type = 'PRIMARY KEY'`,
			table).Scan(&pkColumns)
		if err != nil {
			t.Fatalf("%s primary key: %v", table, err)
		}
		if pkColumns != "id" {
			t.Errorf("%s primary key columns = %q, want id", table, pkColumns)
		}
	}

	for _, idx := range []struct{ name, table string }{
		{"idx_requests_script_started", "requests"},
		{"idx_runs_script_started", "runs"},
	} {
		var def string
		err := s.db.QueryRowContext(ctx, `SELECT indexdef FROM pg_indexes
			WHERE schemaname = current_schema() AND indexname = $1`, idx.name).Scan(&def)
		if err != nil {
			t.Fatalf("index %s: %v", idx.name, err)
		}
		if !strings.Contains(def, "ON "+currentSchema(t, s)+"."+idx.table) || !strings.Contains(def, "(script, started_at)") {
			t.Errorf("index %s definition = %q, want on %s (script, started_at)", idx.name, def, idx.table)
		}
	}
}

func currentSchema(t *testing.T, s *Store) string {
	t.Helper()
	var schema string
	if err := s.db.QueryRowContext(t.Context(), `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	return schema
}

// CLM-3: Open on a postgres:// or postgresql:// URL connects, migrates and
// round-trips every field through each read method.
func TestPostgresRoundTrip(t *testing.T) {
	for _, scheme := range []string{"postgres", "postgresql"} {
		t.Run(scheme, func(t *testing.T) {
			dbURL := postgresTestURL(t)
			u, err := url.Parse(dbURL)
			if err != nil {
				t.Fatal(err)
			}
			u.Scheme = scheme
			s := openPostgresStore(t, u.String())
			ctx := t.Context()

			base := time.UnixMilli(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC).UnixMilli())
			reqs := []Request{
				{Script: "alpha", Method: "GET", URL: "https://example.com/a", StartedAt: base,
					DurationMs: 120, StatusCode: 200},
				{Script: "alpha", Method: "POST", URL: "https://example.com/b", StartedAt: base.Add(time.Second),
					DurationMs: 5000, StatusCode: 0, Err: strPtr("dial tcp: timeout")},
			}
			if err := s.InsertRequests(ctx, reqs); err != nil {
				t.Fatalf("InsertRequests: %v", err)
			}
			runs := []Run{
				{Script: "alpha", StartedAt: base, DurationMs: 300, OK: true},
				{Script: "alpha", StartedAt: base.Add(time.Minute), DurationMs: 50, OK: false, Err: strPtr("boom")},
				{Script: "beta", StartedAt: base, DurationMs: 7, OK: true},
			}
			for _, r := range runs {
				if err := s.InsertRun(ctx, r); err != nil {
					t.Fatalf("InsertRun: %v", err)
				}
			}

			gotReqs, err := s.QueryRequests(ctx, "alpha", base, base.Add(time.Hour))
			if err != nil {
				t.Fatalf("QueryRequests: %v", err)
			}
			if !reflect.DeepEqual(gotReqs, reqs) {
				t.Errorf("QueryRequests = %+v, want %+v", gotReqs, reqs)
			}

			gotRuns, err := s.QueryRuns(ctx, "alpha", base, base.Add(time.Hour))
			if err != nil {
				t.Fatalf("QueryRuns: %v", err)
			}
			if !reflect.DeepEqual(gotRuns, runs[:2]) {
				t.Errorf("QueryRuns = %+v, want %+v", gotRuns, runs[:2])
			}

			latest, err := s.LatestRun(ctx, "alpha")
			if err != nil {
				t.Fatalf("LatestRun: %v", err)
			}
			if latest == nil || !reflect.DeepEqual(*latest, runs[1]) {
				t.Errorf("LatestRun = %+v, want %+v", latest, runs[1])
			}
			none, err := s.LatestRun(ctx, "missing")
			if err != nil || none != nil {
				t.Errorf("LatestRun(missing) = %+v, %v; want nil, nil", none, err)
			}

			if err := s.InsertRequests(ctx, []Request{{Script: "gamma", Method: "GET", URL: "u", StartedAt: base}}); err != nil {
				t.Fatal(err)
			}
			scripts, err := s.ListScripts(ctx)
			if err != nil {
				t.Fatalf("ListScripts: %v", err)
			}
			if want := []string{"alpha", "beta", "gamma"}; !reflect.DeepEqual(scripts, want) {
				t.Errorf("ListScripts = %v, want %v", scripts, want)
			}
		})
	}
}

// CLM-4: OK is a bool, StartedAt after 2038 round-trips, and since/until
// bounds are inclusive at millisecond precision.
func TestPostgresBoolAnd64BitTimes(t *testing.T) {
	s := openPostgresStore(t, postgresTestURL(t))
	ctx := t.Context()

	far := time.UnixMilli(time.Date(2100, 6, 15, 8, 30, 0, 123_000_000, time.UTC).UnixMilli())
	runs := []Run{
		{Script: "far", StartedAt: far, DurationMs: 1, OK: true},
		{Script: "far", StartedAt: far.Add(time.Millisecond), DurationMs: 2, OK: false},
	}
	for _, r := range runs {
		if err := s.InsertRun(ctx, r); err != nil {
			t.Fatalf("InsertRun: %v", err)
		}
	}
	req := Request{Script: "far", Method: "GET", URL: "https://example.com", StartedAt: far, DurationMs: 9, StatusCode: 204}
	if err := s.InsertRequests(ctx, []Request{req}); err != nil {
		t.Fatalf("InsertRequests: %v", err)
	}

	gotRuns, err := s.QueryRuns(ctx, "far", far, far.Add(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotRuns, runs) {
		t.Fatalf("QueryRuns = %+v, want %+v", gotRuns, runs)
	}
	var okTrue, okFalse int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE ok), count(*) FILTER (WHERE NOT ok) FROM runs`).Scan(&okTrue, &okFalse); err != nil {
		t.Fatal(err)
	}
	if okTrue != 1 || okFalse != 1 {
		t.Errorf("stored ok true/false counts = %d/%d, want 1/1", okTrue, okFalse)
	}
	gotReqs, err := s.QueryRequests(ctx, "far", far, far)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotReqs) != 1 || !gotReqs[0].StartedAt.Equal(far) || !reflect.DeepEqual(gotReqs[0], req) {
		t.Errorf("QueryRequests at year 2100 = %+v, want [%+v]", gotReqs, req)
	}

	// Inclusive bounds: rows exactly at since and until are returned, rows
	// one millisecond outside are not.
	edge := time.UnixMilli(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli())
	since, until := edge, edge.Add(10*time.Millisecond)
	stamps := []time.Time{since.Add(-time.Millisecond), since, until, until.Add(time.Millisecond)}
	var edgeReqs []Request
	for i, ts := range stamps {
		edgeReqs = append(edgeReqs, Request{Script: "edge", Method: "GET", URL: fmt.Sprint(i), StartedAt: ts})
		if err := s.InsertRun(ctx, Run{Script: "edge", StartedAt: ts, DurationMs: int64(i), OK: i%2 == 0}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.InsertRequests(ctx, edgeReqs); err != nil {
		t.Fatal(err)
	}
	inReqs, err := s.QueryRequests(ctx, "edge", since, until)
	if err != nil {
		t.Fatal(err)
	}
	if len(inReqs) != 2 || !inReqs[0].StartedAt.Equal(since) || !inReqs[1].StartedAt.Equal(until) {
		t.Errorf("QueryRequests bounds = %+v, want exactly the rows at since and until", inReqs)
	}
	inRuns, err := s.QueryRuns(ctx, "edge", since, until)
	if err != nil {
		t.Fatal(err)
	}
	if len(inRuns) != 2 || !inRuns[0].StartedAt.Equal(since) || !inRuns[1].StartedAt.Equal(until) ||
		inRuns[0].OK != false || inRuns[1].OK != true {
		t.Errorf("QueryRuns bounds = %+v, want exactly the rows at since (OK=false) and until (OK=true)", inRuns)
	}
}

// CLM-5: concurrent Opens on an empty schema are serialised; a later Open
// applies nothing and leaves data and schema_migrations unchanged.
func TestPostgresConcurrentMigrate(t *testing.T) {
	for i := range 5 {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			dbURL := postgresTestURL(t)
			ctx := t.Context()

			var wg sync.WaitGroup
			stores := make([]*Store, 2)
			errs := make([]error, 2)
			start := make(chan struct{})
			for g := range 2 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					stores[g], errs[g] = Open(ctx, dbURL, "")
				}()
			}
			close(start)
			wg.Wait()
			for g := range 2 {
				if stores[g] != nil {
					t.Cleanup(func() { _ = stores[g].Close() })
				}
				if errs[g] != nil {
					t.Fatalf("concurrent Open %d: %v", g, errs[g])
				}
			}
			s := stores[0]

			migrationRows := func() [][2]int64 {
				t.Helper()
				rows, err := s.db.QueryContext(ctx, `SELECT version, applied_at FROM schema_migrations ORDER BY version, applied_at`)
				if err != nil {
					t.Fatal(err)
				}
				defer rows.Close()
				var out [][2]int64
				for rows.Next() {
					var r [2]int64
					if err := rows.Scan(&r[0], &r[1]); err != nil {
						t.Fatal(err)
					}
					out = append(out, r)
				}
				if err := rows.Err(); err != nil {
					t.Fatal(err)
				}
				return out
			}
			before := migrationRows()
			if len(before) != len(migrations) {
				t.Fatalf("schema_migrations rows = %v, want one per version (%d)", before, len(migrations))
			}
			for j, m := range migrations {
				if before[j][0] != int64(m.version) {
					t.Fatalf("schema_migrations rows = %v, want versions in order", before)
				}
			}

			now := time.UnixMilli(time.Now().UnixMilli())
			if err := s.InsertRequests(ctx, []Request{{Script: "a", Method: "GET", URL: "u", StartedAt: now}}); err != nil {
				t.Fatal(err)
			}
			if err := s.InsertRun(ctx, Run{Script: "a", StartedAt: now, OK: true}); err != nil {
				t.Fatal(err)
			}

			openPostgresStore(t, dbURL) // re-Open on the migrated schema
			if after := migrationRows(); !reflect.DeepEqual(after, before) {
				t.Errorf("schema_migrations after re-Open = %v, want unchanged %v", after, before)
			}
			var nReq, nRun int
			if err := s.db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM requests), (SELECT count(*) FROM runs)`).Scan(&nReq, &nRun); err != nil {
				t.Fatal(err)
			}
			if nReq != 1 || nRun != 1 {
				t.Errorf("rows after re-Open requests=%d runs=%d, want 1 and 1", nReq, nRun)
			}
		})
	}
}

// CLM-6: the Postgres pool is not capped at one connection, and concurrent
// inserts and reads from many goroutines all succeed.
func TestPostgresConcurrentAccess(t *testing.T) {
	s := openPostgresStore(t, postgresTestURL(t))
	ctx := t.Context()

	if max := s.db.Stats().MaxOpenConnections; max == 1 {
		t.Fatalf("MaxOpenConnections = 1, want 0 (unlimited) or > 1")
	}

	const workers = 20
	base := time.UnixMilli(time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC).UnixMilli())
	var wg sync.WaitGroup
	errs := make(chan error, workers*3)
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ts := base.Add(time.Duration(w) * time.Second)
			switch w % 3 {
			case 0:
				errs <- s.InsertRequests(ctx, []Request{
					{Script: "conc", Method: "GET", URL: fmt.Sprint(w, "a"), StartedAt: ts},
					{Script: "conc", Method: "GET", URL: fmt.Sprint(w, "b"), StartedAt: ts},
				})
			case 1:
				errs <- s.InsertRun(ctx, Run{Script: "conc", StartedAt: ts, DurationMs: int64(w), OK: w%2 == 0})
			default:
				_, err := s.QueryRequests(ctx, "conc", base, base.Add(time.Hour))
				errs <- err
				_, err = s.QueryRuns(ctx, "conc", base, base.Add(time.Hour))
				errs <- err
				_, err = s.ListScripts(ctx)
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent operation: %v", err)
		}
	}

	var wantReqs, wantRuns int
	for w := range workers {
		switch w % 3 {
		case 0:
			wantReqs += 2
		case 1:
			wantRuns++
		}
	}
	gotReqs, err := s.QueryRequests(ctx, "conc", base, base.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	gotRuns, err := s.QueryRuns(ctx, "conc", base, base.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(gotReqs) != wantReqs || len(gotRuns) != wantRuns {
		t.Errorf("rows present requests=%d runs=%d, want %d and %d", len(gotReqs), len(gotRuns), wantReqs, wantRuns)
	}
	if max := s.db.Stats().MaxOpenConnections; max == 1 {
		t.Errorf("MaxOpenConnections = 1 after load, want 0 or > 1")
	}
}

// CLM-1: DeleteOlderThan removes rows strictly older than cutoff in bounded
// batches, leaves rows at or after cutoff intact, drains backlogs larger than
// a single batch, succeeds on repeat calls, and returns an error wrapping
// context.Canceled when cancelled.
func TestPostgresDeleteOlderThan(t *testing.T) {
	t.Run("boundary", func(t *testing.T) {
		s := openPostgresStore(t, postgresTestURL(t))
		ctx := t.Context()

		cutoff := time.UnixMilli(time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC).UnixMilli())
		stamps := []time.Time{
			cutoff.Add(-48 * time.Hour),
			cutoff.Add(-time.Millisecond),
			cutoff,
			cutoff.Add(time.Millisecond),
			cutoff.Add(48 * time.Hour),
		}

		var reqs []Request
		for i, ts := range stamps {
			reqs = append(reqs, Request{Script: "del_test", Method: "GET", URL: fmt.Sprintf("https://example.com/%d", i), StartedAt: ts})
			if err := s.InsertRun(ctx, Run{Script: "del_test", StartedAt: ts, DurationMs: int64(i), OK: true}); err != nil {
				t.Fatalf("InsertRun: %v", err)
			}
		}
		if err := s.InsertRequests(ctx, reqs); err != nil {
			t.Fatalf("InsertRequests: %v", err)
		}

		if err := s.DeleteOlderThan(ctx, cutoff); err != nil {
			t.Fatalf("first DeleteOlderThan: %v", err)
		}
		if err := s.DeleteOlderThan(ctx, cutoff); err != nil {
			t.Fatalf("second DeleteOlderThan: %v", err)
		}

		for _, table := range []string{"requests", "runs"} {
			var before, at, after int
			query := fmt.Sprintf(`SELECT
				count(*) FILTER (WHERE started_at < $1),
				count(*) FILTER (WHERE started_at = $1),
				count(*) FILTER (WHERE started_at > $1)
				FROM %s`, table)
			if err := s.db.QueryRowContext(ctx, query, cutoff.UnixMilli()).Scan(&before, &at, &after); err != nil {
				t.Fatalf("%s counts: %v", table, err)
			}
			if before != 0 {
				t.Errorf("%s rows before cutoff = %d, want 0", table, before)
			}
			if at != 1 {
				t.Errorf("%s rows at cutoff = %d, want 1", table, at)
			}
			if after != 2 {
				t.Errorf("%s rows after cutoff = %d, want 2", table, after)
			}
		}
	})

	t.Run("multi-batch", func(t *testing.T) {
		s := openPostgresStore(t, postgresTestURL(t))
		ctx := t.Context()

		cutoff := time.UnixMilli(time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC).UnixMilli())
		count := retentionBatchSize + 100
		oldTime := cutoff.Add(-time.Hour)

		reqs := make([]Request, count)
		for i := range count {
			reqs[i] = Request{
				Script:    "batch_test",
				Method:    "GET",
				URL:       fmt.Sprintf("https://example.com/%d", i),
				StartedAt: oldTime,
			}
		}
		if err := s.InsertRequests(ctx, reqs); err != nil {
			t.Fatalf("InsertRequests: %v", err)
		}

		if err := s.DeleteOlderThan(ctx, cutoff); err != nil {
			t.Fatalf("DeleteOlderThan: %v", err)
		}

		var remaining int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM requests`).Scan(&remaining); err != nil {
			t.Fatalf("query remaining requests: %v", err)
		}
		if remaining != 0 {
			t.Errorf("remaining requests = %d, want 0", remaining)
		}
	})

	t.Run("cancelled", func(t *testing.T) {
		s := openPostgresStore(t, postgresTestURL(t))

		cancelCtx, cancel := context.WithCancel(t.Context())
		cancel()

		cutoff := time.Now()
		err := s.DeleteOlderThan(cancelCtx, cutoff)
		if err == nil {
			t.Fatal("DeleteOlderThan with cancelled context succeeded, want error")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("DeleteOlderThan error = %v, want errors.Is(..., context.Canceled)", err)
		}
	})
}
