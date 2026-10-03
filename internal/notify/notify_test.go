package notify_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/trriplejay/notinv/internal/config"
	"github.com/trriplejay/notinv/internal/notify"
	"github.com/trriplejay/notinv/internal/runner"
)

func configured() config.Config {
	return config.Config{DiscordToken: "test-token", DiscordUserID: "recipient"}
}

func TestSendAndCache(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bot test-token" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected request: %s, headers %v", r.Method, r.Header)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		switch r.URL.Path {
		case "/api/v10/users/@me/channels":
			if !reflect.DeepEqual(body, map[string]string{"recipient_id": "recipient"}) {
				t.Errorf("channel body = %v", body)
			}
			_, _ = io.WriteString(w, `{"id":"dm"}`)
		case "/api/v10/channels/dm/messages":
			if !reflect.DeepEqual(body, map[string]string{"content": "hello \"Discord\"\n"}) {
				t.Errorf("message body = %v", body)
			}
			w.WriteHeader(http.StatusCreated)
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	n := notify.New(configured(), nil, notify.Options{BaseURL: server.URL + "/api/v10/", HTTPClient: server.Client()})
	rc := runner.Context{Notify: n.Send}
	for range 2 {
		if err := rc.Notify(context.Background(), "hello \"Discord\"\n"); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"/api/v10/users/@me/channels", "/api/v10/channels/dm/messages", "/api/v10/channels/dm/messages"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("request order = %v, want %v", paths, want)
	}
}

func TestRateLimit(t *testing.T) {
	for _, endpoint := range []string{"channels", "messages"} {
		for _, source := range []string{"body", "header"} {
			t.Run(endpoint+"/"+source, func(t *testing.T) {
				var attempts, delivered int
				var waited atomic.Int64
				var sleeps []time.Duration
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.HasSuffix(r.URL.Path, "/"+endpoint) {
						attempts++
						if attempts == 1 {
							w.Header().Set("Retry-After", "0.75")
							w.WriteHeader(http.StatusTooManyRequests)
							if source == "body" {
								_, _ = io.WriteString(w, `{"retry_after":0.5}`)
							}
							return
						}
						minimum := 500 * time.Millisecond
						if source == "header" {
							minimum = 750 * time.Millisecond
						}
						if time.Duration(waited.Load()) < minimum {
							t.Error("request retried before the rate-limit wait")
						}
					}
					if strings.HasSuffix(r.URL.Path, "/messages") {
						delivered++
					}
					_, _ = io.WriteString(w, `{"id":"dm"}`)
				}))
				defer server.Close()
				n := notify.New(configured(), nil, notify.Options{
					BaseURL: server.URL, HTTPClient: server.Client(),
					Sleep: func(d time.Duration) {
						sleeps = append(sleeps, d)
						waited.Add(int64(d))
					},
				})
				if err := n.Send(context.Background(), "limited"); err != nil {
					t.Fatal(err)
				}
				want := 500 * time.Millisecond
				if source == "header" {
					want = 750 * time.Millisecond
				}
				if attempts != 2 || delivered != 1 || !reflect.DeepEqual(sleeps, []time.Duration{want}) {
					t.Fatalf("attempts=%d delivered=%d sleeps=%v, want 2, 1, [%v]", attempts, delivered, sleeps, want)
				}
			})
		}
	}
}

func TestTransientRetries(t *testing.T) {
	for _, kind := range []string{"server", "network"} {
		for _, recoverRequest := range []bool{false, true} {
			name := kind + "/bounded"
			if recoverRequest {
				name = kind + "/recovered"
			}
			t.Run(name, func(t *testing.T) {
				const capAttempts = 3
				var attempts, delivered atomic.Int32
				var sleeps []time.Duration
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.HasSuffix(r.URL.Path, "/messages") {
						if kind == "server" && (attempts.Add(1) < capAttempts || !recoverRequest) {
							w.WriteHeader(http.StatusServiceUnavailable)
							return
						}
						delivered.Add(1)
					}
					_, _ = io.WriteString(w, `{"id":"dm"}`)
				}))
				defer server.Close()
				client := server.Client()
				transport := client.Transport
				client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
					if kind == "network" && strings.HasSuffix(r.URL.Path, "/messages") && (attempts.Add(1) < capAttempts || !recoverRequest) {
						return nil, errors.New("temporary connection failure")
					}
					return transport.RoundTrip(r)
				})
				n := notify.New(configured(), nil, notify.Options{
					BaseURL: server.URL, HTTPClient: client, MaxAttempts: capAttempts,
					Sleep: func(d time.Duration) { sleeps = append(sleeps, d) },
				})
				// This safety deadline makes a removed attempt cap fail instead of
				// hanging the suite indefinitely; exact counts still decide the test.
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				err := n.Send(ctx, "retry me")
				if (err == nil) != recoverRequest {
					t.Fatalf("Send error = %v, recovery = %v", err, recoverRequest)
				}
				if attempts.Load() != capAttempts || !reflect.DeepEqual(sleeps, []time.Duration{time.Second, 2 * time.Second}) {
					t.Fatalf("attempts=%d sleeps=%v", attempts.Load(), sleeps)
				}
				wantDelivered := int32(0)
				if recoverRequest {
					wantDelivered = 1
				}
				if delivered.Load() != wantDelivered {
					t.Fatalf("delivered=%d want=%d", delivered.Load(), wantDelivered)
				}
			})
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDryRun(t *testing.T) {
	for _, cfg := range []config.Config{
		{DiscordToken: "token", DiscordUserID: "recipient", DiscordDryRun: true},
		{DiscordUserID: "recipient"},
	} {
		var log bytes.Buffer
		client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Error("dry-run made an HTTP request")
			return nil, errors.New("unexpected network call")
		})}
		n := notify.New(cfg, slog.New(slog.NewTextHandler(&log, nil)), notify.Options{HTTPClient: client, MaxAttempts: 1})
		if err := n.Send(context.Background(), "dry message"); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(log.String(), "dry message") || !strings.Contains(log.String(), "notify (dry-run)") {
			t.Fatalf("missing dry-run message: %s", &log)
		}
	}
}

func TestConcurrentCache(t *testing.T) {
	var resolutions, messages atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/@me/channels":
			resolutions.Add(1)
		case "/channels/dm/messages":
			messages.Add(1)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"id":"dm"}`)
	}))
	defer server.Close()
	n := notify.New(configured(), nil, notify.Options{BaseURL: server.URL, HTTPClient: server.Client()})
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			if err := n.Send(context.Background(), "concurrent"); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if resolutions.Load() != 1 || messages.Load() != 10 {
		t.Fatalf("resolutions=%d messages=%d", resolutions.Load(), messages.Load())
	}
}

type fakeSender struct{ message string }

func (f *fakeSender) Send(_ context.Context, msg string) error { f.message = msg; return nil }

func TestSenderSubstitution(t *testing.T) {
	fake := &fakeSender{}
	var sender notify.Sender = fake
	rc := runner.Context{Notify: sender.Send}
	if err := rc.Notify(context.Background(), "fake message"); err != nil {
		t.Fatal(err)
	}
	if fake.message != "fake message" {
		t.Fatalf("fake received %q", fake.message)
	}
}
