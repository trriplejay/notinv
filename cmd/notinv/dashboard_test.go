package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/trriplejay/notinv/internal/runner"
	"github.com/trriplejay/notinv/internal/store"
	"github.com/trriplejay/notinv/internal/storetest"
)

func TestDashboardEmbeddedAndAPIRoutes(t *testing.T) {
	// Neither the dashboard nor its dependencies may depend on the working directory.
	t.Chdir(t.TempDir())
	st, err := store.Open(context.Background(), storetest.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := httptest.NewServer(newMux(st, nil))
	defer srv.Close()

	for _, tc := range []struct {
		path, contentType, body string
		status                  int
	}{
		{"/", "text/html", "<title>notinv", http.StatusOK},
		{"/script.html", "text/html", "<title>notinv · Script details</title>", http.StatusOK},
		{"/script.html?name=checkout%20%26%20inventory%2F%3F%23%25%2B", "text/html", "<title>notinv · Script details</title>", http.StatusOK},
		{"/vendor/chart.umd.min.js", "javascript", "Chart.js v4.5.1", http.StatusOK},
		{"/vendor/chartjs-adapter-date-fns.bundle.min.js", "javascript", "chartjs-adapter-date-fns v3.0.0", http.StatusOK},
		{"/missing.html", "text/plain", "404", http.StatusNotFound},
		{"/api/scripts", "application/json", "[]", http.StatusOK},
		{"/api/scripts/example/runs", "application/json", "[]", http.StatusOK},
		{"/api/scripts/example/requests", "application/json", "[]", http.StatusOK},
		{"/api/scripts/example/requests/series", "application/json", "[]", http.StatusOK},
		{"/api/scripts/example/requests/series?since=invalid", "text/plain", "since must be RFC3339", http.StatusBadRequest},
		{"/healthz", "", "", http.StatusOK},
	} {
		t.Run(tc.path, func(t *testing.T) {
			resp, err := srv.Client().Get(srv.URL + tc.path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != tc.status || !strings.Contains(resp.Header.Get("Content-Type"), tc.contentType) || !strings.Contains(string(body), tc.body) {
				t.Fatalf("%s: status=%d type=%s body=%.120s", tc.path, resp.StatusCode, resp.Header.Get("Content-Type"), body)
			}
		})
	}
}

func TestDashboardScriptsSchedule(t *testing.T) {
	t.Chdir(t.TempDir())
	st, err := store.Open(context.Background(), storetest.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.InsertRun(context.Background(), store.Run{Script: "example"}); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	newMux(st, map[string]string{"example": "@hourly"}).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/scripts", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/scripts: status=%d body=%s", w.Code, w.Body.String())
	}
	var scripts []struct {
		Name     string `json:"name"`
		Schedule string `json:"schedule"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &scripts); err != nil {
		t.Fatal(err)
	}
	if len(scripts) != 1 || scripts[0].Name != "example" || scripts[0].Schedule != "@hourly" {
		t.Fatalf("GET /api/scripts: scripts=%+v; want example with schedule @hourly", scripts)
	}
}

type countingScript struct {
	name     string
	schedule string
	calls    *atomic.Int32
}

func (s countingScript) Name() string     { return s.name }
func (s countingScript) Schedule() string { return s.schedule }
func (s countingScript) Run(ctx context.Context, _ *runner.Context) error {
	s.calls.Add(1)
	return nil
}

// CLM-11: integration test exercising disable, restart and still-disabled across
// the real mux, store and runner. Run count stops after disable POST, stays 0 after
// restart and cron ticks, and historical runs remain queryable.
func TestDashboardScriptDisableAndRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dsn := storetest.DatabaseURL(t)
		ctx := t.Context()

		st, err := store.Open(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if st != nil {
				_ = st.Close()
			}
		}()

		const scriptName = "counter"
		var calls atomic.Int32
		script := countingScript{
			name:     scriptName,
			schedule: "@every 1s",
			calls:    &calls,
		}

		schedules := map[string]string{scriptName: script.Schedule()}
		mux := newMux(st, schedules)

		var logBuf bytes.Buffer
		rc := &runner.Context{
			Log: slog.New(slog.NewTextHandler(&logBuf, nil)),
		}

		// 1. Start runner. First iteration runs immediately.
		runnerCtx, cancelRunner := context.WithCancel(ctx)
		defer cancelRunner()
		runnerDone := make(chan error, 1)
		go func() {
			runnerDone <- runner.Run(runnerCtx, script, rc, st)
		}()

		synctest.Wait()
		if calls.Load() < 1 {
			t.Fatalf("expected script to run at least once, got %d", calls.Load())
		}

		// Advance time so another run occurs.
		time.Sleep(1 * time.Second)
		synctest.Wait()
		initialCount := calls.Load()
		if initialCount < 2 {
			t.Fatalf("expected script to have run at least twice, got %d", initialCount)
		}

		// 2. Disable the script via POST /api/scripts/{name}/enabled
		{
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/api/scripts/"+scriptName+"/enabled", strings.NewReader(`{"enabled":false}`))
			r.Header.Set("Content-Type", "application/json")
			mux.ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("POST disable failed: code=%d body=%s", w.Code, w.Body.String())
			}
		}

		// Verify that further ticks do not increment calls.
		time.Sleep(2 * time.Second)
		synctest.Wait()
		countAfterDisable := calls.Load()
		if countAfterDisable != initialCount {
			t.Fatalf("runs continued after disable: got %d, want %d", countAfterDisable, initialCount)
		}

		// Stop runner 1
		cancelRunner()
		<-runnerDone

		// 3. Restart: Close store, re-Open it, reset counter, start a fresh runner.Run.
		if err := st.Close(); err != nil {
			t.Fatalf("store.Close: %v", err)
		}
		st = nil

		stRestart, err := store.Open(ctx, dsn)
		if err != nil {
			t.Fatalf("store.Open restart: %v", err)
		}
		st = stRestart

		muxRestart := newMux(st, schedules)

		calls.Store(0)
		restartCtx, cancelRestart := context.WithCancel(ctx)
		defer cancelRestart()
		restartDone := make(chan error, 1)
		go func() {
			restartDone <- runner.Run(restartCtx, script, rc, st)
		}()

		// Immediate first iteration check: disabled script should be skipped.
		synctest.Wait()
		if got := calls.Load(); got != 0 {
			t.Fatalf("disabled script ran on startup after restart: got %d, want 0", got)
		}

		// Advance through two cron ticks: count stays 0.
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if got := calls.Load(); got != 0 {
			t.Fatalf("disabled script ran during cron ticks after restart: got %d, want 0", got)
		}

		cancelRestart()
		<-restartDone

		// 4. Assert GET /api/scripts/{name}/runs still returns earlier rows.
		{
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/api/scripts/"+scriptName+"/runs", nil)
			muxRestart.ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("GET /api/scripts/%s/runs: code=%d body=%s", scriptName, w.Code, w.Body.String())
			}
			var runs []struct {
				Start    string  `json:"start"`
				OK       bool    `json:"ok"`
				Duration int64   `json:"duration"`
				Error    *string `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &runs); err != nil {
				t.Fatalf("unmarshal runs: %v", err)
			}
			if len(runs) != int(initialCount) {
				t.Fatalf("runs count = %d, want %d (initial runs before disable)", len(runs), initialCount)
			}
		}
	})
}

// CLM-6 & CLM-7: newMux registers POST /api/scripts/{name}/enabled bare, with no auth wrapper.
// Requests succeed with no credentials, cookies, or auth headers.
func TestNewMuxScriptEnabledRoute(t *testing.T) {
	st, err := store.Open(t.Context(), storetest.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	schedules := map[string]string{"alpha": "@daily"}
	mux := newMux(st, schedules)

	getScripts := func() []struct {
		Name    string `json:"name"`
		Enabled bool   `json:"enabled"`
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/api/scripts", nil)
		mux.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("GET /api/scripts status = %d", w.Code)
		}
		var list []struct {
			Name    string `json:"name"`
			Enabled bool   `json:"enabled"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
			t.Fatal(err)
		}
		return list
	}

	// 1. Initial state: alpha defaults to enabled: true
	list := getScripts()
	if len(list) != 1 || list[0].Name != "alpha" || !list[0].Enabled {
		t.Fatalf("initial GET /api/scripts: got %+v, want alpha enabled=true", list)
	}
	enabled, err := st.ScriptEnabled(t.Context(), "alpha")
	if err != nil || !enabled {
		t.Fatalf("initial st.ScriptEnabled: got (%v, %v), want (true, nil)", enabled, err)
	}

	// 2. Disable: POST false with NO Authorization header or cookie (CLM-7)
	{
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/api/scripts/alpha/enabled", strings.NewReader(`{"enabled":false}`))
		r.Header.Set("Content-Type", "application/json")
		mux.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("POST /api/scripts/alpha/enabled status = %d, body = %s", w.Code, w.Body.String())
		}
		var resp struct {
			Name    string `json:"name"`
			Enabled bool   `json:"enabled"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.Name != "alpha" || resp.Enabled {
			t.Fatalf("POST response: got %+v, want name=alpha, enabled=false", resp)
		}

		// Verify state reflected in store and GET /api/scripts
		enabled, err = st.ScriptEnabled(t.Context(), "alpha")
		if err != nil || enabled {
			t.Fatalf("st.ScriptEnabled after disable: got (%v, %v), want (false, nil)", enabled, err)
		}
		list = getScripts()
		if len(list) != 1 || list[0].Enabled {
			t.Fatalf("GET /api/scripts after disable: got %+v, want alpha enabled=false", list)
		}
	}

	// 3. Re-enable: POST true with NO Authorization header or cookie (CLM-7)
	{
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/api/scripts/alpha/enabled", strings.NewReader(`{"enabled":true}`))
		r.Header.Set("Content-Type", "application/json")
		mux.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("POST /api/scripts/alpha/enabled (re-enable) status = %d, body = %s", w.Code, w.Body.String())
		}
		enabled, err = st.ScriptEnabled(t.Context(), "alpha")
		if err != nil || !enabled {
			t.Fatalf("st.ScriptEnabled after enable: got (%v, %v), want (true, nil)", enabled, err)
		}
		list = getScripts()
		if len(list) != 1 || !list[0].Enabled {
			t.Fatalf("GET /api/scripts after enable: got %+v, want alpha enabled=true", list)
		}
	}

	// 4. Unknown name returns 404 and stores nothing
	{
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/api/scripts/unknown-script/enabled", strings.NewReader(`{"enabled":false}`))
		r.Header.Set("Content-Type", "application/json")
		mux.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Fatalf("POST unknown script status = %d, want 404", w.Code)
		}
		enabled, err = st.ScriptEnabled(t.Context(), "alpha")
		if err != nil || !enabled {
			t.Fatalf("st.ScriptEnabled after 404: got (%v, %v), want (true, nil)", enabled, err)
		}
	}

	// 5. Bad body returns 400
	{
		for _, bad := range []string{"malformed json", `{}`, `{"enabled":"yes"}`} {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/api/scripts/alpha/enabled", strings.NewReader(bad))
			r.Header.Set("Content-Type", "application/json")
			mux.ServeHTTP(w, r)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("POST bad body %q status = %d, want 400", bad, w.Code)
			}
		}
		enabled, err = st.ScriptEnabled(t.Context(), "alpha")
		if err != nil || !enabled {
			t.Fatalf("st.ScriptEnabled after 400: got (%v, %v), want (true, nil)", enabled, err)
		}
	}

	// 6. GET to the toggle route returns a non-2xx status and leaves state unchanged
	{
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/api/scripts/alpha/enabled", nil)
		mux.ServeHTTP(w, r)
		if w.Code >= 200 && w.Code < 300 {
			t.Fatalf("GET toggle route status = %d, want non-2xx", w.Code)
		}
		enabled, err = st.ScriptEnabled(t.Context(), "alpha")
		if err != nil || !enabled {
			t.Fatalf("st.ScriptEnabled after GET on toggle route: got (%v, %v), want (true, nil)", enabled, err)
		}
		list = getScripts()
		if len(list) != 1 || !list[0].Enabled {
			t.Fatalf("GET /api/scripts after GET on toggle route: got %+v, want alpha enabled=true", list)
		}
	}

	// 7. Non-JSON Content-Type (e.g. cross-site text/plain form POST) returns 415 and leaves state unchanged
	{
		for _, ct := range []string{"text/plain", "application/x-www-form-urlencoded", ""} {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/api/scripts/alpha/enabled", strings.NewReader(`{"enabled":false,"a":"="}`))
			if ct != "" {
				r.Header.Set("Content-Type", ct)
			}
			mux.ServeHTTP(w, r)
			if w.Code != http.StatusUnsupportedMediaType {
				t.Fatalf("POST Content-Type %q status = %d, want 415", ct, w.Code)
			}
		}
		enabled, err = st.ScriptEnabled(t.Context(), "alpha")
		if err != nil || !enabled {
			t.Fatalf("st.ScriptEnabled after 415: got (%v, %v), want (true, nil)", enabled, err)
		}
	}
}
