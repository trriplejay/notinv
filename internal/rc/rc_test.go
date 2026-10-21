package rc

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/trriplejay/notinv/internal/store"
	"github.com/trriplejay/notinv/internal/storetest"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func testRequest(t *testing.T, method, url string) *http.Request {
	t.Helper()
	r, err := http.NewRequestWithContext(t.Context(), method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func idleWriter(capacity int, log *slog.Logger) *Writer {
	return &Writer{
		records: make(chan store.Request, capacity), log: log,
		userAgent: "notinv/test", stop: make(chan struct{}), finished: make(chan struct{}),
	}
}

func TestClientDefaultsAndRequestClone(t *testing.T) {
	for _, header := range []string{"", "custom-agent"} {
		t.Run("agent="+header, func(t *testing.T) {
			w := idleWriter(1, slog.Default())
			client := New("named-script", w)
			if client.Timeout != 30*time.Second {
				t.Fatalf("timeout = %s", client.Timeout)
			}
			tr := client.Transport.(*transport)
			if tr.base != http.DefaultTransport {
				t.Fatal("client does not wrap DefaultTransport")
			}
			req := testRequest(t, http.MethodPatch, "https://example.test/resource?q=1")
			if header != "" {
				req.Header.Set("User-Agent", header)
			} else {
				req.Header = nil
			}
			response := &http.Response{StatusCode: http.StatusAccepted, Body: http.NoBody}
			tr.base = roundTripFunc(func(out *http.Request) (*http.Response, error) {
				want := header
				if want == "" {
					want = "notinv/test"
				}
				if out == req || out.Header.Get("User-Agent") != want {
					t.Errorf("outbound request was not cloned with agent %q", want)
				}
				out.Header.Set("X-Delegate", "changed")
				time.Sleep(5 * time.Millisecond)
				return response, nil
			})
			before := time.Now()
			got, err := tr.RoundTrip(req)
			after := time.Now()
			if got != nil {
				if closeErr := got.Body.Close(); closeErr != nil {
					t.Error(closeErr)
				}
			}
			if got != response || err != nil {
				t.Fatalf("RoundTrip = %p, %v", got, err)
			}
			if req.Header.Get("User-Agent") != header || req.Header.Get("X-Delegate") != "" {
				t.Fatal("caller headers mutated")
			}
			select {
			case record := <-w.records:
				if record.Key == "" {
					t.Error("record has empty key")
				}
				if record.Script != "named-script" || record.Method != http.MethodPatch ||
					record.URL != req.URL.String() || record.StatusCode != http.StatusAccepted || record.Err != nil ||
					record.StartedAt.Before(before) || record.StartedAt.After(after) ||
					record.DurationMs < 5 || record.DurationMs > after.Sub(before).Milliseconds() {
					t.Fatalf("unexpected record: %+v", record)
				}
			default:
				t.Fatal("record dropped despite available capacity")
			}
		})
	}
}

func TestRoundTripFidelityWhenRecordingFails(t *testing.T) {
	sentinel := errors.New("delegate failed")
	for _, closed := range []bool{false, true} {
		for _, delegateErr := range []error{nil, sentinel} {
			var logs bytes.Buffer
			w := idleWriter(1, slog.New(slog.NewTextHandler(&logs, nil)))
			w.closed = closed
			w.records <- store.Request{Script: "already-buffered"}
			response := &http.Response{StatusCode: http.StatusTeapot, Body: http.NoBody}
			tr := New("script", w).Transport.(*transport)
			tr.base = roundTripFunc(func(*http.Request) (*http.Response, error) {
				return response, delegateErr
			})
			req := testRequest(t, http.MethodGet, "http://example.test/")
			finished := make(chan struct{})
			var got *http.Response
			var err error
			go func() {
				response, responseErr := tr.RoundTrip(req)
				if response != nil {
					if closeErr := response.Body.Close(); closeErr != nil {
						t.Error(closeErr)
					}
				}
				got, err = response, responseErr
				close(finished)
			}()
			select {
			case <-finished:
			case <-time.After(time.Second):
				t.Fatal("RoundTrip blocked waiting for buffer capacity")
			}
			if got != response || err != delegateErr { //nolint:errorlint // Exact identity is the contract; errors.Is would accept wrapping.
				t.Fatalf("delegate pair changed: %p, %v", got, err)
			}
			want := "dropping request record, buffer full"
			if closed {
				want = "dropping request record, writer closed"
			}
			if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), want) {
				t.Fatalf("missing warning: %s", logs.String())
			}
			if len(w.records) != 1 || (<-w.records).Script != "already-buffered" {
				t.Fatal("full buffer was modified")
			}
		}
	}
}

func TestTransportErrorRecord(t *testing.T) {
	for _, response := range []*http.Response{nil, {StatusCode: http.StatusBadGateway, Body: http.NoBody}} {
		w := idleWriter(1, slog.Default())
		sentinel := errors.New("transport sentinel")
		tr := New("errors", w).Transport.(*transport)
		tr.base = roundTripFunc(func(*http.Request) (*http.Response, error) { return response, sentinel })
		got, err := tr.RoundTrip(testRequest(t, http.MethodGet, "http://example.test/"))
		if got != nil {
			if closeErr := got.Body.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		}
		if got != response || err != sentinel { //nolint:errorlint // Require the exact error, not a wrapper around it.
			t.Fatal("error response pair changed")
		}
		record := <-w.records
		if record.StatusCode != 0 || record.Err == nil || *record.Err != sentinel.Error() {
			t.Fatalf("error record = %+v", record)
		}
	}
}

func openTestStore(t *testing.T) (*store.Store, *sql.DB) {
	t.Helper()
	dsn := storetest.DatabaseURL(t)
	s, err := store.Open(t.Context(), dsn, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	// An independent query handle on the same test schema.
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	return s, db
}

func requestCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var count int
	if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM requests").Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestHTTPRecordsInRealStore(t *testing.T) {
	s, db := openTestStore(t)
	w := NewWriter(s, "v1.2.3", nil, Options{BatchSize: 100, FlushInterval: time.Hour})
	t.Cleanup(w.Close)
	agents := make(chan string, 3)
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		agents <- r.Header.Get("User-Agent")
		if r.URL.Path == "/slow" {
			<-r.Context().Done()
			return
		}
		time.Sleep(5 * time.Millisecond)
		if r.URL.Path == "/bad" {
			rw.WriteHeader(http.StatusServiceUnavailable)
		}
		if _, err := io.WriteString(rw, "real response"); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refused := "http://" + listener.Addr().String() + "/refused"
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	timingBounds := make(map[string]requestTiming)
	for _, tc := range []struct {
		name   string
		url    string
		method string
		status int
	}{
		{"success", server.URL + "/ok", http.MethodGet, http.StatusOK},
		{"non-2xx", server.URL + "/bad", http.MethodPost, http.StatusServiceUnavailable},
		{"timeout", server.URL + "/slow", http.MethodGet, 0},
		{"refused", refused, http.MethodGet, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := New("check-"+tc.name, w)
			if tc.name == "timeout" {
				client.Timeout = 100 * time.Millisecond
			}
			if tc.name == "refused" {
				// Local refusal is often sub-millisecond. Delay the real dial so
				// this fixture can assert a positive, millisecond-precision duration.
				base := http.DefaultTransport.(*http.Transport).Clone()
				t.Cleanup(base.CloseIdleConnections)
				base.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
					time.Sleep(5 * time.Millisecond)
					return (&net.Dialer{}).DialContext(ctx, network, addr)
				}
				client.Transport.(*transport).base = base
			}
			before := time.Now()
			resp, err := client.Do(testRequest(t, tc.method, tc.url))
			after := time.Now()
			if resp != nil {
				defer func() {
					if err := resp.Body.Close(); err != nil {
						t.Error(err)
					}
				}()
			}
			if tc.status != 0 {
				if err != nil || resp == nil || resp.StatusCode != tc.status {
					t.Fatalf("Do = %v, %v", resp, err)
				}
				body, readErr := io.ReadAll(resp.Body)
				if readErr != nil || string(body) != "real response" {
					t.Fatalf("body = %q, %v", body, readErr)
				}
			} else if err == nil || resp != nil {
				t.Fatalf("expected transport failure: %v, %v", resp, err)
			}
			if tc.name == "timeout" {
				var timeout net.Error
				if !errors.As(err, &timeout) || !timeout.Timeout() {
					t.Fatalf("expected client timeout, got %v", err)
				}
			}
			if tc.name != "refused" {
				select {
				case agent := <-agents:
					if agent != "notinv/v1.2.3" {
						t.Fatalf("User-Agent = %q", agent)
					}
				case <-time.After(time.Second):
					t.Fatal("server did not observe request")
				}
			}
			// Shutdown is below the batch threshold and before the interval.
			// Only the last request closes the shared writer.
			if tc.name == "refused" {
				w.Close()
			}
			// Save timing bounds per request for assertions after the final flush.
			timingBounds[tc.name] = requestTiming{before, after}
		})
	}
	if count := requestCount(t, db); count != 4 {
		t.Fatalf("shutdown persisted %d records, want 4", count)
	}
	rows, err := db.QueryContext(t.Context(), "SELECT script, method, url, started_at, duration_ms, status_code, error FROM requests")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Error(err)
		}
	}()
	for rows.Next() {
		var script, method, url string
		var start, duration int64
		var status int
		var failure sql.NullString
		if err := rows.Scan(&script, &method, &url, &start, &duration, &status, &failure); err != nil {
			t.Fatal(err)
		}
		name := strings.TrimPrefix(script, "check-")
		bounds, ok := timingBounds[name]
		if !ok || script != "check-"+name || start < bounds.before.UnixMilli() || start > bounds.after.UnixMilli() ||
			duration <= 0 || duration > bounds.after.Sub(bounds.before).Milliseconds() {
			t.Fatalf("invalid timing/script: %s %d %d", script, start, duration)
		}
		wantURL, wantMethod, wantStatus := server.URL+"/ok", http.MethodGet, http.StatusOK
		switch name {
		case "non-2xx":
			wantURL, wantMethod, wantStatus = server.URL+"/bad", http.MethodPost, http.StatusServiceUnavailable
		case "timeout":
			wantURL, wantStatus = server.URL+"/slow", 0
		case "refused":
			wantURL, wantStatus = refused, 0
		}
		if url != wantURL || method != wantMethod || status != wantStatus || failure.Valid != (wantStatus == 0) {
			t.Fatalf("invalid record: %s %s %s %d %+v", script, method, url, status, failure)
		}
		if wantStatus == 0 && failure.String == "" {
			t.Fatal("missing transport error")
		}
		// http.Client can cancel the transport before its deadline fires; the
		// raw RoundTrip error then says request canceled rather than deadline
		// exceeded. The caller's Timeout result is asserted above.
		if name == "timeout" && !strings.Contains(failure.String, "context deadline exceeded") &&
			!strings.Contains(failure.String, "request canceled") {
			t.Fatalf("unexpected timeout error: %q", failure.String)
		}
		if name == "refused" && !strings.Contains(failure.String, "connection refused") {
			t.Fatalf("unexpected refusal error: %q", failure.String)
		}
		delete(timingBounds, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(timingBounds) != 0 {
		t.Fatalf("missing requests: %v", timingBounds)
	}
}

type requestTiming struct{ before, after time.Time }

func TestWriterFlushTriggers(t *testing.T) {
	for _, trigger := range []string{"batch", "interval", "shutdown"} {
		t.Run(trigger, func(t *testing.T) {
			s, db := openTestStore(t)
			options := Options{BufferSize: 32, BatchSize: 100, FlushInterval: time.Hour}
			if trigger == "batch" {
				options.BatchSize = 2
			}
			if trigger == "interval" {
				options.FlushInterval = 10 * time.Millisecond
			}
			w := NewWriter(s, "test", nil, options)
			t.Cleanup(w.Close)
			for i := 0; i < 20; i++ {
				w.enqueue(store.Request{Script: "flush", StartedAt: time.Now()})
			}
			if trigger == "shutdown" {
				if count := requestCount(t, db); count != 0 {
					t.Fatalf("records flushed before shutdown: %d", count)
				}
				var wg sync.WaitGroup
				for i := 0; i < 4; i++ {
					wg.Go(w.Close)
				}
				wg.Wait()
			} else {
				deadline := time.After(3 * time.Second)
				tick := time.NewTicker(5 * time.Millisecond)
				defer tick.Stop()
				for requestCount(t, db) != 20 {
					select {
					case <-deadline:
						t.Fatal("writer did not flush without shutdown")
					case <-tick.C:
					}
				}
			}
			if count := requestCount(t, db); count != 20 {
				t.Fatalf("persisted %d records, want 20", count)
			}
			w.Close()
		})
	}
}

func TestShutdownFlushAfterCancellation(t *testing.T) {
	dbURL := storetest.DatabaseURL(t)
	s, err := store.Open(t.Context(), dbURL, "")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	t.Run("with lock", func(t *testing.T) {
		tx, err := db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		defer func() { _ = tx.Rollback() }()

		if _, err := tx.ExecContext(t.Context(), "LOCK TABLE requests IN ACCESS EXCLUSIVE MODE"); err != nil {
			t.Fatalf("lock table: %v", err)
		}

		var logs bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&logs, nil))
		w := NewWriter(s, "test", logger, Options{FlushInterval: time.Hour})

		const enqueued = 3
		for i := 0; i < enqueued; i++ {
			w.enqueue(store.Request{
				Script:     "test-script",
				Method:     "GET",
				URL:        "https://example.com",
				StartedAt:  time.Now(),
				DurationMs: 10,
				StatusCode: 200,
			})
		}

		started := time.Now()
		w.Close()
		elapsed := time.Since(started)

		if elapsed >= FlushTimeout+time.Second {
			t.Errorf("Close took %v, want under %v", elapsed, FlushTimeout+time.Second)
		}

		logOutput := logs.String()
		if !strings.Contains(logOutput, "level=WARN") {
			t.Errorf("missing WARN level in log: %s", logOutput)
		}
		if !strings.Contains(logOutput, "failed to persist request records") {
			t.Errorf("missing drop warning in log: %s", logOutput)
		}
		wantCount := fmt.Sprintf("count=%d", enqueued)
		if !strings.Contains(logOutput, wantCount) {
			t.Errorf("warning count missing or incorrect in log %q, want %s", logOutput, wantCount)
		}
	})

	t.Run("without lock", func(t *testing.T) {
		_, cancel := context.WithCancel(t.Context())
		defer cancel()

		var logs bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&logs, nil))
		w := NewWriter(s, "test", logger, Options{FlushInterval: time.Hour})

		const enqueued = 2
		for i := 0; i < enqueued; i++ {
			w.enqueue(store.Request{
				Script:     "test-script",
				Method:     "GET",
				URL:        "https://example.com",
				StartedAt:  time.Now(),
				DurationMs: 10,
				StatusCode: 200,
			})
		}

		cancel()
		w.Close()

		logOutput := logs.String()
		if strings.Contains(logOutput, "level=WARN") || strings.Contains(logOutput, "failed to persist request records") {
			t.Errorf("unexpected drop warning logged: %s", logOutput)
		}

		if got := requestCount(t, db); got != enqueued {
			t.Errorf("persisted records = %d, want %d", got, enqueued)
		}
	})
}

func TestWriterDefaultsAndInsertFailure(t *testing.T) {
	s, db := openTestStore(t)
	if _, err := db.ExecContext(t.Context(), `ALTER TABLE requests ADD CONSTRAINT test_check_script CHECK (script <> 'failed')`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `ALTER TABLE requests DROP CONSTRAINT IF EXISTS test_check_script`)
	})
	var logs bytes.Buffer
	w := NewWriter(s, "", slog.New(slog.NewTextHandler(&logs, nil)), Options{})
	if cap(w.records) != 1024 || w.batchSize != 100 || w.interval != 5*time.Second || w.userAgent != "notinv/dev" {
		t.Fatal("unexpected writer defaults")
	}
	w.enqueue(store.Request{Script: "failed"})
	w.Close()
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "failed to persist request records") {
		t.Fatalf("missing insert failure warning: %s", logs.String())
	}
	if count := requestCount(t, db); count != 0 {
		t.Fatalf("failed insert persisted %d rows", count)
	}
}

func TestTransportAssignsDistinctKeys(t *testing.T) {
	w := idleWriter(10, slog.Default())
	client := New("key-script", w)
	tr := client.Transport.(*transport)
	tr.base = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	})

	keys := make(map[string]bool)
	const count = 5
	for i := 0; i < count; i++ {
		req := testRequest(t, http.MethodGet, fmt.Sprintf("https://example.com/%d", i))
		resp, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}

	for i := 0; i < count; i++ {
		select {
		case record := <-w.records:
			if record.Key == "" {
				t.Errorf("record %d has empty key", i)
			}
			if keys[record.Key] {
				t.Errorf("duplicate key %q assigned", record.Key)
			}
			keys[record.Key] = true
		default:
			t.Fatalf("expected %d records, got %d", count, i)
		}
	}
}
