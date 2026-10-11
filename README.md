# print-kds

A self-hosted Kitchen Display System (KDS) for a 3D print shop floor — a
shared board, meant to run full-screen on a touchscreen or tablet in the
print room, that tracks each print item from queue to shipment.
print-kds is multi-tenant: one shared instance/database can serve many
shops, each logged in and seeing only its own board and orders.

## Tech stack

- **Backend**: Go, [Fiber](https://github.com/gofiber/fiber) as the HTTP
  router
- **Views**: [templ](https://github.com/a-h/templ), rendered server-side
- **Dynamic UI**: [htmx](https://htmx.org/), driven by htmx out-of-band
  swaps rather than a client-side framework — see `docs/architecture.md`
- **Database**: PostgreSQL, via `pgx` (through `database/sql`) and
  [goose](https://github.com/pressly/goose) migrations, embedded and run
  automatically on startup — no separate migrate step
- **Styling**: plain, hand-written CSS — no Tailwind or other utility
  framework. High-contrast dark theme, touch targets sized for a shop
  floor, not a desk

## The board

Four columns, one per `print_items.status`:

| Column | Status | Action |
|---|---|---|
| To Print | `queued` (reprinted items first, then by order `due_date` ascending) | `START PRINT` → `printing`, optionally recording which physical printer the item is going onto |
| On Bed | `printing` | `POP OFF BED` → `cleaning_qc`, or `PRINT FAILED` → `queued` if the print itself fails mid-job (nozzle jam, filament runout, etc.) |
| Clean / QC | `cleaning_qc` | `PASS QC` → `ready_to_pack`, or `RE-PRINT` → `queued`; a partial QC result splits the row, sending only the failed units back to `queued` while the rest ship |
| Ready to Ship | `ready_to_pack` | `GENERATE LABEL` → `packed` (creates a `shipments` row for just that item, so an order can ship in pieces rather than waiting on every item to be ready at once) |

Every action is a single htmx request; the board never does a full page
reload.

Any card still in flight (queued, on the bed, in cleaning/QC, or ready to
ship) can also be `CANCEL`ed at any time, e.g. if a customer pulls an item
or a whole order - a cancelled card drops off the board for good, the same
way a packed one does. If every item on an order ends up cancelled or
packed, the order itself is marked cancelled too (unless it's already
shipped). A queued item's quantity can also be adjusted after the fact
(e.g. "actually we only need 60, not 100") via a small inline quantity
field next to `START PRINT` - only while it's still queued, since once
printing starts the quantity-passed/partial-QC-split flow is how that gets
tracked instead.

A card that's been requeued after a QC failure or a mid-print failure
(`reprint_count > 0`) jumps ahead of every not-yet-attempted item in "To
Print", regardless of due date, and shows a "REPRINT (Nx)" badge so it's
obvious why it's out of due-date order. Every card also shows how long
it's been sitting in its current column (e.g. "In cleaning_qc: 12m"), so a
backed-up stage is visible at a glance rather than invisible until an
order's due date is already blown.

Every card also shows who last touched it ("Last: <email>"), and a
`[ HISTORY ]` toggle expands its full audit trail - every status
transition (and its creation), each with who did it and when.

## Users & roles

Each shop ("tenant") logs in separately at `/login` - `/signup` creates a
brand-new shop and its first user in one step. There are three roles:
`admin` (order intake at `/admin/new-order`, customers at
`/admin/customers`, and user management at `/admin/users`), `staff` (can
operate the board itself - every print-item transition - but not create
orders or manage users), and `client` (a customer's own login - see
"Client portal" below). An admin can add teammates and client logins from
`/admin/users`. See `docs/architecture.md` for how the session cookie ties
a request to its tenant.

## Client portal

A shop's clients can place orders themselves. An admin adds the client
company under `/admin/customers`, then creates a `client` login for it on
`/admin/users` (or `go run ./cmd/createuser -role client -customer "Acme"
...`). A client logs in at `/login` and lands on `/portal`: their own order
list and `/portal/new-order`, where they pick a ship-by date, attach one or
more model files (`.stl`, `.3mf`, `.obj`, `.step`, `.gcode`, `.zip`),
enter a ship-to address, and list print items (file, color, quantity). The
order date is set
automatically and the order number is generated. Clients see only their own
customer's orders and files and have no access to the board. Staff see the
order's placed date on each card and download its files from the card's
`[ FILES ]` toggle.

Uploaded files live on disk under `UPLOAD_DIR` (default `./data/uploads`),
never under the public static directory, and are served only through
authenticated, tenant-checked routes. `MAX_UPLOAD_MB` (default 100) caps a
request's size.

## Batched invoicing

Admins bill shipped work in bulk from `/admin/invoices`. Pick a date range
(it opens on the past 14 days) and optionally one customer, then
**Preview**: every item that was packed and shipped in that range and isn't
on an invoice yet. Cancelled items are never included, and a partially
shipped order bills only the items that have shipped. Prices are set per
item (staff type a unit price right in the preview); items without a price
are listed but left out of the batch until priced. **Create invoices**
makes one invoice per customer (numbered `INV-0001`, ...) and freezes each
line's description, quantity and price. Re-running the same range never
double-bills an item. Open any invoice for a clean, light, printable page -
use the browser's Print / Save as PDF. Orders typed in with a one-off
customer name (no customer record) can't be invoiced; pick a customer on
`/admin/new-order`. Currency is assumed to be USD.

## Getting orders onto the board

`/admin/new-order` is a manual entry form — order number, customer (pick
one from `/admin/customers` or type a one-off name), ship-by date, a
ship-to address, and a list of print items (file name, color, quantity).
Choosing a customer prefills the ship-to from their most recent order;
clients get the same prefill on their portal form.
Submitting it creates the order and its items (all starting `queued`) and
redirects back to the form with a success banner, so entering several
orders in a row doesn't lose your place. Every page links to it via the
"+ New Order" button in the top bar.

There's no automated intake (webhook/storefront integration) yet — orders
only get onto the board by someone typing them in here.

## Schema

Twelve tables — `tenants`, `customers` (FK → `tenants`), `users`
(FK → `tenants`, optionally `customers`), `sessions` (FK → `users`/
`tenants`), `orders` (FK → `tenants`/`customers`), `order_files`
(FK → `orders`), `invoices` (FK → `customers`) with `invoice_lines`
(FK → `invoices`/`print_items`), `print_items`
(FK → `orders`), `shipments` (FK → `orders`), `shipment_items` (a join
table linking `shipments` to the specific `print_items` that shipped on
them, enabling partial shipments), and `print_item_events` (an append-only
audit log of every print item's status transitions, who ran each one, and
when). See `docs/schema.md` for exact columns and constraints,
`internal/database/migrations/` for the migrations themselves.

## Setup

```bash
cp .env.example .env
```

Create a local Postgres database and user matching `.env`'s `DB_*` values
(database `print_kds` by default). `make run` brings the schema up to date
automatically before serving:

```bash
make run
```

Visit `/signup` to create your shop and its first (admin) login. To see
cards on the board right away instead of starting from an empty one, run
`make seed` instead, which also provisions a sample shop/login (printed to
the console when it finishes) alongside a few sample orders/items spread
across every column.

## Development workflow

```bash
make run        # templ generate + go run cmd/api/main.go
make watch      # live reload via air (installs air if missing)
make build      # templ generate + go build -o main cmd/api/main.go
make test       # go test ./... -v
make all        # build + test
make db-reset   # wipe the local DB and rebuild it from migrations
                # (asks for confirmation; pass ARGS=-yes to skip it)
make seed       # populate the DB with sample orders/items (safe to re-run)
```

Views are `.templ` files compiled to `_templ.go` by `templ generate` —
`make run`/`make build`/`make watch` all regenerate first. Don't hand-edit
the generated `_templ.go` files.
