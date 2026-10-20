package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"time"
)

// pingConnector substitutes the SQL driver; these tests never open a database.
type pingConnector struct {
	conn *pingConn
}

func (c pingConnector) Connect(context.Context) (driver.Conn, error) { return c.conn, nil }
func (c pingConnector) Driver() driver.Driver                        { return pingDriver{} }

type pingDriver struct{}

func (pingDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("unexpected driver open")
}

type pingConn struct {
	err   error
	ctx   context.Context
	calls int
}

func (c *pingConn) Ping(ctx context.Context) error {
	c.ctx = ctx
	c.calls++
	return c.err
}
func (*pingConn) Close() error                        { return nil }
func (*pingConn) Begin() (driver.Tx, error)           { return nil, errors.New("unexpected transaction") }
func (*pingConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unexpected statement") }

func TestPing(t *testing.T) {
	failure := errors.New("private driver detail")
	for _, tc := range []struct {
		name       string
		remote     bool
		err        error
		wantScheme string
	}{
		{"success", false, nil, "file"},
		{"local failure", false, failure, "file"},
		{"remote failure", true, failure, "libsql"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := &pingConn{err: tc.err}
			db := sql.OpenDB(pingConnector{conn: conn})
			t.Cleanup(func() {
				if err := db.Close(); err != nil {
					t.Error(err)
				}
			})
			s := &Store{db: db, remote: tc.remote}
			err := s.Ping(t.Context())
			if !errors.Is(err, tc.err) || conn.calls != 1 || conn.ctx != t.Context() {
				t.Fatalf("Ping = %v, calls = %d, context = %v", err, conn.calls, conn.ctx)
			}
			if err != nil && (strings.Contains(err.Error(), failure.Error()) || !strings.Contains(err.Error(), "ping store") || !strings.Contains(err.Error(), tc.wantScheme)) {
				t.Errorf("missing operation/scheme or leaked driver detail: %v", err)
			}
		})
	}
}

// CLM-7: Unit tests with the sentinel password 'S3cr3tPw' in both URL positions.
// store.Open errors (unreachable host, bad password) must not contain it.
func TestOpenRedaction(t *testing.T) {
	const sentinelPw = "S3cr3tPw" //nolint:gosec // G101 false positive: test sentinel, not a real credential

	origBudget := openWaitBudget
	openWaitBudget = 100 * time.Millisecond
	defer func() { openWaitBudget = origBudget }()

	urls := []struct {
		name string
		url  string
	}{
		{"userinfo sentinel", "postgres://user:" + sentinelPw + "@127.0.0.1:54329/testdb"},
		{"query param sentinel", "postgres://127.0.0.1:54329/testdb?password=" + sentinelPw},
		{"both sentinel", "postgres://user:" + sentinelPw + "@127.0.0.1:54329/testdb?PASSWORD=" + sentinelPw},
	}

	for _, tt := range urls {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
			defer cancel()

			_, err := Open(ctx, tt.url, "")
			if err == nil {
				t.Fatal("expected Open to fail for unreachable host, got nil")
			}
			errStr := err.Error()
			if strings.Contains(errStr, sentinelPw) {
				t.Errorf("sentinel password %q leaked in Open error: %s", sentinelPw, errStr)
			}
		})
	}
}
