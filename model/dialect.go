package model

import (
	"fmt"
	"strings"

	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// dialect is the resolved SQL flavor for one configured DSN.
type dialect struct {
	// name is "postgres" or "sqlite"; it identifies the flavor so callers can
	// apply flavor-specific migration steps.
	name string
	// open is the gorm dialector for the resolved flavor.
	open gorm.Dialector
}

// dialectorFor inspects dsn and returns the matching gorm dialector, so one
// binary can serve either backend purely from database.dsn in server.yml.
//
// PostgreSQL is selected by its URI scheme (postgres:// or postgresql://).
// Anything else is treated as SQLite: a bare file path, a file: URI, or an
// explicit sqlite:// / sqlite3:// URI. A file path with no scheme is the
// common case for a local development or test database.
func dialectorFor(dsn string) (dialect, error) {
	s := strings.TrimSpace(dsn)
	if s == "" {
		return dialect{}, fmt.Errorf("empty database DSN")
	}

	lower := strings.ToLower(s)
	switch {
	case strings.HasPrefix(lower, "postgres://") || strings.HasPrefix(lower, "postgresql://"):
		// pgx needs the scheme, so pass the DSN through unchanged.
		return dialect{name: "postgres", open: postgres.Open(s)}, nil
	case strings.HasPrefix(lower, "sqlite://"), strings.HasPrefix(lower, "sqlite3://"):
		// Strip the scheme and hand the driver the path (or file: URI).
		path := strings.TrimPrefix(s, "sqlite3://")
		path = strings.TrimPrefix(path, "sqlite://")
		return dialect{name: "sqlite", open: sqlite.Open(path)}, nil
	default:
		// No scheme: a bare file path, or a file: URI the driver understands.
		return dialect{name: "sqlite", open: sqlite.Open(s)}, nil
	}
}
