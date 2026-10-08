package nintendo

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/robfig/cron/v3"

	"github.com/trriplejay/notinv/internal/runner"
)

// These reduced fixtures preserve the live page's JSON-LD graph, offer, SKU,
// and gallery-array shapes. The in-stock fixture changes only availability.
const unavailablePage = `<html><script type="application/ld+json" data-next-head="">
{"@context":"https://schema.org/","@graph":[
{"@type":["Product"],"name":"Electric OCARINA OF TIME","offers":{"@type":"Offer","url":"/us/store/products/electric-ocarina-of-time-129987/","availability":"https://schema.org/OutOfStock"},"sku":"129987"},
[{"@type":"ImageObject","name":"Electric OCARINA OF TIME gallery item 0"}]
]}</script></html>`

var purchasablePage = strings.Replace(unavailablePage, "OutOfStock", "InStock", 1)

type roundTripper func(*http.Request) (*http.Response, error)

// RoundTrip returns the canned response without using the network.
func (f roundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type trackingBody struct {
	// Reader supplies the canned response content.
	io.Reader
	closed   bool
	closeErr error
}

// Close records cleanup so tests detect an unclosed response body.
func (b *trackingBody) Close() error {
	b.closed = true
	return b.closeErr
}

// TestScriptSchedule checks identity and the exact parser used by the runner.
func TestScriptSchedule(t *testing.T) {
	script := New()
	if script.Name() != "nintendo" || script.Schedule() != "@every 1m" {
		t.Fatal("unexpected script identity or schedule")
	}
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
	if _, err := parser.Parse(script.Schedule()); err != nil {
		t.Fatalf("parse schedule: %v", err)
	}
}

// TestScriptTransitions exercises fetch -> determine -> notify through a real
// runner.Context, with only HTTP transport and notification delivery replaced.
func TestScriptTransitions(t *testing.T) {
	for _, tt := range []struct {
		name   string
		states []bool
		counts []int
	}{
		{"unavailable baseline", []bool{false, false, true, true, false, true}, []int{0, 0, 1, 1, 1, 2}},
		{"purchasable baseline", []bool{true, true, false, false, true, true}, []int{0, 0, 0, 0, 1, 1}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			const url = "https://www.nintendo.com/us/store/products/electric-ocarina-of-time-129987/"
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
					if calls >= len(tt.states) {
						t.Fatal("unexpected extra request")
					}
					page := unavailablePage
					if tt.states[calls] {
						page = purchasablePage
					}
					body := &trackingBody{Reader: strings.NewReader(page)}
					bodies = append(bodies, body)
					calls++
					return &http.Response{StatusCode: http.StatusOK, Body: body, Header: make(http.Header)}, nil
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
			for i, purchasable := range tt.states {
				logs.Reset()
				err := script.Run(ctx, rc)
				if (err == nil) != purchasable {
					t.Errorf("run %d: error = %v, purchasable = %t", i, err, purchasable)
				}
				if len(messages) != tt.counts[i] {
					t.Fatalf("run %d: got %d notifications, want %d", i, len(messages), tt.counts[i])
				}
				if calls != i+1 || !bodies[i].closed {
					t.Fatal("each run must request once and close its response body")
				}
				wantLog := "ok=false"
				if purchasable {
					wantLog = "ok=true"
				}
				if !strings.Contains(logs.String(), "nintendo check completed") || !strings.Contains(logs.String(), wantLog) {
					t.Errorf("missing run result in log: %q", logs.String())
				}
			}
			for _, message := range messages {
				if message != "nintendo item is purchasable: "+url {
					t.Errorf("notification = %q", message)
				}
			}
		})
	}
}

// TestScriptDetermination rejects unsuccessful, missing, and unrelated signals.
func TestScriptDetermination(t *testing.T) {
	for _, tt := range []struct {
		name        string
		status      int
		page        string
		purchasable bool
	}{
		{"in stock", 200, purchasablePage, true},
		{"out of stock", 200, unavailablePage, false},
		{"sold out", 200, strings.Replace(unavailablePage, "OutOfStock", "SoldOut", 1), false},
		{"preorder", 200, strings.Replace(unavailablePage, "OutOfStock", "PreOrder", 1), false},
		{"server error despite stock marker", 503, purchasablePage, false},
		{"not found", 404, purchasablePage, false},
		{"no marker", 200, "<html>Add to cart</html>", false},
		{"unrelated product", 200, strings.ReplaceAll(purchasablePage, "129987", "120831"), false},
		{"recommendation before unavailable target", 200, strings.ReplaceAll(purchasablePage, "129987", "120831") + unavailablePage, false},
		{"invalid JSON", 200, `<script type="application/ld+json">{</script>`, false},
		{"gallery before product", 200, strings.Replace(purchasablePage, `"@graph":[`, `"@graph":[[{"@type":"ImageObject"}],`, 1), true},
		{"oversized", 200, purchasablePage + strings.Repeat(" ", 1<<20), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body := &trackingBody{Reader: strings.NewReader(tt.page)}
			rc := &runner.Context{
				Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
				HTTP: &http.Client{Transport: roundTripper(func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: tt.status, Body: body}, nil
				})},
				Notify: func(context.Context, string) error {
					t.Fatal("first observation must not notify")
					return nil
				},
			}
			script := New()
			err := script.Run(context.Background(), rc)
			if (err == nil) != tt.purchasable || script.lastOK != tt.purchasable {
				t.Fatalf("Run = %v, lastOK = %t, want purchasable = %t", err, script.lastOK, tt.purchasable)
			}
			if !body.closed {
				t.Fatal("response body was not closed")
			}
		})
	}
}

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }

// TestScriptErrors preserves transport/read causes and logs cleanup failures.
func TestScriptErrors(t *testing.T) {
	failure := errors.New("injected failure")
	for _, mode := range []string{"transport", "read", "close"} {
		t.Run(mode, func(t *testing.T) {
			var logs bytes.Buffer
			body := &trackingBody{Reader: strings.NewReader(purchasablePage)}
			if mode == "read" {
				body.Reader = errorReader{failure}
			}
			if mode == "close" {
				body.closeErr = failure
			}
			rc := &runner.Context{
				Log: slog.New(slog.NewTextHandler(&logs, nil)),
				HTTP: &http.Client{Transport: roundTripper(func(*http.Request) (*http.Response, error) {
					if mode == "transport" {
						return nil, failure
					}
					return &http.Response{StatusCode: http.StatusOK, Body: body}, nil
				})},
				Notify: func(context.Context, string) error {
					t.Fatal("first observation must not notify")
					return nil
				},
			}
			err := New().Run(context.Background(), rc)
			if mode == "close" {
				if err != nil || !strings.Contains(logs.String(), "close nintendo response body") || !strings.Contains(logs.String(), "injected failure") {
					t.Fatalf("close failure: error = %v, logs = %s", err, logs.String())
				}
			} else if !errors.Is(err, failure) {
				t.Fatalf("Run = %v, want injected failure", err)
			}
			if mode != "transport" && !body.closed {
				t.Fatal("response body was not closed")
			}
		})
	}
}

// TestScriptNotifyError records the new state even when delivery fails.
func TestScriptNotifyError(t *testing.T) {
	notifyErr := errors.New("notification unavailable")
	calls, notifications := 0, 0
	rc := &runner.Context{
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		HTTP: &http.Client{Transport: roundTripper(func(*http.Request) (*http.Response, error) {
			calls++
			page := unavailablePage
			if calls > 1 {
				page = purchasablePage
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(page))}, nil
		})},
		Notify: func(context.Context, string) error {
			notifications++
			return notifyErr
		},
	}
	script := New()
	if err := script.Run(context.Background(), rc); err == nil || notifications != 0 {
		t.Fatalf("unavailable baseline: error = %v, notifications = %d", err, notifications)
	}
	if err := script.Run(context.Background(), rc); !errors.Is(err, notifyErr) || notifications != 1 {
		t.Fatalf("transition: error = %v, notifications = %d", err, notifications)
	}
	if err := script.Run(context.Background(), rc); err != nil || notifications != 1 {
		t.Fatalf("steady stock: error = %v, notifications = %d", err, notifications)
	}
}
