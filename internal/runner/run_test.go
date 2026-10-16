package runner

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/trriplejay/notinv/internal/store"
)

var _ RunStore = (*store.Store)(nil)

type fakeScript struct {
	name     string
	schedule string
	run      func(context.Context, *Context) error
}

func (s fakeScript) Name() string     { return s.name }
func (s fakeScript) Schedule() string { return s.schedule }
func (s fakeScript) Run(ctx context.Context, rc *Context) error {
	return s.run(ctx, rc)
}

type fakeStore struct {
	mu       sync.Mutex
	runs     []store.Run
	inserted chan struct{}
	err      error
	enabled  map[string]bool
	stateErr map[string]error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		inserted: make(chan struct{}, 16),
		enabled:  make(map[string]bool),
		stateErr: make(map[string]error),
	}
}

func (s *fakeStore) InsertRun(_ context.Context, run store.Run) error {
	s.mu.Lock()
	s.runs = append(s.runs, run)
	s.mu.Unlock()
	select {
	case s.inserted <- struct{}{}:
	default:
	}
	return s.err
}

func (s *fakeStore) ScriptEnabled(_ context.Context, script string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stateErr != nil {
		if err, ok := s.stateErr[script]; ok {
			return false, err
		}
	}
	if s.enabled != nil {
		if en, ok := s.enabled[script]; ok {
			return en, nil
		}
	}
	return true, nil
}

func (s *fakeStore) setEnabled(script string, enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.enabled == nil {
		s.enabled = make(map[string]bool)
	}
	s.enabled[script] = enabled
}

func (s *fakeStore) setStateErr(script string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stateErr == nil {
		s.stateErr = make(map[string]error)
	}
	s.stateErr[script] = err
}

func (s *fakeStore) clearStateErr(script string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stateErr != nil {
		delete(s.stateErr, script)
	}
}

func (s *fakeStore) snapshot() []store.Run {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]store.Run(nil), s.runs...)
}

func (s *fakeStore) waitForRuns(t *testing.T, count int) []store.Run {
	t.Helper()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for {
		if runs := s.snapshot(); len(runs) >= count {
			return runs
		}
		select {
		case <-s.inserted:
		case <-timer.C:
			t.Fatalf("timed out waiting for %d runs", count)
		}
	}
}

func quietContext() *Context {
	return &Context{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// startTestRunner returns an idempotent stop-and-join function. Defer it inside
// the synctest bubble so even assertion failures cancel the runner before exit.
func startTestRunner(t *testing.T, script Script, rc *Context, runs RunStore) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, script, rc, runs)
	}()
	var once sync.Once
	return func() {
		t.Helper()
		once.Do(func() {
			cancel()
			timer := time.NewTimer(200 * time.Millisecond)
			defer timer.Stop()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("Run: %v", err)
				}
			case <-timer.C:
				t.Error("Run did not stop within 200ms")
			}
		})
	}
}

// Cron rounds sub-second @every durations up to one second. synctest advances
// the real runner's timers without wall-clock sleeps or production clock hooks.
func TestRunRecordsRuns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runs := newFakeStore()
		rc := quietContext()
		starts := make(chan time.Time, 16)
		script := fakeScript{name: "success", schedule: "@every 1s", run: func(ctx context.Context, got *Context) error {
			if got != rc || ctx.Done() == nil {
				t.Error("script did not receive its context and services")
			}
			starts <- time.Now()
			time.Sleep(25 * time.Millisecond) // Simulated work, not synchronization.
			return nil
		}}
		started := time.Now()
		stop := startTestRunner(t, script, rc, runs)
		defer stop()
		recorded := runs.waitForRuns(t, 2)
		stop()
		for _, run := range recorded {
			if run.Script != script.name || !run.OK || run.Err != nil || run.DurationMs != 25 {
				t.Errorf("unexpected run: %+v", run)
			}
			select {
			case start := <-starts:
				if !run.StartedAt.Equal(start) {
					t.Errorf("StartedAt = %v, want %v", run.StartedAt, start)
				}
			default:
				t.Error("record persisted without a script invocation")
			}
		}
		if !recorded[0].StartedAt.Equal(started) {
			t.Error("first run did not start immediately")
		}
		if !recorded[1].StartedAt.Equal(recorded[0].StartedAt.Add(time.Second)) {
			t.Error("runs did not follow the one-second schedule")
		}
	})
}

func TestRunSequential(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runs := newFakeStore()
		var active atomic.Bool
		script := fakeScript{name: "slow", schedule: "@every 1s", run: func(context.Context, *Context) error {
			if active.Swap(true) {
				t.Error("concurrent invocations of the same script")
			}
			defer active.Store(false)
			// Work lasts longer than the interval to expose per-run goroutines.
			time.Sleep(1500 * time.Millisecond)
			return nil
		}}
		stop := startTestRunner(t, script, quietContext(), runs)
		defer stop()
		recorded := runs.waitForRuns(t, 3)
		stop()
		for i, run := range recorded {
			if run.DurationMs != 1500 {
				t.Errorf("duration = %d, want 1500", run.DurationMs)
			}
			if i > 0 && !run.StartedAt.Equal(recorded[i-1].StartedAt.Add(2*time.Second)) {
				t.Error("next fire was not recomputed after the preceding run")
			}
		}
	})
}

func TestRunInvalidSchedule(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var buf bytes.Buffer
		var calls atomic.Int32
		runs := newFakeStore()
		script := fakeScript{name: "invalid", schedule: "not a cron", run: func(context.Context, *Context) error {
			calls.Add(1)
			return nil
		}}
		rc := &Context{Log: slog.New(slog.NewTextHandler(&buf, nil))}
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		start := time.Now()
		if err := Run(ctx, script, rc, runs); err == nil {
			t.Fatal("invalid schedule returned nil error")
		}
		if time.Since(start) >= 200*time.Millisecond {
			t.Error("invalid schedule did not return promptly")
		}
		if calls.Load() != 0 || len(runs.snapshot()) != 0 {
			t.Fatal("invalid schedule executed or persisted a run")
		}
		for _, want := range []string{"level=ERROR", "invalid schedule", "script=invalid", "error="} {
			if !strings.Contains(buf.String(), want) {
				t.Errorf("log %q missing %q", buf.String(), want)
			}
		}
	})
}

func TestRunCronSchedule(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runs := newFakeStore()
		script := fakeScript{name: "cron", schedule: "* * * * *", run: func(context.Context, *Context) error {
			return nil
		}}
		stop := startTestRunner(t, script, quietContext(), runs)
		defer stop()
		synctest.Wait()
		if recorded := runs.snapshot(); len(recorded) != 1 || !recorded[0].OK {
			t.Fatalf("startup recorded %+v, want one immediate successful run", recorded)
		}
		// Explicitly advance to the minute boundary of synctest's fake clock.
		time.Sleep(time.Minute)
		synctest.Wait()
		recorded := runs.snapshot()
		if len(recorded) != 2 || !recorded[1].OK {
			t.Fatalf("cron schedule recorded %+v, want a second successful run", recorded)
		}
	})
}

const pacificSchedule = "CRON_TZ=America/Los_Angeles 0 6 * * *"

// A CRON_TZ schedule fires at 06:00 Los Angeles wall time on both sides of a
// DST change; dropping the prefix would yield 06:00Z in both seasons.
func TestCronScheduleTimezone(t *testing.T) {
	sched, err := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor).Parse(pacificSchedule)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name  string
		from  time.Time
		wantZ string
	}{
		{"PST January", time.Date(2026, time.January, 15, 0, 0, 0, 0, time.UTC), "2026-01-15T14:00:00Z"},
		{"PDT July", time.Date(2026, time.July, 15, 0, 0, 0, 0, time.UTC), "2026-07-15T13:00:00Z"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := sched.Next(tt.from).UTC().Format(time.RFC3339); got != tt.wantZ {
				t.Errorf("Next(%v) = %s, want %s", tt.from, got, tt.wantZ)
			}
		})
	}
}

// The real runner executes a CRON_TZ script at 06:00 Los Angeles time, not 06:00 UTC.
func TestRunCronScheduleTimezone(t *testing.T) {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	synctest.Test(t, func(t *testing.T) {
		runs := newFakeStore()
		script := fakeScript{name: "pacific", schedule: pacificSchedule, run: func(context.Context, *Context) error {
			return nil
		}}
		stop := startTestRunner(t, script, quietContext(), runs)
		defer stop()
		synctest.Wait()
		if recorded := runs.snapshot(); len(recorded) != 1 {
			t.Fatalf("startup recorded %+v, want one immediate run", recorded)
		}
		// Fire time is 06:00 LA the next morning; sleep past it, not to it.
		time.Sleep(25 * time.Hour)
		synctest.Wait()
		recorded := runs.snapshot()
		if len(recorded) != 2 || !recorded[1].OK {
			t.Fatalf("recorded %+v, want a second successful run", recorded)
		}
		if h, m, s := recorded[1].StartedAt.In(loc).Clock(); h != 6 || m != 0 || s != 0 {
			t.Errorf("second run at %v LA time, want 06:00:00", recorded[1].StartedAt.In(loc))
		}
	})
}

// A host suspend pauses timers but not the wall clock. When the wall clock
// passes a scheduled time during a suspend, the run happens within maxWait of
// wake rather than after the full remaining timer, and missed runs are not
// replayed.
func TestRunAfterSuspend(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var suspended atomic.Int64
		defer func(orig func() time.Time) { wallNow = orig }(wallNow)
		wallNow = func() time.Time { return time.Now().Add(time.Duration(suspended.Load())).Round(0) }

		runs := newFakeStore()
		script := fakeScript{name: "pacific", schedule: pacificSchedule, run: func(context.Context, *Context) error {
			return nil
		}}
		stop := startTestRunner(t, script, quietContext(), runs)
		defer stop()
		synctest.Wait()
		if recorded := runs.snapshot(); len(recorded) != 1 {
			t.Fatalf("startup recorded %+v, want one immediate run", recorded)
		}
		// The bubble starts at 16:00 Los Angeles time, 14h before the 06:00 run.
		// Wait one awake hour, then suspend past 06:00 and past a second 06:00.
		time.Sleep(time.Hour)
		synctest.Wait()
		suspended.Store(int64(40 * time.Hour))
		time.Sleep(maxWait)
		synctest.Wait()
		if recorded := runs.snapshot(); len(recorded) != 2 {
			t.Fatalf("recorded %d runs within maxWait of wake, want 2 (startup + one catch-up)", len(recorded))
		}
	})
}

func TestRunCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runs := newFakeStore()
		var calls atomic.Int32
		script := fakeScript{name: "long-wait", schedule: "@every 1h", run: func(context.Context, *Context) error {
			calls.Add(1)
			return nil
		}}
		stop := startTestRunner(t, script, quietContext(), runs)
		defer stop()
		synctest.Wait() // The immediate run is done; Run is in the cancellable timer wait.
		stop()          // Requires a clean return within 200ms, not an hour.
		if calls.Load() != 1 || len(runs.snapshot()) != 1 {
			t.Errorf("calls = %d, runs = %d; want only the immediate startup run", calls.Load(), len(runs.snapshot()))
		}
	})
}

func TestRunRecoversPanicAndContinues(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runs := newFakeStore()
		var calls atomic.Int32
		script := fakeScript{name: "recovering", schedule: "@every 1s", run: func(context.Context, *Context) error {
			if calls.Add(1) == 1 {
				panic("first run")
			}
			return nil
		}}
		stop := startTestRunner(t, script, quietContext(), runs)
		defer stop()
		recorded := runs.waitForRuns(t, 2)
		stop()
		if recorded[0].OK || recorded[0].Err == nil || *recorded[0].Err != "panic: first run" {
			t.Errorf("panic run = %+v", recorded[0])
		}
		if !recorded[1].OK || recorded[1].Err != nil {
			t.Errorf("run after panic = %+v", recorded[1])
		}
	})
}

func TestRunIsolation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		badRuns, goodRuns := newFakeStore(), newFakeStore()
		bad := fakeScript{name: "panics", schedule: "@every 1s", run: func(context.Context, *Context) error {
			panic("isolated")
		}}
		good := fakeScript{name: "healthy", schedule: "@every 1s", run: func(context.Context, *Context) error {
			return nil
		}}
		stopBad := startTestRunner(t, bad, quietContext(), badRuns)
		defer stopBad()
		stopGood := startTestRunner(t, good, quietContext(), goodRuns)
		defer stopGood()
		failed := badRuns.waitForRuns(t, 2)
		succeeded := goodRuns.waitForRuns(t, 2)
		stopBad()
		stopGood()
		for _, run := range failed {
			if run.Script != bad.name || run.OK || run.Err == nil || *run.Err != "panic: isolated" {
				t.Errorf("panicking script run = %+v", run)
			}
		}
		for _, run := range succeeded {
			if run.Script != good.name || !run.OK || run.Err != nil {
				t.Errorf("healthy script run = %+v", run)
			}
		}
	})
}

func TestRunLogsAndRecordsOutcomes(t *testing.T) {
	for _, outcome := range []string{"success", "error", "panic"} {
		t.Run(outcome, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var buf bytes.Buffer
				rc := &Context{Log: slog.New(slog.NewTextHandler(&buf, nil))}
				runs := newFakeStore()
				script := fakeScript{name: "logged", schedule: "@every 1s", run: func(context.Context, *Context) error {
					// No other goroutine writes logs until this invocation returns.
					if !strings.Contains(buf.String(), `msg="run start" script=logged`) || strings.Contains(buf.String(), `msg="run end"`) {
						t.Error("start/end logging did not bracket the invocation")
					}
					switch outcome {
					case "error":
						return errors.New("check failed")
					case "panic":
						panic("check crashed")
					default:
						return nil
					}
				}}
				stop := startTestRunner(t, script, rc, runs)
				defer stop()
				recorded := runs.waitForRuns(t, 1)
				stop() // Join before reading the logger's bytes.Buffer.
				wantOK, wantError, errorAttr := true, "", `error=""`
				switch outcome {
				case "error":
					wantOK, wantError, errorAttr = false, "check failed", `error="check failed"`
				case "panic":
					wantOK, wantError, errorAttr = false, "panic: check crashed", `error="panic: check crashed"`
				}
				run := recorded[0]
				if run.OK != wantOK || (wantOK && run.Err != nil) || (!wantOK && (run.Err == nil || *run.Err != wantError)) {
					t.Errorf("recorded outcome = %+v", run)
				}
				var end string
				for _, line := range strings.Split(buf.String(), "\n") {
					if strings.Contains(line, `msg="run end"`) {
						end = line
						break
					}
				}
				okAttr := "ok=true"
				if !wantOK {
					okAttr = "ok=false"
				}
				for _, want := range []string{"script=logged", "duration_ms=0", okAttr, errorAttr} {
					if !strings.Contains(end, want) {
						t.Errorf("end entry %q missing %q", end, want)
					}
				}
			})
		})
	}
}

func TestRunContinuesAfterStoreError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var buf bytes.Buffer
		rc := &Context{Log: slog.New(slog.NewTextHandler(&buf, nil))}
		runs := newFakeStore()
		runs.err = errors.New("storage unavailable")
		script := fakeScript{name: "store-error", schedule: "@every 1s", run: func(context.Context, *Context) error {
			return nil
		}}
		stop := startTestRunner(t, script, rc, runs)
		defer stop()
		runs.waitForRuns(t, 2)
		stop()
		if strings.Count(buf.String(), `msg="insert run failed" script=store-error error="storage unavailable"`) < 2 {
			t.Errorf("store errors were not logged for each run: %s", buf.String())
		}
	})
}

// CLM-3: disabled scripts do not run at startup or on scheduled cron ticks,
// and other concurrently running scripts are unaffected.
func TestRunDisabledGate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runs := newFakeStore()
		runs.setEnabled("scriptA", false)
		runs.setEnabled("scriptB", true)

		var callsA, callsB atomic.Int32
		scriptA := fakeScript{name: "scriptA", schedule: "* * * * *", run: func(context.Context, *Context) error {
			callsA.Add(1)
			return nil
		}}
		scriptB := fakeScript{name: "scriptB", schedule: "* * * * *", run: func(context.Context, *Context) error {
			callsB.Add(1)
			return nil
		}}

		stopA := startTestRunner(t, scriptA, quietContext(), runs)
		defer stopA()
		stopB := startTestRunner(t, scriptB, quietContext(), runs)
		defer stopB()

		// Immediate startup run for B; A must be skipped immediately.
		synctest.Wait()
		if callsA.Load() != 0 {
			t.Fatalf("startup: scriptA calls = %d, want 0", callsA.Load())
		}
		if callsB.Load() != 1 {
			t.Fatalf("startup: scriptB calls = %d, want 1", callsB.Load())
		}

		// Advance across several cron ticks.
		for tick := 1; tick <= 3; tick++ {
			time.Sleep(time.Minute)
			synctest.Wait()
			if callsA.Load() != 0 {
				t.Fatalf("tick %d: scriptA calls = %d, want 0", tick, callsA.Load())
			}
			if callsB.Load() != int32(1+tick) {
				t.Fatalf("tick %d: scriptB calls = %d, want %d", tick, callsB.Load(), 1+tick)
			}
		}

		stopA()
		stopB()

		// Verify recorded runs: none for scriptA, exactly 4 successful runs for scriptB.
		recorded := runs.snapshot()
		if len(recorded) != 4 {
			t.Fatalf("recorded runs = %+v, want 4 runs for scriptB only", recorded)
		}
		for _, r := range recorded {
			if r.Script != "scriptB" || !r.OK {
				t.Errorf("unexpected run record: %+v", r)
			}
		}
	})
}

// CLM-4: disabling mid-flight does not cancel the in-progress run; re-enabling
// between ticks waits for the next scheduled fire rather than firing immediately.
func TestRunDisabledMidFlightAndReEnabled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runs := newFakeStore()
		inRun := make(chan struct{})
		release := make(chan struct{})
		var calls atomic.Int32

		script := fakeScript{name: "flight", schedule: "* * * * *", run: func(ctx context.Context, rc *Context) error {
			calls.Add(1)
			if calls.Load() == 1 {
				close(inRun)
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-release:
					return nil
				}
			}
			return nil
		}}

		stop := startTestRunner(t, script, quietContext(), runs)
		defer stop()

		// Wait until first run is blocked inside script.Run.
		<-inRun

		// Flip store state to disabled while the run is in flight.
		runs.setEnabled("flight", false)

		// Release the blocked run; it must complete successfully and record its run row.
		close(release)
		synctest.Wait()

		recorded := runs.snapshot()
		if len(recorded) != 1 || recorded[0].Script != "flight" || !recorded[0].OK {
			t.Fatalf("completed in-flight run was not recorded: %+v", recorded)
		}
		if calls.Load() != 1 {
			t.Fatalf("calls = %d, want 1", calls.Load())
		}

		// Re-enable the script between ticks.
		runs.setEnabled("flight", true)

		// Advance time partially (30s): still before the next cron minute boundary.
		// Re-enabling must NOT trigger an immediate run.
		time.Sleep(30 * time.Second)
		synctest.Wait()

		if calls.Load() != 1 {
			t.Fatalf("script ran before scheduled cron time: calls = %d, want 1", calls.Load())
		}
		if len(runs.snapshot()) != 1 {
			t.Fatalf("extra run recorded before cron time: %+v", runs.snapshot())
		}

		// Advance past the next cron boundary (35s more -> 65s total).
		// Exactly one run must occur at the cron tick.
		time.Sleep(35 * time.Second)
		synctest.Wait()

		if calls.Load() != 2 {
			t.Fatalf("calls after cron boundary = %d, want 2", calls.Load())
		}
		recorded = runs.snapshot()
		if len(recorded) != 2 || !recorded[1].OK {
			t.Fatalf("runs after cron boundary = %+v, want 2 OK runs", recorded)
		}

		stop()
	})
}

// CLM-4: a script disabled at tick N skips tick N; re-enabling before tick N+1
// fires exactly at tick N+1 (single-wait rather than double-wait).
func TestRunDisabledAtTickAndReEnabledBeforeNextTick(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runs := newFakeStore()
		var calls atomic.Int32
		script := fakeScript{name: "re-enable", schedule: "* * * * *", run: func(context.Context, *Context) error {
			calls.Add(1)
			return nil
		}}

		stop := startTestRunner(t, script, quietContext(), runs)
		defer stop()

		// Immediate startup run (tick 0).
		synctest.Wait()
		if calls.Load() != 1 {
			t.Fatalf("startup: calls = %d, want 1", calls.Load())
		}

		// Disable the script after startup, before tick 1.
		runs.setEnabled("re-enable", false)

		// Advance to tick 1 (1 minute).
		time.Sleep(time.Minute)
		synctest.Wait()

		// Tick 1 passed: script was disabled, so no run occurred.
		if calls.Load() != 1 {
			t.Fatalf("tick 1: calls = %d, want 1 (disabled)", calls.Load())
		}

		// Re-enable the script before tick 2 (after 30s into period 1->2).
		time.Sleep(30 * time.Second)
		synctest.Wait()
		runs.setEnabled("re-enable", true)

		// Advance another 15s (total 45s into period): still before tick 2.
		time.Sleep(15 * time.Second)
		synctest.Wait()
		if calls.Load() != 1 {
			t.Fatalf("before tick 2: calls = %d, want 1 (re-enabling must not trigger immediate run)", calls.Load())
		}

		// Advance 20s (total 65s from tick 1, past tick 2 boundary at 60s).
		time.Sleep(20 * time.Second)
		synctest.Wait()

		// Exactly one run should have occurred at tick 2.
		if calls.Load() != 2 {
			t.Fatalf("at tick 2: calls = %d, want 2 (missed tick 2 due to extra wait)", calls.Load())
		}

		recorded := runs.snapshot()
		if len(recorded) != 2 {
			t.Fatalf("recorded runs = %+v, want 2", recorded)
		}
	})
}

// Governor ruling: remember last known state on read error.
// If the script was previously read as disabled and a subsequent read fails,
// it remains disabled and skips execution while logging a warning.
func TestRunSkipsWhenDisabledAndScriptEnabledFails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var buf bytes.Buffer
		rc := &Context{Log: slog.New(slog.NewTextHandler(&buf, nil))}
		runs := newFakeStore()

		var calls atomic.Int32
		script := fakeScript{name: "disabled-then-fail", schedule: "@every 1s", run: func(context.Context, *Context) error {
			calls.Add(1)
			return nil
		}}

		// Initial state is disabled: successful read returns disabled.
		runs.setEnabled("disabled-then-fail", false)

		stop := startTestRunner(t, script, rc, runs)
		defer stop()

		// Startup check (tick 0): read succeeds, returns disabled -> skipped.
		synctest.Wait()
		if n := calls.Load(); n != 0 {
			t.Fatalf("startup: script ran %d times while disabled, want 0", n)
		}

		// Subsequent read now fails with an error (e.g. database down).
		runs.setStateErr("disabled-then-fail", errors.New("state db unavailable"))

		// Advance 2 ticks. Each tick should log WARN, remember it was disabled, and skip.
		for i := 0; i < 2; i++ {
			time.Sleep(time.Second)
			synctest.Wait()
		}

		if n := calls.Load(); n != 0 {
			t.Fatalf("script ran %d times after read failed when previously disabled, want 0", n)
		}
		recorded := runs.snapshot()
		if len(recorded) != 0 {
			t.Fatalf("recorded runs = %+v, want 0", recorded)
		}
		if !strings.Contains(buf.String(), `level=WARN msg="check script enabled failed" script=disabled-then-fail error="state db unavailable"`) {
			t.Errorf("missing warn log for state lookup failure: %s", buf.String())
		}
	})
}

// When no prior read has succeeded (e.g. startup read fails), runner fails open.
func TestRunFailsOpenWhenScriptEnabledFails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var buf bytes.Buffer
		rc := &Context{Log: slog.New(slog.NewTextHandler(&buf, nil))}
		runs := newFakeStore()
		runs.setStateErr("lookup-fail", errors.New("state db unavailable"))

		var calls atomic.Int32
		script := fakeScript{name: "lookup-fail", schedule: "@every 1s", run: func(context.Context, *Context) error {
			calls.Add(1)
			return nil
		}}

		stop := startTestRunner(t, script, rc, runs)
		defer stop()

		// Advance across startup (first iteration runs immediately) and 2 ticks (total 3 runs).
		synctest.Wait()
		for i := 0; i < 2; i++ {
			time.Sleep(time.Second)
			synctest.Wait()
		}

		if n := calls.Load(); n != 3 {
			t.Fatalf("script ran %d times when state lookup failed with no prior read, want 3", n)
		}
		recorded := runs.snapshot()
		if len(recorded) != 3 {
			t.Fatalf("recorded runs = %+v, want 3", recorded)
		}
		for i, r := range recorded {
			if r.Script != "lookup-fail" || !r.OK {
				t.Errorf("run %d = %+v, want script=lookup-fail ok=true", i, r)
			}
		}
		if !strings.Contains(buf.String(), `level=WARN msg="check script enabled failed" script=lookup-fail error="state db unavailable"`) {
			t.Errorf("missing warn log for state lookup failure: %s", buf.String())
		}
	})
}
