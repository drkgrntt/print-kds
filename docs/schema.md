# Schema

Twelve tables, fifteen migrations (`internal/database/migrations/`),
goose-managed and applied automatically on startup. No ORM — raw SQL via
`database/sql` + `pgx`.

## `tenants`

| Column | Type | Notes |
|---|---|---|
| `id` | `BIGSERIAL PRIMARY KEY` | |
| `name` | `TEXT NOT NULL` | shop display name, shown in the app bar |
| `next_invoice_number` | `BIGINT NOT NULL DEFAULT 1` | per-tenant invoice counter; `CreateBatch` bumps it with `UPDATE ... RETURNING` inside its transaction, so concurrent batches serialize on the tenant row and never reuse a number |
| `created_at` | `TIMESTAMPTZ NOT NULL DEFAULT now()` | |

No slug/subdomain column — tenant resolution is login-based (see
`docs/architecture.md`), never derived from the URL.

## `users`

| Column | Type | Notes |
|---|---|---|
| `id` | `BIGSERIAL PRIMARY KEY` | |
| `tenant_id` | `BIGINT NOT NULL REFERENCES tenants (id) ON DELETE CASCADE` | one user belongs to exactly one tenant |
| `email` | `TEXT NOT NULL UNIQUE` | globally unique — login is by email, not scoped per shop |
| `password_hash` | `TEXT NOT NULL` | bcrypt (`auth.Store.SignUp`/`CreateUser`) |
| `role` | `TEXT NOT NULL DEFAULT 'staff'` | `CHECK` restricted to `admin`, `staff`, `client` — `admin` can create orders (`/admin/new-order`), manage customers and users; `staff` can operate the board (every print-item transition) but not those; `client` is a customer's portal login (`/portal`) with no board access |
| `customer_id` | `BIGINT REFERENCES customers (id) ON DELETE CASCADE` | set only for `client` users (`CHECK ((role = 'client') = (customer_id IS NOT NULL))`); `auth.Store.CreateClientUser` verifies the customer belongs to the same tenant |
| `created_at` | `TIMESTAMPTZ NOT NULL DEFAULT now()` | |

Index: `tenant_id`.

## `sessions`

| Column | Type | Notes |
|---|---|---|
| `token_hash` | `TEXT PRIMARY KEY` | SHA-256 hex digest of the session cookie's random token — the raw token is never stored, only ever held by the browser |
| `user_id` | `BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE` | |
| `tenant_id` | `BIGINT NOT NULL REFERENCES tenants (id) ON DELETE CASCADE` | denormalized from `user_id`'s tenant — every authenticated request pays this lookup, so it's read directly rather than joined through `users` every time |
| `created_at` | `TIMESTAMPTZ NOT NULL DEFAULT now()` | |
| `expires_at` | `TIMESTAMPTZ NOT NULL` | fixed 30-day TTL from creation (`auth.Store.CreateSession`), no sliding refresh |

Index: `expires_at` (for a future expired-session cleanup job — none
scheduled yet). `role` is deliberately *not* stored here — see
`auth.Store.ValidateSession`, which joins to `users` fresh on every
lookup so a role change takes effect on the next request rather than
waiting for the session to be re-created.

## `customers`

| Column | Type | Notes |
|---|---|---|
| `id` | `BIGSERIAL PRIMARY KEY` | |
| `tenant_id` | `BIGINT NOT NULL REFERENCES tenants (id) ON DELETE CASCADE` | |
| `name` | `TEXT NOT NULL` | `UNIQUE (tenant_id, name)` |
| `billing_address` | `TEXT NOT NULL DEFAULT ''` | |
| `email` | `TEXT NOT NULL DEFAULT ''` | billing email |
| `created_at` | `TIMESTAMPTZ NOT NULL DEFAULT now()` | |

A shop's client company. Client users (`users.customer_id`) log in on its
behalf and see only orders with a matching `orders.customer_id`.

## `orders`

| Column | Type | Notes |
|---|---|---|
| `id` | `BIGSERIAL PRIMARY KEY` | |
| `tenant_id` | `BIGINT NOT NULL REFERENCES tenants (id) ON DELETE CASCADE` | added by `00011_add_tenant_id_to_orders.sql`, backfilled onto a "Default Shop" tenant for any order that predates multi-tenancy |
| `order_number` | `TEXT NOT NULL` | customer-facing identifier; unique *per tenant* (see below), not globally |
| `customer_id` | `BIGINT REFERENCES customers (id) ON DELETE SET NULL` | nullable: orders typed in with a one-off name have no customer link. Added by `00014`, which backfilled one customer per distinct `(tenant_id, customer_name)` |
| `customer_name` | `TEXT NOT NULL` | display name; copied from the customer when `customer_id` is set |
| `shipping_address` | `JSONB NOT NULL DEFAULT '{}'` | the order's ship-to address as a JSON object (`board.ShipTo`: `name`, `line1`, `line2`, `city`, `state`, `postal_code`, `country`). Required (name, street, city, state, postal code) on both order forms; `'{}'` on orders that predate it, which the board simply doesn't show an address for |
| `due_date` | `DATE NOT NULL` | the order's **ship-by date** (labelled that way in the UI); drives the "To Print" column's sort order within the reprint-priority grouping (see `print_items.reprint_count` and `board.Store.BoardColumns`) |
| `status` | `TEXT NOT NULL DEFAULT 'pending'` | `CHECK` restricted to `pending`, `in_progress`, `completed`, `shipped`, `cancelled` — derived from its items' statuses, see `board.Store.CancelItem`'s order-cancellation cascade in `docs/architecture.md` |
| `created_at` | `TIMESTAMPTZ NOT NULL DEFAULT now()` | |

Indexes: `tenant_id`, `due_date`, `status`. `UNIQUE (tenant_id,
order_number)` (replacing the original global `UNIQUE (order_number)` —
otherwise one shop taking "ORD-1001" would permanently block every other
shop from ever using it).

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
| `unit_price_cents` | `BIGINT CHECK (unit_price_cents >= 0)` | nullable; set by staff from the invoice preview (`board.Store.SetItemPrice`), which refuses once the item is on an invoice. Items without a price are skipped by `CreateBatch`. Currency is assumed USD |
| `last_updated_by` | `BIGINT REFERENCES users (id) ON DELETE SET NULL` | who most recently ran a transition on this row (or created it, via `CreateOrder`); nullable and `ON DELETE SET NULL` so a departed user's historical items don't break or cascade-delete. Surfaced on `PrintItem` as `LastUpdatedByEmail` via a `LEFT JOIN users`; see `print_item_events` below for the full history, not just the latest touch |

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

## `print_item_events`

| Column | Type | Notes |
|---|---|---|
| `id` | `BIGSERIAL PRIMARY KEY` | |
| `print_item_id` | `BIGINT NOT NULL REFERENCES print_items (id) ON DELETE CASCADE` | |
| `user_id` | `BIGINT REFERENCES users (id) ON DELETE SET NULL` | who performed this event; nullable so a departed user's history entries survive their removal |
| `from_status` | `TEXT NOT NULL DEFAULT ''` | the item's status before this event; `''` marks a creation event (a fresh item from `CreateOrder`, or a sibling row split off by `RecordQCResult`'s partial-QC-failure path) rather than an ordinary transition |
| `to_status` | `TEXT NOT NULL` | the item's status after this event (equal to `from_status` for `UpdateQuantity`'s same-status quantity edit) |
| `detail` | `TEXT NOT NULL DEFAULT ''` | free-form context beyond the bare status change — e.g. `"printer: Printer 3"`, `"reason: nozzle jam"`, `"passed 3/4, reason: layer shift"`, `"quantity 100 -> 60"` |
| `created_at` | `TIMESTAMPTZ NOT NULL DEFAULT now()` | |

Index: `(print_item_id, created_at)`. This is the append-only audit trail
every `board.Store` transition method (and `CreateOrder`) writes a row to
inside its own transaction — see `docs/architecture.md`'s "Audit trail:
who touched a card". No `tenant_id` column, same convention as
`print_items`/`shipments`: `board.Store.ItemHistory` scopes it through the
join to `print_items`/`orders` instead.

## `order_files`

| Column | Type | Notes |
|---|---|---|
| `id` | `BIGSERIAL PRIMARY KEY` | |
| `order_id` | `BIGINT NOT NULL REFERENCES orders (id) ON DELETE CASCADE` | |
| `filename` | `TEXT NOT NULL` | sanitized display/download name |
| `stored_path` | `TEXT NOT NULL` | on-disk path under `UPLOAD_DIR` (random name); never served statically |
| `size_bytes` | `BIGINT NOT NULL` | |
| `content_type` | `TEXT NOT NULL DEFAULT ''` | |
| `created_at` | `TIMESTAMPTZ NOT NULL DEFAULT now()` | |

Index: `order_id`. No `tenant_id`; scoped through `orders` like
`print_items` (`board.Store.GetOrderFile`/`ListOrderFiles`). Deleting a
row or its order does not delete the file on disk.

## `invoices`

| Column | Type | Notes |
|---|---|---|
| `id` | `BIGSERIAL PRIMARY KEY` | |
| `tenant_id` | `BIGINT NOT NULL REFERENCES tenants (id) ON DELETE CASCADE` | |
| `customer_id` | `BIGINT NOT NULL REFERENCES customers (id) ON DELETE CASCADE` | one invoice per customer per batch |
| `number` | `BIGINT NOT NULL` | `UNIQUE (tenant_id, number)`; shown as `INV-0001` |
| `period_start` / `period_end` | `DATE NOT NULL` | the shipped-date range the batch covered (inclusive) |
| `total_cents` | `BIGINT NOT NULL` | sum of its lines |
| `created_by` | `BIGINT REFERENCES users (id) ON DELETE SET NULL` | |
| `created_at` | `TIMESTAMPTZ NOT NULL DEFAULT now()` | |

## `invoice_lines`

| Column | Type | Notes |
|---|---|---|
| `id` | `BIGSERIAL PRIMARY KEY` | |
| `invoice_id` | `BIGINT NOT NULL REFERENCES invoices (id) ON DELETE CASCADE` | |
| `print_item_id` | `BIGINT NOT NULL UNIQUE REFERENCES print_items (id) ON DELETE CASCADE` | the `UNIQUE` is what guarantees an item is invoiced at most once |
| `order_number`, `description`, `quantity`, `unit_price_cents`, `line_total_cents`, `shipped_at` | | snapshots taken at invoice time, so later edits never rewrite a past invoice. `quantity` is the item's `quantity_completed` |

## Relationships

```
tenants (1) ──< users     (many)
tenants (1) ──< orders    (many)
tenants (1) ──< customers (many)
customers (1) ──< users   (many, client role only)
customers (1) ──< orders  (many, nullable)
orders (1)      ──< order_files       (many)
customers (1)   ──< invoices          (many)
invoices (1)    ──< invoice_lines     (many) >── (1) print_items
tenants (1) ──< sessions  (many, denormalized from users)
users (1)   ──< sessions  (many)

orders (1)      ──< print_items       (many)
orders (1)      ──< shipments         (many)
shipments (1)   ──< shipment_items    (many) >── (1) print_items
print_items (1) ──< print_item_events (many)
users (1)       ──< print_item_events (many, nullable - see ON DELETE SET NULL above)
```

`print_items`, `shipments`, `shipment_items`, and `print_item_events` don't
carry their own `tenant_id` — every query that touches them already joins
`orders` (see `board.itemColumns`/`beginItemTxAny`/`ItemHistory`), so
scoping through that join is enough; see `docs/architecture.md`'s "Tenant
isolation in board.Store".

`shipments` and `print_items` are linked many-to-many through
`shipment_items`: `board.Store.GenerateLabel` creates one `shipments` row
per packed item (plus its linking `shipment_items` row) rather than one
`shipments` row per fully-packed order, so an order can ship in pieces as
each item becomes ready instead of waiting on every sibling item to reach
`packed` first.
