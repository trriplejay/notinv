// Command server serves the real dashboard and API over an isolated, seeded
// SQLite database for browser tests. It is not part of the notinv binary.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/trriplejay/notinv/internal/store"
	api "github.com/trriplejay/notinv/internal/web"
	dashboard "github.com/trriplejay/notinv/web"
)

func main() {
	if err := serve(); err != nil {
		log.Fatal(err)
	}
}

func serve() error {
	dir, err := os.MkdirTemp("", "notinv-browser-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	ctx := context.Background()
	db, err := store.Open(ctx, "file:"+filepath.Join(dir, "test.db"), "")
	if err != nil {
		return err
	}
	defer db.Close()
	now := time.Now().UTC().Truncate(time.Second)
	failure := "upstream unavailable"
	for i, ok := range []bool{true, true, true, false} {
		run := store.Run{Script: "browser-check", StartedAt: now.Add(time.Duration(i-4) * time.Minute), OK: ok}
		if !ok {
			run.Err = &failure
		}
		if err := db.InsertRun(ctx, run); err != nil {
			return err
		}
	}
	requests := []store.Request{
		{Script: "browser-check", Method: "GET", URL: "https://service.test/one", StartedAt: now.Add(-3 * time.Minute), StatusCode: 200, DurationMs: 20},
		{Script: "browser-check", Method: "GET", URL: "https://service.test/two", StartedAt: now.Add(-2 * time.Minute), StatusCode: 503, DurationMs: 80},
		{Script: "browser-check", Method: "GET", URL: "https://service.test/one", StartedAt: now.Add(-time.Minute), StatusCode: 302, DurationMs: 50},
	}
	if err := db.InsertRequests(ctx, requests); err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("GET /", dashboard.Handler())
	mux.Handle("GET /api/scripts", api.NewScriptsHandler(db, map[string]string{"browser-check": "@every 1m"}, nil))
	mux.Handle("GET /api/scripts/{name}/requests/series", api.NewSeriesHandler(db, nil))
	mux.Handle("GET /healthz", api.NewHealthHandler(db))
	server := &http.Server{Addr: "127.0.0.1:4173", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	fmt.Println("browser test server ready on", server.Addr)
	return server.ListenAndServe()
}
