package store

import (
	"context"
	"errors"
	"net"
	"strings"
	"time"
)

const (
	retryAttempts = 4
	retryBase     = 50 * time.Millisecond
	retryCap      = time.Second
)

func (s *Store) withRetry(ctx context.Context, fn func() error) error {
	if !s.remote {
		return fn()
	}
	delay := retryBase
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := fn()
		if err == nil || !isTransient(err) || attempt == retryAttempts-1 {
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

// isTransient deliberately recognizes only network timeouts and a small set
// of common transport messages. C-backed libSQL errors need the string fallback.
// Cancellation must not be retried; other errors without these markers are
// treated as permanent. This is a heuristic, not a database error-code classifier.
func isTransient(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return true
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{"timeout", "unavailable", "connection refused", "connection reset"} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}
