// Package testutil contains helpers shared by the integration test suites.
package testutil

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync/atomic"

	"github.com/onsi/ginkgo/v2"

	_ "github.com/jackc/pgx/v5/stdlib" // register the pgx database/sql driver
	"github.com/kakwa/strangle/model"
)

var dbCounter atomic.Uint64

var unsafeDBName = regexp.MustCompile(`[^a-z0-9_]+`)

// TestDSN creates a fresh PostgreSQL database for one spec and returns its
// DSN. The base DSN comes from STRANGLE_TEST_DSN; each call gets its own
// database so packages can run in parallel without sharing tables. Specs skip
// when no DSN is configured. The database is dropped when the spec ends.
func TestDSN(label string) string {
	base := os.Getenv("STRANGLE_TEST_DSN")
	if base == "" {
		ginkgo.Skip("STRANGLE_TEST_DSN not set")
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" {
		ginkgo.Fail(fmt.Sprintf("STRANGLE_TEST_DSN must be a PostgreSQL URL, got %q", base))
	}

	clean := unsafeDBName.ReplaceAllString(strings.ToLower(label), "_")
	clean = strings.Trim(clean, "_")
	if clean == "" {
		clean = "test"
	}
	name := fmt.Sprintf("strangle_test_%s_%d_%d", clean, os.Getpid(), dbCounter.Add(1))

	adminURL := *u
	adminURL.Path = "/postgres"
	adminURL.RawPath = ""
	admin, err := sql.Open("pgx", adminURL.String())
	if err != nil {
		ginkgo.Fail(fmt.Sprintf("open admin database: %v", err))
	}
	if _, err := admin.ExecContext(context.Background(), fmt.Sprintf("CREATE DATABASE %s", quoteIdent(name))); err != nil {
		_ = admin.Close()
		ginkgo.Fail(fmt.Sprintf("create test database %q: %v", name, err))
	}
	ginkgo.DeferCleanup(func() {
		_, _ = admin.ExecContext(context.Background(), fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", quoteIdent(name)))
		_ = admin.Close()
	})

	testURL := *u
	testURL.Path = "/" + name
	testURL.RawPath = ""
	return testURL.String()
}

// OpenDB creates a fresh PostgreSQL database, migrates the schema and returns
// a store backed by it. The database is dropped when the spec ends.
func OpenDB(label string) *model.DB {
	dsn := TestDSN(label)
	d, err := model.Open(context.Background(), dsn)
	if err != nil {
		ginkgo.Fail(fmt.Sprintf("open test database: %v", err))
	}
	return d
}

// SQLDB opens a database/sql handle on the given test database. Integration
// tests use it for the few cases that need to inspect rows directly rather
// than through the store API. The handle is closed when the spec ends.
func SQLDB(dsn string) *sql.DB {
	conn, err := sql.Open("pgx", dsn)
	if err != nil {
		ginkgo.Fail(fmt.Sprintf("open test database: %v", err))
	}
	if err := conn.PingContext(context.Background()); err != nil {
		_ = conn.Close()
		ginkgo.Fail(fmt.Sprintf("ping test database: %v", err))
	}
	ginkgo.DeferCleanup(func() { _ = conn.Close() })
	return conn
}

func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
