// Command dbseed populates the local database with a handful of sample
// orders and print items spread across every board column, so the KDS
// board has something to look at and click through without needing to
// insert rows by hand first.
//
// Run it with:
//
//	go run ./cmd/dbseed
//
// Safe to re-run: the seed tenant/user are idempotent inserts, and orders
// are keyed by (tenant_id, order_number), so a repeat run skips any order
// that's already there (and its items) rather than duplicating cards on
// the board.
package main

import (
	"context"
	"database/sql"
	"fmt"

	"print-kds/internal/database"
	"print-kds/internal/logger"

	"golang.org/x/crypto/bcrypt"

	_ "github.com/joho/godotenv/autoload"
)

// seedTenantName/seedUserEmail/seedUserPassword are the dev login dbseed
// provisions - printed at the end of a run so `go run ./cmd/dbseed` stays
// a one-command path to a clickable, logged-in board.
const (
	seedTenantName   = "Seed Shop"
	seedUserEmail    = "dev@example.com"
	seedUserPassword = "dev-password-123"
)

type seedItem struct {
	fileName      string
	colorMaterial string
	quantity      int
	status        string
}

type seedOrder struct {
	orderNumber  string
	customerName string
	dueInDays    int
	items        []seedItem
}

var seedOrders = []seedOrder{
	{
		orderNumber:  "ORD-1001",
		customerName: "Alice Nguyen",
		dueInDays:    2,
		items: []seedItem{
			{"bracket_v2.gcode", "PETG Black", 2, "queued"},
			{"case_lid.gcode", "PLA White", 1, "printing"},
		},
	},
	{
		orderNumber:  "ORD-1002",
		customerName: "Ben Ortiz",
		dueInDays:    5,
		items: []seedItem{
			{"hinge_arm.gcode", "ABS Gray", 4, "cleaning_qc"},
		},
	},
	{
		orderNumber:  "ORD-1003",
		customerName: "Priya Shah",
		dueInDays:    1,
		items: []seedItem{
			{"mount_bracket.gcode", "PETG Clear", 1, "ready_to_pack"},
			{"spacer_set.gcode", "PLA Black", 6, "queued"},
		},
	},
}

func main() {
	db, err := database.Open()
	if err != nil {
		logger.Fatal("connect to database", "error", err)
	}
	defer db.Close()

	// Defensive, not the normal path: makes dbseed work even against a
	// database `make db-reset` (or a fresh install) just emptied, without
	// requiring `make run` to have been started first.
	if err := database.Migrate(db); err != nil {
		logger.Fatal("run migrations", "error", err)
	}

	ctx := context.Background()

	tenantID, err := ensureSeedTenant(ctx, db)
	if err != nil {
		logger.Fatal("ensure seed tenant", "error", err)
	}
	if err := ensureSeedUser(ctx, db, tenantID); err != nil {
		logger.Fatal("ensure seed user", "error", err)
	}

	applied, skipped := 0, 0
	for _, o := range seedOrders {
		created, err := seedOneOrder(ctx, db, tenantID, o)
		if err != nil {
			logger.Fatal("seed order", "order_number", o.orderNumber, "error", err)
		}
		if created {
			fmt.Printf("  + %s (%s) - %d item(s)\n", o.orderNumber, o.customerName, len(o.items))
			applied++
		} else {
			fmt.Printf("  = %s already seeded\n", o.orderNumber)
			skipped++
		}
	}

	fmt.Printf("\nSeeding complete: %d order(s) created, %d already existed.\n", applied, skipped)
	fmt.Printf("Log in at / with %s / %s\n", seedUserEmail, seedUserPassword)
}

// ensureSeedTenant looks up (or creates) the "Seed Shop" tenant dbseed's
// orders/user hang off of, so a repeat run reuses the same tenant instead
// of spawning a new one every time.
func ensureSeedTenant(ctx context.Context, db *sql.DB) (int64, error) {
	var id int64
	err := db.QueryRowContext(ctx, `SELECT id FROM tenants WHERE name = $1`, seedTenantName).Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != sql.ErrNoRows {
		return 0, fmt.Errorf("look up seed tenant: %w", err)
	}

	if err := db.QueryRowContext(ctx,
		`INSERT INTO tenants (name) VALUES ($1) RETURNING id`, seedTenantName,
	).Scan(&id); err != nil {
		return 0, fmt.Errorf("insert seed tenant: %w", err)
	}
	return id, nil
}

// ensureSeedUser idempotently provisions the dev login - hashed inline
// with bcrypt rather than going through internal/auth.Store, matching
// this command's existing no-Store, raw-SQL style (see seedOneOrder).
// It's a fixed, published dev credential, not a security-sensitive one.
func ensureSeedUser(ctx context.Context, db *sql.DB, tenantID int64) error {
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE email = $1)`, seedUserEmail).Scan(&exists); err != nil {
		return fmt.Errorf("check seed user: %w", err)
	}
	if exists {
		return nil
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(seedUserPassword), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash seed user password: %w", err)
	}

	if _, err := db.ExecContext(ctx, `
		INSERT INTO users (tenant_id, email, password_hash, role)
		VALUES ($1, $2, $3, 'admin')
	`, tenantID, seedUserEmail, string(hash)); err != nil {
		return fmt.Errorf("insert seed user: %w", err)
	}
	return nil
}

// seedOneOrder inserts o and its items in a single transaction, unless an
// order with the same (tenant_id, order_number) already exists (ON
// CONFLICT DO NOTHING), in which case it skips the whole order - including
// its items, since re-seeding items onto an order a previous run already
// seeded (and whatever workflow progress happened to it since) would just
// duplicate cards on the board.
func seedOneOrder(ctx context.Context, db *sql.DB, tenantID int64, o seedOrder) (created bool, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	var orderID int64
	err = tx.QueryRowContext(ctx, `
		INSERT INTO orders (tenant_id, order_number, customer_name, shipping_address, due_date, status)
		VALUES ($1, $2, $3, '{}', CURRENT_DATE + $4::int, 'pending')
		ON CONFLICT (tenant_id, order_number) DO NOTHING
		RETURNING id
	`, tenantID, o.orderNumber, o.customerName, o.dueInDays).Scan(&orderID)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("insert order: %w", err)
	}

	for _, item := range o.items {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO print_items (order_id, file_name, color_material, quantity_required, status)
			VALUES ($1, $2, $3, $4, $5)
		`, orderID, item.fileName, item.colorMaterial, item.quantity, item.status); err != nil {
			return false, fmt.Errorf("insert print item %s: %w", item.fileName, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}

	return true, nil
}
