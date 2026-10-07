package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// CLM-3: the Dockerfile HEALTHCHECK probes /healthz on the listen port, so
// targetURL must correctly derive that probe URL from NOTINV_LISTEN — if the
// default-listen branch or the URL construction regresses, this test fails.
func TestTargetURL(t *testing.T) {
	for _, tt := range []struct {
		name    string
		listen  string
		want    string
		wantErr bool
	}{
		{name: "empty defaults to :8080", listen: "", want: "http://127.0.0.1:8080/healthz"},
		{name: "explicit :8080", listen: ":8080", want: "http://127.0.0.1:8080/healthz"},
		{name: "explicit :9000", listen: ":9000", want: "http://127.0.0.1:9000/healthz"},
		{name: "host replaced with loopback", listen: "0.0.0.0:3000", want: "http://127.0.0.1:3000/healthz"},
		{name: "malformed value with no port", listen: "nocolon", wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := targetURL(tt.listen)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("targetURL(%q) = %q, want error", tt.listen, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("targetURL(%q) returned error: %v", tt.listen, err)
			}
			if got != tt.want {
				t.Errorf("targetURL(%q) = %q, want %q", tt.listen, got, tt.want)
			}
		})
	}
}

func TestProbe(t *testing.T) {
	for _, tt := range []struct {
		name    string
		status  int
		wantErr bool
	}{
		{name: "200 healthy", status: http.StatusOK},
		{name: "503 unhealthy", status: http.StatusServiceUnavailable, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/healthz" {
					t.Errorf("request path = %q, want /healthz", r.URL.Path)
				}
				w.WriteHeader(tt.status)
			}))
			defer server.Close()

			err := probe(server.URL+"/healthz", time.Second)
			if (err != nil) != tt.wantErr {
				t.Errorf("probe() error = %v, wantErr %t", err, tt.wantErr)
			}
		})
	}

	t.Run("unreachable server", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		url := server.URL + "/healthz"
		server.Close()

		if err := probe(url, time.Second); err == nil {
			t.Error("probe() on closed server returned nil, want connection error")
		}
	})
}
