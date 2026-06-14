# dashboard: worker-status version number + favicon logo

## Goal

Port the two visible additions from the "Worker Status v2" design draft into the
worker's embedded React/Vite status dashboard (`cmd/worker/dashboard/`):

1. **Version number** in the topbar (`.brand-ver`), sourced from the `version`
   field already present in `GET /api/v1/state` (added in #828).
2. **Favicon** — the v3 "italic `a` + live caret" brand mark, as a 128×128 PNG
   served with a content-digest `?v=<sha256[:12]>` cache-bust, mirroring upstream
   Symphony commit `4cbe3a96` ("[web] Add Symphony favicon", #90).

The favicon mark is the v3 logo; the design's in-app topbar mark was also updated
from a bare `a` to the same italic-`a`+caret glyph, so the in-app mark and the
favicon read as one identity (scope decision below).

## What I already know (verified)

- The dashboard is already a committed, embedded surface:
  - `cmd/worker/dashboard_assets.go` — `//go:embed dashboard/dist dashboard/fallback.html`; `dashboardHTMLHandler` serves `dist/index.html`, falling back to `fallback.html` when dist isn't built; `dashboardAssetHandler` is an `http.FileServer` over `dashboard/dist`.
  - `dist/` is **gitignored** (only `dist/README.txt` committed). CI runs `npm run build` (`vite build && node scripts/write-dist-readme.mjs`) before `go build` embeds it. So in an un-built checkout the worker serves `fallback.html`.
  - `public/` **is committed** (the `*.woff2` fonts live there); Vite copies `public/*` → `dist/` at build.
- `version` is live: `stateapi.StateResponse.Version` (`json:"version,omitempty"`) is stamped by `resolveVersion()` (ldflags `-X main.version`) in `cmd/worker/stateapi.go:471`; `version_test.go` mutation-guards that wiring.
- Current dashboard has **no favicon** anywhere (`git ls-files | grep favicon` → none) and **no version display**; the topbar mark is a bare `<span>a</span>` in both `index.html` (pre-mount shell) and `App.jsx:399` (`Topbar`).
- `App.jsx`: full state payload lives in `state`/`s`; `Topbar` is rendered at `:467` (loading) and `:516` (loaded). Threading `s.version` into `Topbar` is the version-display seam.
- The rendered favicon already exists: `research/design-pull/favicon.png` (128×128 RGBA, 3429 bytes). Its `sha256[:12] = 61f833fd0705`, which **exactly matches** the `?v=61f833fd0705` literal in the design's `<link>` — confirming the digest is `sha256(favicon.png)[:12]`. `favicon-source.html` documents how it was rasterized (dark `#211C14` tile, linen `#E8E2D3` italic Newsreader `a`, terracotta `#C25E36` caret; theme-independent hard-coded tokens).
- Upstream Symphony #90 serves the favicon through the embedded static-asset controller and puts a content digest in the URL for cache-busting — i.e. serving a favicon from the embedded surface is upstream-aligned, not a deviation.

## Constraints

- Two HTML surfaces need the favicon `<link>`: built `index.html` and committed `fallback.html`. The favicon must work in both the built (release) and fallback (un-built) cases → the PNG and its digest can't depend solely on a Vite build artifact.
- One-source-of-truth (clean-code rule 3): the `?v=` digest must derive from the PNG bytes, not be a hand-maintained literal that can drift.
- gofmt/lint/size budgets apply; `App.test.jsx` snapshot will change with the topbar edits and must be regenerated + reviewed.
- This is a worker/dashboard surface change, not a SPEC state-machine change; `/api/v1/state` contract is unchanged (version already shipped).

## Technical Approach (proposed — favicon mechanism is a verdict, not a menu)

**Favicon serving + digest (decided):** commit `cmd/worker/dashboard/public/favicon.png`
(committed → embedded; Vite also copies it to `dist/`). Serve it from the worker's
embedded surface and inject the digest server-side so there is a single source of
truth and it works for both `index.html` and `fallback.html`:

- Embed the committed PNG; compute `faviconDigest = sha256(bytes)[:12]` once at init.
- Serve `/favicon.png` with `Content-Type: image/png` + long-lived immutable cache headers (digest in the query is the cache-bust, per Symphony #90).
- `dashboardHTMLHandler` reads the HTML and replaces a `__FAVICON_V__` placeholder
  in `<link rel="icon" type="image/png" sizes="128x128" href="/favicon.png?v=__FAVICON_V__">`
  with `faviconDigest` before serving — both index.html and fallback.html carry the placeholder.
- Add an anti-drift / wiring test: served HTML contains `?v=<sha256(embedded png)[:12]>` and `/favicon.png` returns the PNG bytes with the right content-type. (Mirrors Symphony's `extensions_test.exs` favicon assertions.)

**Version display:** thread `s.version` into `Topbar` (App.jsx) and render a
`.brand-ver` next to the brand name; add `.brand-ver` CSS (from the design:
mono, 10.5px, `--ink-mute`). Render nothing when `version` is absent (omitempty /
dev builds). Mirror a static version slot in the `index.html` pre-mount shell and
`fallback.html` is left server-rendered/JS as today.

## Decision (ADR-lite)

**Context:** the v2 design updates the topbar with a version number, a favicon
link, AND a new brand mark (italic `a` + live caret). The favicon and the in-app
mark are one identity; shipping the favicon but leaving the in-app `<span>a</span>`
would make the browser-tab mark and the on-page mark disagree.

**Decision (user-confirmed 2026-06-14):** scope = **version + favicon + in-app v3
mark**. The topbar brand-mark becomes the italic-`a`+caret glyph matching the
favicon, plus `.brand-ver` version, plus the favicon route/link. Out of scope:
other topbar refinements (crumb/meta spacing) and the rest of the v2 prototype.

**Consequences:** touches `index.html` shell + `App.jsx` `Topbar` + `styles.css`
(`.brand-mark .gm`/`.caret`, `.brand-ver`) + the favicon Go seam. `App.test.jsx`
snapshot regenerates. Caret animation must respect `prefers-reduced-motion`
(design already gates it). Slightly larger diff than favicon-only, but the
in-app mark and favicon stay consistent.

## Requirements (evolving)

- Worker status dashboard shows the build version in the topbar, sourced from `/api/v1/state`.
- Worker serves a favicon (128×128 PNG, the v3 mark) with a content-digest cache-bust; both built and fallback HTML reference it.
- Digest is derived from the PNG bytes (no hand-maintained literal).

## Acceptance Criteria (status as of implementation)

- [x] `GET /favicon.png` returns the embedded 128×128 PNG with `image/png` (`TestFaviconRouteServesEmbeddedDigestedPNG`).
- [x] Served dashboard HTML (built index + fallback) links the favicon with `?v=<sha256(png)[:12]>`, never the raw placeholder (asserted in `TestRootDashboardServesStateDepictingReactApp`, verified in both built + fallback embed modes).
- [x] Topbar renders the `version` from `/api/v1/state`; absent version renders no chip (two vitest cases mutation-guarding the `version={s.version}` seam).
- [x] Snapshot unaffected (the only `toMatchSnapshot` covers the rate-limit panel, not the topbar) — no stale snapshot to regenerate; 19/19 vitest pass.
- [x] Go test asserts favicon route + HTML digest wiring (mutation-verified: drop `bytes.ReplaceAll` → placeholder leaks → fail; wrong digest → sha256 cross-check fails; unregistered route → 404 fail).
- [x] gofmt clean / `go vet ./cmd/worker` / golangci-lint 0 issues / file-size budget gate / full `cmd/worker` package + vitest all green.

## Outstanding (outward-facing — awaiting go-ahead)

- [ ] Commit on branch `dashboard-version-favicon` (Trellis bookkeeping stays on main).
- [ ] Open tracking issue + PR (`Closes #N`, SPEC checklist citing Symphony #90, size: within budget — 7 prod files).
- [ ] `@codex review` adversarial pass on head commit; resolve threads.

## Definition of Done

- Tests added/updated (Go favicon+digest test; vitest topbar/version).
- Lint / typecheck / CI green; dashboard built (`npm run build`) so embed is current if release-relevant.
- `@codex review` adversarial pass on the head commit.
- PR body: `Closes #<new issue>`, SPEC-alignment checklist (favicon = Symphony #90 upstream citation), size classification.

## Out of Scope

- Any `/api/v1/state` contract change (version already ships).
- Broader v2 layout/visual changes beyond version + favicon (+ optionally the mark).
- Animations/Tweaks-panel parity from the standalone design prototype.

## Technical Notes / References

- Design pull staged at `research/design-pull/`:
  - `Worker-Status-v2.html` — target topbar (`.brand-ver`, v3 `.brand-mark .gm` + `.caret`, favicon `<link>`).
  - `aiops-platform-Logo-v3.html` — logo rationale ("the unfinished line": italic `a` + console block caret).
  - `favicon.png` (3429 B, sha256[:12]=`61f833fd0705`) + `favicon-source.html` (rasterization recipe).
- Upstream: `openai/symphony@4cbe3a96` — favicon via embedded static-asset controller + content digest; `extensions_test.exs` favicon assertions.
- Repo seams: `cmd/worker/dashboard_assets.go`, `cmd/worker/dashboard/{index.html,fallback.html}`, `cmd/worker/dashboard/src/{App.jsx,styles.css,App.test.jsx}`, `cmd/worker/dashboard/public/`, `internal/stateapi/types.go`, `cmd/worker/stateapi.go`.
