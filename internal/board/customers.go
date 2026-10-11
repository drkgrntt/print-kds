package board

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// ErrDuplicateCustomer is returned by CreateCustomer when the tenant
// already has a customer with that name.
var ErrDuplicateCustomer = errors.New("customer already exists")

// Customer mirrors a customers row: a shop's client company, which client
// users log in on behalf of and which invoices are billed to.
type Customer struct {
	ID             int64
	Name           string
	BillingAddress string
	Email          string
}

// CustomerOrder is one row of a client's portal order list.
type CustomerOrder struct {
	ID          int64
	OrderNumber string
	Status      string
	DueDate     time.Time
	CreatedAt   time.Time
	FileCount   int
	ItemCount   int
}

// OrderFile is an uploaded file's metadata; StoredPath is where the bytes
// live on disk (never served directly - see the portal/admin file routes).
type OrderFile struct {
	ID          int64
	OrderID     int64
	Filename    string
	StoredPath  string
	SizeBytes   int64
	ContentType string
}

// ListCustomers returns tenantID's customers ordered by name.
func (s *Store) ListCustomers(ctx context.Context, tenantID int64) ([]Customer, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, name, billing_address, email FROM customers WHERE tenant_id = $1 ORDER BY name`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("query customers for tenant %d: %w", tenantID, err)
	}
	defer rows.Close()

	var out []Customer
	for rows.Next() {
		var c Customer
		if err := rows.Scan(&c.ID, &c.Name, &c.BillingAddress, &c.Email); err != nil {
			return nil, fmt.Errorf("scan customer: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CreateCustomer adds a customer to tenantID.
func (s *Store) CreateCustomer(ctx context.Context, tenantID int64, c Customer) (Customer, error) {
	c.Name = strings.TrimSpace(c.Name)
	err := s.db.QueryRowContext(ctx, `
INSERT INTO customers (tenant_id, name, billing_address, email)
VALUES ($1, $2, $3, $4)
RETURNING id`, tenantID, c.Name, c.BillingAddress, c.Email).Scan(&c.ID)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return Customer{}, fmt.Errorf("%w: %q", ErrDuplicateCustomer, c.Name)
		}
		return Customer{}, fmt.Errorf("insert customer: %w", err)
	}
	return c, nil
}

// ListCustomerOrders returns a customer's orders, newest first. Both
// tenantID and customerID come from the session, never from the request.
func (s *Store) ListCustomerOrders(ctx context.Context, tenantID, customerID int64) ([]CustomerOrder, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT o.id, o.order_number, o.status, o.due_date, o.created_at,
       (SELECT count(*) FROM order_files f WHERE f.order_id = o.id),
       (SELECT count(*) FROM print_items p WHERE p.order_id = o.id)
FROM orders o
WHERE o.tenant_id = $1 AND o.customer_id = $2
ORDER BY o.created_at DESC, o.id DESC`, tenantID, customerID)
	if err != nil {
		return nil, fmt.Errorf("query orders for customer %d: %w", customerID, err)
	}
	defer rows.Close()

	var out []CustomerOrder
	for rows.Next() {
		var o CustomerOrder
		if err := rows.Scan(&o.ID, &o.OrderNumber, &o.Status, &o.DueDate, &o.CreatedAt, &o.FileCount, &o.ItemCount); err != nil {
			return nil, fmt.Errorf("scan customer order: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// GetOrderFile loads a file's metadata, tenant-scoped via its order. When
// customerID is non-zero (a client download) the order must also belong to
// that customer; staff/admin pass 0. A miss is sql.ErrNoRows.
func (s *Store) GetOrderFile(ctx context.Context, tenantID, customerID, fileID int64) (OrderFile, error) {
	var f OrderFile
	err := s.db.QueryRowContext(ctx, `
SELECT f.id, f.order_id, f.filename, f.stored_path, f.size_bytes, f.content_type
FROM order_files f
JOIN orders o ON o.id = f.order_id
WHERE f.id = $1 AND o.tenant_id = $2 AND ($3 = 0 OR o.customer_id = $3)`,
		fileID, tenantID, customerID,
	).Scan(&f.ID, &f.OrderID, &f.Filename, &f.StoredPath, &f.SizeBytes, &f.ContentType)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return OrderFile{}, sql.ErrNoRows
		}
		return OrderFile{}, fmt.Errorf("load order file %d: %w", fileID, err)
	}
	return f, nil
}

// ListOrderFiles returns an order's files, tenant-scoped (and
// customer-scoped when customerID is non-zero).
func (s *Store) ListOrderFiles(ctx context.Context, tenantID, customerID, orderID int64) ([]OrderFile, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT f.id, f.order_id, f.filename, f.stored_path, f.size_bytes, f.content_type
FROM order_files f
JOIN orders o ON o.id = f.order_id
WHERE f.order_id = $1 AND o.tenant_id = $2 AND ($3 = 0 OR o.customer_id = $3)
ORDER BY f.id`, orderID, tenantID, customerID)
	if err != nil {
		return nil, fmt.Errorf("query files for order %d: %w", orderID, err)
	}
	defer rows.Close()

	var out []OrderFile
	for rows.Next() {
		var f OrderFile
		if err := rows.Scan(&f.ID, &f.OrderID, &f.Filename, &f.StoredPath, &f.SizeBytes, &f.ContentType); err != nil {
			return nil, fmt.Errorf("scan order file: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
