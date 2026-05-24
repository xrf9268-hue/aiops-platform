# Worker dashboard: brand redesign & UX hardening

Status: implemented (PR #354, follow-up to #345 / issue #352)
Scope: `cmd/worker/dashboard` (the web UI embedded into the Go worker via `//go:embed dashboard/dist`). The `cmd/tui` terminal dashboard is intentionally out of scope — it is a faithful port of the upstream Elixir `StatusDashboard` and has its own tests.

## 1. Goals

1. Replace the hand-rolled / generic look with the Anthropic brand system.
2. Provide eye-friendly light **and** dark themes with a manual toggle.
3. Fix real UX gaps surfaced by review: raw-JSON rate limits, undifferentiated metrics, no loading state, content overflow, weak accessibility.
4. Keep it data-legible — branding must not compromise scannability or status semantics.

## 2. Brand system

Reference (Anthropic brand guidelines): ink `#141413`, cream `#faf9f5`, mid gray `#b0aea5`, light gray `#e8e6dc`; accents orange `#d97757` (primary), blue `#6a9bcc`, green `#788c5d`; Poppins headings, Lora body.

### Colour tokens
Colours are semantic CSS variables (`--c-*`) mapped to Tailwind v4 utilities via `@theme inline` (`surface`, `inset`, `fg`, `muted`, `faint`, `accent`, `accent-ink`, `brand-blue`, `brand-green`, and `good/warn/danger` + `*-bg`/`*-line`). Each token is redefined under `.dark`, so one set of utility classes serves both themes.

Key decisions:
- **Warm neutrals**, tuned for comfort: no pure-white surface in light mode (cream `#faf9f5` on a softer canvas); lifted warm navy/charcoal — not pure black — with off-white (`#faf9f5`) text in dark mode.
- **Accent split for AA**: `--c-accent` `#d97757` is the decorative fill (lockup tile, panel rules, hero); `--c-accent-ink` (`#a8492c` light / `#e89478` dark) is the text/link colour, because `#d97757` text fails WCAG AA on cream.
- **Status colours stay functional** (green/amber/red) for "glance and spot trouble". The brand olive green is *not* used as a success colour. Brand orange/blue/green are used only for non-semantic chrome (the panel-title accent cycle).

### Typography
- Self-hosted **Poppins** (400/500/600/700) and **Lora** (400) woff2 (latin subset) under `public/fonts/`, declared with `@font-face` + `font-display: swap`. No runtime CDN dependency (the worker serves these offline). Fallbacks: Arial (Poppins), Georgia (Lora).
- A modular scale (~1.2) is exposed as `@theme` tokens: `text-label`, `text-section`, `text-h1`, `text-display`. Headings use Poppins; descriptive prose uses Lora; data/tables/numbers stay sans + `tabular-nums` (a deliberate deviation from "Lora body" for legibility).
- Brand **wordmark lockup**: a filled hub-and-spoke mark (orchestrator + agents) on a terracotta tile beside the `aiops-platform` wordmark.

### Theming mechanics
- Tailwind v4 class strategy: `@custom-variant dark (&:where(.dark, .dark *))`.
- Manual toggle (sun/moon) persists to `localStorage`; first load falls back to `prefers-color-scheme`.
- An inline `<head>` script in `index.html` applies the saved/system theme before first paint (no FOUC) — per the official Tailwind docs.

## 3. UX / component decisions

- **Rate limits**: rendered as cards (tier badge + Primary/Secondary buckets + Credits). Usage bars fill with *consumed* quota and trend green→amber→red as headroom shrinks, with an "N% used" caption (intuitive "filling toward danger" model). No raw `JSON.stringify`.
- **Metric cards**: severity tones. Blocked/Failed/Retrying get a tinted background + leading dot when `> 0` so anomalies out-rank healthy metrics. Values use a responsive font ramp (`text-lg → sm:text-3xl`) to avoid overflow on narrow screens.
- **Tables → cards**: Running/Blocked/Retry render as a real table at `lg+` and stack into label/value cards below `lg`, driven by one column config so the two views never drift. Long messages/errors use a 2-line clamp (touch has no tooltip); session IDs compact to `4…6` (matching the TUI).
- **Loading**: skeletons so the first paint never shows real-looking zeros; the static shell is styled.

## 4. Accessibility

- Light-theme text tokens meet WCAG AA (≥4.5:1 for body, ≥3:1 for UI/non-text); verified by an audit pass.
- `aria-live="polite"` on the status pill only; `role="alert"` on the error banner; `role="progressbar"` with per-bucket `aria-label` + `aria-valuetext` on usage bars.
- `:focus-visible` ring; `sr-only` section headings keep heading order gapless; decorative SVG is `aria-hidden`.
- `prefers-reduced-motion` disables the live-dot pulse and skeleton animations.

## 5. Responsive

- No horizontal page overflow from 320–1440px (validated with Playwright).
- The 5th status card spans both columns on the 2-col tier instead of orphaning.

## 6. Testing

- Vitest + Testing Library suite (`src/App.test.jsx`): rate-limit formatting, session compaction, metric severity tones, the theme toggle, and a render snapshot of the rate-limit panel.
- Verified per change: `npm run build`, `npm test`, `go build ./cmd/worker/...` + `./cmd/tui/...`, `go test ./...`, and Playwright screenshots (light/dark × desktop/mobile) + overflow audits.

## 7. Note: there is no built-in task board

The Backlog/Todo/In-Progress board in the upstream README screenshot is **Linear** — the external tracker Symphony reads from ("Symphony monitors a Linear board for work…"). Upstream's own `status_dashboard.ex` renders only **Running** + **Backoff queue**, which the TUI and this web dashboard already mirror. A kanban view would be a net-new feature requiring new `/api/v1/state` data (no backlog/todo/done lists exist there today).
