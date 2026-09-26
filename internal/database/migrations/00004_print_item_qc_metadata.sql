-- +goose Up
ALTER TABLE print_items ADD COLUMN qc_failure_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE print_items ADD COLUMN reprint_count INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE print_items DROP COLUMN qc_failure_reason;
ALTER TABLE print_items DROP COLUMN reprint_count;
