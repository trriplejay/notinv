package storetest

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// EnvDatabaseURL names the environment variable holding the Postgres database URL.
const EnvDatabaseURL = "NOTINV_TEST_DATABASE_URL"

var schemaCounter atomic.Int64

// BaseURL returns the value of NOTINV_TEST_DATABASE_URL, or calls t.Fatalf if unset.
func BaseURL(t testing.TB) string {
	t.Helper()
	base := os.Getenv(EnvDatabaseURL)
	if base == "" {
		t.Fatalf("%s is not set; these integration tests need a Postgres database URL", EnvDatabaseURL)
	}
	return base
}

// DatabaseURL creates a fresh, uniquely named schema in the test database
// and returns a postgres URL whose search_path points at it. The schema is
// dropped with CASCADE on t.Cleanup. If NOTINV_TEST_DATABASE_URL is unset or
// the database is unreachable, it calls t.Fatalf and never t.Skip.
func DatabaseURL(t testing.TB) string {
	t.Helper()
	base := BaseURL(t)

	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })

	schema := fmt.Sprintf("notinv_test_%d_%d", time.Now().UnixNano(), schemaCounter.Add(1))
	ctx := context.Background()
	if c, ok := t.(interface{ Context() context.Context }); ok {
		ctx = c.Context()
	}

	if _, err := admin.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`); err != nil {
			t.Errorf("drop schema %s: %v", schema, err)
		}
	})

	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse %s: %v", EnvDatabaseURL, err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}
