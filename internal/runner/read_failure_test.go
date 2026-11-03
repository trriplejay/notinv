package runner

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// (1) A script last read disabled stays skipped (script.Run not called, no run row) while reads fail.
// Avoids first-vs-last trap by starting enabled, transitioning to disabled, then failing reads.
func TestRunReadFailureDisabledStaysSkipped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var buf bytes.Buffer
		rc := &Context{Log: slog.New(slog.NewTextHandler(&buf, nil))}
		runs := newFakeStore()

		var calls atomic.Int32
		script := fakeScript{
			name:     "read-fail-disabled",
			schedule: "@every 1s",
			run: func(context.Context, *Context) error {
				calls.Add(1)
				return nil
			},
		}

		// Tick 0 (startup): script is enabled -> runs once.
		runs.setEnabled("read-fail-disabled", true)

		stop := startTestRunner(t, script, rc, runs)
		defer stop()

		synctest.Wait()
		if n := calls.Load(); n != 1 {
			t.Fatalf("startup (enabled): calls = %d, want 1", n)
		}
		if recorded := runs.snapshot(); len(recorded) != 1 {
			t.Fatalf("startup (enabled): recorded runs = %+v, want 1", recorded)
		}

		// Tick 1: state changes to disabled. Read succeeds -> skipped.
		runs.setEnabled("read-fail-disabled", false)
		time.Sleep(time.Second)
		synctest.Wait()

		if n := calls.Load(); n != 1 {
			t.Fatalf("tick 1 (disabled): calls = %d, want 1", n)
		}
		if recorded := runs.snapshot(); len(recorded) != 1 {
			t.Fatalf("tick 1 (disabled): recorded runs = %+v, want 1", recorded)
		}

		// Ticks 2 & 3: read fails with error. Last-known state was disabled (not initial enabled).
		// Runner must keep skipping execution and insert no new run rows while logging WARN.
		runs.setStateErr("read-fail-disabled", errors.New("storage read failure"))

		for tick := 2; tick <= 3; tick++ {
			time.Sleep(time.Second)
			synctest.Wait()

			if n := calls.Load(); n != 1 {
				t.Fatalf("tick %d (read error, last-known disabled): calls = %d, want 1", tick, n)
			}
			if recorded := runs.snapshot(); len(recorded) != 1 {
				t.Fatalf("tick %d (read error, last-known disabled): recorded runs = %+v, want 1", tick, recorded)
			}
		}

		if !strings.Contains(buf.String(), `level=WARN msg="check script enabled failed" script=read-fail-disabled error="storage read failure"`) {
			t.Errorf("missing warn log for state lookup failure: %s", buf.String())
		}
	})
}

// (2) A script last read enabled keeps running while reads fail.
// Avoids first-vs-last trap by starting disabled, transitioning to enabled, then failing reads.
func TestRunReadFailureEnabledKeepsRunning(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var buf bytes.Buffer
		rc := &Context{Log: slog.New(slog.NewTextHandler(&buf, nil))}
		runs := newFakeStore()

		var calls atomic.Int32
		script := fakeScript{
			name:     "read-fail-enabled",
			schedule: "@every 1s",
			run: func(context.Context, *Context) error {
				calls.Add(1)
				return nil
			},
		}

		// Tick 0 (startup): script is disabled -> skipped.
		runs.setEnabled("read-fail-enabled", false)

		stop := startTestRunner(t, script, rc, runs)
		defer stop()

		synctest.Wait()
		if n := calls.Load(); n != 0 {
			t.Fatalf("startup (disabled): calls = %d, want 0", n)
		}
		if recorded := runs.snapshot(); len(recorded) != 0 {
			t.Fatalf("startup (disabled): recorded runs = %+v, want 0", recorded)
		}

		// Tick 1: state changes to enabled. Read succeeds -> runs once.
		runs.setEnabled("read-fail-enabled", true)
		time.Sleep(time.Second)
		synctest.Wait()

		if n := calls.Load(); n != 1 {
			t.Fatalf("tick 1 (enabled): calls = %d, want 1", n)
		}
		if recorded := runs.snapshot(); len(recorded) != 1 {
			t.Fatalf("tick 1 (enabled): recorded runs = %+v, want 1", recorded)
		}

		// Ticks 2 & 3: read fails with error. Last-known state was enabled (not initial disabled).
		// Runner must continue running on each scheduled tick, recording run rows and logging WARN.
		runs.setStateErr("read-fail-enabled", errors.New("storage read failure"))

		for tick := 2; tick <= 3; tick++ {
			time.Sleep(time.Second)
			synctest.Wait()

			if n := calls.Load(); n != int32(tick) {
				t.Fatalf("tick %d (read error, last-known enabled): calls = %d, want %d", tick, n, tick)
			}
			if recorded := runs.snapshot(); len(recorded) != tick {
				t.Fatalf("tick %d (read error, last-known enabled): recorded runs = %+v, want %d", tick, recorded, tick)
			}
		}

		if !strings.Contains(buf.String(), `level=WARN msg="check script enabled failed" script=read-fail-enabled error="storage read failure"`) {
			t.Errorf("missing warn log for state lookup failure: %s", buf.String())
		}
	})
}

// (3) A script never successfully read runs and a WARN is logged (fails open).
func TestRunReadFailureNeverReadFailsOpenWithWarn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var buf bytes.Buffer
		rc := &Context{Log: slog.New(slog.NewTextHandler(&buf, nil))}
		runs := newFakeStore()
		runs.setStateErr("never-read", errors.New("storage unavailable at startup"))

		var calls atomic.Int32
		script := fakeScript{
			name:     "never-read",
			schedule: "@every 1s",
			run: func(context.Context, *Context) error {
				calls.Add(1)
				return nil
			},
		}

		stop := startTestRunner(t, script, rc, runs)
		defer stop()

		// Tick 0 (startup): never read before, read fails -> fails open, runs immediately.
		synctest.Wait()
		if n := calls.Load(); n != 1 {
			t.Fatalf("startup (never read): calls = %d, want 1", n)
		}
		if recorded := runs.snapshot(); len(recorded) != 1 {
			t.Fatalf("startup (never read): recorded runs = %+v, want 1", recorded)
		}

		// Ticks 1 & 2: continue failing open on each scheduled tick.
		for tick := 1; tick <= 2; tick++ {
			time.Sleep(time.Second)
			synctest.Wait()

			want := int32(tick + 1)
			if n := calls.Load(); n != want {
				t.Fatalf("tick %d (never read): calls = %d, want %d", tick, n, want)
			}
			if recorded := runs.snapshot(); len(recorded) != int(want) {
				t.Fatalf("tick %d (never read): recorded runs = %+v, want %d", tick, recorded, want)
			}
		}

		if !strings.Contains(buf.String(), `level=WARN msg="check script enabled failed" script=never-read error="storage unavailable at startup"`) {
			t.Errorf("missing warn log for state lookup failure: %s", buf.String())
		}
	})
}

// (4) Once reads succeed again, the stored state is obeyed (disabled->skipped, enabled->runs).
func TestRunReadFailureRecoveryObeysStoredState(t *testing.T) {
	t.Run("RecoveryToDisabled", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var buf bytes.Buffer
			rc := &Context{Log: slog.New(slog.NewTextHandler(&buf, nil))}
			runs := newFakeStore()

			var calls atomic.Int32
			script := fakeScript{
				name:     "recovery-disabled",
				schedule: "@every 1s",
				run: func(context.Context, *Context) error {
					calls.Add(1)
					return nil
				},
			}

			// Tick 0: enabled -> runs once.
			runs.setEnabled("recovery-disabled", true)
			stop := startTestRunner(t, script, rc, runs)
			defer stop()

			synctest.Wait()
			if n := calls.Load(); n != 1 {
				t.Fatalf("startup: calls = %d, want 1", n)
			}

			// Tick 1: read fails with outage error -> uses last-known (enabled) -> runs again (calls = 2).
			runs.setStateErr("recovery-disabled", errors.New("temporary outage"))
			time.Sleep(time.Second)
			synctest.Wait()
			if n := calls.Load(); n != 2 {
				t.Fatalf("tick 1 (outage): calls = %d, want 2", n)
			}

			// Outage clears while script is stored as disabled.
			runs.clearStateErr("recovery-disabled")
			runs.setEnabled("recovery-disabled", false)

			// Tick 2: read succeeds -> stored state is disabled -> skipped (calls still 2).
			time.Sleep(time.Second)
			synctest.Wait()
			if n := calls.Load(); n != 2 {
				t.Fatalf("tick 2 (recovered to disabled): calls = %d, want 2", n)
			}
			if recorded := runs.snapshot(); len(recorded) != 2 {
				t.Fatalf("tick 2 (recovered to disabled): recorded runs = %+v, want 2", recorded)
			}

			// Tick 3: subsequent tick remains disabled -> skipped (calls still 2).
			time.Sleep(time.Second)
			synctest.Wait()
			if n := calls.Load(); n != 2 {
				t.Fatalf("tick 3 (disabled): calls = %d, want 2", n)
			}
		})
	})

	t.Run("RecoveryToEnabled", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var buf bytes.Buffer
			rc := &Context{Log: slog.New(slog.NewTextHandler(&buf, nil))}
			runs := newFakeStore()

			var calls atomic.Int32
			script := fakeScript{
				name:     "recovery-enabled",
				schedule: "@every 1s",
				run: func(context.Context, *Context) error {
					calls.Add(1)
					return nil
				},
			}

			// Tick 0: disabled -> skipped (calls = 0).
			runs.setEnabled("recovery-enabled", false)
			stop := startTestRunner(t, script, rc, runs)
			defer stop()

			synctest.Wait()
			if n := calls.Load(); n != 0 {
				t.Fatalf("startup: calls = %d, want 0", n)
			}

			// Tick 1: read fails with outage error -> uses last-known (disabled) -> skipped (calls = 0).
			runs.setStateErr("recovery-enabled", errors.New("temporary outage"))
			time.Sleep(time.Second)
			synctest.Wait()
			if n := calls.Load(); n != 0 {
				t.Fatalf("tick 1 (outage): calls = %d, want 0", n)
			}

			// Outage clears while script is stored as enabled.
			runs.clearStateErr("recovery-enabled")
			runs.setEnabled("recovery-enabled", true)

			// Tick 2: read succeeds -> stored state is enabled -> runs (calls = 1).
			time.Sleep(time.Second)
			synctest.Wait()
			if n := calls.Load(); n != 1 {
				t.Fatalf("tick 2 (recovered to enabled): calls = %d, want 1", n)
			}
			if recorded := runs.snapshot(); len(recorded) != 1 {
				t.Fatalf("tick 2 (recovered to enabled): recorded runs = %+v, want 1", recorded)
			}

			// Tick 3: subsequent tick remains enabled -> runs again (calls = 2).
			time.Sleep(time.Second)
			synctest.Wait()
			if n := calls.Load(); n != 2 {
				t.Fatalf("tick 3 (enabled): calls = %d, want 2", n)
			}
			if recorded := runs.snapshot(); len(recorded) != 2 {
				t.Fatalf("tick 3 (enabled): recorded runs = %+v, want 2", recorded)
			}
		})
	})
}
