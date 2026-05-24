import React, { useEffect, useState } from 'react';

const REFRESH_MS = 5000;

function formatCount(value) {
  return new Intl.NumberFormat().format(Number(value || 0));
}

function formatDate(value) {
  if (!value) return 'n/a';
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return String(value);
  return date.toLocaleString();
}

function formatRuntime(seconds) {
  const total = Math.max(0, Math.floor(Number(seconds || 0)));
  const hours = Math.floor(total / 3600);
  const minutes = Math.floor((total % 3600) / 60);
  const secs = total % 60;
  if (hours > 0) return `${hours}h ${minutes}m ${secs}s`;
  if (minutes > 0) return `${minutes}m ${secs}s`;
  return `${secs}s`;
}

function issueLabel(row) {
  return row.issue_identifier || row.issue_id || 'unknown issue';
}

function jsonDetailsPath(row) {
  const id = row.issue_identifier || row.issue_id;
  return id ? `/api/v1/${encodeURIComponent(id)}` : '/api/v1/state';
}

// compactSession mirrors the TUI's compact_session_id: long IDs render as 4…6.
function compactSession(id) {
  if (!id) return 'n/a';
  if (id.length > 10) return `${id.slice(0, 4)}…${id.slice(-6)}`;
  return id;
}

function toNumber(value) {
  if (value === null || value === undefined || value === '') return null;
  const n = Number(value);
  return Number.isNaN(n) ? null : n;
}

function resolveInitialTheme() {
  if (typeof window === 'undefined') return 'dark';
  try {
    const stored = window.localStorage.getItem('theme');
    if (stored === 'light' || stored === 'dark') return stored;
  } catch {
    /* localStorage unavailable */
  }
  return window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
}

function useTheme() {
  const [theme, setTheme] = useState(resolveInitialTheme);
  useEffect(() => {
    const root = document.documentElement;
    root.classList.toggle('dark', theme === 'dark');
    try {
      window.localStorage.setItem('theme', theme);
    } catch {
      /* ignore persistence failures */
    }
  }, [theme]);
  const toggle = () => setTheme((t) => (t === 'dark' ? 'light' : 'dark'));
  return [theme, toggle];
}

function ThemeToggle({ theme, onToggle }) {
  const next = theme === 'dark' ? 'light' : 'dark';
  return (
    <button
      type="button"
      onClick={onToggle}
      aria-label={`Switch to ${next} theme`}
      title={`Switch to ${next} theme`}
      className="inline-flex items-center justify-center w-9 h-9 rounded-full bg-inset border border-line text-muted hover:text-fg transition-colors"
    >
      {theme === 'dark' ? (
        <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
          <circle cx="12" cy="12" r="4" />
          <path d="M12 2v2M12 20v2M4.93 4.93l1.41 1.41M17.66 17.66l1.41 1.41M2 12h2M20 12h2M6.34 17.66l-1.41 1.41M19.07 4.93l-1.41 1.41" />
        </svg>
      ) : (
        <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
          <path d="M21 12.79A9 9 0 1 1 11.21 3 7 7 0 0 0 21 12.79z" />
        </svg>
      )}
    </button>
  );
}

const BADGE_TONES = {
  default: 'bg-inset text-muted border-line',
  active: 'bg-good-bg text-good border-good-line',
  warning: 'bg-warn-bg text-warn border-warn-line',
  danger: 'bg-danger-bg text-danger border-danger-line',
};

function StateBadge({ value, type = 'default' }) {
  return (
    <span className={`inline-flex items-center px-2.5 py-0.5 rounded-full text-xs font-semibold border ${BADGE_TONES[type]}`}>
      {value}
    </span>
  );
}

const METRIC_TONES = {
  default: { border: 'border-line', value: 'text-fg' },
  good: { border: 'border-good-line', value: 'text-good' },
  warn: { border: 'border-warn-line', value: 'text-warn' },
  danger: { border: 'border-danger-line', value: 'text-danger' },
};

function MetricCard({ label, value, hint, tone = 'default', loading = false }) {
  const toneClass = METRIC_TONES[tone] || METRIC_TONES.default;
  return (
    <article className={`p-4 rounded-2xl bg-surface border flex flex-col ${toneClass.border}`}>
      <span className="text-xs font-semibold uppercase tracking-wide text-muted">{label}</span>
      {loading ? (
        <span className="block my-2 h-8 w-20 rounded bg-muted/20 animate-pulse" aria-hidden="true" />
      ) : (
        <strong className={`block my-2 text-3xl font-bold tracking-tight ${toneClass.value}`}>{value}</strong>
      )}
      <small className="text-muted text-xs">{hint}</small>
    </article>
  );
}

function Panel({ title, subtitle, children }) {
  return (
    <section className="rounded-2xl bg-surface border border-line p-5 overflow-hidden">
      <div className="flex justify-between gap-4 items-baseline mb-4 flex-wrap">
        <h2 className="text-base font-semibold tracking-tight">{title}</h2>
        {subtitle && <span className="text-muted text-sm">{subtitle}</span>}
      </div>
      {children}
    </section>
  );
}

// ResponsiveTable renders a table on md+ screens and a stacked card list on
// mobile, driven by a single column config so the two views never drift.
function ResponsiveTable({ columns, rows, emptyText }) {
  if (!rows || rows.length === 0) {
    return <p className="text-center text-faint py-6">{emptyText}</p>;
  }
  const rowKey = (row, i) => row.issue_id || row.issue_identifier || i;
  return (
    <>
      <div className="hidden md:block overflow-x-auto">
        <table className="w-full border-collapse text-sm min-w-[640px]">
          <thead>
            <tr>
              {columns.map((col) => (
                <th
                  key={col.header}
                  className={`text-xs font-semibold uppercase tracking-wider text-muted border-b border-line pb-3 pr-3 first:pl-0 last:pr-0 ${
                    col.align === 'right' ? 'text-right' : 'text-left'
                  }`}
                >
                  {col.header}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {rows.map((row, i) => (
              <tr key={rowKey(row, i)} className="border-b border-line last:border-0">
                {columns.map((col) => (
                  <td
                    key={col.header}
                    className={`py-3 pr-3 align-top first:pl-0 last:pr-0 ${
                      col.align === 'right' ? 'text-right tabular-nums' : ''
                    }`}
                  >
                    {col.cell(row)}
                  </td>
                ))}
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      <ul className="md:hidden flex flex-col gap-3">
        {rows.map((row, i) => (
          <li key={rowKey(row, i)} className="rounded-xl border border-line bg-inset p-4">
            <div className="mb-3">{columns[0].cell(row)}</div>
            <dl className="grid grid-cols-2 gap-x-4 gap-y-2.5">
              {columns.slice(1).map((col) => (
                <div key={col.header} className="flex flex-col min-w-0">
                  <dt className="text-[11px] font-semibold uppercase tracking-wider text-faint">{col.header}</dt>
                  <dd className="text-sm text-fg mt-0.5">{col.cell(row)}</dd>
                </div>
              ))}
            </dl>
          </li>
        ))}
      </ul>
    </>
  );
}

function UsageBar({ remaining, limit }) {
  if (remaining === null || limit === null || limit <= 0) return null;
  const ratio = Math.max(0, Math.min(1, remaining / limit));
  const pct = Math.round(ratio * 100);
  const tone = ratio > 0.5 ? 'bg-good' : ratio > 0.2 ? 'bg-warn' : 'bg-danger';
  return (
    <div
      className="mt-2 h-1.5 w-full rounded-full bg-black/10 dark:bg-white/10 overflow-hidden"
      role="progressbar"
      aria-valuenow={pct}
      aria-valuemin={0}
      aria-valuemax={100}
    >
      <div className={`h-full rounded-full ${tone}`} style={{ width: `${pct}%` }} />
    </div>
  );
}

function RateLimitBucket({ label, bucket }) {
  const remaining = bucket ? toNumber(bucket.remaining) : null;
  const limit = bucket ? toNumber(bucket.limit) : null;

  let headline = 'n/a';
  if (remaining !== null && limit !== null) headline = `${formatCount(remaining)} / ${formatCount(limit)}`;
  else if (remaining !== null) headline = `${formatCount(remaining)} left`;
  else if (limit !== null) headline = `limit ${formatCount(limit)}`;

  let reset = null;
  if (bucket) {
    for (const key of ['reset_in_seconds', 'resetInSeconds', 'reset_at', 'resetAt', 'resets_at']) {
      if (bucket[key] !== undefined && bucket[key] !== null) {
        const rv = bucket[key];
        reset = typeof rv === 'number' ? `resets in ${Math.round(rv)}s` : `resets ${rv}`;
        break;
      }
    }
  }

  return (
    <div className="rounded-xl bg-inset border border-line p-4">
      <div className="text-xs font-semibold uppercase tracking-wide text-muted">{label}</div>
      <div className="mt-1 text-lg font-semibold tabular-nums text-fg">{headline}</div>
      <UsageBar remaining={remaining} limit={limit} />
      {reset && <div className="mt-2 text-xs text-muted">{reset}</div>}
    </div>
  );
}

function formatCredits(credits) {
  if (!credits || typeof credits !== 'object') return { text: 'n/a', tone: 'text-muted' };
  if (credits.unlimited) return { text: 'Unlimited', tone: 'text-good' };
  if (credits.has_credits) {
    const balance = toNumber(credits.balance);
    return { text: balance !== null ? balance.toFixed(2) : 'Available', tone: 'text-good' };
  }
  return { text: 'None', tone: 'text-danger' };
}

function RateLimits({ rateLimits, loading }) {
  if (loading) {
    return <div className="h-24 rounded-xl bg-muted/20 animate-pulse" aria-hidden="true" />;
  }
  if (!rateLimits || typeof rateLimits !== 'object') {
    return <p className="text-faint text-sm py-2">No rate-limit snapshot reported.</p>;
  }

  const tier = rateLimits.limit_id || rateLimits.limit_name || 'unknown tier';
  const hasShape =
    rateLimits.primary !== undefined ||
    rateLimits.secondary !== undefined ||
    rateLimits.credits !== undefined;

  // Unknown payload shape — keep the data visible rather than guessing layout.
  if (!hasShape) {
    return (
      <pre className="overflow-auto rounded-xl p-4 bg-inset text-fg text-xs leading-relaxed">
        {JSON.stringify(rateLimits, null, 2)}
      </pre>
    );
  }

  const credits = formatCredits(rateLimits.credits);

  return (
    <div className="flex flex-col gap-4">
      <StateBadge value={tier} type="default" />
      <div className="grid grid-cols-1 sm:grid-cols-3 gap-3">
        <RateLimitBucket label="Primary" bucket={rateLimits.primary} />
        <RateLimitBucket label="Secondary" bucket={rateLimits.secondary} />
        <div className="rounded-xl bg-inset border border-line p-4">
          <div className="text-xs font-semibold uppercase tracking-wide text-muted">Credits</div>
          <div className={`mt-1 text-lg font-semibold tabular-nums ${credits.tone}`}>{credits.text}</div>
        </div>
      </div>
    </div>
  );
}

export default function App() {
  const [theme, toggleTheme] = useTheme();
  const [state, setState] = useState(null);
  const [error, setError] = useState(null);
  const [loadedAt, setLoadedAt] = useState(null);
  const [nowTs, setNowTs] = useState(() => Date.now());

  useEffect(() => {
    let cancelled = false;
    async function loadState() {
      try {
        const response = await fetch('/api/v1/state', { headers: { Accept: 'application/json' } });
        if (!response.ok) throw new Error(`GET /api/v1/state returned ${response.status}`);
        const payload = await response.json();
        if (!cancelled) {
          setState(payload);
          setError(null);
          setLoadedAt(new Date());
        }
      } catch (err) {
        if (!cancelled) setError(err instanceof Error ? err.message : String(err));
      }
    }
    loadState();
    const timer = window.setInterval(loadState, REFRESH_MS);
    return () => {
      cancelled = true;
      window.clearInterval(timer);
    };
  }, []);

  // Tick once a second so running-session runtimes count up smoothly instead
  // of jumping on each 5s data refresh.
  useEffect(() => {
    const tick = window.setInterval(() => setNowTs(Date.now()), 1000);
    return () => window.clearInterval(tick);
  }, []);

  const loading = state === null && !error;
  const totals = state?.codex_totals || {};
  const counts = state?.counts || {};
  const totalRuntime = Number(totals.seconds_running || 0);
  const maxAgents = state?.max_concurrent_agents ?? '—';

  const completed = Number(counts.completed_total ?? counts.completed ?? 0);
  const failed = Number(counts.failed_total ?? counts.failed ?? 0);
  const blocked = Number(counts.blocked ?? 0);
  const retrying = Number(counts.retrying ?? 0);

  const status = error ? 'offline' : loadedAt ? 'live' : 'connecting';
  const statusStyles = {
    live: 'bg-good-bg text-good border-good-line',
    offline: 'bg-danger-bg text-danger border-danger-line',
    connecting: 'bg-inset text-muted border-line',
  };
  const statusLabel = { live: 'Live', offline: 'Offline', connecting: 'Connecting…' };

  const runningColumns = [
    {
      header: 'Issue',
      cell: (row) => (
        <div>
          <div className="font-semibold">
            <a href={jsonDetailsPath(row)}>{issueLabel(row)}</a>
          </div>
          {row.workspace_path && (
            <div className="text-xs text-faint mt-0.5 truncate max-w-[12rem]" title={row.workspace_path}>
              {row.workspace_path}
            </div>
          )}
        </div>
      ),
    },
    {
      header: 'State',
      cell: (row) => (
        <StateBadge
          value={row.state || 'running'}
          type={row.state === 'blocked' ? 'danger' : row.state === 'running' ? 'active' : 'default'}
        />
      ),
    },
    {
      header: 'Session',
      cell: (row) => (
        <span className="font-mono text-xs text-muted" title={row.session_id || undefined}>
          {compactSession(row.session_id)}
        </span>
      ),
    },
    {
      header: 'Runtime / turns',
      align: 'right',
      cell: (row) => {
        const runtimeSecs = row.started_at ? (nowTs - new Date(row.started_at).getTime()) / 1000 : 0;
        return (
          <span className="tabular-nums text-muted">
            {formatRuntime(runtimeSecs)} / {formatCount(row.turn_count)}
          </span>
        );
      },
    },
    {
      header: 'Last update',
      cell: (row) => (
        <div className="max-w-[18rem]">
          <div className="truncate text-fg">{row.last_message || row.last_event || 'n/a'}</div>
          <div className="text-xs text-faint mt-0.5">{formatDate(row.last_event_at)}</div>
        </div>
      ),
    },
    {
      header: 'Tokens',
      align: 'right',
      cell: (row) => (
        <div className="tabular-nums text-muted">
          <div>Total {formatCount(row.tokens?.total_tokens)}</div>
          <div className="text-xs text-faint">
            In {formatCount(row.tokens?.input_tokens)} / Out {formatCount(row.tokens?.output_tokens)}
          </div>
        </div>
      ),
    },
  ];

  const blockedColumns = [
    {
      header: 'Issue',
      cell: (row) => (
        <a href={jsonDetailsPath(row)} className="font-semibold">
          {issueLabel(row)}
        </a>
      ),
    },
    { header: 'State', cell: (row) => <StateBadge value={row.state || 'blocked'} type="danger" /> },
    {
      header: 'Session',
      cell: (row) => (
        <span className="font-mono text-xs text-muted" title={row.session_id || undefined}>
          {compactSession(row.session_id)}
        </span>
      ),
    },
    { header: 'Blocked at', cell: (row) => <span className="text-muted">{formatDate(row.blocked_at)}</span> },
    { header: 'Method', cell: (row) => <span className="text-muted">{row.method || 'n/a'}</span> },
    {
      header: 'Error',
      cell: (row) => (
        <span className="text-danger block max-w-[16rem] truncate" title={row.error || undefined}>
          {row.error || 'n/a'}
        </span>
      ),
    },
  ];

  const retryColumns = [
    {
      header: 'Issue',
      cell: (row) => (
        <a href={jsonDetailsPath(row)} className="font-semibold">
          {issueLabel(row)}
        </a>
      ),
    },
    { header: 'Attempt', align: 'right', cell: (row) => <span className="tabular-nums text-warn">#{formatCount(row.attempt)}</span> },
    { header: 'Due at', cell: (row) => <span className="text-muted">{formatDate(row.due_at)}</span> },
    {
      header: 'Error',
      cell: (row) => (
        <span className="text-danger block max-w-[22rem] truncate" title={row.error || undefined}>
          {row.error || 'n/a'}
        </span>
      ),
    },
  ];

  return (
    <main className="w-full max-w-[1280px] mx-auto px-4 py-8">
      {/* Hero */}
      <header className="flex justify-between gap-6 items-start flex-wrap mb-6 p-7 border border-line-strong rounded-3xl bg-surface shadow-[0_24px_80px_rgba(15,23,42,0.12)] dark:shadow-[0_24px_80px_rgba(0,0,0,0.28)]">
        <div>
          <p className="text-accent uppercase text-xs tracking-widest font-bold mb-2">
            aiops-platform
          </p>
          <h1 className="text-4xl sm:text-5xl font-bold tracking-tight leading-none mb-3">
            Operations Dashboard
          </h1>
          <p className="text-muted max-w-2xl">
            Human-readable runtime state from{' '}
            <a href="/api/v1/state">/api/v1/state</a>. Refreshes every{' '}
            {REFRESH_MS / 1000}s.
          </p>
        </div>
        <div className="flex flex-col items-end gap-2 shrink-0">
          <div className="flex items-center gap-2" aria-live="polite">
            <span
              className={`inline-flex items-center gap-2 px-3 py-1.5 rounded-full text-sm font-bold border ${statusStyles[status]}`}
            >
              <span
                className={`w-2 h-2 rounded-full bg-current ${status === 'live' ? 'animate-pulse' : ''}`}
              />
              {statusLabel[status]}
            </span>
            <ThemeToggle theme={theme} onToggle={toggleTheme} />
          </div>
          {maxAgents !== '—' && (
            <span className="text-xs text-muted">
              Max {maxAgents} concurrent agents
            </span>
          )}
        </div>
      </header>

      {/* Error banner */}
      {error && (
        <div role="alert" className="mb-6 px-5 py-4 rounded-2xl bg-danger-bg border border-danger-line text-danger">
          <strong>Error:</strong> {error}
        </div>
      )}

      {/* Metrics row */}
      <section className="grid grid-cols-2 sm:grid-cols-3 lg:grid-cols-5 gap-3.5 mb-6">
        <MetricCard label="Running" value={formatCount(counts.running)} hint="Active sessions" loading={loading} />
        <MetricCard
          label="Retrying"
          value={formatCount(retrying)}
          hint="In retry backoff"
          tone={retrying > 0 ? 'warn' : 'default'}
          loading={loading}
        />
        <MetricCard
          label="Blocked"
          value={formatCount(blocked)}
          hint="Awaiting operator"
          tone={blocked > 0 ? 'danger' : 'default'}
          loading={loading}
        />
        <MetricCard
          label="Completed"
          value={formatCount(completed)}
          hint={`Recent window: ${formatCount(counts.completed)}`}
          tone={completed > 0 ? 'good' : 'default'}
          loading={loading}
        />
        <MetricCard
          label="Failed"
          value={formatCount(failed)}
          hint={`Suppression set: ${formatCount(counts.failed)}`}
          tone={failed > 0 ? 'danger' : 'default'}
          loading={loading}
        />
      </section>

      {/* Token / runtime metrics row */}
      <section className="grid grid-cols-2 sm:grid-cols-4 gap-3.5 mb-6">
        <MetricCard
          label="Total tokens"
          value={formatCount(totals.total_tokens)}
          hint={`In ${formatCount(totals.input_tokens)} / Out ${formatCount(totals.output_tokens)}`}
          loading={loading}
        />
        <MetricCard
          label="Runtime"
          value={formatRuntime(totalRuntime)}
          hint="Completed + active Codex time"
          loading={loading}
        />
        <MetricCard
          label="Poll interval"
          value={`${formatCount(state?.poll_interval_ms)}ms`}
          hint="Tracker poll cadence"
          loading={loading}
        />
        <MetricCard
          label="Snapshot"
          value={loadedAt ? loadedAt.toLocaleTimeString() : '—'}
          hint={state?.generated_at ? `API: ${formatDate(state.generated_at)}` : 'Fetching…'}
          loading={loading}
        />
      </section>

      {/* Rate limits */}
      <Panel title="Rate limits" subtitle="Latest upstream snapshot">
        <RateLimits rateLimits={state?.rate_limits} loading={loading} />
      </Panel>

      {/* Running sessions */}
      <div className="mt-4">
        <Panel title="Running sessions" subtitle="Current active issue work">
          <ResponsiveTable
            columns={runningColumns}
            rows={state?.running || []}
            emptyText={loading ? 'Loading…' : 'No active sessions.'}
          />
        </Panel>
      </div>

      {/* Blocked sessions */}
      <div className="mt-4">
        <Panel title="Blocked sessions" subtitle="Operator input and error indicators">
          <ResponsiveTable
            columns={blockedColumns}
            rows={state?.blocked || []}
            emptyText={loading ? 'Loading…' : 'No blocked sessions.'}
          />
        </Panel>
      </div>

      {/* Retry queue */}
      <div className="mt-4">
        <Panel title="Retry queue" subtitle="Backoff delays before redispatch">
          <ResponsiveTable
            columns={retryColumns}
            rows={state?.retrying || []}
            emptyText={loading ? 'Loading…' : 'No issues are currently backing off.'}
          />
        </Panel>
      </div>

      <footer className="mt-8 text-center text-xs text-faint">
        API snapshot: {formatDate(state?.generated_at)} · Last refreshed: {formatDate(loadedAt)}
      </footer>
    </main>
  );
}
