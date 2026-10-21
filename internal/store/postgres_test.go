package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/trriplejay/notinv/internal/storetest"
)

func openPostgresStore(t *testing.T, databaseURL string) *Store {
	t.Helper()
	s, err := Open(t.Context(), databaseURL)
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
	dbURL := storetest.DatabaseURL(t)
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
		{"requests", "key", "text"},
		{"runs", "key", "text"},
	} {
		if got := columnType(c.table, c.column); got != c.want {
			t.Errorf("%s.%s data_type = %q, want %q", c.table, c.column, got, c.want)
		}
	}

	for _, col := range []struct{ table, column string }{
		{"runs", "ok"},
		{"requests", "key"},
		{"runs", "key"},
	} {
		var nullable string
		if err := s.db.QueryRowContext(ctx, `SELECT is_nullable FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = $1 AND column_name = $2`, col.table, col.column).Scan(&nullable); err != nil {
			t.Fatal(err)
		}
		if nullable != "NO" {
			t.Errorf("%s.%s is_nullable = %q, want NO", col.table, col.column, nullable)
		}
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

	for _, idx := range []struct {
		name, table string
		cols        []string
		unique      bool
	}{
		{"idx_requests_script_started", "requests", []string{"script", "started_at"}, false},
		{"idx_runs_script_started", "runs", []string{"script", "started_at"}, false},
		{"idx_requests_key", "requests", []string{"key"}, true},
		{"idx_runs_key", "runs", []string{"key"}, true},
	} {
		// pg_indexes scoped by schemaname and indexname; the definition text is
		// not matched, because pg_get_indexdef drops the schema qualifier when
		// the schema is on search_path.
		var table string
		err := s.db.QueryRowContext(ctx, `SELECT tablename FROM pg_indexes
			WHERE schemaname = current_schema() AND indexname = $1`, idx.name).Scan(&table)
		if err != nil {
			t.Fatalf("index %s: %v", idx.name, err)
		}
		if table != idx.table {
			t.Errorf("index %s is on table %q, want %q", idx.name, table, idx.table)
		}

		var cols string
		var unique bool
		err = s.db.QueryRowContext(ctx, `SELECT string_agg(a.attname, ',' ORDER BY k.ord), bool_and(i.indisunique)
			FROM pg_index i
			JOIN pg_class c ON c.oid = i.indexrelid
			JOIN pg_namespace n ON n.oid = c.relnamespace
			CROSS JOIN LATERAL unnest(i.indkey) WITH ORDINALITY AS k(attnum, ord)
			JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
			WHERE n.nspname = current_schema() AND c.relname = $1`, idx.name).Scan(&cols, &unique)
		if err != nil {
			t.Fatalf("index %s columns: %v", idx.name, err)
		}
		if want := strings.Join(idx.cols, ","); cols != want {
			t.Errorf("index %s columns = %q, want %q", idx.name, cols, want)
		}
		if unique != idx.unique {
			t.Errorf("index %s unique = %v, want %v", idx.name, unique, idx.unique)
		}
	}
}

// CLM-2: TestPostgresRunOkRefusesNonBoolean asserts Postgres rejects literal non-boolean ok values.
func TestPostgresRunOkRefusesNonBoolean(t *testing.T) {
	dbURL := storetest.DatabaseURL(t)
	s := openPostgresStore(t, dbURL)
	ctx := t.Context()

	script := "bad-ok-script"
	// ok is supplied as the literal 2 so Postgres's type check evaluates it rather than driver conversion.
	_, err := s.db.ExecContext(ctx, `INSERT INTO runs (key, script, started_at, duration_ms, ok) VALUES ($1, $2, $3, $4, 2)`,
		"bad-ok-key", script, int64(1000), int64(10))
	if err == nil {
		t.Fatal("expected insert with ok=2 to fail, got nil error")
	}

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected error to unwrap to *pgconn.PgError, got: %T (%v)", err, err)
	}
	if pgErr.Code != "42804" && pgErr.Code != "22P02" {
		t.Fatalf("expected PgError code 42804 or 22P02, got %q (%s)", pgErr.Code, pgErr.Message)
	}

	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM runs WHERE script = $1`, script).Scan(&count); err != nil {
		t.Fatalf("querying runs count: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 rows for script %s, got %d", script, count)
	}
}

// CLM-3: Open on a postgres:// or postgresql:// URL connects, migrates and
// round-trips every field through each read method.
func TestPostgresRoundTrip(t *testing.T) {
	for _, scheme := range []string{"postgres", "postgresql"} {
		t.Run(scheme, func(t *testing.T) {
			dbURL := storetest.DatabaseURL(t)
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
			for i := range reqs {
				reqs[i].Key = ""
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
	s := openPostgresStore(t, storetest.DatabaseURL(t))
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
			dbURL := storetest.DatabaseURL(t)
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
					stores[g], errs[g] = Open(ctx, dbURL)
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
	s := openPostgresStore(t, storetest.DatabaseURL(t))
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
		s := openPostgresStore(t, storetest.DatabaseURL(t))
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
		s := openPostgresStore(t, storetest.DatabaseURL(t))
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
		s := openPostgresStore(t, storetest.DatabaseURL(t))

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

// CLM-3: Every Store database call runs each attempt under a per-call deadline
// of a few seconds, derived from the caller's context. While a second connection
// holds LOCK TABLE runs, requests IN ACCESS EXCLUSIVE MODE, InsertRun, QueryRuns,
// InsertRequests and QueryRequests called with context.Background() each return
// an error wrapping context.DeadlineExceeded within the per-call bound plus slack.
func TestPostgresCallDeadlineUnderTableLock(t *testing.T) {
	dbURL := storetest.DatabaseURL(t)
	s := openPostgresStore(t, dbURL)
	s.callTimeout = time.Second

	lockDB, err := sql.Open("pgx", dbURL)
	if err != nil {
		t.Fatalf("open lock connection: %v", err)
	}
	t.Cleanup(func() { _ = lockDB.Close() })

	tx, err := lockDB.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("begin lock tx: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })

	if _, err := tx.ExecContext(t.Context(), `LOCK TABLE runs, requests IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("lock tables: %v", err)
	}

	bound := s.callTimeout
	slack := 2 * time.Second

	t.Run("InsertRun", func(t *testing.T) {
		start := time.Now()
		err := s.InsertRun(context.Background(), Run{Script: "test", StartedAt: time.Now(), OK: true})
		elapsed := time.Since(start)
		if err == nil {
			t.Fatal("InsertRun succeeded while table was locked, want DeadlineExceeded")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("InsertRun error = %v, want wrapping context.DeadlineExceeded", err)
		}
		if elapsed < bound-100*time.Millisecond || elapsed > bound+slack {
			t.Errorf("InsertRun elapsed = %v, want within [%v, %v]", elapsed, bound-100*time.Millisecond, bound+slack)
		}
	})

	t.Run("QueryRuns", func(t *testing.T) {
		start := time.Now()
		_, err := s.QueryRuns(context.Background(), "test", time.Now().Add(-time.Hour), time.Now())
		elapsed := time.Since(start)
		if err == nil {
			t.Fatal("QueryRuns succeeded while table was locked, want DeadlineExceeded")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("QueryRuns error = %v, want wrapping context.DeadlineExceeded", err)
		}
		if elapsed < bound-100*time.Millisecond || elapsed > bound+slack {
			t.Errorf("QueryRuns elapsed = %v, want within [%v, %v]", elapsed, bound-100*time.Millisecond, bound+slack)
		}
	})

	t.Run("InsertRequests", func(t *testing.T) {
		start := time.Now()
		err := s.InsertRequests(context.Background(), []Request{{Script: "test", Method: "GET", URL: "http://example.com", StartedAt: time.Now()}})
		elapsed := time.Since(start)
		if err == nil {
			t.Fatal("InsertRequests succeeded while table was locked, want DeadlineExceeded")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("InsertRequests error = %v, want wrapping context.DeadlineExceeded", err)
		}
		if elapsed < bound-100*time.Millisecond || elapsed > bound+slack {
			t.Errorf("InsertRequests elapsed = %v, want within [%v, %v]", elapsed, bound-100*time.Millisecond, bound+slack)
		}
	})

	t.Run("QueryRequests", func(t *testing.T) {
		start := time.Now()
		_, err := s.QueryRequests(context.Background(), "test", time.Now().Add(-time.Hour), time.Now())
		elapsed := time.Since(start)
		if err == nil {
			t.Fatal("QueryRequests succeeded while table was locked, want DeadlineExceeded")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("QueryRequests error = %v, want wrapping context.DeadlineExceeded", err)
		}
		if elapsed < bound-100*time.Millisecond || elapsed > bound+slack {
			t.Errorf("QueryRequests elapsed = %v, want within [%v, %v]", elapsed, bound-100*time.Millisecond, bound+slack)
		}
	})
}

// CLM-2: InsertRequests on the same 3-record batch twice and InsertRun on the same
// Run twice leave 3 requests rows and 1 runs row, and both calls return nil.
func TestPostgresIdempotentInsert(t *testing.T) {
	s := openPostgresStore(t, storetest.DatabaseURL(t))
	ctx := t.Context()

	base := time.Now().Truncate(time.Millisecond)
	batch := []Request{
		{Key: "req-key-1", Script: "idem", Method: "GET", URL: "https://example.com/1", StartedAt: base, DurationMs: 10, StatusCode: 200},
		{Key: "req-key-2", Script: "idem", Method: "POST", URL: "https://example.com/2", StartedAt: base, DurationMs: 20, StatusCode: 201},
		{Key: "req-key-3", Script: "idem", Method: "DELETE", URL: "https://example.com/3", StartedAt: base, DurationMs: 30, StatusCode: 204},
	}

	// First insert
	if err := s.InsertRequests(ctx, batch); err != nil {
		t.Fatalf("first InsertRequests: %v", err)
	}
	// Second insert with the exact same batch
	if err := s.InsertRequests(ctx, batch); err != nil {
		t.Fatalf("second InsertRequests: %v", err)
	}

	var reqCount int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM requests WHERE script = 'idem'`).Scan(&reqCount); err != nil {
		t.Fatalf("count requests: %v", err)
	}
	if reqCount != 3 {
		t.Errorf("requests row count = %d, want 3", reqCount)
	}

	run := Run{
		Key:        "run-key-1",
		Script:     "idem",
		StartedAt:  base,
		DurationMs: 50,
		OK:         true,
	}

	// First insert
	if err := s.InsertRun(ctx, run); err != nil {
		t.Fatalf("first InsertRun: %v", err)
	}
	// Second insert with the exact same run
	if err := s.InsertRun(ctx, run); err != nil {
		t.Fatalf("second InsertRun: %v", err)
	}

	var runCount int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM runs WHERE script = 'idem'`).Scan(&runCount); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runCount != 1 {
		t.Errorf("runs row count = %d, want 1", runCount)
	}

	// Verify auto-key assignment when key is empty
	autoBatch := []Request{
		{Script: "auto", Method: "GET", URL: "https://example.com/auto", StartedAt: base},
	}
	if err := s.InsertRequests(ctx, autoBatch); err != nil {
		t.Fatalf("InsertRequests with empty key: %v", err)
	}
	if autoBatch[0].Key == "" {
		t.Errorf("InsertRequests did not fill empty key on caller batch")
	}

	autoRun := Run{
		Script:    "auto",
		StartedAt: base,
		OK:        true,
	}
	if err := s.InsertRun(ctx, autoRun); err != nil {
		t.Fatalf("InsertRun with empty key: %v", err)
	}
}

// CLM-5: store.Open retries the initial connect and ping on connection-class failures
// (refused, reset, unreachable, 57P03) for about 60 seconds, then returns an error
// saying the database could not be reached within the wait. An auth failure (28P01/28000)
// or unknown database (3D000) returns on the first attempt. A cancelled context returns
// promptly.
func TestPostgresOpenRetry(t *testing.T) {
	base := storetest.BaseURL(t)

	t.Run("retry loop on closed port", func(t *testing.T) {
		origBudget := openWaitBudget
		origBackoff := openInitialBackoff
		openWaitBudget = 2 * time.Second
		openInitialBackoff = 20 * time.Millisecond
		defer func() {
			openWaitBudget = origBudget
			openInitialBackoff = origBackoff
		}()

		// Closed port on localhost (54329)
		closedPortURL := "postgres://user:pass@127.0.0.1:54329/testdb"
		start := time.Now()
		_, err := Open(t.Context(), closedPortURL)
		elapsed := time.Since(start)

		if err == nil {
			t.Fatal("expected Open to fail for closed port, got nil")
		}
		if elapsed < 2*time.Second || elapsed >= 3*time.Second {
			t.Errorf("elapsed time = %v, want at least 2s and less than 3s", elapsed)
		}
		if !strings.Contains(err.Error(), "could not be reached within the wait") {
			t.Errorf("error = %v, want error naming the wait", err)
		}
	})

	t.Run("fail fast on bad password", func(t *testing.T) {
		u, err := url.Parse(base)
		if err != nil {
			t.Fatalf("parse %s: %v", storetest.EnvDatabaseURL, err)
		}
		user := "postgres"
		if u.User != nil && u.User.Username() != "" {
			user = u.User.Username()
		}
		u.User = url.UserPassword(user, "wrong_password_never_valid_12345")

		start := time.Now()
		_, err = Open(t.Context(), u.String())
		elapsed := time.Since(start)

		if err == nil {
			t.Fatal("expected Open to fail for bad password, got nil")
		}
		if elapsed >= 1*time.Second {
			t.Errorf("bad password took %v, want under 1s", elapsed)
		}
	})

	t.Run("fail fast on nonexistent database", func(t *testing.T) {
		u, err := url.Parse(base)
		if err != nil {
			t.Fatalf("parse %s: %v", storetest.EnvDatabaseURL, err)
		}
		u.Path = "/nonexistent_db_3d000_never_exists"

		start := time.Now()
		_, err = Open(t.Context(), u.String())
		elapsed := time.Since(start)

		if err == nil {
			t.Fatal("expected Open to fail for nonexistent database, got nil")
		}
		if elapsed >= 1*time.Second {
			t.Errorf("nonexistent database took %v, want under 1s", elapsed)
		}
	})

	t.Run("prompt return on cancelled context", func(t *testing.T) {
		closedPortURL := "postgres://user:pass@127.0.0.1:54329/testdb"
		ctx, cancel := context.WithCancel(t.Context())
		cancel() // pre-cancelled

		start := time.Now()
		_, err := Open(ctx, closedPortURL)
		elapsed := time.Since(start)

		if err == nil {
			t.Fatal("expected Open to fail for cancelled context, got nil")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected context.Canceled, got %v", err)
		}
		if elapsed >= 500*time.Millisecond {
			t.Errorf("cancelled context took %v, want prompt return under 500ms", elapsed)
		}
	})
}
