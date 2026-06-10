# Dashboard: Worker Status v2 layout update

## Background

Claude Design handoff bundle (`api.anthropic.com/v1/design/h/Q3w7cPJQj0vNDAg0kYkTAw`,
primary file `lean/Worker Status v2.html`) iterates on the v1 design that
`cmd/worker/dashboard` currently implements. The design chat (chat26, 2026-06-10)
identifies the core problem: the three in-flight buckets (Running / Retrying /
Blocked) are split apart — Retrying + Blocked sit at the very bottom below the
reference gauges, so "needs attention" items are the last thing an operator sees.

## Scope (delta v1 → v2, from the design diff + chat26)

1. **Layout restructure** — move the Retrying and Blocked panels from the
   bottom full-width `.grid-2` row into `body-main`, directly under Running
   (KPI order: running → retrying → blocked). Sidebar keeps Rate limits +
   Reconcile roll-up. Delete the bottom `.grid-2` row.
2. **Reconcile roll-up alignment** — `.rollup-grid` becomes a 3-track grid
   (`auto auto 1fr`) with `.rollup-cell` using `subgrid` so label / count /
   id-chips align across both rows; window count rendered as serif-numeric
   `<b>` with the lifetime total dimmed (`.tot`); ≤640px falls back to
   flex-column so nothing overflows.
3. **Tokens header single line** — Running table header becomes
   `Tokens<span class="th-sub"> in / out</span>` with `th.tok-h` nowrap CSS.
4. **Overflow guard** — extend the ≥641px `overflow:hidden` rule to
   `.body-main .table-wrap.narrow` so the per-second retry countdown can't
   flash a transient horizontal scrollbar in the main column.

## Non-goals

- No new data fields, no API changes — the v2 mock is byte-compatible with
  `/api/v1/state`; this is a pure presentation change.
- Keep all production adaptations the repo added over the v1 port (real fetch
  loop, settings popover, RateLimits raw-JSON fallback, IssueLink, sr-only
  rollup descriptions, "Delivered" KPI wording).

## Acceptance

- `cmd/worker/dashboard` vitest suite passes; snapshot updated to the v2 DOM order.
- Visual output matches `lean/Worker Status v2.html` per the handoff README
  ("recreate pixel-perfectly; don't copy prototype internals").
- Repo CI gate (gofmt / vet / golangci / go test) unaffected.
