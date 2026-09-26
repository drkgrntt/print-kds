-- +goose Up
ALTER TABLE print_items ADD COLUMN last_updated_by BIGINT REFERENCES users (id) ON DELETE SET NULL;

-- +goose Down
ALTER TABLE print_items DROP COLUMN last_updated_by;
