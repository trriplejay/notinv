package runner

import (
	"context"
	"log/slog"
	"net/http"
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
