package database

import (
	"database/sql"
	"embed"
	"fmt"

	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrate brings the schema up to date. It's called once from New(), right
// after the connection is opened, so `go run ./cmd/api` (and air) always run
// against current schema without a separate CLI step.
func migrate(db *sql.DB) error {
	goose.SetBaseFS(migrationsFS)

	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set migration dialect: %w", err)
	}

	if err := goose.Up(db, "migrations"); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}

	return nil
}

// Migrate is migrate's exported counterpart, for callers that need a
// connection before all of New()'s other setup (or that already have a raw
// *sql.DB - see Open) - cmd/dbreset uses it to rebuild the schema
// immediately after Reset empties it.
func Migrate(db *sql.DB) error {
	return migrate(db)
}
