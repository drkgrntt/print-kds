-- +goose Up
ALTER TABLE print_items ADD COLUMN unit_price_cents BIGINT CHECK (unit_price_cents >= 0);

-- Per-tenant invoice numbering: bumped with UPDATE ... RETURNING inside
-- the invoice-creating transaction, so concurrent batches serialize on
-- the tenant row and never hand out the same number.
ALTER TABLE tenants ADD COLUMN next_invoice_number BIGINT NOT NULL DEFAULT 1;

CREATE TABLE invoices (
    id BIGSERIAL PRIMARY KEY,
    tenant_id BIGINT NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    customer_id BIGINT NOT NULL REFERENCES customers (id) ON DELETE CASCADE,
    number BIGINT NOT NULL,
    period_start DATE NOT NULL,
    period_end DATE NOT NULL,
    total_cents BIGINT NOT NULL,
    created_by BIGINT REFERENCES users (id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT invoices_tenant_id_number_key UNIQUE (tenant_id, number)
);

CREATE INDEX invoices_customer_id_idx ON invoices (customer_id);

-- UNIQUE (print_item_id) is what guarantees an item is invoiced exactly
-- once; description/unit price/quantity are snapshots so later edits to
-- the item never rewrite a past invoice.
CREATE TABLE invoice_lines (
    id BIGSERIAL PRIMARY KEY,
    invoice_id BIGINT NOT NULL REFERENCES invoices (id) ON DELETE CASCADE,
    print_item_id BIGINT NOT NULL UNIQUE REFERENCES print_items (id) ON DELETE CASCADE,
    order_number TEXT NOT NULL,
    description TEXT NOT NULL,
    quantity INT NOT NULL,
    unit_price_cents BIGINT NOT NULL,
    line_total_cents BIGINT NOT NULL,
    shipped_at TIMESTAMPTZ
);

CREATE INDEX invoice_lines_invoice_id_idx ON invoice_lines (invoice_id);

-- +goose Down
DROP TABLE invoice_lines;
DROP TABLE invoices;
ALTER TABLE tenants DROP COLUMN next_invoice_number;
ALTER TABLE print_items DROP COLUMN unit_price_cents;
