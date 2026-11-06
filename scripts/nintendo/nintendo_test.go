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

// The Target fixtures reduce the observed redsky product_fulfillment_v1 200
// response for TCIN 1013288947, which reported ship-to-home OUT_OF_STOCK. The
// variants change one availability_status enum each.
const targetOutOfStockJSON = `{"data":{"product":{"tcin":"1013288947","fulfillment":{"is_out_of_stock_in_all_store_locations":false,"sold_out":false,"shipping_options":{"availability_status":"OUT_OF_STOCK","loyalty_availability_status":"OUT_OF_STOCK","available_to_promise_quantity":0.0,"reason_code":"INVENTORY_UNAVAILABLE","services":[]},"store_options":[{"order_pickup":{"availability_status":"OUT_OF_STOCK"}}],"scheduled_delivery":{"availability_status":"OUT_OF_STOCK"}}}}}`

const shippingOOS = `"shipping_options":{"availability_status":"OUT_OF_STOCK"`

var (
	targetInStockJSON = strings.Replace(targetOutOfStockJSON, shippingOOS, `"shipping_options":{"availability_status":"IN_STOCK"`, 1)
	// Pickup in stock does not make the product available to ship.
	targetPickupOnlyJSON = strings.Replace(targetOutOfStockJSON, `"order_pickup":{"availability_status":"OUT_OF_STOCK"`, `"order_pickup":{"availability_status":"IN_STOCK"`, 1)
)

const (
	wantNintendoURL = "https://www.nintendo.com/us/store/products/electric-ocarina-of-time-129987/"
	wantTargetURL   = "https://www.target.com/p/hori-electric-ocarina-of-time-nintendo-switch-2/-/A-1013288947"
	nintendoMessage = "Electric OCARINA OF TIME is now purchasable: " + wantNintendoURL
)

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
	if script.Name() != "nintendo" || script.Schedule() != "@every 30s" {
		t.Fatal("unexpected script identity or schedule")
	}
	sched, err := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor).Parse(script.Schedule())
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	t1 := sched.Next(t0)
	if t1.Sub(t0) != 30*time.Second || sched.Next(t1).Sub(t1) != 30*time.Second {
		t.Fatal("successive fire times must advance by 30 seconds")
	}
}

// reply is one canned response for one site in one run.
type reply struct {
	status                          int
	body                            string
	transportErr, readErr, closeErr error
}

// harness drives Script.Run through a fake transport routed by URL, recording
// every request body, notification and log line.
type harness struct {
	t                *testing.T
	ctx              context.Context
	logs             bytes.Buffer
	messages         []string
	nintendo, target reply
	nintendoBodies   []*trackingBody
	targetBodies     []*trackingBody
	notifyErr        error
	rc               *runner.Context
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h := &harness{t: t, ctx: ctx}
	h.rc = &runner.Context{
		Log: slog.New(slog.NewTextHandler(&h.logs, nil)),
		HTTP: &http.Client{Transport: roundTripper(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodGet {
				t.Fatalf("request method = %s", req.Method)
			}
			if req.Context() != ctx {
				t.Error("request lost context")
			}
			var r reply
			var bodies *[]*trackingBody
			switch {
			case req.URL.String() == wantNintendoURL:
				r, bodies = h.nintendo, &h.nintendoBodies
			case req.URL.Host == "redsky.target.com":
				checkTargetRequest(t, req)
				r, bodies = h.target, &h.targetBodies
			default:
				t.Fatalf("unexpected request %s", req.URL)
			}
			if r.transportErr != nil {
				*bodies = append(*bodies, nil)
				return nil, r.transportErr
			}
			body := &trackingBody{Reader: strings.NewReader(r.body), closeErr: r.closeErr}
			if r.readErr != nil {
				body.Reader = errorReader{r.readErr}
			}
			*bodies = append(*bodies, body)
			return &http.Response{StatusCode: r.status, Body: body, Header: make(http.Header)}, nil
		})},
		Notify: func(got context.Context, msg string) error {
			if got != ctx {
				t.Error("Notify lost context")
			}
			h.messages = append(h.messages, msg)
			return h.notifyErr
		},
	}
	return h
}

// checkTargetRequest pins the fixed redsky lookup: endpoint, parameters and
// the JSON/browser headers.
func checkTargetRequest(t *testing.T, req *http.Request) {
	t.Helper()
	if req.URL.Scheme != "https" || req.URL.Path != "/redsky_aggregations/v1/web/product_fulfillment_v1" {
		t.Fatalf("target request = %s", req.URL)
	}
	q := req.URL.Query()
	for key, want := range map[string]string{
		"key": "9f36aeafbe60771e321a7cc95a78140772ab3e96", "tcin": "1013288947",
		"store_id": "2454", "required_store_id": "2454", "scheduled_delivery_store_id": "2454",
		"zip": "51503", "state": "IA", "channel": "WEB", "page": "/p/A-1013288947",
	} {
		if got := q.Get(key); got != want {
			t.Errorf("target query %s = %q, want %q", key, got, want)
		}
	}
	if q.Has("latitude") || q.Has("longitude") {
		t.Error("target query must not carry geolocation")
	}
	if req.Header.Get("Accept") != "application/json" || !strings.HasPrefix(req.Header.Get("User-Agent"), "Mozilla/5.0") {
		t.Errorf("target headers = %v", req.Header)
	}
}

// run performs one Run and checks that each site was requested exactly once
// this run and every response body was closed.
func (h *harness) run(script *Script, nintendo, target reply) error {
	h.t.Helper()
	h.nintendo, h.target = nintendo, target
	before := len(h.nintendoBodies) + len(h.targetBodies)
	err := script.Run(h.ctx, h.rc)
	if len(h.nintendoBodies)+len(h.targetBodies) != before+2 {
		h.t.Fatal("each run must request each site once")
	}
	for _, b := range append(h.nintendoBodies[len(h.nintendoBodies)-1:], h.targetBodies[len(h.targetBodies)-1:]...) {
		if b != nil && !b.closed {
			h.t.Fatal("response body not closed")
		}
	}
	return err
}

func ok(body string) reply { return reply{status: http.StatusOK, body: body} }

// nintendoReply maps o (out of stock), i (in stock), f (503) to a response.
func nintendoReply(c byte) reply {
	switch c {
	case 'i':
		return ok(availableHTML)
	case 'f':
		return reply{status: http.StatusServiceUnavailable, body: availableHTML}
	}
	return ok(unavailableHTML)
}

// targetReply maps o (out of stock), i (in stock), f (435 bot refusal).
func targetReply(c byte) reply {
	switch c {
	case 'i':
		return ok(targetInStockJSON)
	case 'f':
		return reply{status: 435, body: targetInStockJSON}
	}
	return ok(targetOutOfStockJSON)
}

// wantState is the state a site must hold after the given observations:
// the last successful read, ignoring failures; no read means no baseline.
func wantState(seq string) siteState {
	var st siteState
	for i := range len(seq) {
		if seq[i] != 'f' {
			st.hasRun, st.lastOK = true, seq[i] == 'i'
		}
	}
	return st
}

// checkMessage asserts the notification links exactly the newly available sites.
func checkMessage(t *testing.T, msg, want string) {
	t.Helper()
	switch want {
	case "N":
		if msg != nintendoMessage {
			t.Fatalf("message = %q, want %q", msg, nintendoMessage)
		}
	case "T":
		if !strings.Contains(msg, "Electric OCARINA OF TIME") || !strings.Contains(msg, wantTargetURL) || strings.Contains(msg, wantNintendoURL) {
			t.Fatalf("target-only message = %q", msg)
		}
	case "NT":
		if !strings.Contains(msg, "Electric OCARINA OF TIME") || !strings.Contains(msg, wantTargetURL) || !strings.Contains(msg, wantNintendoURL) {
			t.Fatalf("combined message = %q", msg)
		}
	default:
		t.Fatalf("bad expectation %q", want)
	}
}

// TestScriptTransitions checks per-site classification, exact requests,
// closure, silent baselines and one combined alert per run.
func TestScriptTransitions(t *testing.T) {
	for _, tt := range []struct {
		name             string
		nintendo, target string   // per-run site results: o, i, f
		notes            []string // per run: "" none, N, T or NT
		failNotify       bool
	}{
		// The original Nintendo cases, with Target steady out of stock.
		{"restock", "oii", "ooo", []string{"", "N", ""}, false},
		{"available baseline", "iioi", "oooo", []string{"", "", "", "N"}, false},
		{"notify failure", "oiioi", "ooooo", []string{"", "N", "", "", "N"}, true},
		{"target restock", "ooo", "oii", []string{"", "T", ""}, false},
		{"target pickup only is not available", "oo", "oP", []string{"", ""}, false},
		{"both in one run", "oii", "oii", []string{"", "NT", ""}, false},
		{"target after nintendo", "oiiii", "ooiii", []string{"", "N", "T", "", ""}, false},
		{"nintendo after target", "ooii", "oiii", []string{"", "T", "N", ""}, false},
		{"target restocks again", "oooo", "oioi", []string{"", "T", "", "T"}, false},
		{"both in stock baseline", "ii", "ii", []string{"", ""}, false},
		{"target in stock baseline", "oo", "ii", []string{"", ""}, false},
		{"target failed first read", "ooo", "fii", []string{"", "", ""}, false},
		{"nintendo failed first read", "fio", "ooo", []string{"", "", ""}, false},
		{"target notify failure", "ooo", "oii", []string{"", "T", ""}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			notifyErr := errors.New("notification unavailable")
			if tt.failNotify {
				h.notifyErr = notifyErr
			}
			script := New()
			for i := range tt.notes {
				tr := targetReply(tt.target[i])
				if tt.target[i] == 'P' {
					tr = ok(targetPickupOnlyJSON)
				}
				err := h.run(script, nintendoReply(tt.nintendo[i]), tr)
				failed := tt.nintendo[i] == 'f' || tt.target[i] == 'f'
				switch {
				case tt.failNotify && tt.notes[i] != "":
					if !errors.Is(err, notifyErr) {
						t.Fatalf("run %d error = %v, want notify error", i, err)
					}
				case failed:
					if err == nil {
						t.Fatalf("run %d: failed check not reported", i)
					}
				case err != nil:
					t.Fatalf("run %d: %v", i, err)
				}
				targetSeq := strings.ReplaceAll(tt.target[:i+1], "P", "o")
				if script.nintendo.hasRun != wantState(tt.nintendo[:i+1]).hasRun || script.nintendo.lastOK != wantState(tt.nintendo[:i+1]).lastOK {
					t.Fatalf("run %d: wrong nintendo state: %+v", i, script.nintendo)
				}
				if script.target.hasRun != wantState(targetSeq).hasRun || script.target.lastOK != wantState(targetSeq).lastOK {
					t.Fatalf("run %d: wrong target state: %+v", i, script.target)
				}
				want := 0
				for _, n := range tt.notes[:i+1] {
					if n != "" {
						want++
					}
				}
				if len(h.messages) != want {
					t.Fatalf("run %d: %d notifications, want %d", i, len(h.messages), want)
				}
				if tt.notes[i] != "" {
					checkMessage(t, h.messages[len(h.messages)-1], tt.notes[i])
				}
			}
		})
	}
}

// TestScriptInvalidResponses prevents unrelated offers and failed fetches from
// inventing stock transitions or hiding the other site; cleanup errors are
// diagnostic only.
func TestScriptInvalidResponses(t *testing.T) {
	checkErr := errors.New("read or transport failed")

	// Nintendo failures: Nintendo's known state is kept, and a Target restock in
	// the same run still notifies (or nothing is sent when Target is steady).
	for _, tt := range []struct {
		name      string
		reply     reply
		wantError bool
	}{
		{"unrelated SKU", ok(strings.Replace(availableHTML, `"sku":"129987"`, `"sku":"other"`, 1)), true},
		{"missing data", ok(`<button>Add to cart</button>`), true},
		{"bad JSON", ok(`<script type="application/ld+json">{</script>`), true},
		{"unknown availability", ok(strings.Replace(availableHTML, "InStock", "Unknown", 1)), true},
		{"HTTP failure", reply{status: 503, body: availableHTML}, true},
		{"transport failure", reply{transportErr: checkErr}, true},
		{"read failure", reply{status: 200, readErr: checkErr}, true},
		{"close failure", reply{status: 200, body: availableHTML, closeErr: checkErr}, false},
	} {
		for _, targetRestock := range []bool{false, true} {
			name := "nintendo " + tt.name
			if targetRestock {
				name += " with target restock"
			}
			t.Run(name, func(t *testing.T) {
				h := newHarness(t)
				script := &Script{nintendo: siteState{hasRun: true, lastOK: true}, target: siteState{hasRun: true}}
				target := ok(targetOutOfStockJSON)
				if targetRestock {
					target = ok(targetInStockJSON)
				}
				err := h.run(script, tt.reply, target)
				if (err != nil) != tt.wantError {
					t.Fatalf("Run error = %v", err)
				}
				if (tt.reply.readErr != nil || tt.reply.transportErr != nil) && !errors.Is(err, checkErr) {
					t.Fatalf("lost error cause: %v", err)
				}
				if tt.wantError && !strings.Contains(err.Error(), "nintendo") {
					t.Fatalf("error does not name nintendo: %v", err)
				}
				if !script.nintendo.hasRun || !script.nintendo.lastOK {
					t.Fatal("failed check changed known state")
				}
				if tt.reply.closeErr != nil && !strings.Contains(h.logs.String(), "close nintendo response body") {
					t.Fatal("close error not logged")
				}
				switch {
				case !targetRestock && len(h.messages) != 0:
					t.Fatalf("must not notify: %q", h.messages)
				case targetRestock && len(h.messages) != 1:
					t.Fatalf("target restock hidden: %q", h.messages)
				case targetRestock:
					checkMessage(t, h.messages[0], "T")
				}
				if targetRestock && !script.target.lastOK {
					t.Fatal("target observation lost")
				}
			})
		}
	}

	// Target failures: Target's known state is kept, and a Nintendo restock in
	// the same run still notifies.
	for _, tt := range []struct {
		name  string
		reply reply
		cause error
	}{
		{name: "bot refusal 435", reply: reply{status: 435, body: targetInStockJSON}},
		{name: "forbidden", reply: reply{status: 403, body: targetInStockJSON}},
		{name: "server error", reply: reply{status: 500, body: targetInStockJSON}},
		{name: "transport failure", reply: reply{transportErr: checkErr}, cause: checkErr},
		{name: "timeout", reply: reply{transportErr: context.DeadlineExceeded}, cause: context.DeadlineExceeded},
		{name: "read failure", reply: reply{status: 200, readErr: checkErr}, cause: checkErr},
		{name: "non-JSON page", reply: ok(`<html>Access denied</html>`)},
		{name: "unknown status", reply: ok(strings.Replace(targetInStockJSON, `"IN_STOCK"`, `"LIMITED_STOCK"`, 1))},
		{name: "other tcin", reply: ok(strings.Replace(targetInStockJSON, `"1013288947"`, `"12345"`, 1))},
	} {
		t.Run("target "+tt.name, func(t *testing.T) {
			h := newHarness(t)
			script := &Script{nintendo: siteState{hasRun: true}, target: siteState{hasRun: true}}
			err := h.run(script, ok(availableHTML), tt.reply)
			if err == nil || !strings.Contains(err.Error(), "target") {
				t.Fatalf("Run error = %v, want a target failure", err)
			}
			if tt.cause != nil && !errors.Is(err, tt.cause) {
				t.Fatalf("lost error cause: %v", err)
			}
			if script.target != (siteState{hasRun: true, failing: true}) {
				t.Fatalf("failed check changed target state: %+v", script.target)
			}
			if !script.nintendo.lastOK || len(h.messages) != 1 {
				t.Fatalf("nintendo restock hidden: %+v %q", script.nintendo, h.messages)
			}
			checkMessage(t, h.messages[0], "N")
		})
	}

	t.Run("target close failure keeps the read", func(t *testing.T) {
		h := newHarness(t)
		script := &Script{nintendo: siteState{hasRun: true}, target: siteState{hasRun: true}}
		err := h.run(script, ok(unavailableHTML), reply{status: 200, body: targetInStockJSON, closeErr: checkErr})
		if err != nil {
			t.Fatalf("close failure must not fail the check: %v", err)
		}
		if !strings.Contains(h.logs.String(), "level=WARN msg=\"close target response body\"") {
			t.Fatalf("close error not logged as a warning: %s", h.logs.String())
		}
		if !script.target.lastOK || len(h.messages) != 1 {
			t.Fatalf("successful read discarded: %+v %q", script.target, h.messages)
		}
		checkMessage(t, h.messages[0], "T")
	})

	t.Run("target repeated failure then recovery", func(t *testing.T) {
		h := newHarness(t)
		script := New()
		const recovered = `msg="target check recovered"`
		for i, wantRecovered := range []int{0, 0, 0, 1, 1} {
			target := targetReply('o')
			if i < 3 {
				target = targetReply('f')
			}
			err := h.run(script, ok(unavailableHTML), target)
			if i < 3 && (err == nil || !strings.Contains(err.Error(), "target check: unexpected HTTP status 435")) {
				t.Fatalf("run %d error = %v, want the target failure", i, err)
			}
			if i >= 3 && err != nil {
				t.Fatalf("run %d: %v", i, err)
			}
			if got := strings.Count(h.logs.String(), recovered); got != wantRecovered {
				t.Fatalf("run %d: %d recovered lines, want %d", i, got, wantRecovered)
			}
		}
		if strings.Contains(h.logs.String(), `msg="nintendo check recovered"`) || len(h.messages) != 0 {
			t.Fatal("steady nintendo must not recover or notify")
		}
	})
}

// TestTargetAvailable pins the ship-to-home rule: only the exact TCIN's
// shipping_options.availability_status decides, and unreadable data errors.
func TestTargetAvailable(t *testing.T) {
	withShipping := func(status string) string {
		return strings.Replace(targetOutOfStockJSON, shippingOOS, `"shipping_options":{"availability_status":"`+status+`"`, 1)
	}
	for _, tt := range []struct {
		name    string
		body    string
		want    bool
		wantErr bool
	}{
		// Its pickup and scheduled-delivery statuses stay OUT_OF_STOCK.
		{name: "shipping only in stock", body: targetInStockJSON, want: true},
		{name: "out of stock", body: targetOutOfStockJSON},
		{name: "unavailable", body: withShipping("UNAVAILABLE")},
		{name: "discontinued", body: withShipping("DISCONTINUED")},
		{name: "pickup only in stock", body: targetPickupOnlyJSON},
		{name: "empty", body: "", wantErr: true},
		{name: "non-JSON", body: "<html></html>", wantErr: true},
		{name: "null", body: "null", wantErr: true},
		{name: "empty object", body: "{}", wantErr: true},
		{name: "missing tcin", body: strings.Replace(targetInStockJSON, `"tcin":"1013288947",`, "", 1), wantErr: true},
		{name: "other tcin", body: strings.Replace(targetInStockJSON, "1013288947", "1013288948", 1), wantErr: true},
		{name: "missing fulfillment", body: `{"data":{"product":{"tcin":"1013288947"}}}`, wantErr: true},
		{name: "missing shipping options", body: `{"data":{"product":{"tcin":"1013288947","fulfillment":{"scheduled_delivery":{"availability_status":"IN_STOCK"}}}}}`, wantErr: true},
		{name: "missing status", body: `{"data":{"product":{"tcin":"1013288947","fulfillment":{"shipping_options":{"loyalty_availability_status":"IN_STOCK"}}}}}`, wantErr: true},
		{name: "unknown status", body: withShipping("LIMITED_STOCK"), wantErr: true},
		{name: "lowercase status", body: withShipping("in_stock"), wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := targetAvailable([]byte(tt.body))
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Fatalf("targetAvailable = %v, %v; want %v, error %v", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

type errorReader struct{ err error }

// Read simulates a response body that fails during consumption.
func (r errorReader) Read([]byte) (int, error) { return 0, r.err }
