package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/trriplejay/notinv/internal/store"
)

var (
	_ ScriptLister   = (*store.Store)(nil)
	_ RunQuerier     = (*store.Store)(nil)
	_ RequestQuerier = (*store.Store)(nil)
	_ Pinger         = (*store.Store)(nil)
)

var testNow = time.Date(2026, time.October, 5, 12, 0, 0, 0, time.UTC)

func fixedNow() time.Time { return testNow }

type fakeStore struct {
	runs        []store.Run
	requests    []store.Request
	listErr     error
	latestErr   error
	runsErr     error
	requestsErr error
	pingErr     error
	pingCalls   int
	ctx         context.Context
	script      string
	since       time.Time
	until       time.Time
}

func (f *fakeStore) ListScripts(ctx context.Context) ([]string, error) {
	f.ctx = ctx
	names := make(map[string]bool)
	for _, run := range f.runs {
		names[run.Script] = true
	}
	for _, request := range f.requests {
		names[request.Script] = true
	}
	var out []string
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, f.listErr
}

func (f *fakeStore) LatestRun(ctx context.Context, script string) (*store.Run, error) {
	f.ctx = ctx
	var latest *store.Run
	for _, run := range f.runs {
		if run.Script == script && (latest == nil || run.StartedAt.After(latest.StartedAt)) {
			latest = &run
		}
	}
	return latest, f.latestErr
}

func (f *fakeStore) QueryRuns(ctx context.Context, script string, since, until time.Time) ([]store.Run, error) {
	f.ctx, f.script, f.since, f.until = ctx, script, since, until
	var out []store.Run
	for _, run := range f.runs {
		if run.Script == script && !run.StartedAt.Before(since) && !run.StartedAt.After(until) {
			out = append(out, run)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out, f.runsErr
}

func (f *fakeStore) QueryRequests(ctx context.Context, script string, since, until time.Time) ([]store.Request, error) {
	f.ctx, f.script, f.since, f.until = ctx, script, since, until
	var out []store.Request
	for _, request := range f.requests {
		if request.Script == script && !request.StartedAt.Before(since) && !request.StartedAt.After(until) {
			out = append(out, request)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out, f.requestsErr
}

func (f *fakeStore) Ping(ctx context.Context) error {
	f.ctx = ctx
	f.pingCalls++
	return f.pingErr
}

func serve(t *testing.T, pattern, target string, handler http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(pattern, handler)
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

func assertJSON(t *testing.T, w *httptest.ResponseRecorder, want string) {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	var gotValue, wantValue any
	if err := json.Unmarshal(w.Body.Bytes(), &gotValue); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Errorf("body = %s; want %s", w.Body.String(), want)
	}
}

func assertWindow(t *testing.T, f *fakeStore, script string, since time.Time) {
	t.Helper()
	if f.script != script || !f.since.Equal(since) || !f.until.Equal(testNow) {
		t.Errorf("query = %q [%s, %s]; want %q [%s, %s]", f.script, f.since, f.until, script, since, testNow)
	}
	if f.ctx != t.Context() {
		t.Error("request context not forwarded to dependency")
	}
}

// CLM-1: the constructors return usable handlers, including with default clocks.
func TestHandlerConstructors(t *testing.T) {
	f := &fakeStore{}
	for name, handler := range map[string]http.Handler{
		"scripts":  NewScriptsHandler(f, nil),
		"runs":     NewRunsHandler(f, nil),
		"requests": NewRequestsHandler(f, nil),
		"health":   NewHealthHandler(f),
	} {
		t.Run(name, func(t *testing.T) {
			if handler == nil || reflect.ValueOf(handler).IsNil() {
				t.Fatal("nil handler")
			}
		})
	}
}

// CLM-2: latest shape, bounded uptime ratio, blank schedule, and multiple scripts.
func TestScriptsSummary(t *testing.T) {
	failure := "check failed"
	f := &fakeStore{runs: []store.Run{
		{Script: "alpha", StartedAt: testNow.Add(-10 * 24 * time.Hour), OK: true},
		{Script: "alpha", StartedAt: testNow.Add(-7 * 24 * time.Hour), OK: true},
		{Script: "alpha", StartedAt: testNow.Add(-24 * time.Hour), OK: true},
		{Script: "alpha", StartedAt: testNow, Err: &failure},
		{Script: "beta", StartedAt: testNow, OK: true},
	}}
	w := serve(t, "GET /api/scripts", "/api/scripts", NewScriptsHandler(f, fixedNow))
	assertJSON(t, w, `[
		{"name":"alpha","latest":{"time":"2026-10-05T12:00:00Z","ok":false,"error":"check failed"},"uptime":0.6666666666666666,"schedule":""},
		{"name":"beta","latest":{"time":"2026-10-05T12:00:00Z","ok":true,"error":null},"uptime":1,"schedule":""}
	]`)
	assertWindow(t, f, "beta", testNow.Add(-7*24*time.Hour))
}

// CLM-3: a request-only script must not disappear or divide by zero.
func TestScriptsWithoutRuns(t *testing.T) {
	f := &fakeStore{requests: []store.Request{{Script: "request-only", StartedAt: testNow, URL: "https://example.com"}}}
	w := serve(t, "GET /api/scripts", "/api/scripts", NewScriptsHandler(f, fixedNow))
	// JSON parsing rejects NaN/Infinity; exact equality also distinguishes null/absent.
	assertJSON(t, w, `[{"name":"request-only","latest":null,"uptime":0,"schedule":""}]`)
}

// CLM-4: since parsing, inclusive bounds, ascending output, and a seven-day default.
func TestRunsWindow(t *testing.T) {
	failure := "failed"
	for _, tc := range []struct {
		name, query, want string
		since             time.Time
	}{
		{"omitted", "", `[{"start":"2026-10-02T12:00:00Z","ok":true,"duration":12,"error":null}]`, testNow.Add(-7 * 24 * time.Hour)},
		{"empty", "?since=", `[{"start":"2026-10-02T12:00:00Z","ok":true,"duration":12,"error":null}]`, testNow.Add(-7 * 24 * time.Hour)},
		{"explicit", "?since=" + url.QueryEscape("2026-09-25T14:00:00+02:00"), `[{"start":"2026-09-25T12:00:00Z","ok":false,"duration":91,"error":"failed"},{"start":"2026-10-02T12:00:00Z","ok":true,"duration":12,"error":null}]`, testNow.Add(-10 * 24 * time.Hour)},
		{"future", "?since=2026-10-06T12:00:00Z", `[]`, testNow.Add(24 * time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeStore{runs: []store.Run{
				{Script: "alpha", StartedAt: testNow.Add(-3 * 24 * time.Hour), OK: true, DurationMs: 12},
				{Script: "alpha", StartedAt: testNow.Add(-10 * 24 * time.Hour), DurationMs: 91, Err: &failure},
				{Script: "alpha", StartedAt: testNow.Add(time.Second)},
				{Script: "other", StartedAt: testNow},
			}}
			w := serve(t, "GET /api/scripts/{name}/runs", "/api/scripts/alpha/runs"+tc.query, NewRunsHandler(f, fixedNow))
			assertJSON(t, w, tc.want)
			assertWindow(t, f, "alpha", tc.since)
		})
	}
}

// CLM-5: unknown and known-but-empty histories both serialize as [] with 200.
func TestRunsEmpty(t *testing.T) {
	f := &fakeStore{runs: []store.Run{{Script: "old", StartedAt: testNow.Add(-10 * 24 * time.Hour)}}}
	for _, name := range []string{"unknown", "old"} {
		w := serve(t, "GET /api/scripts/{name}/runs", "/api/scripts/"+name+"/runs", NewRunsHandler(f, fixedNow))
		assertJSON(t, w, `[]`)
	}
}

// CLM-6: exact aggregate fields, error OR semantics, fractional means and windows.
func TestRequestsAggregates(t *testing.T) {
	empty, failure := "", "network error"
	for _, tc := range []struct {
		name, query string
		since       time.Time
	}{
		{"default", "", testNow.Add(-7 * 24 * time.Hour)},
		{"empty", "?since=", testNow.Add(-7 * 24 * time.Hour)},
		{"explicit", "?since=2026-10-04T12:00:00Z", testNow.Add(-24 * time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeStore{requests: []store.Request{
				{Script: "alpha", URL: "a", StartedAt: testNow, StatusCode: 399, DurationMs: 10},
				{Script: "alpha", URL: "a", StartedAt: testNow, StatusCode: 400, DurationMs: 20},
				{Script: "alpha", URL: "a", StartedAt: testNow, StatusCode: 200, Err: &failure, DurationMs: 31},
				{Script: "alpha", URL: "a", StartedAt: testNow, StatusCode: 503, Err: &failure, DurationMs: 40},
				{Script: "alpha", URL: "b", StartedAt: testNow, StatusCode: 200, Err: &empty, DurationMs: 7},
				{Script: "alpha", URL: "b", StartedAt: tc.since, StatusCode: 200, DurationMs: 8},
				{Script: "alpha", URL: "outside", StartedAt: tc.since.Add(-time.Second)},
				{Script: "alpha", URL: "future", StartedAt: testNow.Add(time.Second)},
				{Script: "other", URL: "other", StartedAt: testNow},
			}}
			w := serve(t, "GET /api/scripts/{name}/requests", "/api/scripts/alpha/requests"+tc.query, NewRequestsHandler(f, fixedNow))
			assertJSON(t, w, `[{"url":"b","count":2,"errors":0,"avgLatency":7.5},{"url":"a","count":4,"errors":3,"avgLatency":25.25}]`)
			assertWindow(t, f, "alpha", tc.since)
		})
	}
}

// CLM-7: output size follows distinct URLs, never raw request count.
func TestRequestsBoundedByDistinctURLs(t *testing.T) {
	f := &fakeStore{}
	for i := 0; i < 300; i++ {
		f.requests = append(f.requests, store.Request{Script: "alpha", URL: fmt.Sprintf("url-%d", i%3), StartedAt: testNow, DurationMs: 9, StatusCode: 200})
	}
	w := serve(t, "GET /api/scripts/{name}/requests", "/api/scripts/alpha/requests", NewRequestsHandler(f, fixedNow))
	assertJSON(t, w, `[{"url":"url-0","count":100,"errors":0,"avgLatency":9},{"url":"url-1","count":100,"errors":0,"avgLatency":9},{"url":"url-2","count":100,"errors":0,"avgLatency":9}]`)
}

// CLM-8: health depends on the ping result and forwards the request context.
func TestHealthPing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
	}{
		{"reachable", nil, http.StatusOK},
		{"unreachable", errors.New("private database detail"), http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeStore{pingErr: tc.err}
			w := serve(t, "GET /healthz", "/healthz", NewHealthHandler(f))
			if w.Code != tc.status || f.pingCalls != 1 || f.ctx != t.Context() {
				t.Fatalf("status=%d, calls=%d, context=%v", w.Code, f.pingCalls, f.ctx)
			}
			if strings.Contains(w.Body.String(), "private") {
				t.Error("response leaked database error")
			}
		})
	}
}

func TestEmptyCollections(t *testing.T) {
	f := &fakeStore{}
	assertJSON(t, serve(t, "GET /api/scripts", "/api/scripts", NewScriptsHandler(f, fixedNow)), `[]`)
	assertJSON(t, serve(t, "GET /api/scripts/{name}/requests", "/api/scripts/unknown/requests", NewRequestsHandler(f, fixedNow)), `[]`)
}

func TestInvalidSince(t *testing.T) {
	for _, kind := range []string{"runs", "requests"} {
		f := &fakeStore{}
		h := NewRunsHandler(f, fixedNow)
		if kind == "requests" {
			h = NewRequestsHandler(f, fixedNow)
		}
		w := serve(t, "GET /api/scripts/{name}/"+kind, "/api/scripts/alpha/"+kind+"?since=not-a-time", h)
		if w.Code != http.StatusBadRequest || f.ctx != nil {
			t.Errorf("%s: status=%d, dependency called=%v", kind, w.Code, f.ctx != nil)
		}
	}
}

func TestStoreFailures(t *testing.T) {
	failure := errors.New("private database detail")
	for _, operation := range []string{"list", "latest", "script-runs", "runs", "requests"} {
		t.Run(operation, func(t *testing.T) {
			f := &fakeStore{runs: []store.Run{{Script: "alpha", StartedAt: testNow}}}
			pattern, target := "GET /api/scripts", "/api/scripts"
			h := NewScriptsHandler(f, fixedNow)
			switch operation {
			case "list":
				f.listErr = failure
			case "latest":
				f.latestErr = failure
			case "script-runs":
				f.runsErr = failure
			case "runs":
				f.runsErr = failure
				pattern, target = "GET /api/scripts/{name}/runs", "/api/scripts/alpha/runs"
				h = NewRunsHandler(f, fixedNow)
			case "requests":
				f.requestsErr = failure
				pattern, target = "GET /api/scripts/{name}/requests", "/api/scripts/alpha/requests"
				h = NewRequestsHandler(f, fixedNow)
			}
			w := serve(t, pattern, target, h)
			if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "private") {
				t.Errorf("status=%d, body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestDefaultClock(t *testing.T) {
	for _, kind := range []string{"scripts", "runs", "requests"} {
		f := &fakeStore{runs: []store.Run{{Script: "alpha", StartedAt: testNow}}}
		pattern, target := "GET /api/scripts", "/api/scripts"
		h := NewScriptsHandler(f, nil)
		if kind != "scripts" {
			pattern, target = "GET /api/scripts/{name}/"+kind, "/api/scripts/alpha/"+kind
			h = NewRunsHandler(f, nil)
			if kind == "requests" {
				h = NewRequestsHandler(f, nil)
			}
		}
		before := time.Now()
		w := serve(t, pattern, target, h)
		after := time.Now()
		if w.Code != http.StatusOK || f.until.Before(before) || f.until.After(after) || f.until.Sub(f.since) != 7*24*time.Hour {
			t.Errorf("%s: status=%d, window=[%s,%s]", kind, w.Code, f.since, f.until)
		}
	}
}
