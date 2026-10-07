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

// Reduced from the live page's JSON-LD: keep its graph, nested image array,
// product SKU, relative offer URL and availability shape. The positive fixture
// changes only OutOfStock to the corresponding schema.org InStock value.
const unavailablePage = `<script type="application/ld+json" data-next-head="">{"@context":"https://schema.org/","@graph":[[{"@type":"ImageObject"}],{"@type":["Product"],"name":"Electric OCARINA OF TIME","offers":{"@type":"Offer","url":"/us/store/products/electric-ocarina-of-time-129987/","availability":"https://schema.org/OutOfStock"},"sku":"129987"}]}</script>`

var availablePage = strings.Replace(unavailablePage, "OutOfStock", "InStock", 1)

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type trackingBody struct {
	io.Reader
	closed   bool
	closeErr error
}

func (b *trackingBody) Close() error {
	b.closed = true
	return b.closeErr
}

// CLM-2, CLM-6, CLM-7, CLM-8: drive the real Run through injected services,
// asserting each observation and alert rather than merely the final count.
func TestScriptTransitions(t *testing.T) {
	for _, tt := range []struct {
		name   string
		states []bool
		alerts []int
	}{
		{"becomes purchasable", []bool{false, true}, []int{0, 1}},
		{"restocks twice", []bool{false, true, true, false, true}, []int{0, 1, 1, 1, 2}},
		{"purchasable baseline", []bool{true, true, false, true, true}, []int{0, 0, 0, 1, 1}},
		{"no steady repeats", []bool{false, true, true, true, true}, []int{0, 1, 1, 1, 1}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			const url = "https://www.nintendo.com/us/store/products/electric-ocarina-of-time-129987/"
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var logs bytes.Buffer
			var messages []string
			var bodies []*trackingBody
			calls := 0
			rc := &runner.Context{
				Log: slog.New(slog.NewTextHandler(&logs, nil)),
				HTTP: &http.Client{Transport: roundTripper(func(req *http.Request) (*http.Response, error) {
					if req.Method != http.MethodGet || req.URL.String() != url || req.Context() != ctx {
						t.Fatalf("unexpected request: %s %s, context=%v", req.Method, req.URL, req.Context())
					}
					if calls >= len(tt.states) {
						t.Fatal("unexpected extra request")
					}
					page := unavailablePage
					if tt.states[calls] {
						page = availablePage
					}
					body := &trackingBody{Reader: strings.NewReader(page)}
					bodies = append(bodies, body)
					calls++
					return &http.Response{StatusCode: http.StatusOK, Body: body}, nil
				})},
				Notify: func(got context.Context, msg string) error {
					if got != ctx {
						t.Error("Notify lost run context")
					}
					if !strings.Contains(msg, url) {
						t.Errorf("notification does not identify product: %q", msg)
					}
					messages = append(messages, msg)
					return nil
				},
			}
			script := New()
			if script.Name() != "nintendo-ocarina" {
				t.Fatalf("unexpected name: %q", script.Name())
			}
			for i, state := range tt.states {
				logs.Reset()
				if err := script.Run(ctx, rc); err != nil {
					t.Fatalf("run %d: %v", i, err)
				}
				if !script.hasRun || script.lastPurchasable != state {
					t.Fatalf("run %d: observation = %t, want %t", i, script.lastPurchasable, state)
				}
				if len(messages) != tt.alerts[i] {
					t.Fatalf("run %d: %d alerts, want %d", i, len(messages), tt.alerts[i])
				}
				if calls != i+1 || !bodies[i].closed {
					t.Fatal("each run must request once and close the response")
				}
				wantLog := "purchasable=false"
				if state {
					wantLog = "purchasable=true"
				}
				if !strings.Contains(logs.String(), "nintendo check completed") || !strings.Contains(logs.String(), wantLog) {
					t.Errorf("missing observation log: %s", logs.String())
				}
			}
		})
	}
}

// CLM-1: use the runner's parser and check actual fire times.
func TestScriptSchedule(t *testing.T) {
	script := New()
	if script.Schedule() != "@every 1m" {
		t.Fatalf("schedule = %q", script.Schedule())
	}
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
	schedule, err := parser.Parse(script.Schedule())
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, time.October, 1, 0, 0, 0, 0, time.UTC)
	first := schedule.Next(start)
	second := schedule.Next(first)
	if first.Sub(start) != time.Minute || second.Sub(first) != time.Minute {
		t.Fatalf("fire times %v, %v are not one minute apart", first, second)
	}
}

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }

func TestScriptCheckErrorsPreserveObservation(t *testing.T) {
	transportErr := errors.New("transport unavailable")
	readErr := errors.New("read failed")
	for _, tt := range []struct {
		name   string
		status int
		reader io.Reader
		err    error
		cause  error
		want   string
	}{
		{"transport", 0, nil, transportErr, transportErr, "perform nintendo request"},
		{"HTTP status", 503, strings.NewReader(availablePage), nil, nil, "unexpected HTTP status 503"},
		{"read", 200, errorReader{readErr}, nil, readErr, "read nintendo response"},
		{"oversize", 200, strings.NewReader(availablePage + strings.Repeat(" ", 1<<20)), nil, nil, "exceeds 1 MiB"},
		{"missing signal", 200, strings.NewReader("<button>Add to cart</button>"), nil, nil, "availability missing"},
		{"malformed JSON", 200, strings.NewReader(`<script type="application/ld+json">{</script>`), nil, nil, "parse nintendo JSON-LD"},
		{"other product", 200, strings.NewReader(strings.Replace(availablePage, `"sku":"129987"`, `"sku":"other"`, 1)), nil, nil, "availability missing"},
		{"unknown availability", 200, strings.NewReader(strings.Replace(availablePage, "InStock", "Unknown", 1)), nil, nil, "unrecognized nintendo availability"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body := &trackingBody{Reader: tt.reader}
			rc := &runner.Context{
				Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
				HTTP: &http.Client{Transport: roundTripper(func(*http.Request) (*http.Response, error) {
					if tt.err != nil {
						return nil, tt.err
					}
					return &http.Response{StatusCode: tt.status, Body: body}, nil
				})},
				Notify: func(context.Context, string) error { t.Error("failed check notified"); return nil },
			}
			script := &Script{hasRun: true, lastPurchasable: true}
			err := script.Run(context.Background(), rc)
			if err == nil || !strings.Contains(err.Error(), tt.want) || (tt.cause != nil && !errors.Is(err, tt.cause)) {
				t.Fatalf("Run = %v, want %q (cause %v)", err, tt.want, tt.cause)
			}
			if !script.hasRun || !script.lastPurchasable {
				t.Fatal("failed check changed the last observation")
			}
			if tt.err == nil && !body.closed {
				t.Fatal("response body not closed")
			}
		})
	}
}

func TestScriptNotifyAndCloseErrors(t *testing.T) {
	notifyErr := errors.New("notification unavailable")
	closeErr := errors.New("cleanup failed")
	var logs bytes.Buffer
	notifications := 0
	var body *trackingBody
	rc := &runner.Context{
		Log: slog.New(slog.NewTextHandler(&logs, nil)),
		HTTP: &http.Client{Transport: roundTripper(func(*http.Request) (*http.Response, error) {
			body = &trackingBody{Reader: strings.NewReader(availablePage), closeErr: closeErr}
			return &http.Response{StatusCode: http.StatusOK, Body: body}, nil
		})},
		Notify: func(context.Context, string) error { notifications++; return notifyErr },
	}
	script := &Script{hasRun: true, lastPurchasable: false}
	if err := script.Run(context.Background(), rc); !errors.Is(err, notifyErr) || errors.Is(err, closeErr) {
		t.Fatalf("Run = %v, want only notification cause", err)
	}
	if err := script.Run(context.Background(), rc); err != nil || notifications != 1 {
		t.Fatalf("steady state: error=%v, alerts=%d", err, notifications)
	}
	if !body.closed || !strings.Contains(logs.String(), "close nintendo response body") || !strings.Contains(logs.String(), "cleanup failed") {
		t.Fatalf("missing cleanup warning: %s", logs.String())
	}
}
