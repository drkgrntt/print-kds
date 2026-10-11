package board

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// shippedItem creates an order for customerID with one item, drives it to
// packed through the real state machine, then back-dates its shipment to
// shippedAt. Returns the item id.
func shippedItem(t *testing.T, s *Store, tenantID, userID, customerID int64, qty int, shippedAt time.Time) int64 {
	t.Helper()
	ctx := context.Background()
	orderID, err := s.CreateOrder(ctx, tenantID, userID, NewOrder{
		OrderNumber: fmt.Sprintf("INV-%s-%d", t.Name(), time.Now().UnixNano()),
		CustomerID:  customerID,
		DueDate:     time.Now().Add(24 * time.Hour),
		Items:       []NewOrderItem{{FileName: "p.stl", ColorMaterial: "Red", QuantityRequired: qty}},
	})
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	var itemID int64
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM print_items WHERE order_id = $1`, orderID).Scan(&itemID); err != nil {
		t.Fatalf("find item: %v", err)
	}
	if _, err := s.StartPrint(ctx, tenantID, userID, itemID, ""); err != nil {
		t.Fatalf("StartPrint: %v", err)
	}
	if _, err := s.PopOffBed(ctx, tenantID, userID, itemID); err != nil {
		t.Fatalf("PopOffBed: %v", err)
	}
	if _, _, err := s.RecordQCResult(ctx, tenantID, userID, itemID, qty, ""); err != nil {
		t.Fatalf("RecordQCResult: %v", err)
	}
	if _, _, err := s.GenerateLabel(ctx, tenantID, userID, itemID); err != nil {
		t.Fatalf("GenerateLabel: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `
UPDATE shipments SET shipped_at = $2 WHERE id IN (SELECT shipment_id FROM shipment_items WHERE print_item_id = $1)`,
		itemID, shippedAt); err != nil {
		t.Fatalf("backdate shipment: %v", err)
	}
	return itemID
}

func day(daysAgo int) time.Time {
	return time.Now().AddDate(0, 0, -daysAgo).Truncate(24 * time.Hour).Add(12 * time.Hour)
}

func TestInvoiceBatch(t *testing.T) {
	s, tenantID, userID := testStore(t)
	ctx := context.Background()

	acme, _ := s.CreateCustomer(ctx, tenantID, Customer{Name: "Acme"})
	globex, _ := s.CreateCustomer(ctx, tenantID, Customer{Name: "Globex"})

	inRange1 := shippedItem(t, s, tenantID, userID, acme.ID, 2, day(3))
	inRange2 := shippedItem(t, s, tenantID, userID, acme.ID, 1, day(10))
	tooOld := shippedItem(t, s, tenantID, userID, acme.ID, 1, day(30))
	other := shippedItem(t, s, tenantID, userID, globex.ID, 4, day(5))
	unpriced := shippedItem(t, s, tenantID, userID, acme.ID, 1, day(2))
	// Cancelled items never reach packed, so they can't be invoiced.
	cancelledID := newQueuedTestItem(t, s, tenantID, userID, 1)
	if _, err := s.CancelItem(ctx, tenantID, userID, cancelledID); err != nil {
		t.Fatalf("CancelItem: %v", err)
	}

	for id, cents := range map[int64]int64{inRange1: 1000, inRange2: 250, tooOld: 999, other: 500} {
		if err := s.SetItemPrice(ctx, tenantID, id, cents); err != nil {
			t.Fatalf("SetItemPrice(%d): %v", id, err)
		}
	}

	from, to := day(14), day(0)
	cands, err := s.PreviewBatch(ctx, tenantID, 0, from, to)
	if err != nil {
		t.Fatalf("PreviewBatch: %v", err)
	}
	if len(cands) != 4 { // inRange1, inRange2, unpriced, other (not tooOld/cancelled)
		t.Fatalf("preview = %d candidates, want 4: %+v", len(cands), cands)
	}

	res, err := s.CreateBatch(ctx, tenantID, userID, 0, from, to)
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	if len(res.Invoices) != 2 || res.Unpriced != 1 {
		t.Fatalf("batch = %d invoices, %d unpriced; want 2, 1", len(res.Invoices), res.Unpriced)
	}
	var acmeInv, globexInv Invoice
	for _, inv := range res.Invoices {
		if inv.CustomerID == acme.ID {
			acmeInv = inv
		} else {
			globexInv = inv
		}
	}
	if acmeInv.TotalCents != 2*1000+250 || len(acmeInv.Lines) != 2 {
		t.Errorf("acme invoice total=%d lines=%d, want 2250/2", acmeInv.TotalCents, len(acmeInv.Lines))
	}
	if globexInv.TotalCents != 4*500 {
		t.Errorf("globex total = %d, want 2000", globexInv.TotalCents)
	}
	if acmeInv.Number == globexInv.Number || acmeInv.Number == 0 {
		t.Errorf("invoice numbers not distinct/positive: %d, %d", acmeInv.Number, globexInv.Number)
	}

	// Running the same batch again invoices nothing new (only the unpriced item is left).
	again, err := s.CreateBatch(ctx, tenantID, userID, 0, from, to)
	if err != nil || len(again.Invoices) != 0 || again.Unpriced != 1 {
		t.Fatalf("second batch = %+v, %v; want no invoices, 1 unpriced", again, err)
	}

	// Invoiced prices are frozen; the unpriced item can still be priced and picked up later.
	if err := s.SetItemPrice(ctx, tenantID, inRange1, 1); !errors.Is(err, ErrItemInvoiced) {
		t.Errorf("SetItemPrice on invoiced item err = %v, want ErrItemInvoiced", err)
	}
	if err := s.SetItemPrice(ctx, tenantID, unpriced, 300); err != nil {
		t.Fatalf("price late item: %v", err)
	}
	late, err := s.CreateBatch(ctx, tenantID, userID, acme.ID, from, to)
	if err != nil || len(late.Invoices) != 1 || late.Invoices[0].TotalCents != 300 {
		t.Fatalf("late batch = %+v, %v; want one 300c invoice", late, err)
	}

	got, err := s.GetInvoice(ctx, tenantID, acmeInv.ID)
	if err != nil || len(got.Lines) != 2 || got.CustomerName != "Acme" {
		t.Fatalf("GetInvoice = %+v, %v", got, err)
	}
	if list, _ := s.ListInvoices(ctx, tenantID); len(list) != 3 {
		t.Errorf("ListInvoices = %d, want 3", len(list))
	}
}

func TestInvoiceTenantIsolation(t *testing.T) {
	s, tenantID, userID := testStore(t)
	ctx := context.Background()
	otherTenant := testTenantID(t, s.db)

	acme, _ := s.CreateCustomer(ctx, tenantID, Customer{Name: "Acme"})
	item := shippedItem(t, s, tenantID, userID, acme.ID, 1, day(1))
	if err := s.SetItemPrice(ctx, tenantID, item, 100); err != nil {
		t.Fatalf("SetItemPrice: %v", err)
	}

	if err := s.SetItemPrice(ctx, otherTenant, item, 1); !errors.Is(err, ErrItemNotFound) {
		t.Errorf("cross-tenant SetItemPrice err = %v, want ErrItemNotFound", err)
	}
	if c, _ := s.PreviewBatch(ctx, otherTenant, 0, day(7), day(0)); len(c) != 0 {
		t.Errorf("other tenant previewed %d candidates", len(c))
	}
	res, err := s.CreateBatch(ctx, tenantID, userID, 0, day(7), day(0))
	if err != nil || len(res.Invoices) != 1 {
		t.Fatalf("CreateBatch = %+v, %v", res, err)
	}
	if _, err := s.GetInvoice(ctx, otherTenant, res.Invoices[0].ID); !errors.Is(err, ErrInvoiceNotFound) {
		t.Errorf("cross-tenant GetInvoice err = %v, want ErrInvoiceNotFound", err)
	}
}
