package example

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/trriplejay/notinv/internal/runner"
)

// Script checks whether EXAMPLE_URL returns HTTP 200 and notifies on transitions.
// Its zero value is ready to use. Reuse the same instance for successive runs;
// state is in memory only and resets when the process restarts. Run calls on an
// instance must be serial, not concurrent.
type Script struct {
	// hasRun distinguishes an initial observation from a previous failure.
	hasRun bool
	// lastOK belongs to the script, not the runner: each script defines success.
	lastOK bool
}

// Keep this assertion when copying the template: signature drift becomes a
// compile error instead of a surprise when the script is registered later.
var _ runner.Script = (*Script)(nil)

// Name returns the stable identifier used for this script's logs and results.
func (*Script) Name() string { return "example" }

// Schedule requests a run every five minutes using the @every syntax.
func (*Script) Schedule() string { return "@every 5m" }

// Run checks the endpoint, logs the outcome, and notifies only on a state change.
// A nil error means the check succeeded and any required notification succeeded.
func (s *Script) Run(ctx context.Context, rc *runner.Context) error {
	checkErr := s.check(ctx, rc)
	ok := checkErr == nil
	// Additional structured logging is useful even when no notification is due.
	rc.Log.InfoContext(ctx, "example check completed", "ok", ok)

	changed := s.hasRun && s.lastOK != ok
	// Record the observed state even if notification fails. This template reports
	// transitions, not repeated alerts or notification retries on steady states.
	s.hasRun, s.lastOK = true, ok
	if changed {
		msg := fmt.Sprintf("example state changed: ok=%t", ok)
		if err := rc.Notify(ctx, msg); err != nil {
			// Preserve both causes for callers using errors.Is or errors.As.
			return errors.Join(checkErr, fmt.Errorf("notify example transition: %w", err))
		}
	}
	return checkErr
}

func (*Script) check(ctx context.Context, rc *runner.Context) error {
	// Per-script configuration is intentionally separate from central config.
	// Reading each run lets an embedding caller change the environment between
	// runs; a normal service sets EXAMPLE_URL before startup.
	url := os.Getenv("EXAMPLE_URL")
	if url == "" {
		return errors.New("EXAMPLE_URL must be set")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil) //nolint:gosec // G704 false positive: this is a monitoring template whose sole purpose is fetching an operator-configured EXAMPLE_URL, not an SSRF vector
	if err != nil {
		return fmt.Errorf("build example request: %w", err)
	}
	// Always use the supplied client: it owns timeouts and transport settings,
	// and tests can replace its transport without making any network calls.
	resp, err := rc.HTTP.Do(req) //nolint:gosec // G704 false positive: issuing the request to the operator-configured EXAMPLE_URL is the required health-check behavior, not an SSRF vulnerability
	if err != nil {
		return fmt.Errorf("perform example request: %w", err)
	}
	defer func() {
		// Even though this check only needs the status, the body must be closed.
		// A cleanup error is diagnostic; it does not change the observed status.
		if err := resp.Body.Close(); err != nil {
			rc.Log.WarnContext(ctx, "close example response body", "error", err)
		}
	}()

	// The script defines success itself; other templates might inspect JSON or
	// match page content instead of treating exactly HTTP 200 as healthy.
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("example check: unexpected HTTP status %d", resp.StatusCode)
	}
	return nil
}
