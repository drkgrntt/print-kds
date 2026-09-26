-- +goose Up
CREATE TABLE orders (
    id BIGSERIAL PRIMARY KEY,
    order_number TEXT NOT NULL UNIQUE,
    customer_name TEXT NOT NULL,
    shipping_address JSONB NOT NULL DEFAULT '{}',
    due_date DATE NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'in_progress', 'completed', 'shipped')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX orders_due_date_idx ON orders (due_date);
CREATE INDEX orders_status_idx ON orders (status);

-- +goose Down
DROP TABLE orders;
