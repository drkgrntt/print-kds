// Package board is the domain layer for the KDS board: print_items/orders/
// shipments queries and the status-transition state machine the board's
// htmx endpoints drive. One Store per process, sharing the same *sql.DB as
// everything else (see internal/database).
package board

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// ErrNotFound is wrapped into the error a transition method returns when
// itemID doesn't match any row - callers can check it with errors.Is to
// tell "no such item" (404) apart from "wrong status for this transition"
// (409).
var ErrNotFound = errors.New("print item not found")

// ErrDuplicateOrderNumber is wrapped into the error CreateOrder returns
// when order_number's unique constraint rejects the insert - callers use
// this to show a friendly "already exists" message instead of a raw
// constraint-violation error.
var ErrDuplicateOrderNumber = errors.New("order number already exists")

// PrintItem mirrors a print_items row, denormalized with the handful of
// orders columns a KDS card needs (order number, customer, due date) so
// callers never have to join again themselves.
type PrintItem struct {
	ID                int64
	OrderID           int64
	FileName          string
	ColorMaterial     string
	QuantityRequired  int
	QuantityCompleted int
	Status            string
	AssignedPrinter   string
	CreatedAt         time.Time

	OrderNumber  string
	CustomerName string
	DueDate      time.Time
}

// Shipment mirrors a shipments row.
type Shipment struct {
	ID             int64
	OrderID        int64
	Carrier        string
	TrackingNumber string
	LabelURL       string
	ShippedAt      time.Time
}

type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

const itemColumns = `
	pi.id, pi.order_id, pi.file_name, pi.color_material, pi.quantity_required,
	pi.quantity_completed, pi.status, pi.assigned_printer, pi.created_at,
	o.order_number, o.customer_name, o.due_date`

func scanItem(row interface{ Scan(...any) error }, item *PrintItem) error {
	return row.Scan(
		&item.ID, &item.OrderID, &item.FileName, &item.ColorMaterial,
		&item.QuantityRequired, &item.QuantityCompleted, &item.Status, &item.AssignedPrinter,
		&item.CreatedAt, &item.OrderNumber, &item.CustomerName, &item.DueDate,
	)
}

// BoardColumns returns every live (non-packed) print item grouped by
// status. "queued" is sorted by the parent order's due_date ascending (the
// board's "To Print" column is a due-date-ordered work queue); every other
// column is sorted by print_items.created_at ascending (oldest first, i.e.
// however long it's been sitting in that state). Packed items are terminal
// and drop off the live board entirely - this is a shop-floor status
// display, not an order history.
func (s *Store) BoardColumns(ctx context.Context) (map[string][]PrintItem, error) {
	query := `
SELECT` + itemColumns + `
FROM print_items pi
JOIN orders o ON o.id = pi.order_id
WHERE pi.status IN ('queued', 'printing', 'cleaning_qc', 'ready_to_pack')
ORDER BY pi.status, CASE WHEN pi.status = 'queued' THEN o.due_date END ASC, pi.created_at ASC`

	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("query board columns: %w", err)
	}
	defer rows.Close()

	columns := map[string][]PrintItem{
		"queued":        {},
		"printing":      {},
		"cleaning_qc":   {},
		"ready_to_pack": {},
	}

	for rows.Next() {
		var item PrintItem
		if err := scanItem(rows, &item); err != nil {
			return nil, fmt.Errorf("scan print item: %w", err)
		}
		columns[item.Status] = append(columns[item.Status], item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate board columns: %w", err)
	}

	return columns, nil
}

// NewOrderItem is one print item to create alongside a new order (see
// CreateOrder) - always starts life in the "queued" column.
type NewOrderItem struct {
	FileName         string
	ColorMaterial    string
	QuantityRequired int
}

// NewOrder is the manual order-intake form's input to CreateOrder.
type NewOrder struct {
	OrderNumber  string
	CustomerName string
	DueDate      time.Time
	Items        []NewOrderItem
}

// CreateOrder inserts a new order and its print items (all starting
// "queued") in a single transaction - the manual intake form
// (controllers.NewOrderController) is the only caller today. Returns
// ErrDuplicateOrderNumber (wrap-checkable via errors.Is) if order_number's
// unique constraint rejects the insert, so the form can show a friendly
// message instead of a raw constraint error.
func (s *Store) CreateOrder(ctx context.Context, in NewOrder) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	var orderID int64
	err = tx.QueryRowContext(ctx, `
INSERT INTO orders (order_number, customer_name, shipping_address, due_date, status)
VALUES ($1, $2, '{}', $3, 'pending')
RETURNING id`, in.OrderNumber, in.CustomerName, in.DueDate).Scan(&orderID)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return 0, fmt.Errorf("%w: %q", ErrDuplicateOrderNumber, in.OrderNumber)
		}
		return 0, fmt.Errorf("insert order: %w", err)
	}

	for _, item := range in.Items {
		qty := item.QuantityRequired
		if qty < 1 {
			qty = 1
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO print_items (order_id, file_name, color_material, quantity_required, status)
VALUES ($1, $2, $3, $4, 'queued')`, orderID, item.FileName, item.ColorMaterial, qty); err != nil {
			return 0, fmt.Errorf("insert print item %s: %w", item.FileName, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit create order: %w", err)
	}
	return orderID, nil
}

// beginItemTx opens a transaction, locks the print_items row for itemID
// (FOR UPDATE - two people tapping the same card on two tablets at once
// should serialize, not race), and confirms it's currently in
// expectedStatus. The caller owns the transaction from here: it must
// tx.Commit() on success, and the deferred tx.Rollback() every transition
// method registers is a no-op after a successful commit (Rollback on an
// already-committed tx just returns sql.ErrTxDone, which every caller here
// ignores by not checking its return value).
func (s *Store) beginItemTx(ctx context.Context, itemID int64, expectedStatus string) (*sql.Tx, PrintItem, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, PrintItem{}, fmt.Errorf("begin transaction: %w", err)
	}

	var item PrintItem
	row := tx.QueryRowContext(ctx, `
SELECT`+itemColumns+`
FROM print_items pi
JOIN orders o ON o.id = pi.order_id
WHERE pi.id = $1
FOR UPDATE OF pi`, itemID)
	if err := scanItem(row, &item); err != nil {
		tx.Rollback()
		if errors.Is(err, sql.ErrNoRows) {
			return nil, PrintItem{}, fmt.Errorf("%w: id %d", ErrNotFound, itemID)
		}
		return nil, PrintItem{}, fmt.Errorf("load print item %d: %w", itemID, err)
	}

	if item.Status != expectedStatus {
		tx.Rollback()
		return nil, PrintItem{}, fmt.Errorf("print item %d is not %s (status=%s)", itemID, expectedStatus, item.Status)
	}

	return tx, item, nil
}

// StartPrint moves a queued item onto the bed. If every other item in its
// order is still queued (this is the order's first item to start), the
// order itself advances from pending to in_progress.
func (s *Store) StartPrint(ctx context.Context, itemID int64) (PrintItem, error) {
	tx, item, err := s.beginItemTx(ctx, itemID, "queued")
	if err != nil {
		return PrintItem{}, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `UPDATE print_items SET status = 'printing' WHERE id = $1`, itemID); err != nil {
		return PrintItem{}, fmt.Errorf("start print for item %d: %w", itemID, err)
	}
	item.Status = "printing"

	var siblingsAlreadyMoving int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM print_items WHERE order_id = $1 AND id != $2 AND status != 'queued'`,
		item.OrderID, itemID,
	).Scan(&siblingsAlreadyMoving); err != nil {
		return PrintItem{}, fmt.Errorf("check sibling items for order %d: %w", item.OrderID, err)
	}
	if siblingsAlreadyMoving == 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE orders SET status = 'in_progress' WHERE id = $1 AND status = 'pending'`,
			item.OrderID,
		); err != nil {
			return PrintItem{}, fmt.Errorf("advance order %d to in_progress: %w", item.OrderID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return PrintItem{}, fmt.Errorf("commit start print for item %d: %w", itemID, err)
	}
	return item, nil
}

// PopOffBed moves a printing item into cleaning/QC.
func (s *Store) PopOffBed(ctx context.Context, itemID int64) (PrintItem, error) {
	tx, item, err := s.beginItemTx(ctx, itemID, "printing")
	if err != nil {
		return PrintItem{}, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `UPDATE print_items SET status = 'cleaning_qc' WHERE id = $1`, itemID); err != nil {
		return PrintItem{}, fmt.Errorf("pop item %d off bed: %w", itemID, err)
	}
	item.Status = "cleaning_qc"

	if err := tx.Commit(); err != nil {
		return PrintItem{}, fmt.Errorf("commit pop off bed for item %d: %w", itemID, err)
	}
	return item, nil
}

// PassQC moves an item from cleaning/QC to ready-to-pack. If every item in
// its order is now ready_to_pack or packed, the order advances to
// completed (unless it's already shipped).
func (s *Store) PassQC(ctx context.Context, itemID int64) (PrintItem, error) {
	tx, item, err := s.beginItemTx(ctx, itemID, "cleaning_qc")
	if err != nil {
		return PrintItem{}, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `UPDATE print_items SET status = 'ready_to_pack' WHERE id = $1`, itemID); err != nil {
		return PrintItem{}, fmt.Errorf("pass qc for item %d: %w", itemID, err)
	}
	item.Status = "ready_to_pack"

	var remaining int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM print_items WHERE order_id = $1 AND status NOT IN ('ready_to_pack', 'packed')`,
		item.OrderID,
	).Scan(&remaining); err != nil {
		return PrintItem{}, fmt.Errorf("check order %d completion: %w", item.OrderID, err)
	}
	if remaining == 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE orders SET status = 'completed' WHERE id = $1 AND status != 'shipped'`,
			item.OrderID,
		); err != nil {
			return PrintItem{}, fmt.Errorf("advance order %d to completed: %w", item.OrderID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return PrintItem{}, fmt.Errorf("commit pass qc for item %d: %w", itemID, err)
	}
	return item, nil
}

// RePrint sends a failed-QC item back to the queue from scratch - its
// completed quantity resets to 0 since whatever was already printed didn't
// pass QC.
func (s *Store) RePrint(ctx context.Context, itemID int64) (PrintItem, error) {
	tx, item, err := s.beginItemTx(ctx, itemID, "cleaning_qc")
	if err != nil {
		return PrintItem{}, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`UPDATE print_items SET status = 'queued', quantity_completed = 0 WHERE id = $1`,
		itemID,
	); err != nil {
		return PrintItem{}, fmt.Errorf("re-queue item %d: %w", itemID, err)
	}
	item.Status = "queued"
	item.QuantityCompleted = 0

	if err := tx.Commit(); err != nil {
		return PrintItem{}, fmt.Errorf("commit reprint for item %d: %w", itemID, err)
	}
	return item, nil
}

// GenerateLabel marks a ready-to-pack item as packed. If every item in its
// order is now packed, it also creates the order's shipment record and
// marks the order shipped, returning that shipment so the caller can show
// tracking info. There is no real carrier/label-printer integration here -
// the tracking number and label URL are locally generated placeholders
// (see the TODO below); wiring a real carrier API is future work.
func (s *Store) GenerateLabel(ctx context.Context, itemID int64) (PrintItem, *Shipment, error) {
	tx, item, err := s.beginItemTx(ctx, itemID, "ready_to_pack")
	if err != nil {
		return PrintItem{}, nil, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `UPDATE print_items SET status = 'packed' WHERE id = $1`, itemID); err != nil {
		return PrintItem{}, nil, fmt.Errorf("mark item %d packed: %w", itemID, err)
	}
	item.Status = "packed"

	var remaining int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM print_items WHERE order_id = $1 AND status != 'packed'`,
		item.OrderID,
	).Scan(&remaining); err != nil {
		return PrintItem{}, nil, fmt.Errorf("check order %d pack completion: %w", item.OrderID, err)
	}

	var shipment *Shipment
	if remaining == 0 {
		// TODO: no real carrier integration - PKDS-<order>-<unix ts> and a
		// stub /labels/<tracking>.pdf path stand in for a real
		// carrier-issued tracking number and printable label until one is
		// wired up.
		trackingNumber := fmt.Sprintf("PKDS-%d-%d", item.OrderID, time.Now().Unix())
		labelURL := fmt.Sprintf("/labels/%s.pdf", trackingNumber)

		var sh Shipment
		if err := tx.QueryRowContext(ctx, `
INSERT INTO shipments (order_id, carrier, tracking_number, label_url, shipped_at)
VALUES ($1, '', $2, $3, now())
RETURNING id, order_id, carrier, tracking_number, label_url, shipped_at`,
			item.OrderID, trackingNumber, labelURL,
		).Scan(&sh.ID, &sh.OrderID, &sh.Carrier, &sh.TrackingNumber, &sh.LabelURL, &sh.ShippedAt); err != nil {
			return PrintItem{}, nil, fmt.Errorf("create shipment for order %d: %w", item.OrderID, err)
		}
		shipment = &sh

		if _, err := tx.ExecContext(ctx, `UPDATE orders SET status = 'shipped' WHERE id = $1`, item.OrderID); err != nil {
			return PrintItem{}, nil, fmt.Errorf("advance order %d to shipped: %w", item.OrderID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return PrintItem{}, nil, fmt.Errorf("commit generate label for item %d: %w", itemID, err)
	}
	return item, shipment, nil
}
