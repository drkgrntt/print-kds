# Architecture

## Request flow

Every request passes through Fiber middleware registered in
`internal/server/routes.go`, in order:

1. `middleware.RequestID()` — tags the request with a UUID, echoed back as
   `X-Request-ID`. Registered first: nothing after it should run without an
   id to log against, and even a request that never reaches a real handler
   (a 404) still gets one.
2. `middleware.Recover()` — catches a panic anywhere downstream, logs it
   with the request id/method/path/stack, and responds `500` instead of
   taking the process down. Registered right after `RequestID` so its own
   log line can still tag the request that panicked.
3. Static file serving (`/static` → `web/static`), then the registered
   controllers.

There's no auth middleware in this chain — deliberately. This is a single
shared shop-floor display, not a multi-tenant app; anyone who can reach the
board can use it.

## The board's status state machine

`print_items.status` drives which column a card renders in:

```
queued ──(START PRINT)──► printing ──(POP OFF BED)──► cleaning_qc
  ▲                            │                           │
  │                     (PRINT FAILED)      ┌───────────────┤
  │                            │       (RE-PRINT)     (PASS QC)
  └────────────────────────────┘             │              │
                                              ▼              ▼
                                           queued    ready_to_pack ──(GENERATE LABEL)──► packed

queued / printing / cleaning_qc / ready_to_pack ──(CANCEL)──► cancelled
```

`CANCEL` (`board.Store.CancelItem(itemID)`) is reachable from any
non-terminal status - `queued`, `printing`, `cleaning_qc`, or
`ready_to_pack` - not just one specific column, unlike every other
transition method above. `cancelled` is terminal, like `packed`:
`BoardColumns` excludes both, so a cancelled card simply drops off the live
board with no further action available on it. Cancelling an
already-terminal item (`packed`, or already `cancelled`) is rejected the
same way an ordinary wrong-status transition is.

`START PRINT` (`board.Store.StartPrint(itemID, assignedPrinter)`) also
records which physical printer the item is going onto - `assigned_printer`
is a plain free-text column (e.g. "Printer 3"), not a validated/foreign-key
reference (there's no printers table), and it's fine to leave blank.

`PRINT FAILED` (`board.Store.FailPrint(itemID, reason)`) is the other
backward transition: a print can fail mid-job (nozzle jam, filament
runout, bed adhesion failure) rather than only after it's already popped
off the bed and gone through cleaning/QC. It moves the item straight from
`printing` back to `queued` - `quantity_completed` resets to 0,
`assigned_printer` clears back to `''` (whichever printer failed isn't
"still running" this item anymore), and `reprint_count`/
`qc_failure_reason` are updated exactly like a full QC-stage reprint (see
below) - a mid-print failure is the same "this attempt didn't produce a
usable print" event, just caught earlier, so it reuses those columns
rather than adding new ones.

`RE-PRINT` (and `PASS QC`) are really two fast-path shapes of one
transition, `board.Store.RecordQCResult(itemID, quantityPassed,
failureReason)`, driven by how many of the row's `quantity_required` units
actually passed cleaning/QC:

- **`quantityPassed == quantity_required`** (`PASS QC`, the whole row): the
  same row moves `cleaning_qc` → `ready_to_pack`, `quantity_completed` set
  to match. This is the diagram's `PASS QC` arrow above.
- **`quantityPassed == 0`** (`RE-PRINT`, none of it): the same row moves
  `cleaning_qc` → `queued`, `quantity_completed` resets to 0,
  `reprint_count` increments, and `qc_failure_reason` records why. This is
  the diagram's `RE-PRINT` arrow above.
- **`0 < quantityPassed < quantity_required`** (partial QC failure — some
  units good, some not): the row *splits*. The original row shrinks to the
  passed quantity and still takes the `PASS QC` arrow (`quantity_required`
  = `quantityPassed`, status → `ready_to_pack`), while a brand-new sibling
  row is inserted for the failed remainder and takes the `RE-PRINT` arrow
  from scratch (`quantity_required` = the leftover units, status →
  `queued`, `quantity_completed` 0, `reprint_count` = original + 1,
  `qc_failure_reason` recorded). Both rows keep the same `order_id`/
  `file_name`/`color_material` — the split card is not a new order item as
  far as intake is concerned, just a second physical print run for part of
  the same line. Neither row is deleted; this is the only board transition
  that increases the number of live `print_items` rows for an order rather
  than just moving one between columns.

So `RE-PRINT` is no longer strictly "the only backward transition for the
whole item" — a partial split sends part of a row backward while the rest
of it moves forward in the same request, and `PRINT FAILED` is a second,
earlier point (`printing` rather than `cleaning_qc`) where an item can go
backward. It's still true that failing to produce a usable print - whether
caught mid-job or at cleaning/QC - is the only way a card (or part of one)
goes backward at all; these are the normal reasons a print doesn't ship on
the first try, not edge cases to special-case away.

Every transition method above also stamps `status_changed_at = now()` on
the row(s) it moves (including the newly inserted split row in
`RecordQCResult`'s partial-split branch) - this is the one place the
schema tracks *when* a card last moved, not a full audit log of every past
transition, just enough to compute "how long has this card been sitting
here" for the current state (see the "To Print" queue order and the card
UI below).

Because `FailPrint` and `RecordQCResult`'s full-reprint branch both
increment `reprint_count` and send the item back to `queued`, a card that's
failed at least once carries that forward as a priority signal: the "To
Print" column's queue order (see `BoardColumns` below) puts every
`reprint_count > 0` item ahead of every fresh (`reprint_count == 0`) item,
regardless of due date, so a print that's now running behind schedule
visibly jumps the line instead of quietly waiting its turn at its
original due-date position. `internal/views/components/card.templ` shows
this on the card itself as a "REPRINT (Nx)" badge whenever a `queued`
item's `reprint_count > 0`, so it's obvious at a glance why it jumped
ahead of an unfailed card with an earlier due date.

### Order-status side effects

`orders.status` is derived from its print items' statuses, not set
directly by any board action:

- `pending` → `in_progress`: the first time any of the order's items leaves
  `queued`.
- → `completed`: once every item on the order has reached `ready_to_pack`
  or `packed`.
- → `shipped`: every `GENERATE LABEL` call creates its own `shipments` row
  (plus a linking `shipment_items` row) for just the one item it packs, so
  an order ships in pieces as each item becomes ready rather than waiting
  on every sibling item to reach `packed` at once. `orders.status` itself
  only advances to `shipped` once every *non-cancelled* item on the order
  has reached `packed` — a cancelled item doesn't block shipping, it isn't
  going anywhere — and this never re-runs once the order is already
  `shipped`, nor does it touch an order that's `cancelled`.
- → `cancelled`: when `CANCEL` finds every item on the order is now either
  `cancelled` or `packed` (a cancelled item alongside already-packed/shipped
  items is a *partial* cancellation - the order keeps whatever status it
  already had, since some of it is still shipping). This never overrides an
  already-`shipped` order - cancelling one more item on an order that's
  already shipped doesn't un-ship it.

These are read as the intended design for `internal/server/controllers`'
board handlers; if the implementation diverges (e.g. a different rule for
partial shipments), treat the code as authoritative and update this doc to
match rather than the other way around.

## htmx: moving a card without a page reload

Each action button is an `hx-post`/`hx-patch` with `hx-swap="none"` — the
button's own request target does nothing, because the real work happens
via out-of-band (OOB) swaps in the response. A single handler response
contains two fragments:

1. A delete-by-id OOB fragment for the card's *old* position:
   `<div id="item-{id}" hx-swap-oob="delete"></div>` — htmx matches this by
   id wherever it currently sits in the DOM and removes it.
2. The freshly rendered card, OOB-inserted into its *new* column:
   `hx-swap-oob="beforeend:#column-{status}-list"`.

`POST /print-items/:id/qc-result` (`handleQCResult`, backing the
`cleaning_qc` column's `PASS QC`/`RE-PRINT`/partial-QC-mini-form actions —
see the state-machine section above) can add a *third* fragment: when
`board.Store.RecordQCResult` splits a row, the response is delete-old-card
+ insert-updated-card (fragments 1–2 above, for the original row's new
column) + one more insert-only OOB fragment (no matching delete — it's a
brand-new id) for the split-off row landing in `queued`.

This gets a card to visually "move" columns without htmx's OOB swap having
to understand DOM reparenting (it doesn't do that — outerHTML swaps replace
in place, they don't relocate) and without re-rendering the whole board on
every click, which matters on a touchscreen where a re-render mid-tap reads
as unresponsive.

**Gotcha that already bit this once**: `hx-swap-oob` must go on a
throwaway wrapper `<div>`, never on the card's own root element. For any
swap style other than `"true"`/`"outerHTML"` — which a positional style
like `"beforeend:<selector>"` is — htmx discards the OOB element itself and
inserts only its *children* at that position (the OOB element is meant to
be a disposable delivery wrapper, not the content; see htmx's own OOB
docs). Putting `hx-swap-oob` directly on `Card`'s `<article id="item-N"
class="card" ...>` root used to mean htmx stripped that entire tag on every
transition and spliced the card's bare inner `<header>`/`<h3>`/`<p>` tags
loose into the column - no id, no `.card` styling, no `data-status`, not a
real card (`internal/views/components/oob.templ`'s `InsertOOB` wraps
`Card` in a `<div hx-swap-oob="...">` for exactly this reason - don't
un-wrap it). Caught via a real Chromium/Playwright session
(`document.getElementById` after a click) - `curl`-ing the endpoint isn't
enough to catch this class of bug, since the raw HTML the server returns
looks correct either way; it's what htmx's client-side OOB algorithm does
with that HTML that breaks.

## Manual order intake

`/admin/new-order` (`controllers.NewOrderController`) is the only way an
order reaches the database today — there's no webhook/storefront
integration. `GET` renders the form; `POST` validates it and calls
`board.Store.CreateOrder` (order + items in one transaction, all items
starting `queued`), then redirects (303) back to the form with
`?created=<order_number>` for the success banner — redirecting rather than
re-rendering directly means a page refresh after submitting doesn't
resubmit the form. A validation error (missing field, unparseable date, no
non-blank item rows) or a duplicate `order_number` (caught via
`board.ErrDuplicateOrderNumber`, wrapping the `23505` unique-violation from
Postgres) re-renders the form with every typed value preserved instead of
blanking it.

Item rows are parallel form fields (`file_name`/`color_material`/
`quantity_required` repeated once per row, not indexed) rather than a
JS-driven dynamic list: the "+ Add another item" button is itself an htmx
`hx-get` to `/admin/new-order/item-row` (`hx-target="#item-rows"
hx-swap="beforeend"`) that appends one more blank row fragment — no
client-side script needed, and the server reads however many rows came
back via `fasthttp.Args.PeekMulti`, skipping any row whose file name is
blank.

`cmd/dbseed` is the other way to get sample data onto the board (`make
seed`) — a handful of canned orders/items spread across every column,
keyed by `order_number` so a repeat run skips whatever's already there
instead of duplicating cards.

### Editing a queued item's quantity

`board.Store.UpdateQuantity(itemID, newQuantityRequired)` isn't part of the
forward state machine above - it's a same-status edit to
`quantity_required`, for when a customer changes their mind about an
order's quantity after it's already on the board (e.g. "actually we only
need 60, not 100") but before any print has actually started. It's only
valid while the item is still `queued`; once printing/QC/packing have
begun, `quantity_completed` and `RecordQCResult`'s partial-split already
express "how many units are really needed now," so this isn't the right
tool for that anymore. `newQuantityRequired` must be at least 1 -
`ErrInvalidQuantity` otherwise - since reducing an item to zero units is
really `CANCEL`, not a quantity edit. The board's `queued` column shows a
small inline quantity field (`UPDATE QTY`) next to the printer-assignment
form for this - see `internal/views/components/card.templ`.

## Layout

- `cmd/api` — the server entrypoint. `cmd/dbreset`/`cmd/dbseed` are dev CLI
  tools (see Makefile).
- `internal/board` — the domain layer: `Store` wraps every board query and
  status-transition method (`BoardColumns`, `StartPrint`, `PopOffBed`,
  `FailPrint`, `RecordQCResult`, `GenerateLabel`, `CancelItem`) plus
  `CreateOrder` for manual intake and `UpdateQuantity` for a queued item's
  same-status quantity edit. Each transition method opens its own
  transaction and `SELECT ... FOR UPDATE`-locks the item row first, so two
  taps on the same card from two tablets serialize instead of racing.
- `internal/database` — connection setup + goose migrations, embedded from
  `internal/database/migrations/*.sql`, applied automatically on startup
  (`database.New()` calls `migrate()` — no separate migrate step).
- `internal/logger` — a package-level `slog.Logger` wrapper.
- `internal/server` — Fiber wiring: `server.go` (dependency construction),
  `routes.go` (middleware + route registration), `controllers/` (one file
  per page/feature, `render.go`'s `render()` helper writes a templ
  component as the response body), `middleware/`.
- `internal/views` (+ `internal/views/components` for cross-page pieces) —
  `.templ` source, compiled to `_templ.go` by `templ generate` (gitignored
  build output — `make run`/`build`/`watch` all regenerate first; don't
  hand-edit the generated files).
- `web/static` — plain CSS (no framework) and `htmx.min.js`.

IDE diagnostics on a just-edited `.templ` file, or on any file while gopls'
workspace root is a different module (e.g. editing from a sibling repo's
checkout), routinely show false "undefined"/"broken import" errors — this
is a known false alarm, not a real break. Confirm with a real
`go build ./...` (empty output = success) before trusting one.
