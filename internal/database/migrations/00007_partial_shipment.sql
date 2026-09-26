-- +goose Up
CREATE TABLE shipment_items (
    shipment_id BIGINT NOT NULL REFERENCES shipments (id) ON DELETE CASCADE,
    print_item_id BIGINT NOT NULL REFERENCES print_items (id) ON DELETE CASCADE,
    PRIMARY KEY (shipment_id, print_item_id)
);

CREATE INDEX shipment_items_print_item_id_idx ON shipment_items (print_item_id);

-- +goose Down
DROP TABLE shipment_items;
