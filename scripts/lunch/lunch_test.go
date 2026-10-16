package lunch

import (
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

	"github.com/trriplejay/notinv/internal/runner"
	"github.com/trriplejay/notinv/internal/store"
)

const bryantURL = "https://api.mealviewer.com/api/v4/school/Bryant/03-09-2026/03-09-2026/0"

// menuJSON follows the MealViewer v4 shape: menuSchedules -> menuBlocks ->
// cafeteriaLineList.data -> foodItemList.data -> item_Name.
const menuJSON = `{"menuSchedules":[{"dateInformation":{"weekDay":1},"menuBlocks":[
{"blockName":"Breakfast","cafeteriaLineList":{"data":[{"name":"Line","foodItemList":{"data":[{"item_Name":"Cereal"},{"item_Name":"Yogurt"}]}}]}},
{"blockName":"Lunch","cafeteriaLineList":{"data":[
{"name":"Entree","foodItemList":{"data":[{"item_Name":"Pizza"},{"item_Name":"Chicken Nuggets"}]}},
{"name":"Sides","foodItemList":{"data":[{"item_Name":"Green Beans"}]}}]}}]}]}`

type roundTripper func(*http.Request) (*http.Response, error)

// RoundTrip returns the canned response without using the network.
func (f roundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func respond(status int, body string) func(*http.Request) (*http.Response, error) {
	return func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: status,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     http.Header{},
		}, nil
	}
}

// harness records requests and notifications for one script instance.
type harness struct {
	mu       sync.Mutex
	urls     []string
	messages []string
	notifyEr error
	rc       *runner.Context
}

func newHarness(transport func(*http.Request) (*http.Response, error)) *harness {
	h := &harness{}
	h.rc = &runner.Context{
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		HTTP: &http.Client{Transport: roundTripper(func(req *http.Request) (*http.Response, error) {
			h.mu.Lock()
			h.urls = append(h.urls, req.URL.String())
			h.mu.Unlock()
			return transport(req)
		})},
		Notify: func(_ context.Context, msg string) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.messages = append(h.messages, msg)
			return h.notifyEr
		},
	}
	return h
}

func (h *harness) counts() (requests, notifications int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.urls), len(h.messages)
}

// primed returns a script whose silent first run is already spent.
func primed(at time.Time) *Script {
	return &Script{hasRun: true, now: func() time.Time { return at }}
}

func TestIdentity(t *testing.T) {
	s := New()
	if s.Name() != "lunch" || s.Schedule() != "CRON_TZ=America/Los_Angeles 0 6 * * *" {
		t.Fatalf("identity = %q %q", s.Name(), s.Schedule())
	}
}

// CLM-4: the first run is silent; every later run checks and notifies.
func TestFirstRunSilent(t *testing.T) {
	t.Setenv("LUNCH_SCHOOL", "")
	h := newHarness(func(*http.Request) (*http.Response, error) {
		t.Error("first run made an HTTP request")
		return nil, errors.New("unexpected request")
	})
	h.rc.Notify = func(context.Context, string) error {
		t.Error("first run notified")
		return nil
	}
	s := New()
	if err := s.Run(t.Context(), h.rc); err != nil {
		t.Fatalf("first run: %v", err)
	}

	h = newHarness(respond(http.StatusOK, menuJSON))
	if err := s.Run(t.Context(), h.rc); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if reqs, notes := h.counts(); reqs != 1 || notes != 1 {
		t.Fatalf("second run made %d requests and %d notifications, want 1 and 1", reqs, notes)
	}
}

// CLM-5: the URL uses the Pacific date, which differs from the UTC date here.
func TestMenuURLPacificDate(t *testing.T) {
	t.Setenv("LUNCH_SCHOOL", "")
	h := newHarness(respond(http.StatusOK, menuJSON))
	at := time.Date(2026, time.March, 10, 3, 0, 0, 0, time.UTC)
	if err := primed(at).Run(t.Context(), h.rc); err != nil {
		t.Fatal(err)
	}
	if len(h.urls) != 1 || h.urls[0] != bryantURL {
		t.Fatalf("urls = %v, want [%s]", h.urls, bryantURL)
	}
}

// CLM-10: LUNCH_SCHOOL selects the school, escaped; unset or empty means Bryant.
func TestSchoolSelection(t *testing.T) {
	at := time.Date(2026, time.March, 10, 3, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name, env, want string
	}{
		{"empty", "", "Bryant"},
		{"set", "Lincoln", "Lincoln"},
		{"escaped", "Mt Hood/East", "Mt%20Hood%2FEast"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("LUNCH_SCHOOL", tt.env)
			h := newHarness(respond(http.StatusOK, menuJSON))
			if err := primed(at).Run(t.Context(), h.rc); err != nil {
				t.Fatal(err)
			}
			want := "https://api.mealviewer.com/api/v4/school/" + tt.want + "/03-09-2026/03-09-2026/0"
			if len(h.urls) != 1 || h.urls[0] != want {
				t.Fatalf("urls = %v, want [%s]", h.urls, want)
			}
		})
	}
}

// CLM-6: only Lunch items are listed, one per line.
func TestMessageListsOnlyLunchItems(t *testing.T) {
	t.Setenv("LUNCH_SCHOOL", "")
	h := newHarness(respond(http.StatusOK, menuJSON))
	if err := primed(time.Now()).Run(t.Context(), h.rc); err != nil {
		t.Fatal(err)
	}
	if len(h.messages) != 1 {
		t.Fatalf("messages = %q, want one", h.messages)
	}
	var lines []string
	for _, line := range strings.Split(h.messages[0], "\n") {
		if line == "Pizza" || line == "Chicken Nuggets" || line == "Green Beans" {
			lines = append(lines, line)
		}
	}
	if strings.Join(lines, ",") != "Pizza,Chicken Nuggets,Green Beans" {
		t.Errorf("message %q does not list the Lunch items each on its own line", h.messages[0])
	}
	for _, banned := range []string{"Cereal", "Yogurt"} {
		if strings.Contains(h.messages[0], banned) {
			t.Errorf("message %q contains breakfast item %q", h.messages[0], banned)
		}
	}
}

// CLM-7: absent schedules, Lunch block or items give one "no menu" message.
func TestNoMenuFound(t *testing.T) {
	t.Setenv("LUNCH_SCHOOL", "")
	for _, tt := range []struct{ name, body string }{
		{"empty schedules", `{"menuSchedules":[]}`},
		{"missing schedules", `{}`},
		{"no lunch block", `{"menuSchedules":[{"menuBlocks":[{"blockName":"Breakfast","cafeteriaLineList":{"data":[{"foodItemList":{"data":[{"item_Name":"Cereal"}]}}]}}]}]}`},
		{"empty schedule", `{"menuSchedules":[{}]}`},
		{"empty lunch block", `{"menuSchedules":[{"menuBlocks":[{"blockName":"Lunch","cafeteriaLineList":{"data":[]}}]}]}`},
		{"lunch line without items", `{"menuSchedules":[{"menuBlocks":[{"blockName":"Lunch","cafeteriaLineList":{"data":[{"foodItemList":{"data":[]}}]}}]}]}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(respond(http.StatusOK, tt.body))
			if err := primed(time.Now()).Run(t.Context(), h.rc); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(h.messages) != 1 || !strings.Contains(h.messages[0], "No lunch menu") {
				t.Fatalf("messages = %q, want one 'no lunch menu' notice", h.messages)
			}
		})
	}
}

// CLM-8: transport errors, bad status and bad JSON notify once and fail.
func TestFetchFailures(t *testing.T) {
	t.Setenv("LUNCH_SCHOOL", "")
	for _, tt := range []struct {
		name      string
		transport func(*http.Request) (*http.Response, error)
	}{
		{"transport error", func(*http.Request) (*http.Response, error) { return nil, errors.New("connection refused") }},
		{"http 500", respond(http.StatusInternalServerError, "oops")},
		{"malformed json", respond(http.StatusOK, "{not json")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(tt.transport)
			err := primed(time.Now()).Run(t.Context(), h.rc)
			if err == nil {
				t.Fatal("Run returned nil on a failed fetch")
			}
			if len(h.messages) != 1 || !strings.Contains(h.messages[0], "could not be retrieved") {
				t.Fatalf("messages = %q, want one failure notice", h.messages)
			}
		})
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

// CLM-8: driven by the runner, a failed fetch is stored as OK=false with an error.
func TestFailedFetchRecordedByRunner(t *testing.T) {
	t.Setenv("LUNCH_SCHOOL", "")
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(respond(http.StatusInternalServerError, "oops"))
		st := &fakeRunStore{}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		// hasRun is spent so the runner's startup run performs the real check.
		go func() { done <- runner.Run(ctx, primed(time.Now()), h.rc, st) }()
		synctest.Wait()
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		runs := st.snapshot()
		if len(runs) != 1 || runs[0].OK || runs[0].Err == nil || *runs[0].Err == "" {
			t.Fatalf("recorded runs = %+v, want one OK=false run with an error", runs)
		}
		if runs[0].Script != "lunch" {
			t.Errorf("recorded script %q, want lunch", runs[0].Script)
		}
		if _, notes := h.counts(); notes != 1 {
			t.Errorf("%d notifications, want 1", notes)
		}
	})
}

// CLM-9: a notification failure is returned, wrapped.
func TestNotifyErrorPropagates(t *testing.T) {
	t.Setenv("LUNCH_SCHOOL", "")
	notifyErr := errors.New("discord unavailable")
	for _, body := range []string{menuJSON, `{"menuSchedules":[]}`} {
		h := newHarness(respond(http.StatusOK, body))
		h.notifyEr = notifyErr
		err := primed(time.Now()).Run(t.Context(), h.rc)
		if !errors.Is(err, notifyErr) {
			t.Errorf("Run error = %v, want it to wrap %v", err, notifyErr)
		}
	}
	// A fetch failure whose notification also fails keeps both errors.
	h := newHarness(respond(http.StatusInternalServerError, ""))
	h.notifyEr = notifyErr
	if err := primed(time.Now()).Run(t.Context(), h.rc); !errors.Is(err, notifyErr) {
		t.Errorf("Run error = %v, want it to wrap %v", err, notifyErr)
	}
}

// CLM-1: under the real runner the startup run is silent and recorded OK, and
// the next run fires at 06:00 Pacific with the Pacific-date request.
func TestRunnerSchedulesLunchAtSixPacific(t *testing.T) {
	t.Setenv("LUNCH_SCHOOL", "")
	synctest.Test(t, func(t *testing.T) {
		// The bubble starts at 2000-01-01T00:00Z (Dec 31 16:00 PST); the next
		// 06:00 PST is 14:00Z on Jan 1, so the Pacific date is 01-01-2000.
		const wantURL = "https://api.mealviewer.com/api/v4/school/Bryant/01-01-2000/01-01-2000/0"
		h := newHarness(respond(http.StatusOK, menuJSON))
		st := &fakeRunStore{}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- runner.Run(ctx, New(), h.rc, st) }()
		defer func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("Run: %v", err)
			}
		}()

		synctest.Wait()
		if reqs, notes := h.counts(); reqs != 0 || notes != 0 {
			t.Fatalf("startup made %d requests and %d notifications, want none", reqs, notes)
		}
		if runs := st.snapshot(); len(runs) != 1 || !runs[0].OK {
			t.Fatalf("startup recorded %+v, want one OK run", runs)
		}

		time.Sleep(13 * time.Hour) // 13:00Z: before 06:00 PST.
		synctest.Wait()
		if reqs, notes := h.counts(); reqs != 0 || notes != 0 {
			t.Fatalf("fired early (UTC 06:00 schedule?): %d requests, %d notifications", reqs, notes)
		}

		time.Sleep(2 * time.Hour) // Past 14:00Z.
		synctest.Wait()
		reqs, notes := h.counts()
		if reqs != 1 || notes != 1 {
			t.Fatalf("scheduled run made %d requests and %d notifications, want 1 and 1", reqs, notes)
		}
		if h.urls[0] != wantURL {
			t.Errorf("url = %s, want %s", h.urls[0], wantURL)
		}
		lines := strings.Split(h.messages[0], "\n")
		for _, want := range []string{"Pizza", "Chicken Nuggets", "Green Beans"} {
			found := false
			for _, line := range lines {
				found = found || line == want
			}
			if !found {
				t.Errorf("message %q lacks line %q", h.messages[0], want)
			}
		}
		if strings.Contains(h.messages[0], "Cereal") {
			t.Error("message contains a breakfast item")
		}
		if runs := st.snapshot(); len(runs) != 2 || !runs[1].OK {
			t.Errorf("recorded %+v, want two OK runs", runs)
		}
	})
}
