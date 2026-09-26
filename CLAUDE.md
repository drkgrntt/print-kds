# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with
code in this repository.

## What this is

print-kds is a self-hosted Kitchen Display System (KDS) for a 3D print shop
floor: a shared board, meant to run full-screen on a touchscreen or
tablet in the print room, tracking each print item from queue through
printing, cleaning/QC, packing, and shipment. It's multi-tenant: one
shared instance/database serves many shops, each behind a login, isolated
by `orders.tenant_id` (see `internal/board.Store.beginItemTxAny`). Two
roles - `admin` (order intake, user management) and `staff` (board
operation only) - gate `/admin/*` routes; see `internal/auth` and
`internal/server/middleware/auth.go`. See `README.md` for the full feature
rundown and `docs/architecture.md` / `docs/schema.md` for the technical
detail behind the board and its data model.

## Commands

```bash
make run        # templ generate + go run cmd/api/main.go
make watch      # live reload via air (installs air if missing)
make build      # templ generate + go build -o main cmd/api/main.go
make test       # go test ./... -v
make all        # build + test
make db-reset   # wipe the local DB and rebuild it from migrations
                # (asks for confirmation; ARGS=-yes skips it)
make seed       # populate the DB with sample orders/items (safe to re-run)
```

Run a single test: `go test ./internal/server/controllers/ -run TestName -v`
— standard Go tooling, no custom test runner.

Views are `.templ` files compiled to `_templ.go` by `templ generate` —
`make run`/`build`/`watch` all regenerate first. If you edit a `.templ`
file directly, regenerate before relying on the `_templ.go` output; don't
hand-edit the generated files (they're gitignored — build output, not
source).

The IDE's own diagnostics lag behind this, and also routinely misfire on
any file edited while gopls' workspace root is a different module (e.g.
editing from a sibling checkout). This has been a false alarm every time
it's come up here — always confirm with a real `go build ./...` (empty
output = success) before trusting an IDE diagnostic over it.

## Working with Go, Templ, htmx, and Postgres

- **Go**: standard `net/http`-adjacent Fiber router. Handlers live in
  `internal/server/controllers/`, one file per page/feature; call
  `render(c, views.Whatever(...))` (see `controllers/render.go`) to write a
  templ component as the response rather than building HTML by hand.
- **Templ**: views live in `internal/views/` (`.templ` source +
  generated `_templ.go`), with `internal/views/components/` for pieces
  reused across pages. Keep components server-rendered — no client-side
  templating layer.
- **htmx**: the board's column transitions use out-of-band swaps, not full
  page reloads — see `docs/architecture.md`'s "htmx: moving a card without
  a page reload" section before adding a new state-transition endpoint,
  and match that pattern rather than inventing a new one.
- **Postgres**: raw SQL via `database/sql` + `pgx` (`internal/database`),
  no ORM. Migrations are goose, numbered sequentially
  (`internal/database/migrations/NNNNN_description.sql`), embedded at
  build time and applied automatically on startup — don't add a manual
  migrate step, and don't edit an already-applied migration; add a new one
  instead.
- **Tenant scoping**: every `internal/board.Store` method takes a
  `tenantID int64` and enforces it in `beginItemTx`/`beginItemTxAny`'s
  `FOR UPDATE` query (`AND o.tenant_id = $N`) or `BoardColumns`'/
  `CreateOrder`'s query — that's the actual security boundary, not just
  `middleware.RequireAuth`. A new `Store` method needs the same treatment;
  a controller reads the current tenant via
  `middleware.CurrentTenantID(c)`, never from a form field or param.

## Coding standards

- **CSS**: plain, hand-written CSS only — no Tailwind or other utility
  framework. High-contrast dark theme (this runs in a basement/shop
  environment, not an office), touch targets at least 48px so it's usable
  on a shop-floor touchscreen without misfires.
- **State transitions**: every board action is a single htmx request with
  `hx-swap="none"` plus OOB-swapped fragments in the response — never a
  full page reload, and never a client-side-only state change that isn't
  backed by a real `print_items.status` update.
- **Tests**: standard `go test`, colocated with the code
  (`internal/server/controllers/*_test.go`), no fixture files or mocking
  framework beyond small helper constructors where useful.

## Token-saving strategies

- Prefer modular edits (a single function/handler/migration) over
  rewriting a whole file when only part of it changed.
- Keep status output concise — a one-line summary of what changed, not a
  restatement of the diff.
- For a multi-step change (new migration + controller + view + CSS, say),
  use plan mode to lay out the steps before touching files, rather than
  discovering the shape of the change through several rounds of edits.

## Architecture

**Layout**: `cmd/api` is the server entrypoint; `cmd/dbreset`/`cmd/dbseed`
are dev CLI tools (`make db-reset`/`make seed`); `cmd/createuser`
provisions a login onto an existing tenant (e.g. the migration-backfilled
"Default Shop") outside the self-serve `/signup` flow, which only ever
creates a brand-new tenant. `internal/` is split by concern:

- `internal/board` — the domain layer: `Store` (print_items/orders/
  shipments queries, the status-transition state machine, and
  `CreateOrder` for the manual intake form), every method tenant-scoped
  (see "Tenant scoping" above). See `docs/architecture.md`.
- `internal/auth` — tenants/users/sessions: `Store.SignUp` (new tenant +
  first admin user), `Authenticate`, `CreateSession`/`ValidateSession`/
  `DeleteSession` (opaque token, hashed before storage), and
  `ListUsers`/`CreateUser`/`DeleteUser` for admin-driven user management.
- `internal/database` — connection setup + goose migrations, embedded from
  `internal/database/migrations/*.sql` and run automatically on every
  startup (`database.New()` calls `migrate()` — no separate migrate step).
  `reset.go`'s `Open`/`Reset` back `cmd/dbreset`, which drops every table
  and replays migrations from scratch for local dev.
- `internal/logger` — a package-level `slog.Logger` wrapper
  (`Info`/`Warn`/`Debug`/`Error`/`Fatal`).
- `internal/server` — Fiber app wiring: `server.go` (dependency
  construction), `routes.go` (middleware chain + route registration —
  read this first to find where a given endpoint lives),
  `controllers/` (one file per page/feature; `render.go`'s `render()`
  helper writes a templ component as the response), `middleware/`
  (`RequestID`, `Recover`, `RequireAuth`/`RequireAdmin` — see
  `docs/architecture.md`'s auth section).
- `internal/views` (+ `internal/views/components`) — templ view templates
  rendered server-side and swapped into the page via htmx; the board's
  column/card components and the KDS layout shell live here.
  `layout.templ`'s `Layout` takes a `LayoutData{TenantName, IsAdmin}` for
  the header's tenant name/nav/logout — every page passes one through.
- `web/static` — the CSS design system (plain files, no build step beyond
  serving them statically) and `htmx.min.js`.

**The board** (`docs/architecture.md` has the full detail): four columns
keyed off `print_items.status` (`queued`/`printing`/`cleaning_qc`/
`ready_to_pack`, plus terminal `packed`), each with one or two actions that
`POST`/`PATCH` a transition endpoint and get back OOB-swapped HTML to move
the card between columns without a page reload. `orders.status` is derived
from its items' statuses rather than set directly — see
`docs/architecture.md`'s "Order-status side effects".

**Migrations**: goose, numbered sequentially
(`internal/database/migrations/NNNNN_description.sql`), embedded at build
time and applied automatically on startup. Add a new migration by
following the existing numbering/naming pattern; don't edit an
already-applied one.

## Keeping the docs current

If a change adds a new `internal/` package or shifts the architecture,
update this file's Architecture section. A new user-facing feature or
env-var change belongs in `README.md`. A schema change belongs in
`docs/schema.md`; a change to the board's state machine or OOB-swap
mechanism belongs in `docs/architecture.md`. Don't let a change land
without touching whichever of these it actually affects.
