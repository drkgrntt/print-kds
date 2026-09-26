-- +goose Up
ALTER TABLE print_items DROP CONSTRAINT print_items_status_check;
ALTER TABLE print_items ADD CONSTRAINT print_items_status_check
    CHECK (status IN ('queued', 'printing', 'cleaning_qc', 'ready_to_pack', 'packed', 'cancelled'));

ALTER TABLE orders DROP CONSTRAINT orders_status_check;
ALTER TABLE orders ADD CONSTRAINT orders_status_check
    CHECK (status IN ('pending', 'in_progress', 'completed', 'shipped', 'cancelled'));

-- +goose Down
ALTER TABLE print_items DROP CONSTRAINT print_items_status_check;
ALTER TABLE print_items ADD CONSTRAINT print_items_status_check
    CHECK (status IN ('queued', 'printing', 'cleaning_qc', 'ready_to_pack', 'packed'));

ALTER TABLE orders DROP CONSTRAINT orders_status_check;
ALTER TABLE orders ADD CONSTRAINT orders_status_check
    CHECK (status IN ('pending', 'in_progress', 'completed', 'shipped'));
