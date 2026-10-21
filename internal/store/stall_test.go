package store

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trriplejay/notinv/internal/storetest"
)

// silentServer accepts TCP connections and never answers, like a Postgres
// that takes the connection but stalls.
func silentServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	return ln.Addr().String()
}

// A ping that times out against a server that accepts but never answers is
// retried until the wait budget is spent, rather than failing on the first
// per-attempt deadline.
func TestOpenWaitsOnSilentServer(t *testing.T) {
	origBudget, origPing, origBackoff := openWaitBudget, openPingTimeout, openInitialBackoff
	openWaitBudget = 1500 * time.Millisecond
	openPingTimeout = 200 * time.Millisecond
	openInitialBackoff = 10 * time.Millisecond
	defer func() { openWaitBudget, openPingTimeout, openInitialBackoff = origBudget, origPing, origBackoff }()

	dbURL := "postgres://user:secretpw@" + silentServer(t) + "/db?sslmode=disable&connect_timeout=30"

	t.Run("waits for the budget", func(t *testing.T) {
		start := time.Now()
		s, err := Open(t.Context(), dbURL)
		elapsed := time.Since(start)
		if err == nil {
			_ = s.Close()
			t.Fatal("Open succeeded against a silent server")
		}
		if elapsed < openWaitBudget {
			t.Errorf("Open returned after %v, want it to keep waiting at least %v (err: %v)", elapsed, openWaitBudget, err)
		}
		if elapsed > openWaitBudget+openPingTimeout+time.Second {
			t.Errorf("Open took %v, want about %v", elapsed, openWaitBudget)
		}
		if !strings.Contains(err.Error(), "could not be reached within the wait") {
			t.Errorf("error = %v, want error naming the wait", err)
		}
		if strings.Contains(err.Error(), "secretpw") {
			t.Errorf("error leaks password: %v", err)
		}
	})

	t.Run("cancelled context stays prompt", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		time.AfterFunc(300*time.Millisecond, cancel)
		start := time.Now()
		_, err := Open(ctx, dbURL)
		elapsed := time.Since(start)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("error = %v, want context.Canceled", err)
		}
		if elapsed > 300*time.Millisecond+openPingTimeout+200*time.Millisecond {
			t.Errorf("cancelled Open took %v, want prompt return", elapsed)
		}
	})
}

// migrate inside Open is time-limited: while another session holds the
// migration advisory lock, Open returns a migrate error wrapping
// context.DeadlineExceeded within openMigrateTimeout.
func TestOpenMigrateBoundedUnderLockHolder(t *testing.T) {
	orig := openMigrateTimeout
	openMigrateTimeout = time.Second
	defer func() { openMigrateTimeout = orig }()

	dbURL := storetest.DatabaseURL(t)
	holder, err := sql.Open("pgx", dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Close() })
	tx, err := holder.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	if _, err := tx.ExecContext(t.Context(), `SELECT pg_advisory_xact_lock($1)`, migrationLockKey); err != nil {
		t.Fatalf("hold migration lock: %v", err)
	}

	start := time.Now()
	s, err := Open(t.Context(), dbURL)
	elapsed := time.Since(start)
	if err == nil {
		_ = s.Close()
		t.Fatal("Open succeeded while the migration lock was held")
	}
	if s != nil {
		t.Error("Open returned a Store alongside an error")
	}
	if !strings.Contains(err.Error(), "migrate") {
		t.Errorf("error = %v, want a migrate error", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want wrapping context.DeadlineExceeded", err)
	}
	if elapsed > openMigrateTimeout+2*time.Second {
		t.Errorf("Open took %v, want within about %v", elapsed, openMigrateTimeout)
	}

	// Once the holder releases the lock, Open migrates normally.
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(t.Context(), dbURL)
	if err != nil {
		t.Fatalf("Open after lock release: %v", err)
	}
	_ = s.Close()
}
