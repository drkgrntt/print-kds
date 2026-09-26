# Schema

Four tables, seven migrations (`internal/database/migrations/`), goose-managed
and applied automatically on startup. No ORM — raw SQL via `database/sql` +
`pgx`.

## `orders`

| Column | Type | Notes |
|---|---|---|
| `id` | `BIGSERIAL PRIMARY KEY` | |
| `order_number` | `TEXT NOT NULL UNIQUE` | customer-facing identifier |
| `customer_name` | `TEXT NOT NULL` | |
| `shipping_address` | `JSONB NOT NULL DEFAULT '{}'` | free-form; no fixed address schema |
| `due_date` | `DATE NOT NULL` | drives the "To Print" column's sort order within the reprint-priority grouping (see `print_items.reprint_count` and `board.Store.BoardColumns`) |
| `status` | `TEXT NOT NULL DEFAULT 'pending'` | `CHECK` restricted to `pending`, `in_progress`, `completed`, `shipped`, `cancelled` — derived from its items' statuses, see `board.Store.CancelItem`'s order-cancellation cascade in `docs/architecture.md` |
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
| `status` | `TEXT NOT NULL DEFAULT 'queued'` | `CHECK` restricted to `queued`, `printing`, `cleaning_qc`, `ready_to_pack`, `packed`, `cancelled` — this is the column the KDS board renders; `cancelled` is terminal like `packed` and drops off the live board (see `board.Store.CancelItem`) |
| `assigned_printer` | `TEXT NOT NULL DEFAULT ''` | which physical printer has/had this job; plain free text (e.g. "Printer 3"), no printers table to validate against; set (optionally) by `StartPrint` when an item leaves `queued`, cleared back to `''` by `FailPrint` if the print fails mid-job; blank until assigned |
| `qc_failure_reason` | `TEXT NOT NULL DEFAULT ''` | free-form note on why this attempt didn't produce a usable print; set by `RecordQCResult` whenever `quantity_passed < quantity_required` (blank on an ordinary full pass) or by `FailPrint` when a print fails mid-job |
| `reprint_count` | `INTEGER NOT NULL DEFAULT 0` | how many times this row (or, for a split row, its original) has failed to produce a usable print; incremented by `RecordQCResult` on a full or partial QC failure, or by `FailPrint` on a mid-print failure |
| `created_at` | `TIMESTAMPTZ NOT NULL DEFAULT now()` | |
| `status_changed_at` | `TIMESTAMPTZ NOT NULL DEFAULT now()` | time of this row's most recent status transition; overwritten to `now()` by every transition method (`StartPrint`, `PopOffBed`, `FailPrint`, `RecordQCResult`, `GenerateLabel`, `CancelItem`) and by `UpdateQuantity`'s same-status edit, so `now() - status_changed_at` shows how long a card has been sitting in its current board column - not a full transition history, just the latest move |

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

Index: `order_id`. One order can have more than one shipment row — in fact
every `GENERATE LABEL` tap now creates its own new `shipments` row for just
the one item packed (see `shipment_items` below and
`board.Store.GenerateLabel`), so a multi-item order routinely ends up with
several — nothing enforces uniqueness on `order_id` here.

## `shipment_items`

| Column | Type | Notes |
|---|---|---|
| `shipment_id` | `BIGINT NOT NULL REFERENCES shipments (id) ON DELETE CASCADE` | part of the composite PK |
| `print_item_id` | `BIGINT NOT NULL REFERENCES print_items (id) ON DELETE CASCADE` | part of the composite PK |

Primary key: `(shipment_id, print_item_id)`. Index: `print_item_id`. Join
table recording which specific `print_items` rows went out on which
`shipments` row — this is what makes partial shipments possible: `orders`
no longer has to be all-or-nothing before anything ships. In practice a
given `print_item_id` never appears in more than one row here (an item
ships once), but that isn't enforced beyond the composite PK — there's no
extra uniqueness constraint on `print_item_id` alone.

## Relationships

```
orders (1)    ──< print_items    (many)
orders (1)    ──< shipments      (many)
shipments (1) ──< shipment_items (many) >── (1) print_items
```

`shipments` and `print_items` are linked many-to-many through
`shipment_items`: `board.Store.GenerateLabel` creates one `shipments` row
per packed item (plus its linking `shipment_items` row) rather than one
`shipments` row per fully-packed order, so an order can ship in pieces as
each item becomes ready instead of waiting on every sibling item to reach
`packed` first.
