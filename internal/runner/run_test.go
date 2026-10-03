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
}

func newFakeStore() *fakeStore {
	return &fakeStore{inserted: make(chan struct{}, 16)}
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
		stop := startTestRunner(t, script, rc, runs)
		defer stop()
		synctest.Wait()
		if len(runs.snapshot()) != 0 {
			t.Fatal("script ran before its first scheduled fire")
		}
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
		if len(runs.snapshot()) != 0 {
			t.Fatal("cron script ran immediately")
		}
		// Explicitly advance to the minute boundary of synctest's fake clock.
		time.Sleep(time.Minute)
		synctest.Wait()
		recorded := runs.snapshot()
		if len(recorded) != 1 || !recorded[0].OK {
			t.Fatalf("cron schedule recorded %+v, want one successful run", recorded)
		}
	})
}

func TestRunCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runs := newFakeStore()
		script := fakeScript{name: "long-wait", schedule: "@every 1h", run: func(context.Context, *Context) error {
			t.Error("cancelled schedule executed a script")
			return nil
		}}
		stop := startTestRunner(t, script, quietContext(), runs)
		defer stop()
		synctest.Wait() // Ensure Run has reached the cancellable timer wait.
		stop()          // Requires a clean return within 200ms, not an hour.
		if len(runs.snapshot()) != 0 {
			t.Error("cancelled schedule persisted a run")
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
