-- +goose Up
CREATE TABLE shipments (
    id BIGSERIAL PRIMARY KEY,
    order_id BIGINT NOT NULL REFERENCES orders (id) ON DELETE CASCADE,
    carrier TEXT NOT NULL DEFAULT '',
    tracking_number TEXT NOT NULL DEFAULT '',
    label_url TEXT NOT NULL DEFAULT '',
    shipped_at TIMESTAMPTZ
);

CREATE INDEX shipments_order_id_idx ON shipments (order_id);

-- +goose Down
DROP TABLE shipments;
