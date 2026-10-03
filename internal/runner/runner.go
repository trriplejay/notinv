package runner

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/trriplejay/notinv/internal/config"
	"github.com/trriplejay/notinv/internal/notify"
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
