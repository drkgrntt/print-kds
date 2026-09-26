package database

import (
	"database/sql"
	"fmt"
	"strings"
)

// Open connects to the database configured by the DB_* env vars without
// running migrations - a reset needs a raw connection first so it can empty
// the schema before putting it back with Migrate.
func Open() (*sql.DB, error) {
	return sql.Open("pgx", connString())
}

// Reset drops every table in the configured schema, then runs every
// migration back up from scratch.
func Reset(db *sql.DB) error {
	schema := dbSchema
	if schema == "" {
		schema = "public"
	}

	rows, err := db.Query(`SELECT tablename FROM pg_tables WHERE schemaname = $1`, schema)
	if err != nil {
		return fmt.Errorf("list tables: %w", err)
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			return fmt.Errorf("list tables: %w", err)
		}
		tables = append(tables, table)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("list tables: %w", err)
	}

	for _, table := range tables {
		if _, err := db.Exec(fmt.Sprintf(`DROP TABLE IF EXISTS %s.%s CASCADE`, quoteIdent(schema), quoteIdent(table))); err != nil {
			return fmt.Errorf("drop table %s: %w", table, err)
		}
	}

	return Migrate(db)
}

// quoteIdent wraps a Postgres identifier in double quotes, doubling any
// quote characters it contains.
func quoteIdent(ident string) string {
	return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"`
}
