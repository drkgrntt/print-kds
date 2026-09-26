// Command createuser provisions a login for a tenant outside the self-serve
// signup flow (/signup) - needed because signup only ever creates a
// brand-new tenant alongside its first user, with no way to attach a login
// to a tenant that already exists. That's exactly the situation the
// 00011_add_tenant_id_to_orders.sql migration leaves behind: it backfills
// every pre-existing order onto a "Default Shop" tenant that has data but
// no user who can log into it.
//
// Run it with:
//
//	go run ./cmd/createuser -tenant "Default Shop" -email you@example.com -password "..."
//
// -tenant is looked up by name, and created if no tenant with that name
// exists yet. -role defaults to "admin", since this tool's main job is
// bootstrapping the first (owner) login for a shop.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"

	"print-kds/internal/auth"
	"print-kds/internal/database"
	"print-kds/internal/logger"

	_ "github.com/joho/godotenv/autoload"
)

func main() {
	tenantName := flag.String("tenant", "", "shop name (looked up by name, created if it doesn't exist)")
	email := flag.String("email", "", "login email")
	password := flag.String("password", "", "login password (at least 8 characters)")
	role := flag.String("role", string(auth.RoleAdmin), "admin or staff")
	flag.Parse()

	if *tenantName == "" || *email == "" || *password == "" {
		fmt.Fprintln(os.Stderr, "usage: createuser -tenant \"Shop Name\" -email you@example.com -password \"...\" [-role admin|staff]")
		os.Exit(1)
	}
	if *role != string(auth.RoleAdmin) && *role != string(auth.RoleStaff) {
		fmt.Fprintf(os.Stderr, "invalid -role %q: must be %q or %q\n", *role, auth.RoleAdmin, auth.RoleStaff)
		os.Exit(1)
	}

	db, err := database.Open()
	if err != nil {
		logger.Fatal("connect to database", "error", err)
	}
	defer db.Close()

	if err := database.Migrate(db); err != nil {
		logger.Fatal("run migrations", "error", err)
	}

	ctx := context.Background()
	tenantID, err := ensureTenant(ctx, db, *tenantName)
	if err != nil {
		logger.Fatal("ensure tenant", "error", err)
	}

	store := auth.NewStore(db)
	user, err := store.CreateUser(ctx, tenantID, *email, *password, auth.Role(*role))
	if err != nil {
		logger.Fatal("create user", "error", err)
	}

	fmt.Printf("Created %s user %s for tenant %q (id %d).\n", user.Role, user.Email, *tenantName, tenantID)
}

func ensureTenant(ctx context.Context, db *sql.DB, name string) (int64, error) {
	var id int64
	err := db.QueryRowContext(ctx, `SELECT id FROM tenants WHERE name = $1`, name).Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != sql.ErrNoRows {
		return 0, fmt.Errorf("look up tenant %q: %w", name, err)
	}

	if err := db.QueryRowContext(ctx,
		`INSERT INTO tenants (name) VALUES ($1) RETURNING id`, name,
	).Scan(&id); err != nil {
		return 0, fmt.Errorf("insert tenant %q: %w", name, err)
	}
	return id, nil
}
