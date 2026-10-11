package board

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"
)

func newCustomerOrder(t *testing.T, s *Store, tenantID, userID, customerID int64, files ...NewOrderFile) int64 {
	t.Helper()
	id, err := s.CreateOrder(context.Background(), tenantID, userID, NewOrder{
		OrderNumber: fmt.Sprintf("CUST-%s-%d", t.Name(), time.Now().UnixNano()),
		CustomerID:  customerID,
		DueDate:     time.Now().Add(48 * time.Hour),
		Items:       []NewOrderItem{{FileName: "a.stl", ColorMaterial: "Red", QuantityRequired: 1}},
		Files:       files,
	})
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	return id
}

func TestCustomerScoping(t *testing.T) {
	s, tenantID, userID := testStore(t)
	ctx := context.Background()

	acme, err := s.CreateCustomer(ctx, tenantID, Customer{Name: "Acme"})
	if err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}
	globex, err := s.CreateCustomer(ctx, tenantID, Customer{Name: "Globex"})
	if err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}
	if _, err := s.CreateCustomer(ctx, tenantID, Customer{Name: "Acme"}); !errors.Is(err, ErrDuplicateCustomer) {
		t.Errorf("duplicate customer err = %v, want ErrDuplicateCustomer", err)
	}

	acmeOrder := newCustomerOrder(t, s, tenantID, userID, acme.ID,
		NewOrderFile{Filename: "a.stl", StoredPath: "/tmp/a", SizeBytes: 1},
		NewOrderFile{Filename: "b.stl", StoredPath: "/tmp/b", SizeBytes: 2})
	newCustomerOrder(t, s, tenantID, userID, globex.ID)

	orders, err := s.ListCustomerOrders(ctx, tenantID, acme.ID)
	if err != nil {
		t.Fatalf("ListCustomerOrders: %v", err)
	}
	if len(orders) != 1 || orders[0].ID != acmeOrder || orders[0].FileCount != 2 || orders[0].ItemCount != 1 {
		t.Fatalf("acme orders = %+v, want only its own order with 2 files", orders)
	}

	files, err := s.ListOrderFiles(ctx, tenantID, acme.ID, acmeOrder)
	if err != nil || len(files) != 2 {
		t.Fatalf("ListOrderFiles = %v, %v; want 2 files", files, err)
	}
	// Another customer in the same tenant can't see Acme's files.
	if got, _ := s.ListOrderFiles(ctx, tenantID, globex.ID, acmeOrder); len(got) != 0 {
		t.Errorf("globex saw acme files: %v", got)
	}
	if _, err := s.GetOrderFile(ctx, tenantID, globex.ID, files[0].ID); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("globex GetOrderFile err = %v, want ErrNoRows", err)
	}
	// Staff (customerID 0) can, but only within their own tenant.
	if _, err := s.GetOrderFile(ctx, tenantID, 0, files[0].ID); err != nil {
		t.Errorf("staff GetOrderFile: %v", err)
	}
	otherTenant := testTenantID(t, s.db)
	if _, err := s.GetOrderFile(ctx, otherTenant, 0, files[0].ID); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("cross-tenant GetOrderFile err = %v, want ErrNoRows", err)
	}
}

func TestCreateOrder_CustomerFromOtherTenantRejected(t *testing.T) {
	s, tenantA, userA := testStore(t)
	tenantB := testTenantID(t, s.db)
	ctx := context.Background()

	cust, err := s.CreateCustomer(ctx, tenantB, Customer{Name: "B's customer"})
	if err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}
	_, err = s.CreateOrder(ctx, tenantA, userA, NewOrder{
		OrderNumber: fmt.Sprintf("X-%d", time.Now().UnixNano()),
		CustomerID:  cust.ID,
		DueDate:     time.Now(),
		Items:       []NewOrderItem{{FileName: "a.stl", QuantityRequired: 1}},
	})
	if !errors.Is(err, ErrCustomerNotFound) {
		t.Errorf("err = %v, want ErrCustomerNotFound", err)
	}
}

func TestShipToRoundTrip(t *testing.T) {
	s, tenantID, userID := testStore(t)
	ctx := context.Background()
	want := ShipTo{Name: "Alice", Line1: "1 Main St", Line2: "Apt 2", City: "Austin", State: "TX", PostalCode: "78701"}

	if _, err := s.CreateOrder(ctx, tenantID, userID, NewOrder{
		OrderNumber:  fmt.Sprintf("SHIP-%d", time.Now().UnixNano()),
		CustomerName: "Alice",
		DueDate:      time.Now().Add(24 * time.Hour),
		ShipTo:       want,
		Items:        []NewOrderItem{{FileName: "a.stl", QuantityRequired: 1}},
	}); err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	cols, err := s.BoardColumns(ctx, tenantID)
	if err != nil || len(cols["queued"]) != 1 {
		t.Fatalf("BoardColumns = %v, %v", cols, err)
	}
	got := cols["queued"][0].ShipTo
	if got != want {
		t.Errorf("ShipTo = %+v, want %+v", got, want)
	}
	if lines := got.Lines(); len(lines) != 4 || lines[3] != "Austin, TX 78701" {
		t.Errorf("Lines = %q", lines)
	}
	if (ShipTo{}).MissingField() != "recipient name" || !(ShipTo{}).IsEmpty() {
		t.Error("zero ShipTo should be empty and missing its name")
	}
}
