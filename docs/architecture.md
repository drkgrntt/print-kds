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
                                                            │
                              ┌─────────────────────────────┤
                       (RE-PRINT)                     (PASS QC)
                              │                              │
                              ▼                              ▼
                           queued                    ready_to_pack ──(GENERATE LABEL)──► packed
```

`RE-PRINT` is the only backward transition — it exists because QC failures
are the normal reason a print doesn't ship, not an edge case to special-case
away.

### Order-status side effects

`orders.status` is derived from its print items' statuses, not set
directly by any board action:

- `pending` → `in_progress`: the first time any of the order's items leaves
  `queued`.
- → `completed`: once every item on the order has reached `ready_to_pack`
  or `packed`.
- → `shipped`: when `GENERATE LABEL` finds every item on the order already
  `packed` — this is also the point a `shipments` row gets inserted.

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

## Layout

- `cmd/api` — the server entrypoint. `cmd/dbreset`/`cmd/dbseed` are dev CLI
  tools (see Makefile).
- `internal/board` — the domain layer: `Store` wraps every board query and
  status-transition method (`BoardColumns`, `StartPrint`, `PopOffBed`,
  `PassQC`, `RePrint`, `GenerateLabel`) plus `CreateOrder` for manual
  intake. Each transition method opens its own transaction and
  `SELECT ... FOR UPDATE`-locks the item row first, so two taps on the same
  card from two tablets serialize instead of racing.
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
