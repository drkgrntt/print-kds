// Command dbseed populates the local database with a handful of sample
// orders and print items spread across every board column, so the KDS
// board has something to look at and click through without needing to
// insert rows by hand first.
//
// Run it with:
//
//	go run ./cmd/dbseed
//
// Safe to re-run: orders are keyed by their unique order_number, so a
// repeat run skips any order that's already there (and its items) rather
// than duplicating cards on the board.
package main

import (
	"context"
	"database/sql"
	"fmt"

	"print-kds/internal/database"
	"print-kds/internal/logger"

	_ "github.com/joho/godotenv/autoload"
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
	applied, skipped := 0, 0
	for _, o := range seedOrders {
		created, err := seedOneOrder(ctx, db, o)
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
}

// seedOneOrder inserts o and its items in a single transaction, unless an
// order with the same order_number already exists (ON CONFLICT DO
// NOTHING), in which case it skips the whole order - including its items,
// since re-seeding items onto an order a previous run already seeded (and
// whatever workflow progress happened to it since) would just duplicate
// cards on the board.
func seedOneOrder(ctx context.Context, db *sql.DB, o seedOrder) (created bool, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	var orderID int64
	err = tx.QueryRowContext(ctx, `
		INSERT INTO orders (order_number, customer_name, shipping_address, due_date, status)
		VALUES ($1, $2, '{}', CURRENT_DATE + $3::int, 'pending')
		ON CONFLICT (order_number) DO NOTHING
		RETURNING id
	`, o.orderNumber, o.customerName, o.dueInDays).Scan(&orderID)
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
