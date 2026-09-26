-- +goose Up
ALTER TABLE print_items ADD COLUMN status_changed_at TIMESTAMPTZ NOT NULL DEFAULT now();

-- +goose Down
ALTER TABLE print_items DROP COLUMN status_changed_at;
