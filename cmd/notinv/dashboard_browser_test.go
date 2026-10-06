package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/trriplejay/notinv/internal/store"
)

// TestDashboardBrowserServer is the opt-in real API/SQLite assembly used by
// web/tests/dashboard.test.cjs. Ordinary Go runs exercise the finite route test.
func TestDashboardBrowserServer(t *testing.T) {
	listen := os.Getenv("NOTINV_BROWSER_TEST_LISTEN")
	if listen == "" {
		return
	}
	ctx := context.Background()
	st, err := store.Open(ctx, "file:"+filepath.Join(t.TempDir(), "browser.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	now := time.Now().UTC().Truncate(time.Second)
	const name = "checkout & inventory"
	const other = "<img src=x onerror=alert(1)>"
	failure := "upstream timeout"
	for _, run := range []store.Run{
		{Script: name, StartedAt: now.Add(-time.Minute), OK: true},
		{Script: name, StartedAt: now.Add(-2 * time.Minute), OK: false, Err: &failure},
	} {
		if err := st.InsertRun(ctx, run); err != nil {
			t.Fatal(err)
		}
	}
	requests := []store.Request{
		{Script: name, URL: "https://shop.example/cart", StartedAt: now.Add(-10 * time.Minute), DurationMs: 10, StatusCode: 200},
		{Script: name, URL: "https://shop.example/cart", StartedAt: now.Add(-9 * time.Minute), DurationMs: 20, StatusCode: 302},
		{Script: name, URL: "https://shop.example/cart", StartedAt: now.Add(-8 * time.Minute), DurationMs: 30, StatusCode: 404},
		{Script: name, URL: "https://shop.example/cart", StartedAt: now.Add(-7 * time.Minute), DurationMs: 40, StatusCode: 503},
		{Script: name, URL: "https://shop.example/stock", StartedAt: now.Add(-6 * time.Minute), DurationMs: 50, StatusCode: 200, Err: &failure},
		{Script: name, URL: "https://shop.example/stock", StartedAt: now.Add(-5 * time.Minute), DurationMs: 60, StatusCode: 0},
		{Script: name, URL: "https://shop.example/stock", StartedAt: now.Add(-2 * time.Hour), DurationMs: 70, StatusCode: 200},
		{Script: name, URL: "https://shop.example/stock", StartedAt: now.Add(-48 * time.Hour), DurationMs: 80, StatusCode: 200},
		// A request-only script exercises latest:null through the real API.
		{Script: other, URL: "https://shop.example/health", StartedAt: now.Add(-time.Minute), DurationMs: 12, StatusCode: 200},
	}
	if err := st.InsertRequests(ctx, requests); err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{
		Addr: listen, Handler: newMux(st, map[string]string{name: "*/5 * * * *", other: "@hourly"}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	defer func() { _ = srv.Close() }()
	t.Log("browser assembly ready")
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		t.Fatal(err)
	}
}
