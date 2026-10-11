-- +goose Up
CREATE TABLE customers (
    id BIGSERIAL PRIMARY KEY,
    tenant_id BIGINT NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    billing_address TEXT NOT NULL DEFAULT '',
    email TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT customers_tenant_id_name_key UNIQUE (tenant_id, name)
);

CREATE INDEX customers_tenant_id_idx ON customers (tenant_id);

ALTER TABLE users ADD COLUMN customer_id BIGINT REFERENCES customers (id) ON DELETE CASCADE;
ALTER TABLE users DROP CONSTRAINT users_role_check;
ALTER TABLE users ADD CONSTRAINT users_role_check CHECK (role IN ('admin', 'staff', 'client'));
ALTER TABLE users ADD CONSTRAINT users_client_customer_check CHECK ((role = 'client') = (customer_id IS NOT NULL));
CREATE INDEX users_customer_id_idx ON users (customer_id);

ALTER TABLE orders ADD COLUMN customer_id BIGINT REFERENCES customers (id) ON DELETE SET NULL;
CREATE INDEX orders_customer_id_idx ON orders (customer_id);

-- Backfill: one customer per distinct (tenant, free-text customer_name),
-- then link existing orders to it. customer_name stays for display.
INSERT INTO customers (tenant_id, name)
SELECT DISTINCT tenant_id, customer_name FROM orders WHERE customer_name <> '';

UPDATE orders o SET customer_id = c.id
FROM customers c
WHERE c.tenant_id = o.tenant_id AND c.name = o.customer_name;

CREATE TABLE order_files (
    id BIGSERIAL PRIMARY KEY,
    order_id BIGINT NOT NULL REFERENCES orders (id) ON DELETE CASCADE,
    filename TEXT NOT NULL,
    stored_path TEXT NOT NULL,
    size_bytes BIGINT NOT NULL,
    content_type TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX order_files_order_id_idx ON order_files (order_id);

-- +goose Down
DROP TABLE order_files;
DROP INDEX orders_customer_id_idx;
ALTER TABLE orders DROP COLUMN customer_id;
DELETE FROM users WHERE role = 'client';
DROP INDEX users_customer_id_idx;
ALTER TABLE users DROP CONSTRAINT users_client_customer_check;
ALTER TABLE users DROP CONSTRAINT users_role_check;
ALTER TABLE users ADD CONSTRAINT users_role_check CHECK (role IN ('admin', 'staff'));
ALTER TABLE users DROP COLUMN customer_id;
DROP TABLE customers;
