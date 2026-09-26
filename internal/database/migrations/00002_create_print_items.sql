-- +goose Up
CREATE TABLE print_items (
    id BIGSERIAL PRIMARY KEY,
    order_id BIGINT NOT NULL REFERENCES orders (id) ON DELETE CASCADE,
    file_name TEXT NOT NULL,
    color_material TEXT NOT NULL,
    quantity_required INTEGER NOT NULL DEFAULT 1,
    quantity_completed INTEGER NOT NULL DEFAULT 0,
    status TEXT NOT NULL DEFAULT 'queued'
        CHECK (status IN ('queued', 'printing', 'cleaning_qc', 'ready_to_pack', 'packed')),
    assigned_printer TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX print_items_order_id_idx ON print_items (order_id);
CREATE INDEX print_items_status_idx ON print_items (status);

-- +goose Down
DROP TABLE print_items;
