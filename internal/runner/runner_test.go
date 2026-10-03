package runner

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/trriplejay/notinv/internal/config"
)

// TestNewContextNotify verifies the notifier-backed dry-run caller path.
func TestNewContextNotify(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil)).With("script", "example")
	client := &http.Client{Transport: notifyTransport(func(*http.Request) (*http.Response, error) {
		t.Error("dry-run made an HTTP request")
		return nil, errors.New("unexpected HTTP request")
	})}
	rc := NewContext(log, client, &config.Config{DiscordDryRun: true})
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

type notifyTransport func(*http.Request) (*http.Response, error)

func (f notifyTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestNewContextConfiguredNotify(t *testing.T) {
	var paths []string
	client := &http.Client{Transport: notifyTransport(func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.Path)
		if r.URL.Host != "discord.com" || r.Header.Get("Authorization") != "Bot configured-token" {
			t.Errorf("unexpected notifier request: %s, auth %q", r.URL, r.Header.Get("Authorization"))
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		_ = r.Body.Close()
		if len(paths) == 1 && string(body) != `{"recipient_id":"configured-user"}` {
			t.Errorf("resolution body = %s", body)
		}
		if len(paths) == 2 && string(body) != `{"content":"real message"}` {
			t.Errorf("message body = %s", body)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":"dm"}`))}, nil
	})}
	rc := NewContext(slog.Default(), client, &config.Config{DiscordToken: "configured-token", DiscordUserID: "configured-user"})
	if err := rc.Notify(context.Background(), "real message"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(paths, ",") != "/api/v10/users/@me/channels,/api/v10/channels/dm/messages" {
		t.Fatalf("notifier requests = %v", paths)
	}
}
