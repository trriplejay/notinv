package runner

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/trriplejay/notinv/internal/config"
	"github.com/trriplejay/notinv/internal/notify"
	storepkg "github.com/trriplejay/notinv/internal/store"
)

// Script is a named periodic check with its own schedule and success criteria.
type Script interface {
	// Name returns a unique name used in logs, the DB, and the dashboard.
	Name() string

	// Schedule returns a cron expression or "@every <duration>".
	Schedule() string

	// Run performs one check; a nil return signals success.
	Run(ctx context.Context, rc *Context) error
}

// Context provides the shared services available during a script run.
type Context struct {
	// Log is the logger pre-tagged with a script=<name> attribute.
	Log *slog.Logger

	// HTTP is the client to use for requests, including its configured timeouts.
	HTTP *http.Client

	// Notify sends a Discord DM, or logs the message when configured for dry-run.
	// Callers may replace it to inject a different notification implementation.
	Notify func(ctx context.Context, msg string) error
}

// NewContext wires shared services to a config-driven Discord notifier.
// The caller must supply a non-nil config, a non-nil logger already tagged with
// script=<name>, and a non-nil HTTP client with appropriate timeouts.
func NewContext(log *slog.Logger, httpClient *http.Client, cfg *config.Config) *Context {
	rc := &Context{Log: log, HTTP: httpClient}
	notifier := notify.New(*cfg, log, notify.Options{HTTPClient: httpClient})
	rc.Notify = notifier.Send
	return rc
}

// maxWait bounds each timer wait between runs. Timers follow the monotonic
// clock, which stops while the host is suspended (a sleeping laptop, or Docker
// Desktop's VM), so a single long timer would fire late by the time spent
// asleep. Rechecking the wall clock at this interval runs a schedule whose time
// passed during a suspend within maxWait of wake. Missed runs are not replayed.
const maxWait = time.Minute

// wallNow reads the wall clock without a monotonic reading, so differences
// computed from it include time the host spent suspended. Tests replace it to
// simulate a suspend.
var wallNow = func() time.Time { return time.Now().Round(0) }

// RunStore persists the outcome of each completed script run and reports
// whether a script is currently enabled.
type RunStore interface {
	InsertRun(ctx context.Context, run storepkg.Run) error
	ScriptEnabled(ctx context.Context, script string) (bool, error)
}

// Run schedules one script, executing and recording its runs sequentially.
// The first run starts immediately so results are available right after
// startup; later runs follow the schedule, computed after each run finishes.
// Cancellation stops the wait promptly; an active script must honor ctx itself.
// The caller must supply a non-nil logger in rc.
func Run(ctx context.Context, script Script, rc *Context, store RunStore) error {
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
	sched, err := parser.Parse(script.Schedule())
	if err != nil {
		rc.Log.ErrorContext(ctx, "invalid schedule", slog.String("script", script.Name()), slog.Any("error", err))
		return err
	}

	var hasLastEnabled bool
	var lastEnabled bool

	for first := true; ; first = false {
		if !first && !waitUntil(ctx, sched.Next(wallNow())) {
			return nil
		}
		// Prefer shutdown if the timer and cancellation became ready together.
		if ctx.Err() != nil {
			return nil
		}

		checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		enabled, err := store.ScriptEnabled(checkCtx, script.Name())
		cancel()
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			rc.Log.WarnContext(ctx, "check script enabled failed", slog.String("script", script.Name()), slog.Any("error", err))
			if hasLastEnabled {
				enabled = lastEnabled
			} else {
				enabled = true
			}
		} else {
			lastEnabled = enabled
			hasLastEnabled = true
		}

		if !enabled {
			continue
		}

		rc.Log.InfoContext(ctx, "run start", slog.String("script", script.Name()))
		start := time.Now()
		runErr := func() (err error) {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("panic: %v", r)
				}
			}()
			return script.Run(ctx, rc)
		}()
		dur := time.Since(start)
		ok := runErr == nil
		errStr := ""
		var errPtr *string
		if runErr != nil {
			errStr = runErr.Error()
			errPtr = &errStr
		}
		rc.Log.InfoContext(ctx, "run end",
			slog.String("script", script.Name()),
			slog.Int64("duration_ms", dur.Milliseconds()),
			slog.Bool("ok", ok),
			slog.String("error", errStr))

		run := storepkg.Run{
			Key:        storepkg.RandomKey(),
			Script:     script.Name(),
			StartedAt:  start,
			DurationMs: dur.Milliseconds(),
			OK:         ok,
			Err:        errPtr,
		}
		if err := store.InsertRun(ctx, run); err != nil {
			rc.Log.ErrorContext(ctx, "insert run failed", slog.String("script", script.Name()), slog.Any("error", err))
		}
	}
}

// waitUntil blocks until the wall clock reaches next, rechecking at least every
// maxWait, and reports false if ctx is cancelled first.
func waitUntil(ctx context.Context, next time.Time) bool {
	for {
		d := next.Sub(wallNow())
		if d <= 0 {
			return true
		}
		timer := time.NewTimer(min(d, maxWait))
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
}
