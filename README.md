# print-kds

A self-hosted Kitchen Display System (KDS) for a 3D print shop floor — a
shared board, meant to run full-screen on a touchscreen or tablet in the
print room, that tracks each print item from queue to shipment. There's no
login and no multi-tenancy: one shop, one board, one shared display.

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

## Getting orders onto the board

`/admin/new-order` is a manual entry form — order number, customer name,
due date, and a list of print items (file name, color/material, quantity).
Submitting it creates the order and its items (all starting `queued`) and
redirects back to the form with a success banner, so entering several
orders in a row doesn't lose your place. Every page links to it via the
"+ New Order" button in the top bar.

There's no automated intake (webhook/storefront integration) yet — orders
only get onto the board by someone typing them in here.

## Schema

Four tables — `orders`, `print_items` (FK → `orders`), `shipments`
(FK → `orders`), and `shipment_items` (a join table linking `shipments` to
the specific `print_items` that shipped on them, enabling partial
shipments). See `docs/schema.md` for exact columns and constraints,
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

To see cards on the board right away instead of starting from an empty
one, either use `/admin/new-order` (see above) or run `make seed`, which
populates a few sample orders/items spread across every column.

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
