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
	"slices"
	"strings"
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

// ErrCustomerNotFound is returned when a customer id doesn't exist in the
// caller's tenant.
var ErrCustomerNotFound = errors.New("customer not found")

// ErrInvalidQuantity is wrapped into the error RecordQCResult returns when
// quantityPassed falls outside [0, quantity_required] for the item -
// callers use this to show a 400 instead of the 409 a status conflict
// would get.
var ErrInvalidQuantity = errors.New("quantity passed is out of range")

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
	QCFailureReason   string
	ReprintCount      int
	CreatedAt         time.Time
	StatusChangedAt   time.Time

	OrderNumber  string
	CustomerName string
	DueDate      time.Time // the order's ship-by date
	// OrderCreatedAt is when the order was placed; FileCount is how many
	// uploaded files it carries (see ListOrderFiles).
	OrderCreatedAt time.Time
	FileCount      int
	ShipTo         ShipTo

	// LastUpdatedByEmail is the email of whoever most recently ran a
	// transition on this item - "" until its first transition, or if
	// that user has since been removed (last_updated_by ON DELETE SET
	// NULL). See PrintItemEvent for the full history, not just the most
	// recent touch.
	LastUpdatedByEmail string
}

// PrintItemEvent is one row of a print item's audit trail
// (print_item_events) - who did what, and when. FromStatus == "" marks a
// creation event (a fresh item from CreateOrder, or a sibling row split
// off by a partial QC failure) rather than an ordinary transition.
// UserEmail is "" if the acting user has since been removed.
type PrintItemEvent struct {
	ID         int64
	FromStatus string
	ToStatus   string
	Detail     string
	UserEmail  string
	CreatedAt  time.Time
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
	pi.quantity_completed, pi.status, pi.assigned_printer, pi.qc_failure_reason,
	pi.reprint_count, pi.created_at, pi.status_changed_at,
	o.order_number, o.customer_name, o.due_date, o.created_at, o.shipping_address,
	(SELECT count(*) FROM order_files f WHERE f.order_id = o.id), u.email`

// itemJoins must accompany every query built on itemColumns - it resolves
// u.email (last_updated_by's display name) alongside the orders join
// every such query already needs.
const itemJoins = `
JOIN orders o ON o.id = pi.order_id
LEFT JOIN users u ON u.id = pi.last_updated_by`

func scanItem(row interface{ Scan(...any) error }, item *PrintItem) error {
	var lastUpdatedByEmail sql.NullString
	var shipTo []byte
	if err := row.Scan(
		&item.ID, &item.OrderID, &item.FileName, &item.ColorMaterial,
		&item.QuantityRequired, &item.QuantityCompleted, &item.Status, &item.AssignedPrinter,
		&item.QCFailureReason, &item.ReprintCount,
		&item.CreatedAt, &item.StatusChangedAt, &item.OrderNumber, &item.CustomerName, &item.DueDate,
		&item.OrderCreatedAt, &shipTo, &item.FileCount, &lastUpdatedByEmail,
	); err != nil {
		return err
	}
	item.LastUpdatedByEmail = lastUpdatedByEmail.String
	item.ShipTo = parseShipTo(shipTo)
	return nil
}

// lookupUserEmail resolves userID's email within tx, for stamping the
// acting user's email directly onto a PrintItem this request is about to
// return - the caller shouldn't have to make a second round trip (or wait
// for the next BoardColumns load) just to see who they are.
func lookupUserEmail(ctx context.Context, tx *sql.Tx, userID int64) (string, error) {
	var email string
	if err := tx.QueryRowContext(ctx, `SELECT email FROM users WHERE id = $1`, userID).Scan(&email); err != nil {
		return "", fmt.Errorf("look up user %d: %w", userID, err)
	}
	return email, nil
}

// recordEvent appends one row to print_item_events - the append-only
// audit trail every transition method (and CreateOrder, for a fresh
// item's creation) writes to inside its own transaction. fromStatus ==
// "" marks a creation event rather than a status transition.
func recordEvent(ctx context.Context, tx *sql.Tx, itemID, userID int64, fromStatus, toStatus, detail string) error {
	if _, err := tx.ExecContext(ctx, `
INSERT INTO print_item_events (print_item_id, user_id, from_status, to_status, detail)
VALUES ($1, $2, $3, $4, $5)`,
		itemID, userID, fromStatus, toStatus, detail,
	); err != nil {
		return fmt.Errorf("record event for item %d: %w", itemID, err)
	}
	return nil
}

// ItemHistory returns itemID's full audit trail, oldest first - tenant
// scoped the same way beginItemTxAny is (a cross-tenant itemID returns an
// empty slice, not another shop's history).
func (s *Store) ItemHistory(ctx context.Context, tenantID, itemID int64) ([]PrintItemEvent, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT e.id, e.from_status, e.to_status, e.detail, e.created_at, u.email
FROM print_item_events e
JOIN print_items pi ON pi.id = e.print_item_id
JOIN orders o ON o.id = pi.order_id
LEFT JOIN users u ON u.id = e.user_id
WHERE e.print_item_id = $1 AND o.tenant_id = $2
ORDER BY e.created_at ASC`, itemID, tenantID)
	if err != nil {
		return nil, fmt.Errorf("query item %d history: %w", itemID, err)
	}
	defer rows.Close()

	var events []PrintItemEvent
	for rows.Next() {
		var e PrintItemEvent
		var userEmail sql.NullString
		if err := rows.Scan(&e.ID, &e.FromStatus, &e.ToStatus, &e.Detail, &e.CreatedAt, &userEmail); err != nil {
			return nil, fmt.Errorf("scan item %d history: %w", itemID, err)
		}
		e.UserEmail = userEmail.String
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate item %d history: %w", itemID, err)
	}
	return events, nil
}

// BoardColumns returns every live (non-packed) print item grouped by
// status. "queued" is sorted by reprint priority first, then by the parent
// order's due_date ascending: an item with reprint_count > 0 (requeued
// after a QC failure or a mid-print failure) jumps ahead of every item that
// hasn't failed an attempt yet, regardless of due date, since it's now
// running behind schedule and the shop floor wants that visible rather than
// having it quietly wait in due-date order; within each of those two groups
// (reprinted vs. fresh) the due-date ascending order still applies. Every
// other column is sorted by print_items.created_at ascending (oldest
// first, i.e. however long it's been sitting in that state). Packed items
// are terminal and drop off the live board entirely - this is a
// shop-floor status display, not an order history.
func (s *Store) BoardColumns(ctx context.Context, tenantID int64) (map[string][]PrintItem, error) {
	query := `
SELECT` + itemColumns + `
FROM print_items pi
` + itemJoins + `
WHERE pi.status IN ('queued', 'printing', 'cleaning_qc', 'ready_to_pack') AND o.tenant_id = $1
ORDER BY pi.status,
	CASE WHEN pi.status = 'queued' THEN (pi.reprint_count = 0) END,
	CASE WHEN pi.status = 'queued' THEN o.due_date END ASC,
	pi.created_at ASC`

	rows, err := s.db.QueryContext(ctx, query, tenantID)
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

// NewOrderFile is one uploaded file already written to disk; CreateOrder
// only records its metadata (see order_files).
type NewOrderFile struct {
	Filename    string
	StoredPath  string
	SizeBytes   int64
	ContentType string
}

// NewOrder is the order-intake input to CreateOrder - from the admin
// intake form or a client's portal order. When CustomerID is non-zero it
// must belong to the tenant, and the order's customer_name is taken from
// that customer (CustomerName is ignored); with CustomerID 0 the
// free-text CustomerName is used and the order has no customer link.
type NewOrder struct {
	OrderNumber  string
	CustomerID   int64
	CustomerName string
	DueDate      time.Time
	ShipTo       ShipTo
	Items        []NewOrderItem
	Files        []NewOrderFile
}

// CreateOrder inserts a new order and its print items (all starting
// "queued") in a single transaction - the manual intake form
// (controllers.NewOrderController) is the only caller today. Returns
// ErrDuplicateOrderNumber (wrap-checkable via errors.Is) if order_number's
// unique constraint rejects the insert, so the form can show a friendly
// message instead of a raw constraint error.
func (s *Store) CreateOrder(ctx context.Context, tenantID, userID int64, in NewOrder) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	customerName := in.CustomerName
	var customerID sql.NullInt64
	if in.CustomerID != 0 {
		if err := tx.QueryRowContext(ctx,
			`SELECT name FROM customers WHERE id = $1 AND tenant_id = $2`,
			in.CustomerID, tenantID,
		).Scan(&customerName); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return 0, ErrCustomerNotFound
			}
			return 0, fmt.Errorf("load customer %d: %w", in.CustomerID, err)
		}
		customerID = sql.NullInt64{Int64: in.CustomerID, Valid: true}
	}

	shipTo, err := in.ShipTo.marshal()
	if err != nil {
		return 0, fmt.Errorf("encode ship-to address: %w", err)
	}

	var orderID int64
	err = tx.QueryRowContext(ctx, `
INSERT INTO orders (tenant_id, order_number, customer_id, customer_name, shipping_address, due_date, status)
VALUES ($1, $2, $3, $4, $5, $6, 'pending')
RETURNING id`, tenantID, in.OrderNumber, customerID, customerName, shipTo, in.DueDate).Scan(&orderID)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return 0, fmt.Errorf("%w: %q", ErrDuplicateOrderNumber, in.OrderNumber)
		}
		return 0, fmt.Errorf("insert order: %w", err)
	}

	for _, item := range in.Items {
		qty := max(item.QuantityRequired, 1)
		var itemID int64
		if err := tx.QueryRowContext(ctx, `
INSERT INTO print_items (order_id, file_name, color_material, quantity_required, status, last_updated_by)
VALUES ($1, $2, $3, $4, 'queued', $5)
RETURNING id`, orderID, item.FileName, item.ColorMaterial, qty, userID).Scan(&itemID); err != nil {
			return 0, fmt.Errorf("insert print item %s: %w", item.FileName, err)
		}
		if err := recordEvent(ctx, tx, itemID, userID, "", "queued", ""); err != nil {
			return 0, err
		}
	}

	for _, f := range in.Files {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO order_files (order_id, filename, stored_path, size_bytes, content_type)
VALUES ($1, $2, $3, $4, $5)`, orderID, f.Filename, f.StoredPath, f.SizeBytes, f.ContentType); err != nil {
			return 0, fmt.Errorf("insert order file %s: %w", f.Filename, err)
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
func (s *Store) beginItemTx(ctx context.Context, tenantID, itemID int64, expectedStatus string) (*sql.Tx, PrintItem, error) {
	return s.beginItemTxAny(ctx, tenantID, itemID, expectedStatus)
}

// beginItemTxAny is beginItemTx's more permissive sibling: it accepts any
// number of allowed statuses rather than exactly one, for a transition like
// CancelItem that's valid from several starting statuses at once.
// beginItemTx (the single-status case every other transition method uses)
// is just this with one allowed status, so there's one lock+check
// implementation rather than two.
func (s *Store) beginItemTxAny(ctx context.Context, tenantID, itemID int64, allowedStatuses ...string) (*sql.Tx, PrintItem, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, PrintItem{}, fmt.Errorf("begin transaction: %w", err)
	}

	var item PrintItem
	row := tx.QueryRowContext(ctx, `
SELECT`+itemColumns+`
FROM print_items pi
`+itemJoins+`
WHERE pi.id = $1 AND o.tenant_id = $2
FOR UPDATE OF pi`, itemID, tenantID)
	if err := scanItem(row, &item); err != nil {
		tx.Rollback()
		if errors.Is(err, sql.ErrNoRows) {
			return nil, PrintItem{}, fmt.Errorf("%w: id %d", ErrNotFound, itemID)
		}
		return nil, PrintItem{}, fmt.Errorf("load print item %d: %w", itemID, err)
	}

	if !slices.Contains(allowedStatuses, item.Status) {
		tx.Rollback()
		return nil, PrintItem{}, fmt.Errorf("print item %d is not %s (status=%s)", itemID, strings.Join(allowedStatuses, "/"), item.Status)
	}

	return tx, item, nil
}

// StartPrint moves a queued item onto the bed, recording which physical
// printer it's going onto (assignedPrinter is free text - e.g. "Printer 3"
// - and may be blank; nobody is required to fill it in). If every other
// item in its order is still queued (this is the order's first item to
// start), the order itself advances from pending to in_progress.
func (s *Store) StartPrint(ctx context.Context, tenantID, userID, itemID int64, assignedPrinter string) (PrintItem, error) {
	tx, item, err := s.beginItemTx(ctx, tenantID, itemID, "queued")
	if err != nil {
		return PrintItem{}, err
	}
	defer tx.Rollback()

	email, err := lookupUserEmail(ctx, tx, userID)
	if err != nil {
		return PrintItem{}, err
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE print_items SET status = 'printing', assigned_printer = $2, status_changed_at = now(), last_updated_by = $3 WHERE id = $1`,
		itemID, assignedPrinter, userID,
	); err != nil {
		return PrintItem{}, fmt.Errorf("start print for item %d: %w", itemID, err)
	}
	fromStatus := item.Status
	item.Status = "printing"
	item.AssignedPrinter = assignedPrinter
	item.StatusChangedAt = time.Now()
	item.LastUpdatedByEmail = email

	detail := ""
	if assignedPrinter != "" {
		detail = fmt.Sprintf("printer: %s", assignedPrinter)
	}
	if err := recordEvent(ctx, tx, itemID, userID, fromStatus, item.Status, detail); err != nil {
		return PrintItem{}, err
	}

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
func (s *Store) PopOffBed(ctx context.Context, tenantID, userID, itemID int64) (PrintItem, error) {
	tx, item, err := s.beginItemTx(ctx, tenantID, itemID, "printing")
	if err != nil {
		return PrintItem{}, err
	}
	defer tx.Rollback()

	email, err := lookupUserEmail(ctx, tx, userID)
	if err != nil {
		return PrintItem{}, err
	}

	if _, err := tx.ExecContext(ctx, `UPDATE print_items SET status = 'cleaning_qc', status_changed_at = now(), last_updated_by = $2 WHERE id = $1`, itemID, userID); err != nil {
		return PrintItem{}, fmt.Errorf("pop item %d off bed: %w", itemID, err)
	}
	fromStatus := item.Status
	item.Status = "cleaning_qc"
	item.StatusChangedAt = time.Now()
	item.LastUpdatedByEmail = email

	if err := recordEvent(ctx, tx, itemID, userID, fromStatus, item.Status, ""); err != nil {
		return PrintItem{}, err
	}

	if err := tx.Commit(); err != nil {
		return PrintItem{}, fmt.Errorf("commit pop off bed for item %d: %w", itemID, err)
	}
	return item, nil
}

// FailPrint sends a printing item straight back to queued when the print
// itself fails mid-job (nozzle jam, filament runout, bed adhesion failure,
// etc.) rather than finishing and only turning out bad at cleaning/QC
// afterward. This is conceptually the same "this attempt didn't produce a
// usable print, try again" event as RecordQCResult's full-reprint case
// (quantityPassed == 0), just triggered earlier - so it reuses
// reprint_count/qc_failure_reason rather than adding new columns.
// assigned_printer is cleared back to ” since whichever printer failed
// isn't "still running" this item anymore.
func (s *Store) FailPrint(ctx context.Context, tenantID, userID, itemID int64, reason string) (PrintItem, error) {
	tx, item, err := s.beginItemTx(ctx, tenantID, itemID, "printing")
	if err != nil {
		return PrintItem{}, err
	}
	defer tx.Rollback()

	email, err := lookupUserEmail(ctx, tx, userID)
	if err != nil {
		return PrintItem{}, err
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE print_items
		SET status = 'queued', quantity_completed = 0, assigned_printer = '', reprint_count = reprint_count + 1, qc_failure_reason = $2, status_changed_at = now(), last_updated_by = $3
		WHERE id = $1`,
		itemID, reason, userID,
	); err != nil {
		return PrintItem{}, fmt.Errorf("fail print for item %d: %w", itemID, err)
	}
	fromStatus := item.Status
	item.Status = "queued"
	item.QuantityCompleted = 0
	item.AssignedPrinter = ""
	item.ReprintCount++
	item.QCFailureReason = reason
	item.StatusChangedAt = time.Now()
	item.LastUpdatedByEmail = email

	detail := ""
	if reason != "" {
		detail = fmt.Sprintf("reason: %s", reason)
	}
	if err := recordEvent(ctx, tx, itemID, userID, fromStatus, item.Status, detail); err != nil {
		return PrintItem{}, err
	}

	if err := tx.Commit(); err != nil {
		return PrintItem{}, fmt.Errorf("commit fail print for item %d: %w", itemID, err)
	}
	return item, nil
}

// RecordQCResult moves a cleaning_qc item forward based on how many of its
// quantity_required units actually passed. quantityPassed == the row's full
// quantity_required is an ordinary full pass (status -> ready_to_pack,
// quantity_completed set to match). quantityPassed == 0 is a full reprint
// (status -> queued, quantity_completed reset to 0, reprint_count
// incremented, qc_failure_reason recorded) - same as the old RePrint
// behavior, just recording metadata now. Anything in between splits the
// row: the original row keeps only the passed quantity (quantity_required
// shrinks to quantityPassed, status -> ready_to_pack, quantity_completed =
// quantityPassed) and a NEW sibling row is inserted for the failed
// remainder (same order/file/color, quantity_required = original required
// minus quantityPassed, status queued, quantity_completed 0, reprint_count
// = original + 1, qc_failure_reason recorded) - this is the only case
// where the returned split item is non-nil. failureReason is stored
// whenever any units failed (quantityPassed < required); it's ignored
// (not persisted) when the whole thing passed.
func (s *Store) RecordQCResult(ctx context.Context, tenantID, userID, itemID int64, quantityPassed int, failureReason string) (updated PrintItem, split *PrintItem, err error) {
	tx, item, err := s.beginItemTx(ctx, tenantID, itemID, "cleaning_qc")
	if err != nil {
		return PrintItem{}, nil, err
	}
	defer tx.Rollback()

	if quantityPassed < 0 || quantityPassed > item.QuantityRequired {
		tx.Rollback()
		return PrintItem{}, nil, fmt.Errorf("%w: %d passed of %d required", ErrInvalidQuantity, quantityPassed, item.QuantityRequired)
	}

	email, err := lookupUserEmail(ctx, tx, userID)
	if err != nil {
		return PrintItem{}, nil, err
	}

	fromStatus := item.Status
	originalRequired := item.QuantityRequired

	switch quantityPassed {
	case item.QuantityRequired:
		// Full pass.
		if _, err := tx.ExecContext(ctx,
			`UPDATE print_items SET status = 'ready_to_pack', quantity_completed = $2, status_changed_at = now(), last_updated_by = $3 WHERE id = $1`,
			itemID, quantityPassed, userID,
		); err != nil {
			return PrintItem{}, nil, fmt.Errorf("pass qc for item %d: %w", itemID, err)
		}
		item.Status = "ready_to_pack"
		item.QuantityCompleted = quantityPassed
		item.StatusChangedAt = time.Now()
		item.LastUpdatedByEmail = email

		if err := recordEvent(ctx, tx, itemID, userID, fromStatus, item.Status, fmt.Sprintf("passed %d/%d", quantityPassed, originalRequired)); err != nil {
			return PrintItem{}, nil, err
		}

	case 0:
		// Full reprint.
		if _, err := tx.ExecContext(ctx,
			`UPDATE print_items
			SET status = 'queued', quantity_completed = 0, reprint_count = reprint_count + 1, qc_failure_reason = $2, status_changed_at = now(), last_updated_by = $3
			WHERE id = $1`,
			itemID, failureReason, userID,
		); err != nil {
			return PrintItem{}, nil, fmt.Errorf("re-queue item %d: %w", itemID, err)
		}
		item.Status = "queued"
		item.QuantityCompleted = 0
		item.ReprintCount++
		item.QCFailureReason = failureReason
		item.StatusChangedAt = time.Now()
		item.LastUpdatedByEmail = email

		detail := fmt.Sprintf("passed 0/%d", originalRequired)
		if failureReason != "" {
			detail += fmt.Sprintf(", reason: %s", failureReason)
		}
		if err := recordEvent(ctx, tx, itemID, userID, fromStatus, item.Status, detail); err != nil {
			return PrintItem{}, nil, err
		}

	default:
		// Partial split: the original row keeps the passed quantity and
		// moves on, a new sibling row is inserted for the failed
		// remainder. Compute the failed remainder before overwriting
		// item.QuantityRequired below.
		failedRemainder := item.QuantityRequired - quantityPassed

		if _, err := tx.ExecContext(ctx,
			`UPDATE print_items
			SET status = 'ready_to_pack', quantity_required = $2, quantity_completed = $2, status_changed_at = now(), last_updated_by = $3
			WHERE id = $1`,
			itemID, quantityPassed, userID,
		); err != nil {
			return PrintItem{}, nil, fmt.Errorf("split-pass qc for item %d: %w", itemID, err)
		}
		item.Status = "ready_to_pack"
		item.QuantityRequired = quantityPassed
		item.QuantityCompleted = quantityPassed
		item.StatusChangedAt = time.Now()
		item.LastUpdatedByEmail = email

		detail := fmt.Sprintf("passed %d/%d", quantityPassed, originalRequired)
		if failureReason != "" {
			detail += fmt.Sprintf(", reason: %s", failureReason)
		}
		if err := recordEvent(ctx, tx, itemID, userID, fromStatus, item.Status, detail); err != nil {
			return PrintItem{}, nil, err
		}

		var newItem PrintItem
		newItem.OrderNumber = item.OrderNumber
		newItem.CustomerName = item.CustomerName
		newItem.DueDate = item.DueDate
		newItem.LastUpdatedByEmail = email

		if err := tx.QueryRowContext(ctx, `
INSERT INTO print_items (
	order_id, file_name, color_material, quantity_required, quantity_completed,
	status, assigned_printer, qc_failure_reason, reprint_count, status_changed_at, last_updated_by
)
VALUES ($1, $2, $3, $4, 0, 'queued', $5, $6, $7, now(), $8)
RETURNING id, order_id, file_name, color_material, quantity_required,
	quantity_completed, status, assigned_printer, qc_failure_reason,
	reprint_count, created_at, status_changed_at`,
			item.OrderID, item.FileName, item.ColorMaterial,
			failedRemainder,
			item.AssignedPrinter, failureReason, item.ReprintCount+1, userID,
		).Scan(
			&newItem.ID, &newItem.OrderID, &newItem.FileName, &newItem.ColorMaterial,
			&newItem.QuantityRequired, &newItem.QuantityCompleted, &newItem.Status,
			&newItem.AssignedPrinter, &newItem.QCFailureReason, &newItem.ReprintCount,
			&newItem.CreatedAt, &newItem.StatusChangedAt,
		); err != nil {
			return PrintItem{}, nil, fmt.Errorf("insert split item for %d: %w", itemID, err)
		}
		split = &newItem

		splitDetail := fmt.Sprintf("split from item #%d after partial QC failure", itemID)
		if failureReason != "" {
			splitDetail += fmt.Sprintf(", reason: %s", failureReason)
		}
		if err := recordEvent(ctx, tx, newItem.ID, userID, "", "queued", splitDetail); err != nil {
			return PrintItem{}, nil, err
		}
	}

	var remaining int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM print_items WHERE order_id = $1 AND status NOT IN ('ready_to_pack', 'packed')`,
		item.OrderID,
	).Scan(&remaining); err != nil {
		return PrintItem{}, nil, fmt.Errorf("check order %d completion: %w", item.OrderID, err)
	}
	if remaining == 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE orders SET status = 'completed' WHERE id = $1 AND status != 'shipped'`,
			item.OrderID,
		); err != nil {
			return PrintItem{}, nil, fmt.Errorf("advance order %d to completed: %w", item.OrderID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return PrintItem{}, nil, fmt.Errorf("commit qc result for item %d: %w", itemID, err)
	}
	return item, split, nil
}

// GenerateLabel marks a ready-to-pack item as packed and ships it
// immediately: every call creates its own shipments row (plus a linking
// shipment_items row) for just this one item, rather than waiting for
// every sibling item on the order to reach packed first. This is what lets
// an order ship in pieces - if 9 of 10 items are ready and packed but the
// 10th is stuck in a reprint loop, the 9 go out now instead of the whole
// order waiting on the straggler.
//
// orders.status only advances to 'shipped' once every NON-cancelled item
// on the order has reached packed - a cancelled sibling doesn't block
// shipping, it's not going anywhere - and that update is guarded to never
// re-run once the order is already 'shipped', and never touches an order
// that's 'cancelled'.
//
// There is no real carrier/label-printer integration here - the tracking
// number and label URL are locally generated placeholders (see the TODO
// below); wiring a real carrier API is future work.
func (s *Store) GenerateLabel(ctx context.Context, tenantID, userID, itemID int64) (PrintItem, *Shipment, error) {
	tx, item, err := s.beginItemTx(ctx, tenantID, itemID, "ready_to_pack")
	if err != nil {
		return PrintItem{}, nil, err
	}
	defer tx.Rollback()

	email, err := lookupUserEmail(ctx, tx, userID)
	if err != nil {
		return PrintItem{}, nil, err
	}

	if _, err := tx.ExecContext(ctx, `UPDATE print_items SET status = 'packed', status_changed_at = now(), last_updated_by = $2 WHERE id = $1`, itemID, userID); err != nil {
		return PrintItem{}, nil, fmt.Errorf("mark item %d packed: %w", itemID, err)
	}
	fromStatus := item.Status
	item.Status = "packed"
	item.StatusChangedAt = time.Now()
	item.LastUpdatedByEmail = email

	if err := recordEvent(ctx, tx, itemID, userID, fromStatus, item.Status, ""); err != nil {
		return PrintItem{}, nil, err
	}

	// TODO: no real carrier integration - PKDS-<order>-<item>-<unix ts> and
	// a stub /labels/<tracking>.pdf path stand in for a real
	// carrier-issued tracking number and printable label until one is
	// wired up. The item id is folded into the tracking number (not just
	// order + timestamp) because every call now creates its own per-item
	// shipment - two items on the same order packed within the same second
	// would otherwise collide on order+timestamp alone.
	trackingNumber := fmt.Sprintf("PKDS-%d-%d-%d", item.OrderID, item.ID, time.Now().Unix())
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

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO shipment_items (shipment_id, print_item_id) VALUES ($1, $2)`,
		sh.ID, item.ID,
	); err != nil {
		return PrintItem{}, nil, fmt.Errorf("link shipment %d to item %d: %w", sh.ID, item.ID, err)
	}
	shipment := &sh

	var remaining int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM print_items WHERE order_id = $1 AND status NOT IN ('packed', 'cancelled')`,
		item.OrderID,
	).Scan(&remaining); err != nil {
		return PrintItem{}, nil, fmt.Errorf("check order %d pack completion: %w", item.OrderID, err)
	}
	if remaining == 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE orders SET status = 'shipped' WHERE id = $1 AND status NOT IN ('shipped', 'cancelled')`,
			item.OrderID,
		); err != nil {
			return PrintItem{}, nil, fmt.Errorf("advance order %d to shipped: %w", item.OrderID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return PrintItem{}, nil, fmt.Errorf("commit generate label for item %d: %w", itemID, err)
	}
	return item, shipment, nil
}

// CancelItem marks a print item cancelled - a customer pulling an item (or a
// whole order) mid-flight. Unlike every other transition method, this one is
// valid from any non-terminal starting status (queued, printing,
// cleaning_qc, ready_to_pack), not just one - a cancellation can land at any
// point in the item's life - so it locks the row via beginItemTxAny with all
// four allowed rather than beginItemTx's single-status check. Cancelling an
// already-terminal item (packed, or already cancelled) is rejected the same
// way an ordinary wrong-status transition is.
//
// cancelled is terminal like packed: BoardColumns excludes both, so a
// cancelled card simply drops off the live board.
//
// If every item on the parent order is now cancelled or packed (a
// cancelled item alongside already-packed/shipped items is a partial
// cancellation - the order stays whatever it was), the order itself moves
// to cancelled too, unless it's already shipped (a shipped order is done;
// cancelling one more item on it after the fact doesn't un-ship it).
func (s *Store) CancelItem(ctx context.Context, tenantID, userID, itemID int64) (PrintItem, error) {
	tx, item, err := s.beginItemTxAny(ctx, tenantID, itemID, "queued", "printing", "cleaning_qc", "ready_to_pack")
	if err != nil {
		return PrintItem{}, err
	}
	defer tx.Rollback()

	email, err := lookupUserEmail(ctx, tx, userID)
	if err != nil {
		return PrintItem{}, err
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE print_items SET status = 'cancelled', status_changed_at = now(), last_updated_by = $2 WHERE id = $1`,
		itemID, userID,
	); err != nil {
		return PrintItem{}, fmt.Errorf("cancel item %d: %w", itemID, err)
	}
	fromStatus := item.Status
	item.Status = "cancelled"
	item.StatusChangedAt = time.Now()
	item.LastUpdatedByEmail = email

	if err := recordEvent(ctx, tx, itemID, userID, fromStatus, item.Status, ""); err != nil {
		return PrintItem{}, err
	}

	var stillLive int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM print_items WHERE order_id = $1 AND status NOT IN ('cancelled', 'packed')`,
		item.OrderID,
	).Scan(&stillLive); err != nil {
		return PrintItem{}, fmt.Errorf("check order %d cancellation: %w", item.OrderID, err)
	}
	if stillLive == 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE orders SET status = 'cancelled' WHERE id = $1 AND status != 'shipped'`,
			item.OrderID,
		); err != nil {
			return PrintItem{}, fmt.Errorf("cancel order %d: %w", item.OrderID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return PrintItem{}, fmt.Errorf("commit cancel item %d: %w", itemID, err)
	}
	return item, nil
}

// UpdateQuantity edits a queued item's quantity_required - a customer
// reducing (or increasing) an order after it's already on the board, before
// any print has actually started. Only valid while the item is still
// queued: once printing/QC/packing have begun, quantity_completed and any
// partial-QC split already carry the "how many units are really needed
// now" information, so this isn't the right tool for that anymore. Rejects
// newQuantityRequired < 1 with ErrInvalidQuantity - cancelling an item
// entirely is CancelItem, not a quantity edit down to zero.
func (s *Store) UpdateQuantity(ctx context.Context, tenantID, userID, itemID int64, newQuantityRequired int) (PrintItem, error) {
	tx, item, err := s.beginItemTx(ctx, tenantID, itemID, "queued")
	if err != nil {
		return PrintItem{}, err
	}
	defer tx.Rollback()

	if newQuantityRequired < 1 {
		tx.Rollback()
		return PrintItem{}, fmt.Errorf("%w: %d", ErrInvalidQuantity, newQuantityRequired)
	}

	email, err := lookupUserEmail(ctx, tx, userID)
	if err != nil {
		return PrintItem{}, err
	}

	oldQuantity := item.QuantityRequired

	if _, err := tx.ExecContext(ctx,
		`UPDATE print_items SET quantity_required = $2, status_changed_at = now(), last_updated_by = $3 WHERE id = $1`,
		itemID, newQuantityRequired, userID,
	); err != nil {
		return PrintItem{}, fmt.Errorf("update quantity for item %d: %w", itemID, err)
	}
	item.QuantityRequired = newQuantityRequired
	item.StatusChangedAt = time.Now()
	item.LastUpdatedByEmail = email

	if err := recordEvent(ctx, tx, itemID, userID, item.Status, item.Status, fmt.Sprintf("quantity %d -> %d", oldQuantity, newQuantityRequired)); err != nil {
		return PrintItem{}, err
	}

	if err := tx.Commit(); err != nil {
		return PrintItem{}, fmt.Errorf("commit update quantity for item %d: %w", itemID, err)
	}
	return item, nil
}
