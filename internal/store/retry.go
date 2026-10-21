package store

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

const (
	retryAttempts = 4
	retryBase     = 50 * time.Millisecond
	retryCap      = time.Second
)

var perCallTimeout = 3 * time.Second

var onAttempt func(attempt int)

func (s *Store) withRetry(ctx context.Context, fn func(ctx context.Context) error) error {
	delay := retryBase
	for attempt := 0; ; attempt++ {
		if onAttempt != nil {
			onAttempt(attempt)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		attemptCtx, cancel := context.WithTimeout(ctx, perCallTimeout)
		err := fn(attemptCtx)
		cancel()
		if err == nil {
			return nil
		}
		if errors.Is(err, context.DeadlineExceeded) || (attemptCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil) {
			if !errors.Is(err, context.DeadlineExceeded) {
				err = fmt.Errorf("%w: %w", context.DeadlineExceeded, err)
			}
			return err
		}
		if errors.Is(err, context.Canceled) {
			return err
		}
		if !isTransient(err) || attempt == retryAttempts-1 {
			return err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		delay = min(delay*2, retryCap)
	}
}

// isTransient classifies transient connection and concurrency errors that
// warrant retrying. Transient means connection refused or reset, a lost or
// terminated connection (SQLSTATE class 08, 57P01), or a serialization conflict
// (40001, 40P01). Permanent errors (23xxx, 42xxx, 28P01/28000, 3D000),
// context.Canceled and context.DeadlineExceeded are never transient.
func isTransient(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		code := pgErr.SQLState()
		if code == "" {
			code = pgErr.Code
		}
		if strings.HasPrefix(code, "08") || code == "57P01" || code == "57P03" || code == "40001" || code == "40P01" {
			return true
		}
		if code == "28P01" || code == "28000" || code == "3D000" {
			return false
		}
		return false
	}
	var connErr *pgconn.ConnectError
	if errors.As(err, &connErr) {
		return true
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) {
		return true
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return true
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{
		"timeout",
		"unavailable",
		"connection refused",
		"connection reset",
		"broken pipe",
		"connection to the server was lost",
		"server closed the query connection",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}
