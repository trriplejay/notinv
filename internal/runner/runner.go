package runner

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/robfig/cron/v3"

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

	// Notify is dry-run (logs instead of sending) pending the Discord notifier.
	// Callers may replace it to inject a different notification implementation.
	Notify func(ctx context.Context, msg string) error
}

// NewContext wires a logger and HTTP client to a dry-run notification function.
// The caller must supply a non-nil logger already tagged with script=<name> and
// a non-nil HTTP client with appropriate timeouts for the script.
func NewContext(log *slog.Logger, httpClient *http.Client) *Context {
	rc := &Context{Log: log, HTTP: httpClient}
	rc.Notify = func(ctx context.Context, msg string) error {
		rc.Log.InfoContext(ctx, "notify (dry-run)", slog.String("message", msg))
		return nil
	}
	return rc
}

// RunStore persists the outcome of each completed script run.
type RunStore interface {
	InsertRun(ctx context.Context, run storepkg.Run) error
}

// Run schedules one script, executing and recording its runs sequentially.
// Cancellation stops the wait promptly; an active script must honor ctx itself.
// The caller must supply a non-nil logger in rc.
func Run(ctx context.Context, script Script, rc *Context, store RunStore) error {
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
	sched, err := parser.Parse(script.Schedule())
	if err != nil {
		rc.Log.ErrorContext(ctx, "invalid schedule", slog.String("script", script.Name()), slog.Any("error", err))
		return err
	}

	for {
		next := sched.Next(time.Now())
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		// Prefer shutdown if the timer and cancellation became ready together.
		if ctx.Err() != nil {
			return nil
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
