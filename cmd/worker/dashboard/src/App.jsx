import React, { useCallback, useEffect, useRef, useState } from 'react';

// Lean Worker-status dashboard — a read-only view of GET /api/v1/state.
// Ported pixel-for-pixel from the Claude Design "lean/Worker Status v2.html"
// handoff; the styling lives in styles.css. The mock data the prototype
// shipped with is replaced here by the real /api/v1/state contract
// (internal/stateapi · StateResponse), which it mirrors field-for-field.

const REFRESH_MS = 5000;

// ── state API URL ────────────────────────────────────────────────────────────
// Build the absolute endpoint from the page origin; never carry credentials in
// the URL (origin === 'null' happens for file:// / sandboxed iframes).
export function stateAPIURL(locationLike = window.location) {
  const origin = locationLike?.origin;
  if (!origin || origin === 'null') return '/api/v1/state';
  return new URL('/api/v1/state', origin).toString();
}

// ── formatters ───────────────────────────────────────────────────────────────
const nf = new Intl.NumberFormat('en-US');
const compact = (n) => {
  const v = Number(n || 0);
  if (v >= 1_000_000) return (v / 1e6).toFixed(v >= 1e7 ? 0 : 1).replace(/\.0$/, '') + 'M';
  if (v >= 1_000) return (v / 1e3).toFixed(v >= 1e4 ? 0 : 1).replace(/\.0$/, '') + 'k';
  return nf.format(v);
};

function dur(sec) {
  const total = Math.max(0, Math.round(Number(sec) || 0));
  const h = Math.floor(total / 3600);
  const m = Math.floor((total % 3600) / 60);
  const s = total % 60;
  if (h) return `${h}h ${m}m`;
  if (m) return `${m}m ${s}s`;
  return `${s}s`;
}

function windowDurationLabel(minutes) {
  if (minutes == null) return 'window';
  const total = Number(minutes);
  if (!Number.isFinite(total) || total <= 0) return 'window';
  if (total % 1440 === 0) return `${total / 1440}-day window`;
  if (total % 60 === 0) return `${total / 60}-hour window`;
  return `${total}-minute window`;
}

// Relative-time helpers tolerate missing / malformed timestamps (the API marks
// started_at / last_event_at / blocked_at / due_at omitempty) — the prototype
// assumed every field was present; real snapshots don't guarantee it.
function sinceISO(iso) {
  if (!iso) return '—';
  const ms = new Date(iso).getTime();
  if (Number.isNaN(ms)) return '—';
  return dur((Date.now() - ms) / 1000);
}

function issueLabel(row) {
  return row.issue_identifier || row.issue_id || 'unknown issue';
}

// Which WORKFLOW.md (the profile, e.g. reviewer vs maker) produced a run (#983).
// The path is the discriminator operators recognize; a default-workflow run
// reports source=default with no path. A missing value renders "unknown" so the
// cell is explicit diagnostic data, never blank (matching the model/provider
// convention from #977).
function workflowProfile(row) {
  return row.workflow_path || row.workflow_source || 'unknown';
}
function rowRuntime(row) {
  if (Number(row.runtime_seconds) > 0) return dur(row.runtime_seconds);
  return sinceISO(row.started_at || row.blocked_at);
}
function budgetLabel(budget) {
  const parts = [];
  if (budget?.max_tokens_per_claim) parts.push(`${compact(budget.max_tokens_per_claim)} tokens/claim`);
  if (budget?.max_runtime_seconds_per_claim) parts.push(`${dur(budget.max_runtime_seconds_per_claim)}/claim`);
  return parts.length ? parts.join(' · ') : 'off';
}
function detailPath(row) {
  const id = row.issue_identifier || row.issue_id;
  return id ? `/api/v1/${encodeURIComponent(id)}` : '/api/v1/state';
}

// ── tiny icons ───────────────────────────────────────────────────────────────
const I = {
  refresh: (
    <svg aria-hidden="true" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
      <path d="M21 12a9 9 0 1 1-2.64-6.36" /><polyline points="21 3 21 9 15 9" />
    </svg>
  ),
  gear: (
    <svg aria-hidden="true" width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
      <circle cx="12" cy="12" r="3" />
      <path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z" />
    </svg>
  ),
  check: (
    <svg aria-hidden="true" width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round">
      <path d="M9 11l3 3L22 4" /><path d="M21 12v7a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h11" />
    </svg>
  ),
};

// ── display settings (brand / theme / density) ───────────────────────────────
// Production replacement for the prototype's design-tool Tweaks panel. Values
// persist to localStorage and drive <html data-brand|theme|density>; the
// .theme-swapping class suppresses transitions for one frame so var()-driven
// colors snap to the new token layer instead of getting stuck mid-transition.
const BRANDS = ['linen', 'anthropic'];
const THEMES = ['light', 'dark'];
const DENSITIES = ['compact', 'comfy'];

function systemTheme() {
  if (typeof window === 'undefined' || !window.matchMedia) return 'light';
  return window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
}

function readSetting(key, allowed, fallback) {
  try {
    const v = window.localStorage.getItem(`dashboard.${key}`);
    if (allowed.includes(v)) return v;
  } catch {
    /* localStorage unavailable */
  }
  return fallback;
}

function useSettings() {
  const [brand, setBrand] = useState(() => readSetting('brand', BRANDS, 'linen'));
  const [theme, setTheme] = useState(() => readSetting('theme', THEMES, systemTheme()));
  const [density, setDensity] = useState(() => readSetting('density', DENSITIES, 'compact'));

  useEffect(() => {
    const root = document.documentElement;
    root.classList.add('theme-swapping');
    root.dataset.brand = brand;
    root.dataset.theme = theme;
    root.dataset.density = density;
    try {
      window.localStorage.setItem('dashboard.brand', brand);
      window.localStorage.setItem('dashboard.theme', theme);
      window.localStorage.setItem('dashboard.density', density);
    } catch {
      /* ignore persistence failures */
    }
    void root.offsetWidth; // force reflow → commit new colors with no transition
    const id = requestAnimationFrame(() => root.classList.remove('theme-swapping'));
    return () => cancelAnimationFrame(id);
  }, [brand, theme, density]);

  return { brand, setBrand, theme, setTheme, density, setDensity };
}

function Seg({ label, value, options, onChange }) {
  return (
    <div className="set-row">
      <div className="set-lbl">{label}</div>
      <div className="seg" role="group" aria-label={label}>
        {options.map((o) => (
          <button key={o.value} type="button" aria-pressed={value === o.value} onClick={() => onChange(o.value)}>
            {o.label}
          </button>
        ))}
      </div>
    </div>
  );
}

function SettingsControl({ settings }) {
  const { brand, setBrand, theme, setTheme, density, setDensity } = settings;
  const [open, setOpen] = useState(false);
  const wrapRef = useRef(null);

  useEffect(() => {
    if (!open) return undefined;
    const onDown = (e) => {
      if (wrapRef.current && !wrapRef.current.contains(e.target)) setOpen(false);
    };
    const onKey = (e) => {
      if (e.key === 'Escape') setOpen(false);
    };
    window.addEventListener('mousedown', onDown);
    window.addEventListener('keydown', onKey);
    return () => {
      window.removeEventListener('mousedown', onDown);
      window.removeEventListener('keydown', onKey);
    };
  }, [open]);

  return (
    <div className="settings-wrap" ref={wrapRef}>
      <button
        type="button"
        className="icon-btn"
        aria-label="Display settings"
        aria-haspopup="true"
        aria-expanded={open}
        title="Display settings"
        onClick={() => setOpen((o) => !o)}
      >
        {I.gear}
      </button>
      {open && (
        <div className="settings-pop" role="dialog" aria-label="Display settings">
          <Seg label="Brand" value={brand} onChange={setBrand} options={[{ value: 'linen', label: 'Linen' }, { value: 'anthropic', label: 'Anthropic' }]} />
          <Seg label="Theme" value={theme} onChange={setTheme} options={[{ value: 'light', label: 'Light' }, { value: 'dark', label: 'Dark' }]} />
          <Seg label="Density" value={density} onChange={setDensity} options={[{ value: 'compact', label: 'Compact' }, { value: 'comfy', label: 'Comfy' }]} />
        </div>
      )}
    </div>
  );
}

// ── KPI card ─────────────────────────────────────────────────────────────────
function Kpi({ label, value, sub, flag, status }) {
  return (
    <div className="kpi">
      {status ? <div className={'stripe ' + status} /> : null}
      <div className="kpi-label"><span className={'kpi-dot' + (status ? ' ' + status : '')} />{label}</div>
      <div className="kpi-value tnum">{value}</div>
      {sub != null && <div className="kpi-sub">{sub}</div>}
      {flag != null && (
        <div className="kpi-sub" style={{ color: 'var(--warning)', fontWeight: 600, marginTop: '1px' }}>{flag}</div>
      )}
    </div>
  );
}

// ── rate-limit window cell ─────────────────────────────────────────────────
// Modern Codex token_count rate-limit window: { used_percent (0-100 float),
// window_duration_mins | window_minutes (int|null),
// resets_at (unix seconds) | resets_in_seconds }.
// Percent-of-window — never a per-category quota.
function RateWindow({ label, win }) {
  const pct = Math.max(0, Math.min(100, Number(win.used_percent) || 0));
  const cls = pct >= 85 ? 'bad' : pct >= 60 ? 'warn' : 'ok';
  const winLabel = windowDurationLabel(win.window_duration_mins ?? win.window_minutes);
  let resetSecs = null;
  if (win.resets_at != null) resetSecs = Number(win.resets_at) - Math.floor(Date.now() / 1000);
  else if (win.resets_in_seconds != null) resetSecs = Number(win.resets_in_seconds);
  else if (win.reset_in_seconds != null) resetSecs = Number(win.reset_in_seconds);
  return (
    <div className="rate-cell">
      <div className="rate-k">{label}</div>
      <div className="rate-v tnum">{pct.toFixed(pct < 10 ? 1 : 0)}<small> % used</small></div>
      <div className="rate-bar"><i className={cls} style={{ width: pct + '%' }} /></div>
      <span className="sr-only">{cls === 'bad' ? 'over limit' : cls === 'warn' ? 'approaching limit' : 'within limit'}</span>
      <div className="rate-meta">{winLabel}{resetSecs != null && Number.isFinite(resetSecs) ? ` · resets in ${dur(resetSecs)}` : ''}</div>
    </div>
  );
}

// A window is "percent-shaped" (renderable as a RateWindow) when it carries a
// numeric used_percent. Anything else falls back to a raw JSON dump so the
// operator never loses data when the runner reports an unfamiliar shape.
function isPercentWindow(win) {
  return win && typeof win === 'object' && typeof win.used_percent === 'number';
}

// Keys the percent-window path renders explicitly (windows + tier label); any
// other key in the snapshot is surfaced as raw JSON so nothing is dropped.
const RATE_LIMIT_KNOWN_KEYS = new Set(['primary', 'secondary', 'plan_type', 'limit_name']);

function RateLimits({ rl }) {
  if (rl == null) {
    return (
      <div className="empty">
        <div className="em-t">No rate-limit data yet</div>
        <div className="em-s">The runner hasn&apos;t reported a rate-limit snapshot — common on a Claude runner, or before the first rate-limit event.</div>
      </div>
    );
  }
  // rate_limits is a raw map[string]any passthrough of the runner's payload
  // (orchestrator.RateLimitSnapshot); the worker never normalizes it. Take the
  // Codex percent-window path only when EVERY window key present is percent-
  // shaped — if any present window is an unfamiliar shape, dump the whole
  // snapshot as raw JSON rather than rendering some windows and silently
  // dropping the rest (keep-data-visible).
  const windowKeys = ['primary', 'secondary'].filter((k) => rl[k] != null && typeof rl[k] === 'object');
  const allPercent = windowKeys.length > 0 && windowKeys.every((k) => isPercentWindow(rl[k]));
  if (!allPercent) {
    return <pre className="rate-raw">{JSON.stringify(rl, null, 2)}</pre>;
  }
  const tier = rl.plan_type || rl.limit_name;
  // Any key outside the recognized window/tier vocabulary (e.g. a `credits`
  // object) is still surfaced verbatim below the cards so the percent-window
  // path never silently drops part of the passthrough payload.
  const extras = Object.fromEntries(Object.entries(rl).filter(([k]) => !RATE_LIMIT_KNOWN_KEYS.has(k)));
  const hasExtras = Object.keys(extras).length > 0;
  return (
    <>
      {tier ? (
        <div className="rate-tier"><span className="tier-badge">{tier}</span><span>Codex plan · percent of each rolling window</span></div>
      ) : null}
      <div className="rate-grid">
        {isPercentWindow(rl.primary) ? <RateWindow label="Primary window" win={rl.primary} /> : null}
        {isPercentWindow(rl.secondary) ? <RateWindow label="Secondary window" win={rl.secondary} /> : null}
      </div>
      {hasExtras ? (
        <pre className="rate-raw" style={{ marginTop: 'var(--gap)' }}>{JSON.stringify(extras, null, 2)}</pre>
      ) : null}
    </>
  );
}

// ── badge / empty / issue link ───────────────────────────────────────────────
const Badge = ({ kind, children }) => (
  <span className={'badge ' + kind}><span className="bd" />{children}</span>
);

const Empty = ({ title, sub }) => (
  <div className="empty">
    <div className="ico">{I.check}</div>
    <div className="em-t">{title}</div>
    <div className="em-s">{sub}</div>
  </div>
);

const IssueLink = ({ row }) => (
  <a className="issue-id" href={detailPath(row)}>{issueLabel(row)}</a>
);

// ── running table ──────────────────────────────────────────────────────────
function RunningTable({ rows }) {
  if (!rows.length) return <Empty title="No active sessions" sub="The worker is polling and idle — nothing is running right now." />;
  return (
    <div className="table-wrap running-table-wrap">
      <table className="sessions running-sessions">
        <caption className="sr-only">Running sessions</caption>
        <thead><tr>
          <th scope="col">Issue</th><th scope="col">State</th><th scope="col">Model</th><th scope="col">Runtime</th>
          <th scope="col">Latest activity</th><th scope="col" className="r tok-h">Tokens<span className="th-sub"> in / out</span></th>
        </tr></thead>
        <tbody>
          {rows.map((r) => {
            // Resolved per-claim model/runtime; a missing model renders as
            // "unknown" (explicit diagnostic data, never blank) and collapses
            // into the issue detail line for narrow layouts (#977).
            const model = r.agent_model || 'unknown';
            const provider = r.agent_provider || 'unknown';
            const profile = workflowProfile(r);
            return (
            <tr key={r.issue_id}>
              <td className="issue-cell">
                <div className="issue">
                  <IssueLink row={r} />
                  <div className="upd" style={{ maxWidth: '38ch' }} title={r.last_message}>{r.last_message}</div>
                  <span className="wp">model {model} · {provider} · workflow {profile} · {r.workspace_path} · {r.session_id}{r.codex_app_server_pid ? ` · pid ${r.codex_app_server_pid}` : ''}</span>
                </div>
              </td>
              <td data-label="State"><Badge kind="running">{r.state}</Badge><div className="dim">turn {r.turn_count}</div></td>
              <td data-label="Model"><span className="mono" style={{ fontSize: '11.5px' }}>{model}</span><div className="dim">{provider}</div></td>
              <td data-label="Runtime"><span className="runtime tnum">{rowRuntime(r)}</span><div className="dim">current claim</div></td>
              <td data-label="Latest activity"><span className="mono" style={{ fontSize: '11.5px' }}>{r.last_event}</span><div className="dim">{sinceISO(r.last_event_at)} ago</div></td>
              <td className="tok" data-label="Tokens in / out"><b>{compact(r.tokens?.input_tokens)}</b> / {compact(r.tokens?.output_tokens)}<div className="dim">{compact(r.tokens?.total_tokens)} total</div></td>
            </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

// ── retry table ──────────────────────────────────────────────────────────────
function RetryTable({ rows }) {
  if (!rows.length) return <Empty title="No sessions retrying" sub="Nothing has hit a transient failure. Backoff queue is empty." />;
  return (
    <div className="table-wrap narrow">
      <table className="sessions">
        <caption className="sr-only">Retrying sessions</caption>
        <thead><tr>
          <th scope="col">Issue</th><th scope="col">Attempt</th><th scope="col">Next retry</th><th scope="col">Error</th>
        </tr></thead>
        <tbody>
          {rows.map((r) => {
            const secs = r.due_at ? (new Date(r.due_at).getTime() - Date.now()) / 1000 : NaN;
            const next = !Number.isFinite(secs) ? '—' : secs > 0 ? 'in ' + dur(secs) : 'due now';
            const startupPhase = r.startup_failure?.phase ? `startup_phase=${r.startup_failure.phase}` : '';
            const kindLine = [r.kind, startupPhase].filter(Boolean).join(' · ');
            return (
              <tr key={r.issue_id}>
                <td className="issue-cell">
                  <div className="issue">
                    <IssueLink row={r} />
                    <span className="wp" title={r.startup_failure?.error || kindLine}>{kindLine}</span>
                  </div>
                </td>
                <td data-label="Attempt"><span className="attempt"><span className="bd" />attempt {r.attempt}</span></td>
                <td data-label="Next retry"><span className="runtime tnum">{next}</span><div className="dim">backoff</div></td>
                <td data-label="Error"><div className="upd err" title={r.error}>{r.error}</div></td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

// ── blocked table ────────────────────────────────────────────────────────────
function BlockedTable({ rows }) {
  if (!rows.length) return <Empty title="Nothing blocked" sub="No claims need operator attention." />;
  return (
    <div className="table-wrap narrow">
      <table className="sessions">
        <caption className="sr-only">Blocked claims</caption>
        <thead><tr>
          <th scope="col">Issue</th><th scope="col">State</th><th scope="col">Waiting</th><th scope="col">Detail</th>
        </tr></thead>
        <tbody>
          {rows.map((r) => (
            <tr key={r.issue_id}>
              <td className="issue-cell">
                <div className="issue">
                  <IssueLink row={r} />
                  <span className="wp">{r.workspace_path}{r.session_id ? ` · ${r.session_id}` : ''}</span>
                </div>
              </td>
              <td data-label="State"><Badge kind="blocked">{r.state}</Badge>{r.method ? <div className="dim">{r.method}</div> : null}</td>
              <td data-label="Waiting"><span className="runtime tnum">{sinceISO(r.blocked_at)}</span><div className="dim">runtime {rowRuntime(r)}</div></td>
              <td data-label="Detail"><div className="upd" title={r.error}>{r.error}</div><div className="dim">{compact(r.tokens?.total_tokens)} claim tokens</div></td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function CompletedUsageTable({ rows }) {
  if (!rows.length) return <Empty title="No completed usage yet" sub="Completed-session token totals appear after clean handoffs in this process." />;
  return (
    <div className="table-wrap narrow">
      <table className="sessions">
        <caption className="sr-only">Completed session usage</caption>
        <thead><tr>
          <th scope="col">Issue</th><th scope="col">Outcome</th><th scope="col">Runtime</th><th scope="col" className="r">Tokens</th>
        </tr></thead>
        <tbody>
          {rows.map((r) => (
            <tr key={`${r.issue_id}-${r.session_id}-${r.completed_at || r.outcome}`}>
              <td className="issue-cell">
                <div className="issue">
                  <IssueLink row={r} />
                  <span className="wp">workflow {workflowProfile(r)}{r.session_id ? ` · ${r.session_id}` : ''}</span>
                </div>
              </td>
              <td data-label="Outcome"><Badge kind="done">{r.outcome || 'completed'}</Badge><div className="dim">{r.agent_model || r.agent_provider || 'unknown'}</div></td>
              <td data-label="Runtime"><span className="runtime tnum">{dur(r.runtime_seconds)}</span><div className="dim">{sinceISO(r.completed_at)} ago</div></td>
              <td className="tok" data-label="Tokens"><b>{compact(r.tokens?.total_tokens)}</b><div className="dim">{compact(r.tokens?.input_tokens)} in / {compact(r.tokens?.output_tokens)} out</div></td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

// ── topbar (shared by loading + loaded states) ───────────────────────────────
function Topbar({ status, generatedAt, version, onRefresh, settings }) {
  const pill = { live: 'live', offline: 'offline', connecting: 'conn' }[status];
  const pillLabel = { live: 'Live', offline: 'Disconnected', connecting: 'Connecting…' }[status];
  return (
    <div className="topbar">
      <div className="brand">
        <div className="brand-mark"><span className="gm">a<i className="caret" /></span></div>
        <div className="brand-name">aiops</div>
        {version ? <span className="brand-ver" title="worker release">{version}</span> : null}
      </div>
      <span className="crumb">GET <b>/api/v1/state</b></span>
      <div className="spacer" />
      {generatedAt ? <span className="meta-inline">updated {sinceISO(generatedAt)} ago</span> : null}
      <span className={'live-pill ' + pill}><span className="dot" />{pillLabel}</span>
      <button type="button" className="icon-btn" aria-label="Refresh now" title="Refresh now" onClick={onRefresh}>{I.refresh}</button>
      <SettingsControl settings={settings} />
    </div>
  );
}

// Reconcile roll-up bucket descriptions — single source of truth, used both as
// the hover title (sighted mouse users) and as sr-only text inside each cell
// (screen-reader / keyboard users), since `title` on a non-focusable element is
// reachable by neither. The visible label/count/chips already carry the data;
// these explain why each bucket is informational vs worth inspecting.
const STOPPED_DESC = 'Runs reaped by reconcile after making progress — worth inspecting, not a guaranteed success.';
const HANDOFF_DESC = 'Runs the reconcile loop stopped after the agent had already completed its handoff (opened its PR / wrote back to the tracker) — finished before being reaped; informational, not an error.';
const NO_HANDOFF_DESC = 'Clean runner exits that left the issue active with no guarded handoff — the worker will continue normally, but the run did not deliver tracker progress.';

// ── main app ─────────────────────────────────────────────────────────────────
export default function App() {
  const settings = useSettings();
  const [state, setState] = useState(null);
  const [error, setError] = useState(null);
  const [loadedAt, setLoadedAt] = useState(null);
  const [, setTick] = useState(0);
  const mounted = useRef(true);

  const loadState = useCallback(async () => {
    try {
      const response = await fetch(stateAPIURL(), { headers: { Accept: 'application/json' } });
      if (!response.ok) throw new Error(`GET /api/v1/state returned ${response.status}`);
      const payload = await response.json();
      if (!mounted.current) return;
      setState(payload);
      setError(null);
      setLoadedAt(new Date());
    } catch (err) {
      if (mounted.current) setError(err instanceof Error ? err.message : String(err));
    }
  }, []);

  useEffect(() => {
    mounted.current = true;
    loadState();
    const timer = window.setInterval(loadState, REFRESH_MS);
    return () => {
      mounted.current = false;
      window.clearInterval(timer);
    };
  }, [loadState]);

  // Tick once a second so relative times ("updated Xs ago", running runtimes)
  // advance smoothly instead of jumping on each 5s data refresh.
  useEffect(() => {
    const t = window.setInterval(() => setTick((v) => v + 1), 1000);
    return () => window.clearInterval(t);
  }, []);

  const status = error ? 'offline' : loadedAt ? 'live' : 'connecting';

  // Before the first successful fetch: a slim shell rather than a misleading
  // "idle" render (which would claim the worker is healthy and empty).
  if (!state) {
    return (
      <div className="app">
        <Topbar status={status} generatedAt={null} onRefresh={loadState} settings={settings} />
        <div className="title-row">
          <div>
            <h1 className="page-title">Worker <em>status</em></h1>
            <p className="page-sub">{error ? 'Could not reach the worker — retrying every 5s.' : 'Connecting to the worker state API…'}</p>
          </div>
        </div>
        {error ? <div className="err-banner" role="alert">GET /api/v1/state — {error}</div> : null}
        <div className="kpi-row">
          {['Running', 'Retrying', 'Blocked', 'Delivered'].map((label) => (
            <div className="kpi" key={label}>
              <div className="kpi-label"><span className="kpi-dot" />{label}</div>
              <div className="skel" style={{ height: 'var(--kpi-size)', width: '2.4ch', margin: '5px 0 2px' }} aria-hidden="true" />
              <div className="kpi-sub">{error ? 'unavailable' : 'loading…'}</div>
            </div>
          ))}
        </div>
      </div>
    );
  }

  const s = state;
  const c = s.counts || {};
  const ct = s.codex_totals || {};
  const rl = s.rate_limits;
  const total = (c.running || 0) + (c.retrying || 0) + (c.blocked || 0);
  const pollSec = Math.round((s.poll_interval_ms || 0) / 1000);
  // Worker default runtime/provider (agent.default). The model is resolved
  // per-run by the agent and rendered in the Running table.
  const agentDefault = s.agent_default || 'unknown';
  const budget = budgetLabel(s.budget_guardrails);
  const maxConc = s.max_concurrent_agents ?? '—';
  const byState = s.max_concurrent_agents_by_state;
  const byStateStr = byState && Object.keys(byState).length
    ? ' · ' + Object.entries(byState).map(([k, v]) => `${k} ${v}`).join(', ')
    : '';
  const stopped = s.reconcile_stopped_with_progress || [];
  const handoff = s.agent_handoff_reconcile_stopped || [];
  const noHandoff = s.active_success_no_handoff || [];
  const deliveredRecent = c.agent_handoff_reconcile_stopped || 0;
  const deliveredTotal = c.agent_handoff_reconcile_stopped_total || 0;
  const deliveredFlag = [
    c.reconcile_stopped_with_progress > 0 ? `${c.reconcile_stopped_with_progress} stopped w/ progress` : '',
    c.active_success_no_handoff > 0 ? `${c.active_success_no_handoff} no handoff` : '',
  ].filter(Boolean).join(' · ') || null;
  const online = status === 'live';

  return (
    <div className="app">
      {/* polite live region — announces connection + workload changes only.
          Excludes the per-second clock so it doesn't spam. */}
      <div className="sr-only" role="status" aria-live="polite">
        {online ? 'Connected, live.' : status === 'offline' ? 'Disconnected.' : 'Connecting.'}{' '}
        {total > 0
          ? `${total} ${total === 1 ? 'claim' : 'claims'} in flight: ${c.running} running, ${c.retrying} retrying, ${c.blocked} blocked.`
          : 'Worker idle, nothing in flight.'}
      </div>

      <Topbar status={status} generatedAt={s.generated_at} version={s.version} onRefresh={loadState} settings={settings} />

      {error ? <div className="err-banner" role="alert">Showing last snapshot — refresh failed: {error}</div> : null}

      {/* title */}
      <div className="title-row">
        <div>
          <h1 className="page-title">Worker <em>status</em></h1>
          <p className="page-sub">
            {total > 0
              ? <>Read-only view of <b>{total}</b> {total === 1 ? 'claim' : 'claims'} in flight — {c.running} running, {c.retrying} retrying, {c.blocked} blocked.</>
              : <>The worker is healthy and <b>idle</b> — polling with nothing in flight.</>}
          </p>
        </div>
        <div className="title-meta">
          <div className="row"><span>poll</span><b>every {pollSec}s</b></div>
          <div className="row"><span>concurrency</span><b>{maxConc} max{byStateStr}</b></div>
          <div className="row"><span>default agent</span><b>{agentDefault}</b></div>
          <div className="row"><span>claim budget</span><b>{budget}</b></div>
        </div>
      </div>

      {/* KPI strip */}
      <div className="kpi-row">
        <Kpi label="Running" value={c.running || 0} sub={`${maxConc} max concurrent`} status={c.running ? 'running' : null} />
        <Kpi label="Retrying" value={c.retrying || 0} sub="in backoff queue" status={c.retrying ? 'retry' : null} />
        <Kpi label="Blocked" value={c.blocked || 0} sub="needs operator attention" status={c.blocked ? 'blocked' : null} />
        <Kpi
          label="Delivered"
          value={deliveredRecent}
          sub={`${compact(deliveredTotal)} state handoffs lifetime · ${compact(c.completed_total)} completed exits`}
          flag={deliveredFlag}
          status="done"
        />
      </div>

      {/* codex totals strip (process lifetime; may include earlier issues) */}
      <div className="meta-strip">
        <div className="meta-cell"><div className="meta-k">Process input tokens</div><div className="meta-v tnum">{compact(ct.input_tokens)}</div><div className="meta-sub">process lifetime</div></div>
        <div className="meta-cell"><div className="meta-k">Process output tokens</div><div className="meta-v tnum">{compact(ct.output_tokens)}</div><div className="meta-sub">process lifetime</div></div>
        <div className="meta-cell"><div className="meta-k">Process total tokens</div><div className="meta-v tnum">{compact(ct.total_tokens)}</div><div className="meta-sub">may include older issues</div></div>
        <div className="meta-cell"><div className="meta-k">Process runtime</div><div className="meta-v tnum">{dur(ct.seconds_running)}</div><div className="meta-sub">cumulative agent time</div></div>
      </div>

      {/* Live work (Running → Retrying → Blocked, matching the KPI order) in
          the wider left column; system context (Rate limits + the conditional
          Reconcile roll-up) stacks in the narrower right column. Grouping all
          three in-flight buckets in one column keeps attention items together
          and directly under the matching KPIs, instead of scattering them
          above and below the reference data; collapses to one column ≤980px
          (see styles.css). */}
      <div className="body-grid">
        <div className="body-main">
          {/* running */}
          <div className="panel">
            <div className="panel-head">
              <div className="panel-title"><span className="accent-stroke" />Running<Badge kind="running">{c.running || 0}</Badge></div>
              <div className="panel-meta"><b>{c.running || 0}</b> / {maxConc} concurrent</div>
            </div>
            <RunningTable rows={s.running || []} />
          </div>

          {/* retrying */}
          <div className="panel">
            <div className="panel-head">
              <div className="panel-title"><span className="accent-stroke" />Retrying<Badge kind="retry">{c.retrying || 0}</Badge></div>
              <div className="panel-meta">backoff queue</div>
            </div>
            <RetryTable rows={s.retrying || []} />
          </div>

          {/* blocked */}
          <div className="panel">
            <div className="panel-head">
              <div className="panel-title"><span className="accent-stroke" />Blocked<Badge kind="blocked">{c.blocked || 0}</Badge></div>
              <div className="panel-meta">needs attention</div>
            </div>
            <BlockedTable rows={s.blocked || []} />
          </div>
        </div>

        <div className="body-side">
          {/* rate limits — raw Codex snapshot, may be null */}
          <div className="panel">
            <div className="panel-head">
              <div className="panel-title"><span className="accent-stroke" />Rate limits</div>
              <div className="panel-meta">{rl ? <>Codex usage snapshot{rl.limit_name ? <> · <b>{rl.limit_name}</b></> : null}</> : 'no snapshot yet'}</div>
            </div>
            <RateLimits rl={rl} />
          </div>

          <div className="panel">
            <div className="panel-head">
              <div className="panel-title"><span className="accent-stroke" />Completed usage<Badge kind="done">{(s.completed_session_usage || []).length}</Badge></div>
              <div className="panel-meta">current process</div>
            </div>
            <CompletedUsageTable rows={s.completed_session_usage || []} />
          </div>

          {/* Reconcile roll-up — Stopped-with-progress (#557), Agent-handoff
              (#617), and active-success/no-handoff (#988) folded into one compact strip; each
              bucket's explanatory prose is exposed via a hover title + an
              sr-only span so it costs no visible vertical budget. Hidden when
              all buckets are empty. */}
          {(stopped.length > 0 || handoff.length > 0 || noHandoff.length > 0) ? (
            <div className="panel">
              <div className="panel-head">
                <div className="panel-title"><span className="accent-stroke" />Reconcile roll-up</div>
                <div className="panel-meta">handoff and continuation outcomes</div>
              </div>
              <div className="rollup-grid">
                {noHandoff.length > 0 ? (
                  <div className="rollup-cell" title={NO_HANDOFF_DESC}>
                    <span className="rollup-k"><span className="kpi-dot retry" />No handoff</span>
                    <span className="rollup-v"><b>{c.active_success_no_handoff}</b> this window<span className="tot">· {c.active_success_no_handoff_total} total</span></span>
                    <span className="rollup-ids">{noHandoff.map((id) => <span key={id} className="tier-badge">{id}</span>)}</span>
                    <span className="sr-only">{NO_HANDOFF_DESC}</span>
                  </div>
                ) : null}
                {stopped.length > 0 ? (
                  <div className="rollup-cell" title={STOPPED_DESC}>
                    <span className="rollup-k"><span className="kpi-dot retry" />Stopped w/ progress</span>
                    <span className="rollup-v"><b>{c.reconcile_stopped_with_progress}</b> this window<span className="tot">· {c.reconcile_stopped_with_progress_total} total</span></span>
                    <span className="rollup-ids">{stopped.map((id) => <span key={id} className="tier-badge">{id}</span>)}</span>
                    <span className="sr-only">{STOPPED_DESC}</span>
                  </div>
                ) : null}
                {handoff.length > 0 ? (
                  <div className="rollup-cell" title={HANDOFF_DESC}>
                    <span className="rollup-k"><span className="kpi-dot done" />Agent handoff</span>
                    <span className="rollup-v"><b>{c.agent_handoff_reconcile_stopped}</b> this window<span className="tot">· {c.agent_handoff_reconcile_stopped_total} total</span></span>
                    <span className="rollup-ids">{handoff.map((id) => <span key={id} className="tier-badge">{id}</span>)}</span>
                    <span className="sr-only">{HANDOFF_DESC}</span>
                  </div>
                ) : null}
              </div>
            </div>
          ) : null}
        </div>
      </div>
    </div>
  );
}
