package runner

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

// TestNewContextNotify verifies the dry-run caller path without a sender.
func TestNewContextNotify(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil)).With("script", "example")
	client := &http.Client{}
	rc := NewContext(log, client)
	if rc.Log != log || rc.HTTP != client {
		t.Fatal("NewContext did not preserve the supplied services")
	}
	if err := rc.Notify(context.Background(), "some message"); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	for _, want := range []string{"some message", "notify (dry-run)", "script=example"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("log %q does not contain %q", buf.String(), want)
		}
	}
}
