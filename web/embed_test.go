package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEmbeddedDashboard(t *testing.T) {
	// Serving must not depend on the working directory or source checkout.
	t.Chdir(t.TempDir())
	mux := http.NewServeMux()
	mux.Handle("GET /", Handler())
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	for _, tc := range []struct {
		path, contentType, body string
		status                  int
	}{
		{"/", "text/html", "<title>notinv · Monitoring</title>", http.StatusOK},
		{"/vendor/chart.umd.min.js", "javascript", "Chart.js v4.5.1", http.StatusOK},
		{"/vendor/chartjs-adapter-date-fns.bundle.min.js", "javascript", "chartjs-adapter-date-fns v3.0.0", http.StatusOK},
		{"/missing", "text/plain", "404", http.StatusNotFound},
		{"/healthz", "", "", http.StatusNoContent},
	} {
		t.Run(tc.path, func(t *testing.T) {
			response, err := server.Client().Get(server.URL + tc.path)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != tc.status || !strings.Contains(response.Header.Get("Content-Type"), tc.contentType) || !strings.Contains(string(body), tc.body) {
				t.Fatalf("unexpected response: status=%d content-type=%s body-length=%d", response.StatusCode, response.Header.Get("Content-Type"), len(body))
			}
			if tc.path == "/" {
				want, err := staticFS.ReadFile("static/index.html")
				if err != nil {
					t.Fatal(err)
				}
				if string(body) != string(want) {
					t.Fatal("root did not return the embedded index.html body")
				}
			}
		})
	}
}
