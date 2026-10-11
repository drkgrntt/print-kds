package board

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

var (
	// ErrItemInvoiced is returned by SetItemPrice for an item already on an
	// invoice - its price is frozen on the invoice line.
	ErrItemInvoiced = errors.New("item already invoiced")
	// ErrItemNotFound is returned when a print item doesn't exist in the
	// caller's tenant.
	ErrItemNotFound = errors.New("print item not found")
	// ErrInvoiceNotFound is returned by GetInvoice for a missing/other
	// tenant's invoice.
	ErrInvoiceNotFound = errors.New("invoice not found")
)

// InvoiceCandidate is one shipped, not-yet-invoiced print item. UnitPriceCents
// is nil until staff set a price (SetItemPrice) - unpriced items are
// reported by PreviewBatch but skipped by CreateBatch.
type InvoiceCandidate struct {
	ItemID         int64
	OrderNumber    string
	CustomerID     int64
	CustomerName   string
	Description    string
	Quantity       int
	UnitPriceCents *int64
	ShippedAt      time.Time
}

// Invoice mirrors an invoices row plus its customer; Lines is filled by
// GetInvoice only.
type Invoice struct {
	ID                     int64
	Number                 int64
	CustomerID             int64
	CustomerName           string
	CustomerEmail          string
	CustomerBillingAddress string
	PeriodStart            time.Time
	PeriodEnd              time.Time
	TotalCents             int64
	CreatedAt              time.Time
	Lines                  []InvoiceLine
}

// InvoiceLine mirrors an invoice_lines row (all snapshot values).
type InvoiceLine struct {
	OrderNumber    string
	Description    string
	Quantity       int
	UnitPriceCents int64
	LineTotalCents int64
	ShippedAt      time.Time

	itemID int64 // set by CreateBatch so lines can be inserted with their item
}

// BatchResult is what CreateBatch did: the invoices it created, and how many
// otherwise-invoiceable items it skipped for having no price yet.
type BatchResult struct {
	Invoices []Invoice
	Unpriced int
}

// candidateQuery selects invoiceable items: packed, with a shipment whose
// shipped_at falls in [from, to] (inclusive dates), on an order linked to a
// customer, and not already on an invoice. Cancelled items are never
// packed, so they're excluded by the status filter. Quantity is
// quantity_completed - what actually passed QC and shipped.
const candidateQuery = `
SELECT pi.id, o.order_number, c.id, c.name,
       pi.file_name || CASE WHEN pi.color_material <> '' THEN ' (' || pi.color_material || ')' ELSE '' END,
       pi.quantity_completed, pi.unit_price_cents, sh.shipped_at
FROM print_items pi
JOIN orders o ON o.id = pi.order_id
JOIN customers c ON c.id = o.customer_id
JOIN LATERAL (
	SELECT max(s.shipped_at) AS shipped_at
	FROM shipment_items si JOIN shipments s ON s.id = si.shipment_id
	WHERE si.print_item_id = pi.id
) sh ON sh.shipped_at IS NOT NULL
WHERE o.tenant_id = $1
  AND ($2 = 0 OR c.id = $2)
  AND pi.status = 'packed'
  AND sh.shipped_at >= $3::date AND sh.shipped_at < ($4::date + 1)
  AND NOT EXISTS (SELECT 1 FROM invoice_lines il WHERE il.print_item_id = pi.id)
ORDER BY c.name, sh.shipped_at, pi.id`

func scanCandidates(rows *sql.Rows) ([]InvoiceCandidate, error) {
	defer rows.Close()
	var out []InvoiceCandidate
	for rows.Next() {
		var c InvoiceCandidate
		var price sql.NullInt64
		if err := rows.Scan(&c.ItemID, &c.OrderNumber, &c.CustomerID, &c.CustomerName,
			&c.Description, &c.Quantity, &price, &c.ShippedAt); err != nil {
			return nil, fmt.Errorf("scan invoice candidate: %w", err)
		}
		if price.Valid {
			p := price.Int64
			c.UnitPriceCents = &p
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// PreviewBatch lists what CreateBatch would invoice for [from, to]
// (customerID 0 = every customer), including items still missing a price.
func (s *Store) PreviewBatch(ctx context.Context, tenantID, customerID int64, from, to time.Time) ([]InvoiceCandidate, error) {
	rows, err := s.db.QueryContext(ctx, candidateQuery, tenantID, customerID, from, to)
	if err != nil {
		return nil, fmt.Errorf("query invoice candidates: %w", err)
	}
	return scanCandidates(rows)
}

// SetItemPrice sets a print item's unit price (cents), tenant-scoped via
// its order. An item already on an invoice is refused (ErrItemInvoiced).
func (s *Store) SetItemPrice(ctx context.Context, tenantID, itemID, cents int64) error {
	if cents < 0 {
		return fmt.Errorf("price must not be negative")
	}
	res, err := s.db.ExecContext(ctx, `
UPDATE print_items pi SET unit_price_cents = $3
FROM orders o
WHERE pi.id = $1 AND o.id = pi.order_id AND o.tenant_id = $2
  AND NOT EXISTS (SELECT 1 FROM invoice_lines il WHERE il.print_item_id = pi.id)`,
		itemID, tenantID, cents)
	if err != nil {
		return fmt.Errorf("set price for item %d: %w", itemID, err)
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return nil
	}
	var exists, invoiced bool
	if err := s.db.QueryRowContext(ctx, `
SELECT true, EXISTS (SELECT 1 FROM invoice_lines il WHERE il.print_item_id = pi.id)
FROM print_items pi JOIN orders o ON o.id = pi.order_id
WHERE pi.id = $1 AND o.tenant_id = $2`, itemID, tenantID).Scan(&exists, &invoiced); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrItemNotFound
		}
		return fmt.Errorf("check item %d: %w", itemID, err)
	}
	return ErrItemInvoiced
}

// CreateBatch invoices every priced, shipped, not-yet-invoiced item in
// [from, to] - one invoice per customer (customerID 0 = every customer) -
// in a single transaction. Items without a price are skipped and counted in
// BatchResult.Unpriced so they stay available for a later batch. Candidate
// rows are locked FOR UPDATE and invoice_lines.print_item_id is UNIQUE, so
// two concurrent submissions can never invoice an item twice (the loser
// fails and rolls back).
func (s *Store) CreateBatch(ctx context.Context, tenantID, userID, customerID int64, from, to time.Time) (BatchResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return BatchResult{}, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, candidateQuery+" FOR UPDATE OF pi", tenantID, customerID, from, to)
	if err != nil {
		return BatchResult{}, fmt.Errorf("query invoice candidates: %w", err)
	}
	cands, err := scanCandidates(rows)
	if err != nil {
		return BatchResult{}, err
	}

	var result BatchResult
	byCustomer := map[int64]*Invoice{}
	var order []int64
	for _, c := range cands {
		if c.UnitPriceCents == nil {
			result.Unpriced++
			continue
		}
		inv, ok := byCustomer[c.CustomerID]
		if !ok {
			inv = &Invoice{CustomerID: c.CustomerID, CustomerName: c.CustomerName, PeriodStart: from, PeriodEnd: to}
			byCustomer[c.CustomerID] = inv
			order = append(order, c.CustomerID)
		}
		line := InvoiceLine{
			OrderNumber:    c.OrderNumber,
			Description:    c.Description,
			Quantity:       c.Quantity,
			UnitPriceCents: *c.UnitPriceCents,
			LineTotalCents: int64(c.Quantity) * *c.UnitPriceCents,
			ShippedAt:      c.ShippedAt,
			itemID:         c.ItemID,
		}
		inv.Lines = append(inv.Lines, line)
		inv.TotalCents += line.LineTotalCents
	}

	for _, custID := range order {
		inv := byCustomer[custID]
		if err := tx.QueryRowContext(ctx,
			`UPDATE tenants SET next_invoice_number = next_invoice_number + 1 WHERE id = $1 RETURNING next_invoice_number - 1`,
			tenantID).Scan(&inv.Number); err != nil {
			return BatchResult{}, fmt.Errorf("allocate invoice number: %w", err)
		}
		if err := tx.QueryRowContext(ctx, `
INSERT INTO invoices (tenant_id, customer_id, number, period_start, period_end, total_cents, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING id, created_at`,
			tenantID, custID, inv.Number, from, to, inv.TotalCents, userID,
		).Scan(&inv.ID, &inv.CreatedAt); err != nil {
			return BatchResult{}, fmt.Errorf("insert invoice: %w", err)
		}
		for _, line := range inv.Lines {
			if _, err := tx.ExecContext(ctx, `
INSERT INTO invoice_lines (invoice_id, print_item_id, order_number, description, quantity, unit_price_cents, line_total_cents, shipped_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
				inv.ID, line.itemID, line.OrderNumber, line.Description, line.Quantity,
				line.UnitPriceCents, line.LineTotalCents, line.ShippedAt,
			); err != nil {
				var pgErr *pgconn.PgError
				if errors.As(err, &pgErr) && pgErr.Code == "23505" {
					return BatchResult{}, ErrItemInvoiced
				}
				return BatchResult{}, fmt.Errorf("insert invoice line: %w", err)
			}
		}
		result.Invoices = append(result.Invoices, *inv)
	}

	if err := tx.Commit(); err != nil {
		return BatchResult{}, fmt.Errorf("commit invoice batch: %w", err)
	}
	return result, nil
}

// ListInvoices returns tenantID's invoices (no lines), newest first.
func (s *Store) ListInvoices(ctx context.Context, tenantID int64) ([]Invoice, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT i.id, i.number, i.customer_id, c.name, i.period_start, i.period_end, i.total_cents, i.created_at
FROM invoices i JOIN customers c ON c.id = i.customer_id
WHERE i.tenant_id = $1
ORDER BY i.number DESC`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("query invoices: %w", err)
	}
	defer rows.Close()

	var out []Invoice
	for rows.Next() {
		var inv Invoice
		if err := rows.Scan(&inv.ID, &inv.Number, &inv.CustomerID, &inv.CustomerName,
			&inv.PeriodStart, &inv.PeriodEnd, &inv.TotalCents, &inv.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan invoice: %w", err)
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

// GetInvoice loads one invoice with its lines, tenant-scoped.
func (s *Store) GetInvoice(ctx context.Context, tenantID, invoiceID int64) (Invoice, error) {
	var inv Invoice
	err := s.db.QueryRowContext(ctx, `
SELECT i.id, i.number, i.customer_id, c.name, c.email, c.billing_address,
       i.period_start, i.period_end, i.total_cents, i.created_at
FROM invoices i JOIN customers c ON c.id = i.customer_id
WHERE i.id = $1 AND i.tenant_id = $2`, invoiceID, tenantID,
	).Scan(&inv.ID, &inv.Number, &inv.CustomerID, &inv.CustomerName, &inv.CustomerEmail,
		&inv.CustomerBillingAddress, &inv.PeriodStart, &inv.PeriodEnd, &inv.TotalCents, &inv.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Invoice{}, ErrInvoiceNotFound
		}
		return Invoice{}, fmt.Errorf("load invoice %d: %w", invoiceID, err)
	}

	rows, err := s.db.QueryContext(ctx, `
SELECT order_number, description, quantity, unit_price_cents, line_total_cents, COALESCE(shipped_at, 'epoch')
FROM invoice_lines WHERE invoice_id = $1 ORDER BY id`, invoiceID)
	if err != nil {
		return Invoice{}, fmt.Errorf("query invoice lines: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var l InvoiceLine
		if err := rows.Scan(&l.OrderNumber, &l.Description, &l.Quantity, &l.UnitPriceCents, &l.LineTotalCents, &l.ShippedAt); err != nil {
			return Invoice{}, fmt.Errorf("scan invoice line: %w", err)
		}
		inv.Lines = append(inv.Lines, l)
	}
	return inv, rows.Err()
}
