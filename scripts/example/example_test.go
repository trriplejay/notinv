package example

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/trriplejay/notinv/internal/runner"
)

type roundTripper func(*http.Request) (*http.Response, error)

// RoundTrip returns the canned response without using the network.
func (f roundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type trackingBody struct {
	// Reader supplies the canned response content.
	io.Reader
	closed bool
}

// Close records cleanup so tests detect an unclosed response body.
func (b *trackingBody) Close() error {
	b.closed = true
	return nil
}

// TestScriptTransitions exercises the contract using only injected services.
func TestScriptTransitions(t *testing.T) {
	for _, statuses := range [][]int{
		{200, 500, 500, 200, 200},
		{500, 500, 200, 200, 500},
	} {
		t.Run(http.StatusText(statuses[0]), func(t *testing.T) {
			const url = "https://example.invalid/health"
			t.Setenv("EXAMPLE_URL", url)
			var logs bytes.Buffer
			var messages []string
			var bodies []*trackingBody
			calls := 0
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			rc := &runner.Context{
				Log: slog.New(slog.NewTextHandler(&logs, nil)),
				HTTP: &http.Client{Transport: roundTripper(func(req *http.Request) (*http.Response, error) {
					if req.Method != http.MethodGet || req.URL.String() != url {
						t.Errorf("request = %s %s, want GET %s", req.Method, req.URL, url)
					}
					if req.Context() != ctx {
						t.Error("request did not preserve the run context")
					}
					if calls >= len(statuses) {
						t.Fatal("unexpected extra request")
					}
					body := &trackingBody{Reader: strings.NewReader("unused content")}
					bodies = append(bodies, body)
					status := statuses[calls]
					calls++
					return &http.Response{StatusCode: status, Body: body, Header: make(http.Header)}, nil
				})},
				Notify: func(got context.Context, msg string) error {
					if got != ctx {
						t.Error("Notify did not preserve the run context")
					}
					messages = append(messages, msg)
					return nil
				},
			}
			var script runner.Script = &Script{}
			if script.Name() != "example" || script.Schedule() != "@every 5m" {
				t.Fatal("unexpected script identity or schedule")
			}
			wantNotifications := 0
			for i, status := range statuses {
				logs.Reset()
				err := script.Run(ctx, rc)
				if (err == nil) != (status == http.StatusOK) {
					t.Errorf("run %d: error = %v for status %d", i, err, status)
				}
				if i > 0 && statuses[i-1] != status {
					wantNotifications++
				}
				if len(messages) != wantNotifications {
					t.Fatalf("run %d: got %d notifications, want %d", i, len(messages), wantNotifications)
				}
				if i > 0 && statuses[i-1] != status {
					want := "example state changed: ok=false"
					if status == http.StatusOK {
						want = "example state changed: ok=true"
					}
					if messages[len(messages)-1] != want {
						t.Errorf("notification = %q, want %q", messages[len(messages)-1], want)
					}
				}
				if calls != i+1 || !bodies[i].closed {
					t.Fatal("each run must request once and close its response body")
				}
				wantLog := "ok=false"
				if status == http.StatusOK {
					wantLog = "ok=true"
				}
				if !strings.Contains(logs.String(), "example check completed") || !strings.Contains(logs.String(), wantLog) {
					t.Errorf("missing run result in log: %q", logs.String())
				}
			}
		})
	}
}

// TestScriptErrors verifies configuration errors and wrapped transport failures.
func TestScriptErrors(t *testing.T) {
	for _, tt := range []struct {
		name string
		url  string
		want string
	}{
		{"unset URL", "", "EXAMPLE_URL must be set"},
		{"invalid URL", "://bad", "build example request"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("EXAMPLE_URL", tt.url)
			var logs bytes.Buffer
			rc := &runner.Context{
				Log: slog.New(slog.NewTextHandler(&logs, nil)),
				HTTP: &http.Client{Transport: roundTripper(func(*http.Request) (*http.Response, error) {
					t.Fatal("invalid config must not issue a request")
					return nil, errors.New("unexpected request")
				})},
				Notify: func(context.Context, string) error {
					t.Fatal("first failure must establish a baseline without notifying")
					return nil
				},
			}
			err := new(Script).Run(context.Background(), rc)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Run error = %v, want %q", err, tt.want)
			}
		})
	}
}

// TestScriptTransportAndNotifyErrors keeps both error causes inspectable.
func TestScriptTransportAndNotifyErrors(t *testing.T) {
	t.Setenv("EXAMPLE_URL", "https://example.invalid/health")
	transportErr := errors.New("transport unavailable")
	notifyErr := errors.New("notification unavailable")
	var logs bytes.Buffer
	calls, notifications := 0, 0
	rc := &runner.Context{
		Log: slog.New(slog.NewTextHandler(&logs, nil)),
		HTTP: &http.Client{Transport: roundTripper(func(*http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(""))}, nil
			}
			return nil, transportErr
		})},
		Notify: func(context.Context, string) error {
			notifications++
			return notifyErr
		},
	}
	script := &Script{}
	if err := script.Run(context.Background(), rc); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	if notifications != 0 {
		t.Fatal("baseline notified")
	}
	err := script.Run(context.Background(), rc)
	if !errors.Is(err, transportErr) || !errors.Is(err, notifyErr) {
		t.Fatalf("Run = %v, want transport and notification causes", err)
	}
	if notifications != 1 {
		t.Fatalf("got %d notifications, want 1", notifications)
	}
	// A failed send does not change the observed endpoint state or cause retries.
	err = script.Run(context.Background(), rc)
	if !errors.Is(err, transportErr) || errors.Is(err, notifyErr) || notifications != 1 {
		t.Fatalf("steady failure: error = %v, notifications = %d", err, notifications)
	}
}
