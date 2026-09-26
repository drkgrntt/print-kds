# Schema

Three tables, three migrations (`internal/database/migrations/`), goose-managed
and applied automatically on startup. No ORM — raw SQL via `database/sql` +
`pgx`.

## `orders`

| Column | Type | Notes |
|---|---|---|
| `id` | `BIGSERIAL PRIMARY KEY` | |
| `order_number` | `TEXT NOT NULL UNIQUE` | customer-facing identifier |
| `customer_name` | `TEXT NOT NULL` | |
| `shipping_address` | `JSONB NOT NULL DEFAULT '{}'` | free-form; no fixed address schema |
| `due_date` | `DATE NOT NULL` | drives the "To Print" column's sort order |
| `status` | `TEXT NOT NULL DEFAULT 'pending'` | `CHECK` restricted to `pending`, `in_progress`, `completed`, `shipped` |
| `created_at` | `TIMESTAMPTZ NOT NULL DEFAULT now()` | |

Indexes: `due_date`, `status`.

## `print_items`

| Column | Type | Notes |
|---|---|---|
| `id` | `BIGSERIAL PRIMARY KEY` | |
| `order_id` | `BIGINT NOT NULL REFERENCES orders (id) ON DELETE CASCADE` | |
| `file_name` | `TEXT NOT NULL` | the 3D model/gcode file this item prints from |
| `color_material` | `TEXT NOT NULL` | e.g. "Black PLA" |
| `quantity_required` | `INTEGER NOT NULL DEFAULT 1` | |
| `quantity_completed` | `INTEGER NOT NULL DEFAULT 0` | |
| `status` | `TEXT NOT NULL DEFAULT 'queued'` | `CHECK` restricted to `queued`, `printing`, `cleaning_qc`, `ready_to_pack`, `packed` — this is the column the KDS board renders |
| `assigned_printer` | `TEXT NOT NULL DEFAULT ''` | which physical printer has/had this job; blank until assigned |
| `created_at` | `TIMESTAMPTZ NOT NULL DEFAULT now()` | |

Indexes: `order_id`, `status`. `ON DELETE CASCADE` — deleting an order
deletes its print items, there's no orphaned-item state to worry about.

## `shipments`

| Column | Type | Notes |
|---|---|---|
| `id` | `BIGSERIAL PRIMARY KEY` | |
| `order_id` | `BIGINT NOT NULL REFERENCES orders (id) ON DELETE CASCADE` | |
| `carrier` | `TEXT NOT NULL DEFAULT ''` | |
| `tracking_number` | `TEXT NOT NULL DEFAULT ''` | |
| `label_url` | `TEXT NOT NULL DEFAULT ''` | |
| `shipped_at` | `TIMESTAMPTZ` | nullable — set when the label is actually generated |

Index: `order_id`. One order can have more than one shipment row (e.g. a
reshipped/split order), so this isn't a 1:1 with `orders` — nothing enforces
uniqueness on `order_id` here.

## Relationships

```
orders (1) ──< print_items (many)
orders (1) ──< shipments   (many)
```

`print_items` has no direct relationship to `shipments` — a shipment is
recorded against the order as a whole, not against individual print items.
If per-item shipment tracking becomes necessary (partial shipments), that's
a schema change, not something the current tables support.
