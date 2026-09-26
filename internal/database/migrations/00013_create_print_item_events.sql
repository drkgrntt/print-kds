-- +goose Up
CREATE TABLE print_item_events (
    id BIGSERIAL PRIMARY KEY,
    print_item_id BIGINT NOT NULL REFERENCES print_items (id) ON DELETE CASCADE,
    user_id BIGINT REFERENCES users (id) ON DELETE SET NULL,
    from_status TEXT NOT NULL DEFAULT '',
    to_status TEXT NOT NULL,
    detail TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX print_item_events_print_item_id_idx ON print_item_events (print_item_id, created_at);

-- +goose Down
DROP TABLE print_item_events;
