package board

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"print-kds/internal/database"

	"github.com/joho/godotenv"
)

// init loads the repo-root .env explicitly. godotenv/autoload (pulled in
// by internal/database) only ever looks for ".env" in the process's
// starting working directory, which for `go test` is this package's own
// source directory (internal/board), not the repo root - so without this
// the DB_* vars database.New() needs would never be found. The error is
// ignored: a missing .env just means the DB_* vars have to already be set
// in the real environment (e.g. CI), which is a fine fallback.
func init() {
	_ = godotenv.Load("../../.env")
}

// testStore opens a connection the same way database.New() does (applying
// migrations along the way) and hands back a *Store wrapping it plus a
// fresh throwaway tenant id and a user id belonging to that tenant, so
// these tests exercise the real state machine (and its last_updated_by/
// print_item_events audit trail) against real Postgres rather than a mock.
func testStore(t *testing.T) (*Store, int64, int64) {
	t.Helper()
	svc := database.New()
	s := NewStore(svc.DB())
	tenantID := testTenantID(t, svc.DB())
	return s, tenantID, testUserID(t, svc.DB(), tenantID)
}

// testTenantID creates a throwaway tenant, cleaned up (cascading through
// orders -> print_items/shipments via ON DELETE CASCADE) once the test
// finishes.
func testTenantID(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	var id int64
	name := fmt.Sprintf("Test Tenant %s %d", t.Name(), time.Now().UnixNano())
	if err := db.QueryRowContext(context.Background(), `INSERT INTO tenants (name) VALUES ($1) RETURNING id`, name).Scan(&id); err != nil {
		t.Fatalf("create test tenant: %v", err)
	}
	t.Cleanup(func() {
		if _, err := db.ExecContext(context.Background(), `DELETE FROM tenants WHERE id = $1`, id); err != nil {
			t.Errorf("cleanup test tenant %d: %v", id, err)
		}
	})
	return id
}

// testUserID creates a throwaway user under tenantID - the "acting user"
// transition methods stamp onto last_updated_by/print_item_events.
// password_hash is a dummy value since these tests never authenticate,
// only reference the user's id/email.
func testUserID(t *testing.T, db *sql.DB, tenantID int64) int64 {
	t.Helper()
	var id int64
	email := fmt.Sprintf("test-%s-%d@example.com", t.Name(), time.Now().UnixNano())
	if err := db.QueryRowContext(context.Background(),
		`INSERT INTO users (tenant_id, email, password_hash, role) VALUES ($1, $2, 'x', 'staff') RETURNING id`,
		tenantID, email,
	).Scan(&id); err != nil {
		t.Fatalf("create test user: %v", err)
	}
	t.Cleanup(func() {
		if _, err := db.ExecContext(context.Background(), `DELETE FROM users WHERE id = $1`, id); err != nil {
			t.Errorf("cleanup test user %d: %v", id, err)
		}
	})
	return id
}

// newQueuedTestItem creates a throwaway order with a single print item
// (quantityRequired units), still sitting in queued. Returns the item id
// and a cleanup func that deletes the order (cascades to its print items)
// once the test is done.
func newQueuedTestItem(t *testing.T, s *Store, tenantID, userID int64, quantityRequired int) int64 {
	t.Helper()
	ctx := context.Background()

	orderNumber := fmt.Sprintf("TEST-%s-%d", t.Name(), time.Now().UnixNano())
	orderID, err := s.CreateOrder(ctx, tenantID, userID, NewOrder{
		OrderNumber:  orderNumber,
		CustomerName: "QC Test Customer",
		DueDate:      time.Now().Add(24 * time.Hour),
		Items: []NewOrderItem{
			{FileName: "widget.gcode", ColorMaterial: "Black PLA", QuantityRequired: quantityRequired},
		},
	})
	if err != nil {
		t.Fatalf("create test order: %v", err)
	}
	t.Cleanup(func() {
		if _, err := s.db.ExecContext(context.Background(), `DELETE FROM orders WHERE id = $1`, orderID); err != nil {
			t.Errorf("cleanup test order %d: %v", orderID, err)
		}
	})

	var itemID int64
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM print_items WHERE order_id = $1`, orderID).Scan(&itemID); err != nil {
		t.Fatalf("load test item for order %d: %v", orderID, err)
	}

	return itemID
}

// newQCTestItem creates a throwaway order with a single print item
// (quantityRequired units) and drives it through StartPrint/PopOffBed so
// it's sitting in cleaning_qc, ready for RecordQCResult. Returns the item
// id and a cleanup func that deletes the order (cascades to its print
// items) once the test is done.
func newQCTestItem(t *testing.T, s *Store, tenantID, userID int64, quantityRequired int) int64 {
	t.Helper()
	ctx := context.Background()
	itemID := newQueuedTestItem(t, s, tenantID, userID, quantityRequired)

	if _, err := s.StartPrint(ctx, tenantID, userID, itemID, ""); err != nil {
		t.Fatalf("start print for item %d: %v", itemID, err)
	}
	if _, err := s.PopOffBed(ctx, tenantID, userID, itemID); err != nil {
		t.Fatalf("pop item %d off bed: %v", itemID, err)
	}

	return itemID
}

func TestRecordQCResult_FullPass(t *testing.T) {
	s, tenantID, userID := testStore(t)
	itemID := newQCTestItem(t, s, tenantID, userID, 5)

	updated, split, err := s.RecordQCResult(context.Background(), tenantID, userID, itemID, 5, "")
	if err != nil {
		t.Fatalf("RecordQCResult: %v", err)
	}
	if split != nil {
		t.Fatalf("expected nil split, got %+v", split)
	}
	if updated.Status != "ready_to_pack" {
		t.Errorf("status = %q, want ready_to_pack", updated.Status)
	}
	if updated.QuantityCompleted != 5 {
		t.Errorf("quantity_completed = %d, want 5", updated.QuantityCompleted)
	}
	if updated.QuantityRequired != 5 {
		t.Errorf("quantity_required = %d, want 5", updated.QuantityRequired)
	}
	if updated.ReprintCount != 0 {
		t.Errorf("reprint_count = %d, want 0", updated.ReprintCount)
	}
}

func TestRecordQCResult_FullReprint(t *testing.T) {
	s, tenantID, userID := testStore(t)
	itemID := newQCTestItem(t, s, tenantID, userID, 4)

	updated, split, err := s.RecordQCResult(context.Background(), tenantID, userID, itemID, 0, "warped bed adhesion")
	if err != nil {
		t.Fatalf("RecordQCResult: %v", err)
	}
	if split != nil {
		t.Fatalf("expected nil split, got %+v", split)
	}
	if updated.Status != "queued" {
		t.Errorf("status = %q, want queued", updated.Status)
	}
	if updated.QuantityCompleted != 0 {
		t.Errorf("quantity_completed = %d, want 0", updated.QuantityCompleted)
	}
	if updated.ReprintCount != 1 {
		t.Errorf("reprint_count = %d, want 1", updated.ReprintCount)
	}
	if updated.QCFailureReason != "warped bed adhesion" {
		t.Errorf("qc_failure_reason = %q, want %q", updated.QCFailureReason, "warped bed adhesion")
	}
}

func TestRecordQCResult_PartialSplit(t *testing.T) {
	s, tenantID, userID := testStore(t)
	itemID := newQCTestItem(t, s, tenantID, userID, 10)

	updated, split, err := s.RecordQCResult(context.Background(), tenantID, userID, itemID, 7, "3 units cracked")
	if err != nil {
		t.Fatalf("RecordQCResult: %v", err)
	}
	if split == nil {
		t.Fatalf("expected non-nil split")
	}
	t.Cleanup(func() {
		if _, err := s.db.ExecContext(context.Background(), `DELETE FROM print_items WHERE id = $1`, split.ID); err != nil {
			t.Errorf("cleanup split item %d: %v", split.ID, err)
		}
	})

	if updated.Status != "ready_to_pack" {
		t.Errorf("original status = %q, want ready_to_pack", updated.Status)
	}
	if updated.QuantityRequired != 7 {
		t.Errorf("original quantity_required = %d, want 7", updated.QuantityRequired)
	}
	if updated.QuantityCompleted != 7 {
		t.Errorf("original quantity_completed = %d, want 7", updated.QuantityCompleted)
	}

	if split.Status != "queued" {
		t.Errorf("split status = %q, want queued", split.Status)
	}
	if split.QuantityRequired != 3 {
		t.Errorf("split quantity_required = %d, want 3", split.QuantityRequired)
	}
	if split.QuantityCompleted != 0 {
		t.Errorf("split quantity_completed = %d, want 0", split.QuantityCompleted)
	}
	if split.ReprintCount != 1 {
		t.Errorf("split reprint_count = %d, want 1", split.ReprintCount)
	}
	if split.QCFailureReason != "3 units cracked" {
		t.Errorf("split qc_failure_reason = %q, want %q", split.QCFailureReason, "3 units cracked")
	}
	if split.OrderID != updated.OrderID {
		t.Errorf("split order_id = %d, want %d", split.OrderID, updated.OrderID)
	}
}

func TestRecordQCResult_InvalidQuantity(t *testing.T) {
	s, tenantID, userID := testStore(t)
	itemID := newQCTestItem(t, s, tenantID, userID, 5)

	_, _, err := s.RecordQCResult(context.Background(), tenantID, userID, itemID, 6, "")
	if !errors.Is(err, ErrInvalidQuantity) {
		t.Fatalf("err = %v, want ErrInvalidQuantity", err)
	}

	_, _, err = s.RecordQCResult(context.Background(), tenantID, userID, itemID, -1, "")
	if !errors.Is(err, ErrInvalidQuantity) {
		t.Fatalf("err = %v, want ErrInvalidQuantity", err)
	}
}

func TestStartPrint_AssignedPrinter(t *testing.T) {
	s, tenantID, userID := testStore(t)
	ctx := context.Background()
	itemID := newQueuedTestItem(t, s, tenantID, userID, 1)

	updated, err := s.StartPrint(ctx, tenantID, userID, itemID, "Printer 3")
	if err != nil {
		t.Fatalf("StartPrint: %v", err)
	}
	if updated.Status != "printing" {
		t.Errorf("status = %q, want printing", updated.Status)
	}
	if updated.AssignedPrinter != "Printer 3" {
		t.Errorf("assigned_printer = %q, want %q", updated.AssignedPrinter, "Printer 3")
	}

	// Reload from the DB to confirm it was actually persisted, not just
	// set on the in-memory return value.
	var reloaded PrintItem
	row := s.db.QueryRowContext(ctx, `
SELECT`+itemColumns+`
FROM print_items pi
`+itemJoins+`
WHERE pi.id = $1`, itemID)
	if err := scanItem(row, &reloaded); err != nil {
		t.Fatalf("reload item %d: %v", itemID, err)
	}
	if reloaded.AssignedPrinter != "Printer 3" {
		t.Errorf("reloaded assigned_printer = %q, want %q", reloaded.AssignedPrinter, "Printer 3")
	}
}

// TestStartPrint_WrongTenantIsNotFound confirms the tenant boundary is
// enforced in the store itself (beginItemTxAny's WHERE o.tenant_id = $2),
// not just in the auth middleware above it - a cross-tenant item id must
// behave exactly like a nonexistent one.
func TestStartPrint_WrongTenantIsNotFound(t *testing.T) {
	s, tenantA, userA := testStore(t)
	_, tenantB, userB := testStore(t)
	itemID := newQueuedTestItem(t, s, tenantA, userA, 1)

	if _, err := s.StartPrint(context.Background(), tenantB, userB, itemID, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound for a cross-tenant item id", err)
	}
}

// newTestOrderItem creates a throwaway order (with the given due date) and
// its single print item, still sitting in queued. Like
// newQueuedTestItem/newQCTestItem, but with an explicit due date so tests
// can control due-date ordering directly (the other helpers hardcode
// due_date to now+24h, which - since orders.due_date is a plain DATE
// column - collapses to the same calendar day for anything created within
// the same test run).
func newTestOrderItem(t *testing.T, s *Store, tenantID, userID int64, dueDate time.Time, quantityRequired int) int64 {
	t.Helper()
	ctx := context.Background()

	orderNumber := fmt.Sprintf("TEST-%s-%d", t.Name(), time.Now().UnixNano())
	orderID, err := s.CreateOrder(ctx, tenantID, userID, NewOrder{
		OrderNumber:  orderNumber,
		CustomerName: "Queue Priority Test Customer",
		DueDate:      dueDate,
		Items: []NewOrderItem{
			{FileName: "widget.gcode", ColorMaterial: "Black PLA", QuantityRequired: quantityRequired},
		},
	})
	if err != nil {
		t.Fatalf("create test order: %v", err)
	}
	t.Cleanup(func() {
		if _, err := s.db.ExecContext(context.Background(), `DELETE FROM orders WHERE id = $1`, orderID); err != nil {
			t.Errorf("cleanup test order %d: %v", orderID, err)
		}
	})

	var itemID int64
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM print_items WHERE order_id = $1`, orderID).Scan(&itemID); err != nil {
		t.Fatalf("load test item for order %d: %v", orderID, err)
	}

	return itemID
}

// TestBoardColumns_ReprintJumpsQueue confirms the "queued" column's sort
// puts a reprinted item ahead of a fresh (never-failed) item even when the
// fresh item's order has an earlier due_date - see BoardColumns' doc
// comment for the reprint-priority rule this is exercising.
func TestBoardColumns_ReprintJumpsQueue(t *testing.T) {
	s, tenantID, userID := testStore(t)
	ctx := context.Background()

	now := time.Now()

	// Fresh item: due tomorrow - the earlier due date, so it would sort
	// first under due-date-only ordering.
	freshItemID := newTestOrderItem(t, s, tenantID, userID, now.Add(24*time.Hour), 1)

	// Reprinted item: due in two weeks (later due date, so it would
	// otherwise sort behind freshItemID), but it fails cleaning/QC
	// entirely and gets requeued with reprint_count > 0, which should
	// still put it ahead of the fresh item.
	reprintOrderDueDate := now.Add(14 * 24 * time.Hour)
	reprintItemID := newTestOrderItem(t, s, tenantID, userID, reprintOrderDueDate, 2)
	if _, err := s.StartPrint(ctx, tenantID, userID, reprintItemID, ""); err != nil {
		t.Fatalf("start print for item %d: %v", reprintItemID, err)
	}
	if _, err := s.PopOffBed(ctx, tenantID, userID, reprintItemID); err != nil {
		t.Fatalf("pop item %d off bed: %v", reprintItemID, err)
	}
	if _, _, err := s.RecordQCResult(ctx, tenantID, userID, reprintItemID, 0, "warped"); err != nil {
		t.Fatalf("RecordQCResult full reprint: %v", err)
	}

	columns, err := s.BoardColumns(ctx, tenantID)
	if err != nil {
		t.Fatalf("BoardColumns: %v", err)
	}

	queued := columns["queued"]
	freshIdx, reprintIdx := -1, -1
	for i, item := range queued {
		switch item.ID {
		case freshItemID:
			freshIdx = i
		case reprintItemID:
			reprintIdx = i
		}
	}
	if freshIdx == -1 {
		t.Fatalf("fresh item %d not found in queued column", freshItemID)
	}
	if reprintIdx == -1 {
		t.Fatalf("reprint item %d not found in queued column", reprintItemID)
	}
	if reprintIdx >= freshIdx {
		t.Errorf("reprinted item at index %d, fresh item at index %d - want reprinted item first", reprintIdx, freshIdx)
	}
}

// TestPopOffBed_StatusChangedAt confirms PopOffBed advances
// status_changed_at rather than leaving it at the value set on insert.
func TestPopOffBed_StatusChangedAt(t *testing.T) {
	s, tenantID, userID := testStore(t)
	ctx := context.Background()
	itemID := newQueuedTestItem(t, s, tenantID, userID, 1)

	started, err := s.StartPrint(ctx, tenantID, userID, itemID, "")
	if err != nil {
		t.Fatalf("StartPrint: %v", err)
	}
	before := started.StatusChangedAt
	if before.IsZero() {
		t.Fatalf("StatusChangedAt is zero after StartPrint")
	}

	popped, err := s.PopOffBed(ctx, tenantID, userID, itemID)
	if err != nil {
		t.Fatalf("PopOffBed: %v", err)
	}
	if popped.StatusChangedAt.IsZero() {
		t.Fatalf("StatusChangedAt is zero after PopOffBed")
	}
	if !popped.StatusChangedAt.After(before) && !popped.StatusChangedAt.Equal(before) {
		t.Errorf("StatusChangedAt after PopOffBed (%v) did not move forward from before (%v)", popped.StatusChangedAt, before)
	}
}

// newMultiItemTestOrder creates a throwaway order with len(quantities)
// print items, all queued, and returns their item ids in the same order as
// quantities. Like newQueuedTestItem, but for the cancel-cascade tests that
// need more than one sibling item on the same order.
func newMultiItemTestOrder(t *testing.T, s *Store, tenantID, userID int64, quantities []int) []int64 {
	t.Helper()
	ctx := context.Background()

	orderNumber := fmt.Sprintf("TEST-%s-%d", t.Name(), time.Now().UnixNano())
	items := make([]NewOrderItem, len(quantities))
	for i, qty := range quantities {
		items[i] = NewOrderItem{
			FileName:         fmt.Sprintf("widget-%d.gcode", i),
			ColorMaterial:    "Black PLA",
			QuantityRequired: qty,
		}
	}
	orderID, err := s.CreateOrder(ctx, tenantID, userID, NewOrder{
		OrderNumber:  orderNumber,
		CustomerName: "Cancel Test Customer",
		DueDate:      time.Now().Add(24 * time.Hour),
		Items:        items,
	})
	if err != nil {
		t.Fatalf("create test order: %v", err)
	}
	t.Cleanup(func() {
		if _, err := s.db.ExecContext(context.Background(), `DELETE FROM orders WHERE id = $1`, orderID); err != nil {
			t.Errorf("cleanup test order %d: %v", orderID, err)
		}
	})

	rows, err := s.db.QueryContext(ctx, `SELECT id, file_name FROM print_items WHERE order_id = $1 ORDER BY file_name`, orderID)
	if err != nil {
		t.Fatalf("load test items for order %d: %v", orderID, err)
	}
	defer rows.Close()

	itemIDs := make([]int64, len(quantities))
	for rows.Next() {
		var id int64
		var fileName string
		if err := rows.Scan(&id, &fileName); err != nil {
			t.Fatalf("scan test item for order %d: %v", orderID, err)
		}
		var idx int
		if _, err := fmt.Sscanf(fileName, "widget-%d.gcode", &idx); err != nil {
			t.Fatalf("parse index from file name %q: %v", fileName, err)
		}
		itemIDs[idx] = id
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate test items for order %d: %v", orderID, err)
	}

	return itemIDs
}

// orderStatus reads orders.status directly - the cancel-cascade tests need
// to confirm CancelItem's order-status side effect, which isn't otherwise
// exposed on PrintItem.
func orderStatus(t *testing.T, s *Store, orderID int64) string {
	t.Helper()
	var status string
	if err := s.db.QueryRowContext(context.Background(), `SELECT status FROM orders WHERE id = $1`, orderID).Scan(&status); err != nil {
		t.Fatalf("load order %d status: %v", orderID, err)
	}
	return status
}

func TestCancelItem_FromQueued(t *testing.T) {
	s, tenantID, userID := testStore(t)
	ctx := context.Background()
	itemID := newQueuedTestItem(t, s, tenantID, userID, 2)

	updated, err := s.CancelItem(ctx, tenantID, userID, itemID)
	if err != nil {
		t.Fatalf("CancelItem: %v", err)
	}
	if updated.Status != "cancelled" {
		t.Errorf("status = %q, want cancelled", updated.Status)
	}

	columns, err := s.BoardColumns(ctx, tenantID)
	if err != nil {
		t.Fatalf("BoardColumns: %v", err)
	}
	for status, items := range columns {
		for _, item := range items {
			if item.ID == itemID {
				t.Errorf("cancelled item %d still present in BoardColumns column %q", itemID, status)
			}
		}
	}
}

func TestCancelItem_FromCleaningQC(t *testing.T) {
	s, tenantID, userID := testStore(t)
	ctx := context.Background()
	itemID := newQCTestItem(t, s, tenantID, userID, 2)

	updated, err := s.CancelItem(ctx, tenantID, userID, itemID)
	if err != nil {
		t.Fatalf("CancelItem: %v", err)
	}
	if updated.Status != "cancelled" {
		t.Errorf("status = %q, want cancelled", updated.Status)
	}

	columns, err := s.BoardColumns(ctx, tenantID)
	if err != nil {
		t.Fatalf("BoardColumns: %v", err)
	}
	for status, items := range columns {
		for _, item := range items {
			if item.ID == itemID {
				t.Errorf("cancelled item %d still present in BoardColumns column %q", itemID, status)
			}
		}
	}
}

// TestCancelItem_LastLiveItemCancelsOrder confirms cancelling the last
// still-live item on a multi-item order flips orders.status to cancelled.
func TestCancelItem_LastLiveItemCancelsOrder(t *testing.T) {
	s, tenantID, userID := testStore(t)
	ctx := context.Background()
	itemIDs := newMultiItemTestOrder(t, s, tenantID, userID, []int{1, 1})

	var orderID int64
	if err := s.db.QueryRowContext(ctx, `SELECT order_id FROM print_items WHERE id = $1`, itemIDs[0]).Scan(&orderID); err != nil {
		t.Fatalf("load order id for item %d: %v", itemIDs[0], err)
	}

	if _, err := s.CancelItem(ctx, tenantID, userID, itemIDs[0]); err != nil {
		t.Fatalf("CancelItem(%d): %v", itemIDs[0], err)
	}
	if status := orderStatus(t, s, orderID); status == "cancelled" {
		t.Fatalf("order %d already cancelled after only one of two items was cancelled", orderID)
	}

	if _, err := s.CancelItem(ctx, tenantID, userID, itemIDs[1]); err != nil {
		t.Fatalf("CancelItem(%d): %v", itemIDs[1], err)
	}
	if status := orderStatus(t, s, orderID); status != "cancelled" {
		t.Errorf("order %d status = %q, want cancelled", orderID, status)
	}
}

// TestCancelItem_SiblingStillLiveLeavesOrderAlone confirms cancelling one
// item on a multi-item order, while a sibling is still live, does NOT
// change orders.status.
func TestCancelItem_SiblingStillLiveLeavesOrderAlone(t *testing.T) {
	s, tenantID, userID := testStore(t)
	ctx := context.Background()
	itemIDs := newMultiItemTestOrder(t, s, tenantID, userID, []int{1, 1})

	var orderID int64
	if err := s.db.QueryRowContext(ctx, `SELECT order_id FROM print_items WHERE id = $1`, itemIDs[0]).Scan(&orderID); err != nil {
		t.Fatalf("load order id for item %d: %v", itemIDs[0], err)
	}
	before := orderStatus(t, s, orderID)

	if _, err := s.CancelItem(ctx, tenantID, userID, itemIDs[0]); err != nil {
		t.Fatalf("CancelItem(%d): %v", itemIDs[0], err)
	}

	after := orderStatus(t, s, orderID)
	if after != before {
		t.Errorf("order %d status changed from %q to %q after cancelling only one of two live items", orderID, before, after)
	}
	if after == "cancelled" {
		t.Errorf("order %d status = cancelled with a sibling item still live", orderID)
	}
}

func TestUpdateQuantity_HappyPath(t *testing.T) {
	s, tenantID, userID := testStore(t)
	ctx := context.Background()
	itemID := newQueuedTestItem(t, s, tenantID, userID, 100)

	updated, err := s.UpdateQuantity(ctx, tenantID, userID, itemID, 60)
	if err != nil {
		t.Fatalf("UpdateQuantity: %v", err)
	}
	if updated.QuantityRequired != 60 {
		t.Errorf("quantity_required = %d, want 60", updated.QuantityRequired)
	}

	var reloaded int
	if err := s.db.QueryRowContext(ctx, `SELECT quantity_required FROM print_items WHERE id = $1`, itemID).Scan(&reloaded); err != nil {
		t.Fatalf("reload item %d: %v", itemID, err)
	}
	if reloaded != 60 {
		t.Errorf("reloaded quantity_required = %d, want 60", reloaded)
	}
}

func TestUpdateQuantity_OutOfRange(t *testing.T) {
	s, tenantID, userID := testStore(t)
	ctx := context.Background()
	itemID := newQueuedTestItem(t, s, tenantID, userID, 5)

	_, err := s.UpdateQuantity(ctx, tenantID, userID, itemID, 0)
	if !errors.Is(err, ErrInvalidQuantity) {
		t.Fatalf("err = %v, want ErrInvalidQuantity", err)
	}

	_, err = s.UpdateQuantity(ctx, tenantID, userID, itemID, -1)
	if !errors.Is(err, ErrInvalidQuantity) {
		t.Fatalf("err = %v, want ErrInvalidQuantity", err)
	}
}

// TestUpdateQuantity_RejectsNonQueued confirms UpdateQuantity refuses an
// item that isn't queued with a plain status-conflict error, not
// ErrInvalidQuantity - the quantity itself is fine, it's the item's status
// that's wrong.
func TestUpdateQuantity_RejectsNonQueued(t *testing.T) {
	s, tenantID, userID := testStore(t)
	ctx := context.Background()
	itemID := newQueuedTestItem(t, s, tenantID, userID, 5)

	if _, err := s.StartPrint(ctx, tenantID, userID, itemID, ""); err != nil {
		t.Fatalf("start print for item %d: %v", itemID, err)
	}

	_, err := s.UpdateQuantity(ctx, tenantID, userID, itemID, 3)
	if err == nil {
		t.Fatalf("expected an error updating quantity on a printing item, got nil")
	}
	if errors.Is(err, ErrInvalidQuantity) {
		t.Fatalf("err = %v, want a status-conflict error, not ErrInvalidQuantity", err)
	}
}

// advanceToReadyToPack drives a queued item all the way to ready_to_pack
// (StartPrint -> PopOffBed -> a full-pass RecordQCResult) so GenerateLabel
// tests can exercise packing/shipping without repeating that boilerplate
// setup at every call site.
func advanceToReadyToPack(t *testing.T, s *Store, tenantID, userID, itemID int64, quantityRequired int) {
	t.Helper()
	ctx := context.Background()

	if _, err := s.StartPrint(ctx, tenantID, userID, itemID, ""); err != nil {
		t.Fatalf("start print for item %d: %v", itemID, err)
	}
	if _, err := s.PopOffBed(ctx, tenantID, userID, itemID); err != nil {
		t.Fatalf("pop item %d off bed: %v", itemID, err)
	}
	if _, _, err := s.RecordQCResult(ctx, tenantID, userID, itemID, quantityRequired, ""); err != nil {
		t.Fatalf("pass qc for item %d: %v", itemID, err)
	}
}

// orderIDForItem looks up an item's parent order id directly - the
// GenerateLabel tests need it to read orders.status via orderStatus.
func orderIDForItem(t *testing.T, s *Store, itemID int64) int64 {
	t.Helper()
	var orderID int64
	if err := s.db.QueryRowContext(context.Background(), `SELECT order_id FROM print_items WHERE id = $1`, itemID).Scan(&orderID); err != nil {
		t.Fatalf("load order id for item %d: %v", itemID, err)
	}
	return orderID
}

// shipmentItemIDs reads shipment_items directly - the GenerateLabel tests
// need to confirm which print_items rows a given shipment actually links
// to, which isn't otherwise exposed on Shipment.
func shipmentItemIDs(t *testing.T, s *Store, shipmentID int64) []int64 {
	t.Helper()
	rows, err := s.db.QueryContext(context.Background(),
		`SELECT print_item_id FROM shipment_items WHERE shipment_id = $1 ORDER BY print_item_id`, shipmentID)
	if err != nil {
		t.Fatalf("query shipment_items for shipment %d: %v", shipmentID, err)
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan shipment_items row: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate shipment_items for shipment %d: %v", shipmentID, err)
	}
	return ids
}

// TestGenerateLabel_PartialShipmentDoesNotAdvanceOrder confirms packing one
// item on a multi-item order creates a shipment containing just that item,
// and does not advance orders.status to shipped while a sibling item is
// still ready_to_pack (not yet packed).
func TestGenerateLabel_PartialShipmentDoesNotAdvanceOrder(t *testing.T) {
	s, tenantID, userID := testStore(t)
	ctx := context.Background()
	itemIDs := newMultiItemTestOrder(t, s, tenantID, userID, []int{2, 3})
	orderID := orderIDForItem(t, s, itemIDs[0])

	advanceToReadyToPack(t, s, tenantID, userID, itemIDs[0], 2)
	advanceToReadyToPack(t, s, tenantID, userID, itemIDs[1], 3)

	packed, shipment, err := s.GenerateLabel(ctx, tenantID, userID, itemIDs[0])
	if err != nil {
		t.Fatalf("GenerateLabel(%d): %v", itemIDs[0], err)
	}
	if packed.Status != "packed" {
		t.Errorf("status = %q, want packed", packed.Status)
	}
	if shipment == nil {
		t.Fatalf("expected non-nil shipment")
	}

	linked := shipmentItemIDs(t, s, shipment.ID)
	if len(linked) != 1 || linked[0] != itemIDs[0] {
		t.Errorf("shipment %d links items %v, want just [%d]", shipment.ID, linked, itemIDs[0])
	}

	if status := orderStatus(t, s, orderID); status == "shipped" {
		t.Errorf("order %d status = shipped with a sibling item still ready_to_pack", orderID)
	}
}

// TestGenerateLabel_LastItemShipsOrder confirms that once the last item on
// an order is packed, orders.status advances to shipped.
func TestGenerateLabel_LastItemShipsOrder(t *testing.T) {
	s, tenantID, userID := testStore(t)
	ctx := context.Background()
	itemIDs := newMultiItemTestOrder(t, s, tenantID, userID, []int{1, 1})
	orderID := orderIDForItem(t, s, itemIDs[0])

	advanceToReadyToPack(t, s, tenantID, userID, itemIDs[0], 1)
	advanceToReadyToPack(t, s, tenantID, userID, itemIDs[1], 1)

	if _, _, err := s.GenerateLabel(ctx, tenantID, userID, itemIDs[0]); err != nil {
		t.Fatalf("GenerateLabel(%d): %v", itemIDs[0], err)
	}
	if status := orderStatus(t, s, orderID); status == "shipped" {
		t.Fatalf("order %d already shipped after only one of two items was packed", orderID)
	}

	if _, _, err := s.GenerateLabel(ctx, tenantID, userID, itemIDs[1]); err != nil {
		t.Fatalf("GenerateLabel(%d): %v", itemIDs[1], err)
	}
	if status := orderStatus(t, s, orderID); status != "shipped" {
		t.Errorf("order %d status = %q, want shipped", orderID, status)
	}
}

// TestGenerateLabel_CancelledSiblingDoesNotBlockShipping confirms a
// cancelled sibling item doesn't prevent the order from reaching shipped
// once every other (non-cancelled) item is packed.
func TestGenerateLabel_CancelledSiblingDoesNotBlockShipping(t *testing.T) {
	s, tenantID, userID := testStore(t)
	ctx := context.Background()
	itemIDs := newMultiItemTestOrder(t, s, tenantID, userID, []int{1, 1})
	orderID := orderIDForItem(t, s, itemIDs[0])

	if _, err := s.CancelItem(ctx, tenantID, userID, itemIDs[1]); err != nil {
		t.Fatalf("CancelItem(%d): %v", itemIDs[1], err)
	}

	advanceToReadyToPack(t, s, tenantID, userID, itemIDs[0], 1)
	if _, _, err := s.GenerateLabel(ctx, tenantID, userID, itemIDs[0]); err != nil {
		t.Fatalf("GenerateLabel(%d): %v", itemIDs[0], err)
	}

	if status := orderStatus(t, s, orderID); status != "shipped" {
		t.Errorf("order %d status = %q, want shipped with a cancelled sibling", orderID, status)
	}
}

// TestGenerateLabel_SeparateCallsCreateSeparateShipments confirms two items
// on the same order, packed via two separate GenerateLabel calls, produce
// two separate shipments rows rather than sharing one - "ship what's ready
// now" means each item ships independently as soon as it's ready.
func TestGenerateLabel_SeparateCallsCreateSeparateShipments(t *testing.T) {
	s, tenantID, userID := testStore(t)
	ctx := context.Background()
	itemIDs := newMultiItemTestOrder(t, s, tenantID, userID, []int{1, 1})

	advanceToReadyToPack(t, s, tenantID, userID, itemIDs[0], 1)
	advanceToReadyToPack(t, s, tenantID, userID, itemIDs[1], 1)

	_, shipmentA, err := s.GenerateLabel(ctx, tenantID, userID, itemIDs[0])
	if err != nil {
		t.Fatalf("GenerateLabel(%d): %v", itemIDs[0], err)
	}
	_, shipmentB, err := s.GenerateLabel(ctx, tenantID, userID, itemIDs[1])
	if err != nil {
		t.Fatalf("GenerateLabel(%d): %v", itemIDs[1], err)
	}

	if shipmentA == nil || shipmentB == nil {
		t.Fatalf("expected both shipments non-nil, got %+v, %+v", shipmentA, shipmentB)
	}
	if shipmentA.ID == shipmentB.ID {
		t.Fatalf("expected two distinct shipments, both calls returned shipment %d", shipmentA.ID)
	}

	linkedA := shipmentItemIDs(t, s, shipmentA.ID)
	if len(linkedA) != 1 || linkedA[0] != itemIDs[0] {
		t.Errorf("shipment %d links items %v, want just [%d]", shipmentA.ID, linkedA, itemIDs[0])
	}
	linkedB := shipmentItemIDs(t, s, shipmentB.ID)
	if len(linkedB) != 1 || linkedB[0] != itemIDs[1] {
		t.Errorf("shipment %d links items %v, want just [%d]", shipmentB.ID, linkedB, itemIDs[1])
	}
}

func TestFailPrint(t *testing.T) {
	s, tenantID, userID := testStore(t)
	ctx := context.Background()
	itemID := newQueuedTestItem(t, s, tenantID, userID, 3)

	if _, err := s.StartPrint(ctx, tenantID, userID, itemID, "Printer 3"); err != nil {
		t.Fatalf("start print for item %d: %v", itemID, err)
	}

	updated, err := s.FailPrint(ctx, tenantID, userID, itemID, "nozzle jam")
	if err != nil {
		t.Fatalf("FailPrint: %v", err)
	}
	if updated.Status != "queued" {
		t.Errorf("status = %q, want queued", updated.Status)
	}
	if updated.QuantityCompleted != 0 {
		t.Errorf("quantity_completed = %d, want 0", updated.QuantityCompleted)
	}
	if updated.AssignedPrinter != "" {
		t.Errorf("assigned_printer = %q, want empty", updated.AssignedPrinter)
	}
	if updated.ReprintCount != 1 {
		t.Errorf("reprint_count = %d, want 1", updated.ReprintCount)
	}
	if updated.QCFailureReason != "nozzle jam" {
		t.Errorf("qc_failure_reason = %q, want %q", updated.QCFailureReason, "nozzle jam")
	}
}

// testUserEmail reads a test user's email back directly - the audit-trail
// tests need it to confirm PrintItem.LastUpdatedByEmail/PrintItemEvent.UserEmail
// actually match the acting user, not just that they're non-empty.
func testUserEmail(t *testing.T, s *Store, userID int64) string {
	t.Helper()
	var email string
	if err := s.db.QueryRowContext(context.Background(), `SELECT email FROM users WHERE id = $1`, userID).Scan(&email); err != nil {
		t.Fatalf("load test user %d email: %v", userID, err)
	}
	return email
}

// TestStartPrint_RecordsLastUpdatedByAndEvent confirms StartPrint stamps
// last_updated_by (surfaced as PrintItem.LastUpdatedByEmail) and appends a
// print_item_events row - the audit trail this migration set exists to
// provide.
func TestStartPrint_RecordsLastUpdatedByAndEvent(t *testing.T) {
	s, tenantID, userID := testStore(t)
	ctx := context.Background()
	itemID := newQueuedTestItem(t, s, tenantID, userID, 1)
	email := testUserEmail(t, s, userID)

	updated, err := s.StartPrint(ctx, tenantID, userID, itemID, "Printer 3")
	if err != nil {
		t.Fatalf("StartPrint: %v", err)
	}
	if updated.LastUpdatedByEmail != email {
		t.Errorf("LastUpdatedByEmail = %q, want %q", updated.LastUpdatedByEmail, email)
	}

	// newQueuedTestItem's CreateOrder already wrote a "created" event -
	// StartPrint should append a second one, not replace it (events is an
	// append-only log).
	events, err := s.ItemHistory(ctx, tenantID, itemID)
	if err != nil {
		t.Fatalf("ItemHistory: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("len(events) = %d, want 2: %+v", len(events), events)
	}
	if events[0].FromStatus != "" || events[0].ToStatus != "queued" {
		t.Errorf("events[0] = %+v, want the creation event", events[0])
	}
	if events[1].FromStatus != "queued" || events[1].ToStatus != "printing" {
		t.Errorf("events[1] = %+v, want queued -> printing", events[1])
	}
	if events[1].UserEmail != email {
		t.Errorf("events[1].UserEmail = %q, want %q", events[1].UserEmail, email)
	}
}

// TestItemHistory_CrossTenantIsEmpty mirrors
// TestStartPrint_WrongTenantIsNotFound: an item's history must not leak
// across the tenant boundary any more than the item itself does.
func TestItemHistory_CrossTenantIsEmpty(t *testing.T) {
	s, tenantA, userA := testStore(t)
	_, tenantB, _ := testStore(t)
	ctx := context.Background()
	itemID := newQueuedTestItem(t, s, tenantA, userA, 1)

	if _, err := s.StartPrint(ctx, tenantA, userA, itemID, ""); err != nil {
		t.Fatalf("StartPrint: %v", err)
	}

	events, err := s.ItemHistory(ctx, tenantB, itemID)
	if err != nil {
		t.Fatalf("ItemHistory: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("len(events) = %d under the wrong tenant, want 0: %+v", len(events), events)
	}
}

// TestRecordQCResult_PartialFailureRecordsSplitCreationEvent confirms the
// sibling row a partial QC failure creates gets its own "created" event,
// not just a silent insert - the history log should explain where a new
// card on the board came from.
func TestRecordQCResult_PartialFailureRecordsSplitCreationEvent(t *testing.T) {
	s, tenantID, userID := testStore(t)
	itemID := newQCTestItem(t, s, tenantID, userID, 10)

	_, split, err := s.RecordQCResult(context.Background(), tenantID, userID, itemID, 7, "3 units cracked")
	if err != nil {
		t.Fatalf("RecordQCResult: %v", err)
	}
	if split == nil {
		t.Fatalf("expected non-nil split")
	}
	t.Cleanup(func() {
		if _, err := s.db.ExecContext(context.Background(), `DELETE FROM print_items WHERE id = $1`, split.ID); err != nil {
			t.Errorf("cleanup split item %d: %v", split.ID, err)
		}
	})

	events, err := s.ItemHistory(context.Background(), tenantID, split.ID)
	if err != nil {
		t.Fatalf("ItemHistory: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1: %+v", len(events), events)
	}
	if events[0].FromStatus != "" {
		t.Errorf("event.FromStatus = %q, want empty (a creation event)", events[0].FromStatus)
	}
	if events[0].ToStatus != "queued" {
		t.Errorf("event.ToStatus = %q, want queued", events[0].ToStatus)
	}
}
