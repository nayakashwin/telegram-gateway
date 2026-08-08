// Package testdb provides isolated Postgres schemas for integration tests.
package testdb

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nayakashwin/telegram-gateway/internal/store"
)

var schemaCounter atomic.Int64

// DSN returns the connection string for the isolated schema. Useful for tests
// that need their own connection (e.g. LISTEN/NOTIFY).
func DSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	n := schemaCounter.Add(1)
	schema := fmt.Sprintf("test_%d_%d", n, os.Getpid())

	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("admin pool: %v", err)
	}
	defer pool.Close()
	if _, err := pool.Exec(context.Background(), fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS %q`, schema)); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		pool, err := pgxpool.New(context.Background(), dsn)
		if err != nil {
			return
		}
		defer pool.Close()
		_, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP SCHEMA IF EXISTS %q CASCADE`, schema))
	})

	// Point the store at the schema via search_path. pgx's ConnString() drops
	// RuntimeParams, so build the DSN with an explicit query parameter instead.
	dsn = addSearchPath(dsn, schema)
	return dsn
}

// addSearchPath appends ?search_path= (or &search_path=) to a DSN.
func addSearchPath(dsn, schema string) string {
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + "search_path=" + schema
}

// NewStore creates a Store isolated in its own Postgres schema. It skips the
// test if TEST_DATABASE_URL is unset. The schema is dropped on cleanup.
func NewStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.New(context.Background(), DSN(t), store.DefaultPoolConfig())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}
