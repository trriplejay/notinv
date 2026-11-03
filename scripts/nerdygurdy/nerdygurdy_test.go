package nerdygurdy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/trriplejay/notinv/internal/runner"
	"github.com/trriplejay/notinv/internal/store"
)

const wantURL = "https://www.nerdygurdy.nl/product/ng-linotte-kit/"

// outOfStockHTML is reduced from the live NG Linotte kit page (WooCommerce,
// product id 2113). It keeps the decoys that make a naive scan wrong: an upsell
// item marked instock before the kit's wrapper, and a related item marked
// instock with its own p.stock in-stock after the kit's summary.
const outOfStockHTML = `<!DOCTYPE html><html><body class="product-template-default single single-product postid-2113">
<section class="up-sells upsells products"><ul class="products columns-4">
<li class="product type-product post-450 status-publish first instock product_cat-accessories"><a href="/product/rosin/">Rosin</a><p class="stock in-stock">In stock</p></li>
</ul></section>
<div id="product-2113" class="product type-product post-2113 status-publish first outofstock product_cat-kits has-post-thumbnail purchasable product-type-simple">
<div class="summary entry-summary">
<h1 class="product_title entry-title">NG Linotte kit</h1>
<div class="woocommerce-product-details__short-description"><p><strong>New stock is added each Sunday around 20:30 Central European Time.</strong></p></div>
<p class="stock out-of-stock">Out of stock</p>
</div>
<section class="related products"><h2>Related products</h2><ul class="products columns-4">
<li class="product type-product post-451 status-publish first instock product_cat-accessories"><a href="/product/strings/">Strings</a><p class="stock in-stock">In stock</p></li>
</ul></section>
</div>
</body></html>`

var inStockHTML = strings.NewReplacer(
	"first outofstock product_cat-kits", "first instock product_cat-kits",
	`<p class="stock out-of-stock">Out of stock</p>`, `<p class="stock in-stock">In stock</p>`,
).Replace(outOfStockHTML)

const baseSentence = "New stock is added each Sunday around 20:30 Central European Time."

func editedSentence(html string) string {
	return strings.Replace(html, baseSentence, "New stock is added each Saturday around 18:00 Central European Time.", 1)
}

func removedSentence(html string) string {
	return strings.Replace(html, "<p><strong>"+baseSentence+"</strong></p>", "", 1)
}

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

type errorReader struct{ err error }

// Read simulates a response body that fails during consumption.
func (r errorReader) Read([]byte) (int, error) { return 0, r.err }

// harness serves a switchable page and records requests, bodies and messages.
type harness struct {
	mu         sync.Mutex
	page       string
	requests   int
	bodies     []*trackingBody
	messages   []string
	failNotify int // number of upcoming Notify calls that fail
	notifyErr  error
	logs       bytes.Buffer
	rc         *runner.Context
}

func newHarness(t *testing.T, page string) *harness {
	h := &harness{page: page, notifyErr: errors.New("discord unavailable")}
	h.rc = &runner.Context{
		Log: slog.New(slog.NewTextHandler(&syncWriter{h: h}, &slog.HandlerOptions{Level: slog.LevelDebug})),
		HTTP: &http.Client{Transport: roundTripper(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodGet || req.URL.String() != wantURL {
				t.Errorf("request = %s %s", req.Method, req.URL)
			}
			h.mu.Lock()
			defer h.mu.Unlock()
			h.requests++
			body := &trackingBody{Reader: strings.NewReader(h.page)}
			h.bodies = append(h.bodies, body)
			return &http.Response{StatusCode: http.StatusOK, Body: body, Header: make(http.Header)}, nil
		})},
		Notify: func(_ context.Context, msg string) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.messages = append(h.messages, msg)
			if h.failNotify > 0 {
				h.failNotify--
				return h.notifyErr
			}
			return nil
		},
	}
	return h
}

// syncWriter serialises log writes into the harness buffer.
type syncWriter struct{ h *harness }

func (w *syncWriter) Write(p []byte) (int, error) {
	w.h.mu.Lock()
	defer w.h.mu.Unlock()
	return w.h.logs.Write(p)
}

func (h *harness) setPage(page string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.page = page
}

func (h *harness) counts() (requests, notifications int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.requests, len(h.messages)
}

func (h *harness) log() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.logs.String()
}

func amsterdam(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func fixed(at time.Time) func() time.Time { return func() time.Time { return at } }

// insideWindow returns a clock at Sunday 2026-01-04 20:30 Amsterdam.
func insideWindow(t *testing.T) func() time.Time {
	return fixed(time.Date(2026, time.January, 4, 20, 30, 0, 0, amsterdam(t)))
}

// CLM-2: exact identity, and the runner's parser fires once a minute on
// Sundays 19:00-22:59 Amsterdam, on CET and CEST dates.
func TestScriptSchedule(t *testing.T) {
	s := New()
	if s.Name() != "nerdygurdy" {
		t.Fatalf("Name = %q", s.Name())
	}
	if s.Schedule() != "CRON_TZ=Europe/Amsterdam * 19-22 * * 0" {
		t.Fatalf("Schedule = %q", s.Schedule())
	}
	sched, err := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor).Parse(s.Schedule())
	if err != nil {
		t.Fatal(err)
	}
	loc := amsterdam(t)
	for _, tt := range []struct {
		name               string
		sunday, nextSunday time.Time
		zoneOffset         int
	}{
		{"CET", time.Date(2026, time.January, 4, 0, 0, 0, 0, loc), time.Date(2026, time.January, 11, 19, 0, 0, 0, loc), 3600},
		{"CEST", time.Date(2026, time.July, 5, 0, 0, 0, 0, loc), time.Date(2026, time.July, 12, 19, 0, 0, 0, loc), 7200},
	} {
		t.Run(tt.name, func(t *testing.T) {
			want := tt.sunday.Add(19 * time.Hour)
			if _, off := want.Zone(); off != tt.zoneOffset {
				t.Fatalf("fixture %v has offset %d, want %d", want, off, tt.zoneOffset)
			}
			// From noon on each day Monday..Saturday of the preceding week.
			for d := 1; d <= 6; d++ {
				from := tt.sunday.AddDate(0, 0, d-7).Add(12 * time.Hour)
				if got := sched.Next(from); !got.Equal(want) {
					t.Fatalf("Next(%v) = %v, want %v", from, got, want)
				}
			}
			fire := want
			n := 1
			for {
				next := sched.Next(fire)
				if fire.In(loc).Hour() == 22 && fire.In(loc).Minute() == 59 {
					if !next.Equal(tt.nextSunday) {
						t.Fatalf("after %v: %v, want %v", fire, next, tt.nextSunday)
					}
					break
				}
				if next.Sub(fire) != time.Minute {
					t.Fatalf("after %v: %v, want 1 minute later", fire, next)
				}
				fire = next
				n++
			}
			if n != 240 {
				t.Fatalf("%d fires in the window, want 240", n)
			}
		})
	}
}

// CLM-3: window edges in Amsterdam time whatever the zone of t.
func TestInWindowEdges(t *testing.T) {
	ams := amsterdam(t)
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name string
		at   time.Time
		want bool
	}{
		// CET (UTC+1), Sunday 2026-01-04, expressed in UTC.
		{"CET Sun 18:59", time.Date(2026, time.January, 4, 17, 59, 0, 0, time.UTC), false},
		{"CET Sun 19:00", time.Date(2026, time.January, 4, 18, 0, 0, 0, time.UTC), true},
		{"CET Sun 22:59", time.Date(2026, time.January, 4, 21, 59, 59, 0, time.UTC), true},
		{"CET Sun 23:00", time.Date(2026, time.January, 4, 22, 0, 0, 0, time.UTC), false},
		{"CET Mon 20:30", time.Date(2026, time.January, 5, 19, 30, 0, 0, time.UTC), false},
		{"CET Sat 20:30", time.Date(2026, time.January, 3, 20, 30, 0, 0, ams), false},
		// CEST (UTC+2), Sunday 2026-07-05.
		{"CEST Sun 18:59", time.Date(2026, time.July, 5, 16, 59, 0, 0, time.UTC), false},
		{"CEST Sun 19:00", time.Date(2026, time.July, 5, 17, 0, 0, 0, time.UTC), true},
		{"CEST Sun 22:59", time.Date(2026, time.July, 5, 20, 59, 0, 0, time.UTC), true},
		{"CEST Sun 23:00", time.Date(2026, time.July, 5, 21, 0, 0, 0, time.UTC), false},
		{"CEST Mon 20:30", time.Date(2026, time.July, 6, 20, 30, 0, 0, ams), false},
		// UTC says Sunday 22:30, but Amsterdam is already Monday 00:30.
		{"UTC Sunday is Amsterdam Monday", time.Date(2026, time.July, 5, 22, 30, 0, 0, time.UTC), false},
		// Tokyo says Monday 03:30, but Amsterdam is Sunday 19:30.
		{"Tokyo Monday is Amsterdam Sunday", time.Date(2026, time.January, 5, 3, 30, 0, 0, tokyo), true},
		// Los Angeles Sunday 11:00 PST is Amsterdam Sunday 20:00.
		{"LA Sunday morning", time.Date(2026, time.January, 4, 11, 0, 0, 0, la), true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := inWindow(tt.at)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("inWindow(%v) = %t, want %t", tt.at, got, tt.want)
			}
		})
	}

	t.Run("location load failure", func(t *testing.T) {
		loadErr := errors.New("zoneinfo missing")
		orig := loadLocation
		loadLocation = func(string) (*time.Location, error) { return nil, loadErr }
		t.Cleanup(func() { loadLocation = orig })
		if _, err := inWindow(time.Now()); !errors.Is(err, loadErr) {
			t.Fatalf("inWindow error = %v, want %v", err, loadErr)
		}
		// Run surfaces it instead of fetching or going dormant silently.
		h := newHarness(t, inStockHTML)
		s := New()
		if err := s.Run(context.Background(), h.rc); !errors.Is(err, loadErr) {
			t.Fatalf("Run error = %v, want %v", err, loadErr)
		}
		if reqs, notes := h.counts(); reqs != 0 || notes != 0 {
			t.Fatalf("%d requests, %d notifications, want none", reqs, notes)
		}
	})
}

// CLM-8, CLM-9: only the kit's wrapper and its own p.stock decide stock;
// decoys never count, ambiguity is an error, and the restock sentence is ignored.
func TestInStock(t *testing.T) {
	noWrapper := strings.Replace(outOfStockHTML, `id="product-2113" `, "", 1)
	for _, tt := range []struct {
		name    string
		body    string
		want    bool
		wantErr string
	}{
		{name: "out of stock", body: outOfStockHTML, want: false},
		{name: "in stock", body: inStockHTML, want: true},
		{name: "out, sentence edited", body: editedSentence(outOfStockHTML), want: false},
		{name: "in, sentence edited", body: editedSentence(inStockHTML), want: true},
		{name: "out, sentence removed", body: removedSentence(outOfStockHTML), want: false},
		{name: "in, sentence removed", body: removedSentence(inStockHTML), want: true},
		{name: "decoys only, no wrapper", body: noWrapper, wantErr: "nerdygurdy: stock status not found"},
		{name: "decoys only, in-stock decoys", body: strings.Replace(inStockHTML, `id="product-2113" `, "", 1), wantErr: "nerdygurdy: stock status not found"},
		{name: "other product id", body: strings.Replace(inStockHTML, `id="product-2113"`, `id="product-21130"`, 1), wantErr: "nerdygurdy: stock status not found"},
		{name: "wrapper without stock class", body: strings.Replace(outOfStockHTML, "first outofstock product_cat-kits", "first product_cat-kits", 1), wantErr: "ambiguous"},
		{name: "wrapper with both classes", body: strings.Replace(outOfStockHTML, "first outofstock product_cat-kits", "first outofstock instock product_cat-kits", 1), wantErr: "ambiguous"},
		{name: "wrapper onbackorder", body: strings.Replace(outOfStockHTML, "first outofstock product_cat-kits", "first onbackorder product_cat-kits", 1), wantErr: `"onbackorder"`},
		{name: "paragraph on backorder", body: strings.Replace(outOfStockHTML, `class="stock out-of-stock"`, `class="stock available-on-backorder"`, 1), wantErr: `"available-on-backorder"`},
		{name: "paragraph without state", body: strings.Replace(outOfStockHTML, `class="stock out-of-stock"`, `class="stock"`, 1), wantErr: "ambiguous"},
		{name: "wrapper in, paragraph out", body: strings.Replace(outOfStockHTML, "first outofstock product_cat-kits", "first instock product_cat-kits", 1), wantErr: "disagree"},
		{name: "wrapper out, paragraph in", body: strings.Replace(inStockHTML, "first instock product_cat-kits", "first outofstock product_cat-kits", 1), wantErr: "disagree"},
		// Without the kit's own p.stock, the related item's p.stock in-stock still never counts.
		{name: "related p.stock ignored", body: strings.Replace(outOfStockHTML, `<p class="stock out-of-stock">Out of stock</p>`, "", 1), want: false},
		{name: "single quotes, reordered", body: `<li class='product type-product post-450 instock'></li><div data-x='1' class='product type-product post-2113 instock' id='product-2113'><p id='s' class='stock in-stock'>In stock</p></div>`, want: true},
		{name: "spaced attributes", body: `<div class = "product outofstock" id = "product-2113"><p class = "out-of-stock stock">Out of stock</p></div>`, want: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := inStock([]byte(tt.body))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("inStock = %t, %v; want error containing %q", got, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("inStock = %t, want %t", got, tt.want)
			}
		})
	}
}

// CLM-1: a changed or missing restock sentence is a Warn only; Run still
// succeeds and sends nothing for it.
func TestRestockLineMismatch(t *testing.T) {
	for _, tt := range []struct {
		name      string
		page      string
		wantWarn  bool
		wantFound string
	}{
		{"unchanged", outOfStockHTML, false, ""},
		{"edited", editedSentence(outOfStockHTML), true, "New stock is added each Saturday around 18:00 Central European Time."},
		{"removed", removedSentence(outOfStockHTML), true, "<absent>"},
		{"edited, in stock", editedSentence(inStockHTML), true, "Saturday around 18:00"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, tt.page)
			s := &Script{now: insideWindow(t)}
			if err := s.Run(context.Background(), h.rc); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if reqs, notes := h.counts(); reqs != 1 || notes != 0 {
				t.Fatalf("%d requests, %d notifications, want 1 and 0", reqs, notes)
			}
			if !s.hasRun || s.lastInStock != (tt.page == editedSentence(inStockHTML)) {
				t.Fatalf("stock result changed by the sentence: %+v", s)
			}
			logs := h.log()
			warned := strings.Contains(logs, "level=WARN")
			if warned != tt.wantWarn {
				t.Fatalf("warned = %t, want %t; logs:\n%s", warned, tt.wantWarn, logs)
			}
			if tt.wantWarn {
				if !strings.Contains(logs, "restock sentence changed") || !strings.Contains(logs, tt.wantFound) {
					t.Fatalf("warning lacks mismatch or found text %q; logs:\n%s", tt.wantFound, logs)
				}
			}
		})
	}
}

// CLM-5, CLM-6, CLM-7, CLM-11: baseline is silent, only out->in notifies, a
// failed DM is returned and not resent, and a later restock still notifies.
func TestScriptTransitions(t *testing.T) {
	for _, tt := range []struct {
		name       string
		states     []bool
		counts     []int
		failNotify int
	}{
		{"restock", []bool{false, true, true}, []int{0, 1, 1}, 0},
		{"in-stock baseline stays silent", []bool{true, true, true, false, true}, []int{0, 0, 0, 0, 1}, 0},
		{"out of stock stays silent", []bool{false, false, false}, []int{0, 0, 0}, 0},
		{"notify failure", []bool{false, true, true, false, true}, []int{0, 1, 1, 1, 2}, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			h := newHarness(t, outOfStockHTML)
			h.failNotify = tt.failNotify
			s := &Script{now: insideWindow(t)}
			for i, state := range tt.states {
				page := outOfStockHTML
				if state {
					page = inStockHTML
				}
				h.setPage(page)
				err := s.Run(ctx, h.rc)
				transition := i > 0 && !tt.states[i-1] && state
				if tt.failNotify > 0 && transition && i == 1 {
					if !errors.Is(err, h.notifyErr) {
						t.Fatalf("run %d error = %v, want notify error", i, err)
					}
				} else if err != nil {
					t.Fatalf("run %d: %v", i, err)
				}
				if !s.hasRun || s.lastInStock != state {
					t.Fatalf("run %d: wrong observed state: %+v", i, s)
				}
				reqs, notes := h.counts()
				if reqs != i+1 || !h.bodies[i].closed {
					t.Fatalf("run %d: %d requests, closed=%t; each run must request once and close its body", i, reqs, h.bodies[i].closed)
				}
				if notes != tt.counts[i] {
					t.Fatalf("run %d: %d notifications, want %d", i, notes, tt.counts[i])
				}
				if transition {
					msg := h.messages[len(h.messages)-1]
					if !strings.Contains(msg, "NG Linotte kit") || !strings.Contains(msg, wantURL) {
						t.Fatalf("message %q must name the kit and contain %s", msg, wantURL)
					}
				}
			}
		})
	}
}

// CLM-10: failed checks return a descriptive error, keep state, send nothing
// and close the body; a body over 2 MiB is a read failure.
func TestScriptInvalidResponses(t *testing.T) {
	checkErr := errors.New("read or transport failed")
	padded := func(size int) string {
		return inStockHTML + strings.Repeat(" ", size-len(inStockHTML))
	}
	for _, tt := range []struct {
		name                            string
		body                            string
		status                          int
		transportErr, readErr, closeErr error
		wantErr                         string
	}{
		{name: "missing marker", body: `<html><p class="stock in-stock">In stock</p></html>`, status: 200, wantErr: "nerdygurdy: stock status not found"},
		{name: "HTTP 503", body: inStockHTML, status: 503, wantErr: "unexpected HTTP status 503"},
		{name: "transport failure", transportErr: checkErr, wantErr: "perform nerdygurdy request"},
		{name: "read failure", status: 200, readErr: checkErr, wantErr: "read nerdygurdy response"},
		{name: "body over 2 MiB", body: padded(2<<20 + 1), status: 200, wantErr: "read nerdygurdy response"},
		{name: "close failure", body: outOfStockHTML, status: 200, closeErr: checkErr},
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
				Notify: func(context.Context, string) error { t.Error("must not notify"); return nil },
			}
			s := &Script{started: true, hasRun: true, lastInStock: false}
			err := s.Run(context.Background(), rc)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Run: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Run error = %v, want it to contain %q", err, tt.wantErr)
			}
			if (tt.readErr != nil || tt.transportErr != nil) && !errors.Is(err, checkErr) {
				t.Fatalf("lost error cause: %v", err)
			}
			if tt.transportErr == nil && !body.closed {
				t.Fatal("body not closed")
			}
			if !s.started || !s.hasRun || s.lastInStock {
				t.Fatalf("failed check changed known state: %+v", s)
			}
			if tt.closeErr != nil && !strings.Contains(logs.String(), "close nerdygurdy response body") {
				t.Fatal("close error not logged")
			}
		})
	}

	t.Run("body of exactly 2 MiB is read", func(t *testing.T) {
		h := newHarness(t, padded(2<<20))
		s := &Script{started: true, hasRun: true}
		if err := s.Run(context.Background(), h.rc); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if _, notes := h.counts(); notes != 1 || !s.lastInStock {
			t.Fatalf("%d notifications, state %+v; want the restock observed", notes, s)
		}
	})
}

// CLM-4: a startup outside the window makes no request, sends nothing and sets
// no baseline; the next run baselines silently.
func TestStartupOutsideWindowDormant(t *testing.T) {
	ams := amsterdam(t)
	for _, tt := range []struct {
		name string
		at   time.Time
	}{
		{"weekday", time.Date(2026, time.January, 7, 20, 30, 0, 0, ams)},
		{"Sunday 18:59", time.Date(2026, time.January, 4, 18, 59, 0, 0, ams)},
		{"Sunday 23:00", time.Date(2026, time.July, 5, 23, 0, 0, 0, ams)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			failing := &runner.Context{
				Log: slog.New(slog.NewTextHandler(&logs, nil)),
				HTTP: &http.Client{Transport: roundTripper(func(*http.Request) (*http.Response, error) {
					t.Error("dormant startup must not fetch")
					return nil, errors.New("unexpected request")
				})},
				Notify: func(context.Context, string) error { t.Error("dormant startup must not notify"); return nil },
			}
			s := &Script{now: fixed(tt.at)}
			if err := s.Run(context.Background(), failing); err != nil {
				t.Fatalf("startup Run: %v", err)
			}
			if !s.started || s.hasRun || s.lastInStock {
				t.Fatalf("startup changed observation state: %+v", s)
			}
			if !strings.Contains(logs.String(), "nerdygurdy startup outside window; dormant") {
				t.Fatalf("dormant startup not logged:\n%s", logs.String())
			}

			h := newHarness(t, inStockHTML)
			if err := s.Run(context.Background(), h.rc); err != nil {
				t.Fatalf("next Run: %v", err)
			}
			if reqs, notes := h.counts(); reqs != 1 || notes != 0 {
				t.Fatalf("next run made %d requests and %d notifications, want 1 and 0", reqs, notes)
			}
			if !s.hasRun || !s.lastInStock {
				t.Fatalf("next run did not baseline: %+v", s)
			}
		})
	}
}

// CLM-5: a startup inside the window fetches and baselines silently even when in stock.
func TestStartupInsideWindowBaselines(t *testing.T) {
	h := newHarness(t, inStockHTML)
	s := &Script{now: insideWindow(t)}
	for i := range 3 {
		if err := s.Run(context.Background(), h.rc); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if reqs, notes := h.counts(); reqs != i+1 || notes != 0 {
			t.Fatalf("run %d: %d requests and %d notifications, want %d and 0", i, reqs, notes, i+1)
		}
	}
	if !s.hasRun || !s.lastInStock {
		t.Fatalf("state = %+v, want in-stock baseline", s)
	}
}

type fakeRunStore struct {
	mu   sync.Mutex
	runs []store.Run
}

func (f *fakeRunStore) InsertRun(_ context.Context, run store.Run) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, run)
	return nil
}

func (f *fakeRunStore) ScriptEnabled(_ context.Context, _ string) (bool, error) {
	return true, nil
}

func (f *fakeRunStore) snapshot() []store.Run {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.Run(nil), f.runs...)
}

// CLM-4, CLM-7: under the real runner, the Saturday startup is dormant and
// recorded OK; the first fire is Sunday 19:00 CET and baselines silently (an
// out-of-stock run recorded OK); a later fire on a restock sends one DM.
func TestRunnerStartupDormantThenSundayWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// The bubble starts at 2000-01-01T00:00Z: Saturday 01:00 CET.
		// Sunday 2000-01-02 19:00 CET is 18:00Z, 42 hours later.
		h := newHarness(t, outOfStockHTML)
		st := &fakeRunStore{}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- runner.Run(ctx, New(), h.rc, st) }()
		defer func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("runner.Run: %v", err)
			}
		}()

		synctest.Wait()
		if reqs, notes := h.counts(); reqs != 0 || notes != 0 {
			t.Fatalf("startup made %d requests and %d notifications, want none", reqs, notes)
		}
		if runs := st.snapshot(); len(runs) != 1 || !runs[0].OK || runs[0].Script != "nerdygurdy" {
			t.Fatalf("startup recorded %+v, want one OK nerdygurdy run", runs)
		}

		// Just before the window: nothing fires.
		time.Sleep(42*time.Hour - time.Second)
		synctest.Wait()
		if reqs, _ := h.counts(); reqs != 0 {
			t.Fatalf("%d requests before Sunday 19:00, want none", reqs)
		}

		// Sunday 19:00 CET: the first in-window fire baselines silently.
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if got := time.Now().UTC(); !got.Equal(time.Date(2000, time.January, 2, 18, 0, 1, 0, time.UTC)) {
			t.Fatalf("clock = %v", got)
		}
		if reqs, notes := h.counts(); reqs != 1 || notes != 0 {
			t.Fatalf("first fire made %d requests and %d notifications, want 1 and 0", reqs, notes)
		}
		if runs := st.snapshot(); len(runs) != 2 || !runs[1].OK {
			t.Fatalf("first fire recorded %+v, want a second OK run", runs)
		}

		// The kit restocks; the next minute's fire sends one DM.
		h.setPage(inStockHTML)
		time.Sleep(time.Minute)
		synctest.Wait()
		if reqs, notes := h.counts(); reqs != 2 || notes != 1 {
			t.Fatalf("restock fire made %d requests and %d notifications, want 2 and 1", reqs, notes)
		}
		if runs := st.snapshot(); len(runs) != 3 || !runs[2].OK {
			t.Fatalf("restock fire recorded %+v, want a third OK run", runs)
		}
	})
}
