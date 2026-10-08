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
	"time"

	"github.com/robfig/cron/v3"

	"github.com/trriplejay/notinv/internal/runner"
)

// These reduced fixtures retain the real page's JSON-LD @graph, array-valued
// @type, SKU, and Offer shape. The fetched Ocarina page reports OutOfStock;
// the buyable fixture changes only that schema.org enum to InStock.
const unavailableHTML = `<html><script type="application/ld+json" data-next-head="">{"@context":"https://schema.org/","@graph":[{"@type":["Product"],"name":"Electric OCARINA OF TIME","offers":{"@type":"Offer","url":"/us/store/products/electric-ocarina-of-time-129987/","availability":"https://schema.org/OutOfStock"},"sku":"129987"},[{"@type":"ImageObject"}]]}</script></html>`

var availableHTML = strings.Replace(unavailableHTML, "OutOfStock", "InStock", 1)

type roundTripper func(*http.Request) (*http.Response, error)

// RoundTrip returns the canned response without using the network.
func (f roundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type trackingBody struct {
	io.Reader
	closed   bool
	closeErr error
}

// Close records cleanup so tests detect an unclosed response body.
func (b *trackingBody) Close() error { b.closed = true; return b.closeErr }

// TestScriptSchedule checks both the declaration and the runner's cron parser.
func TestScriptSchedule(t *testing.T) {
	script := &Script{}
	if script.Name() != "nintendo" || script.Schedule() != "@every 1m" {
		t.Fatal("unexpected script identity or schedule")
	}
	sched, err := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor).Parse(script.Schedule())
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	t1 := sched.Next(t0)
	if t1.Sub(t0) != time.Minute || sched.Next(t1).Sub(t1) != time.Minute {
		t.Fatal("successive fire times must advance by one minute")
	}
}

// TestScriptTransitions checks classification, exact requests, closure, and alerts.
func TestScriptTransitions(t *testing.T) {
	for _, tt := range []struct {
		name       string
		states     []bool
		counts     []int
		failNotify bool
	}{
		{"restock", []bool{false, true, true}, []int{0, 1, 1}, false},
		{"available baseline", []bool{true, true, false, true}, []int{0, 0, 0, 1}, false},
		{"notify failure", []bool{false, true, true, false, true}, []int{0, 1, 1, 1, 2}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var logs bytes.Buffer
			var messages []string
			var bodies []*trackingBody
			calls := 0
			notifyErr := errors.New("notification unavailable")
			rc := &runner.Context{
				Log: slog.New(slog.NewTextHandler(&logs, nil)),
				HTTP: &http.Client{Transport: roundTripper(func(req *http.Request) (*http.Response, error) {
					const wantURL = "https://www.nintendo.com/us/store/products/electric-ocarina-of-time-129987/"
					if req.Method != http.MethodGet || req.URL.String() != wantURL {
						t.Fatalf("request = %s %s", req.Method, req.URL)
					}
					if req.Context() != ctx {
						t.Error("request lost context")
					}
					if calls >= len(tt.states) {
						t.Fatal("unexpected extra request")
					}
					html := unavailableHTML
					if tt.states[calls] {
						html = availableHTML
					}
					body := &trackingBody{Reader: strings.NewReader(html)}
					bodies = append(bodies, body)
					calls++
					return &http.Response{StatusCode: http.StatusOK, Body: body, Header: make(http.Header)}, nil
				})},
				Notify: func(got context.Context, msg string) error {
					if got != ctx {
						t.Error("Notify lost context")
					}
					messages = append(messages, msg)
					if tt.failNotify {
						return notifyErr
					}
					return nil
				},
			}
			script := New()
			for i, state := range tt.states {
				err := script.Run(ctx, rc)
				transition := i > 0 && !tt.states[i-1] && state
				if tt.failNotify && transition {
					if !errors.Is(err, notifyErr) {
						t.Fatalf("run %d error = %v, want notify error", i, err)
					}
				} else if err != nil {
					t.Fatalf("run %d: %v", i, err)
				}
				if !script.hasRun || script.lastOK != state {
					t.Fatalf("run %d: wrong observed state: %+v", i, script)
				}
				if calls != i+1 || !bodies[i].closed {
					t.Fatal("each run must request once and close its body")
				}
				if len(messages) != tt.counts[i] {
					t.Fatalf("run %d: %d notifications, want %d", i, len(messages), tt.counts[i])
				}
				if transition && !strings.Contains(messages[len(messages)-1], "Electric OCARINA OF TIME") {
					t.Fatal("notification must name the product")
				}
			}
		})
	}
}

// TestScriptInvalidResponses prevents unrelated offers and failed fetches from
// inventing stock transitions; cleanup errors are diagnostic only.
func TestScriptInvalidResponses(t *testing.T) {
	checkErr := errors.New("read or transport failed")
	for _, tt := range []struct {
		name                            string
		body                            string
		status                          int
		transportErr, readErr, closeErr error
		wantError                       bool
	}{
		{name: "unrelated SKU", body: strings.Replace(availableHTML, `"sku":"129987"`, `"sku":"other"`, 1), status: 200, wantError: true},
		{name: "missing data", body: `<button>Add to cart</button>`, status: 200, wantError: true},
		{name: "bad JSON", body: `<script type="application/ld+json">{</script>`, status: 200, wantError: true},
		{name: "unknown availability", body: strings.Replace(availableHTML, "InStock", "Unknown", 1), status: 200, wantError: true},
		{name: "HTTP failure", body: availableHTML, status: 503, wantError: true},
		{name: "transport failure", transportErr: checkErr, wantError: true},
		{name: "read failure", status: 200, readErr: checkErr, wantError: true},
		{name: "close failure", body: availableHTML, status: 200, closeErr: checkErr},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			body := &trackingBody{Reader: strings.NewReader(tt.body), closeErr: tt.closeErr}
			if tt.readErr != nil {
				body.Reader = errorReader{tt.readErr}
			}
			rc := &runner.Context{
				Log: slog.New(slog.NewTextHandler(&logs, nil)),
				HTTP: &http.Client{Transport: roundTripper(func(*http.Request) (*http.Response, error) {
					if tt.transportErr != nil {
						return nil, tt.transportErr
					}
					return &http.Response{StatusCode: tt.status, Body: body}, nil
				})},
				Notify: func(context.Context, string) error { t.Fatal("must not notify"); return nil },
			}
			script := &Script{hasRun: true, lastOK: true}
			err := script.Run(context.Background(), rc)
			if (err != nil) != tt.wantError {
				t.Fatalf("Run error = %v", err)
			}
			if (tt.readErr != nil || tt.transportErr != nil) && !errors.Is(err, checkErr) {
				t.Fatalf("lost error cause: %v", err)
			}
			if tt.transportErr == nil && !body.closed {
				t.Fatal("body not closed")
			}
			if !script.hasRun || !script.lastOK {
				t.Fatal("failed check changed known state")
			}
			if tt.closeErr != nil && !strings.Contains(logs.String(), "close nintendo response body") {
				t.Fatal("close error not logged")
			}
		})
	}
}

type errorReader struct{ err error }

// Read simulates a response body that fails during consumption.
func (r errorReader) Read([]byte) (int, error) { return 0, r.err }
