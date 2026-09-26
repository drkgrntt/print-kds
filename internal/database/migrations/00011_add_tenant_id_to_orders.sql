-- +goose Up
INSERT INTO tenants (name) VALUES ('Default Shop');

ALTER TABLE orders ADD COLUMN tenant_id BIGINT REFERENCES tenants (id) ON DELETE CASCADE;
UPDATE orders SET tenant_id = (SELECT id FROM tenants WHERE name = 'Default Shop');
ALTER TABLE orders ALTER COLUMN tenant_id SET NOT NULL;
CREATE INDEX orders_tenant_id_idx ON orders (tenant_id);

ALTER TABLE orders DROP CONSTRAINT orders_order_number_key;
ALTER TABLE orders ADD CONSTRAINT orders_tenant_id_order_number_key UNIQUE (tenant_id, order_number);

-- +goose Down
ALTER TABLE orders DROP CONSTRAINT orders_tenant_id_order_number_key;
ALTER TABLE orders ADD CONSTRAINT orders_order_number_key UNIQUE (order_number);
DROP INDEX orders_tenant_id_idx;
ALTER TABLE orders DROP COLUMN tenant_id;
DELETE FROM tenants WHERE name = 'Default Shop';
