package main

import "testing"

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
