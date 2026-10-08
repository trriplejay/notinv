package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/trriplejay/notinv/internal/store"
)

func TestDashboardEmbeddedAndAPIRoutes(t *testing.T) {
	// Neither the dashboard nor its dependencies may depend on the working directory.
	t.Chdir(t.TempDir())
	st, err := store.Open(context.Background(), "file:test.db", "")
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
	st, err := store.Open(context.Background(), "file:test.db", "")
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
