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

function StateBadge({ value, type = 'default' }) {
  const colors = {
    default: 'bg-slate-800 text-slate-300 border-slate-700/50',
    active: 'bg-emerald-900/40 text-emerald-300 border-emerald-700/40',
    warning: 'bg-amber-900/40 text-amber-300 border-amber-700/40',
    danger: 'bg-red-900/40 text-red-300 border-red-700/40',
  };
  return (
    <span className={`inline-flex items-center px-2.5 py-0.5 rounded-full text-xs font-semibold border ${colors[type]}`}>
      {value}
    </span>
  );
}

function MetricCard({ label, value, hint }) {
  return (
    <article className="p-4 rounded-2xl bg-slate-900/80 border border-slate-700/20 flex flex-col">
      <span className="text-xs font-semibold uppercase tracking-wide text-slate-400">{label}</span>
      <strong className="block my-2 text-3xl font-bold tracking-tight">{value}</strong>
      <small className="text-slate-500 text-xs">{hint}</small>
    </article>
  );
}

function Panel({ title, subtitle, children }) {
  return (
    <section className="rounded-2xl bg-slate-900/80 border border-slate-700/20 p-5 overflow-hidden">
      <div className="flex justify-between gap-4 items-baseline mb-4 flex-wrap">
        <h2 className="text-base font-semibold tracking-tight">{title}</h2>
        {subtitle && <span className="text-slate-500 text-sm">{subtitle}</span>}
      </div>
      {children}
    </section>
  );
}

function DataTable({ columns, rows, emptyText }) {
  return (
    <div className="overflow-x-auto">
      <table className="w-full border-collapse text-sm">
        <thead>
          <tr>
            {columns.map((col) => (
              <th
                key={col}
                className="text-left text-xs font-semibold uppercase tracking-wider text-slate-400 border-b border-slate-700/30 pb-3 pt-0 pr-2 first:pl-0"
              >
                {col}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.length === 0 ? (
            <tr>
              <td
                colSpan={columns.length}
                className="text-center text-slate-500 py-6 border-b border-slate-700/15"
              >
                {emptyText}
              </td>
            </tr>
          ) : (
            rows
          )}
        </tbody>
      </table>
    </div>
  );
}

function RunningRow({ row }) {
  const runtimeSecs = row.started_at
    ? (Date.now() - new Date(row.started_at).getTime()) / 1000
    : 0;

  const badgeType =
    row.state === 'running' ? 'active'
    : row.state === 'blocked' ? 'danger'
    : 'default';

  return (
    <tr className="border-b border-slate-700/15 last:border-0">
      <td className="py-3 pr-2 align-top">
        <div className="font-semibold">
          <a href={jsonDetailsPath(row)}>{issueLabel(row)}</a>
        </div>
        {row.workspace_path && (
          <div className="text-xs text-slate-500 mt-0.5 truncate max-w-[12rem]">{row.workspace_path}</div>
        )}
      </td>
      <td className="py-3 pr-2 align-top">
        <StateBadge value={row.state || 'running'} type={badgeType} />
      </td>
      <td className="py-3 pr-2 align-top font-mono text-xs text-slate-300">
        {row.session_id || 'n/a'}
      </td>
      <td className="py-3 pr-2 align-top tabular-nums text-slate-300">
        {formatRuntime(runtimeSecs)} / {formatCount(row.turn_count)}
      </td>
      <td className="py-3 pr-2 align-top max-w-[18rem]">
        <div className="truncate text-slate-200">{row.last_message || row.last_event || 'n/a'}</div>
        <div className="text-xs text-slate-500 mt-0.5">{formatDate(row.last_event_at)}</div>
      </td>
      <td className="py-3 pr-0 align-top tabular-nums text-right text-slate-300">
        <div>Total {formatCount(row.tokens?.total_tokens)}</div>
        <div className="text-xs text-slate-500">
          In {formatCount(row.tokens?.input_tokens)} / Out {formatCount(row.tokens?.output_tokens)}
        </div>
      </td>
    </tr>
  );
}

function BlockedRow({ row }) {
  return (
    <tr className="border-b border-slate-700/15 last:border-0">
      <td className="py-3 pr-2 align-top">
        <a href={jsonDetailsPath(row)} className="font-semibold">{issueLabel(row)}</a>
      </td>
      <td className="py-3 pr-2 align-top">
        <StateBadge value={row.state || 'blocked'} type="danger" />
      </td>
      <td className="py-3 pr-2 align-top font-mono text-xs text-slate-300">
        {row.session_id || 'n/a'}
      </td>
      <td className="py-3 pr-2 align-top text-slate-300">{formatDate(row.blocked_at)}</td>
      <td className="py-3 pr-2 align-top text-slate-300">{row.method || 'n/a'}</td>
      <td className="py-3 pr-0 align-top text-red-400 max-w-[16rem] truncate">{row.error || 'n/a'}</td>
    </tr>
  );
}

function RetryRow({ row }) {
  return (
    <tr className="border-b border-slate-700/15 last:border-0">
      <td className="py-3 pr-2 align-top">
        <a href={jsonDetailsPath(row)} className="font-semibold">{issueLabel(row)}</a>
      </td>
      <td className="py-3 pr-2 align-top tabular-nums text-amber-400">#{formatCount(row.attempt)}</td>
      <td className="py-3 pr-2 align-top text-slate-300">{formatDate(row.due_at)}</td>
      <td className="py-3 pr-0 align-top text-red-400 max-w-[22rem] truncate">{row.error || 'n/a'}</td>
    </tr>
  );
}

export default function App() {
  const [state, setState] = useState(null);
  const [error, setError] = useState(null);
  const [loadedAt, setLoadedAt] = useState(null);

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

  const totals = state?.codex_totals || {};
  const counts = state?.counts || {};
  const totalRuntime = Number(totals.seconds_running || 0);
  const maxAgents = state?.max_concurrent_agents ?? '—';

  return (
    <main className="w-full max-w-[1280px] mx-auto px-4 py-8">
      {/* Hero */}
      <header className="flex justify-between gap-6 items-start mb-6 p-7 border border-slate-400/25 rounded-3xl bg-slate-900/70 shadow-[0_24px_80px_rgba(0,0,0,0.28)]">
        <div>
          <p className="text-sky-400 uppercase text-xs tracking-widest font-bold mb-2">
            aiops-platform
          </p>
          <h1 className="text-4xl sm:text-5xl font-bold tracking-tight leading-none mb-3">
            Operations Dashboard
          </h1>
          <p className="text-slate-400 max-w-2xl">
            Human-readable runtime state from{' '}
            <a href="/api/v1/state">/api/v1/state</a>. Refreshes every{' '}
            {REFRESH_MS / 1000}s.
          </p>
        </div>
        <div className="flex flex-col items-end gap-2 shrink-0">
          <span
            className={`inline-flex items-center gap-2 px-3 py-1.5 rounded-full text-sm font-bold border ${
              error
                ? 'bg-red-900/20 text-red-300 border-red-700/40'
                : 'bg-emerald-900/20 text-emerald-300 border-emerald-700/30'
            }`}
          >
            <span className="w-2 h-2 rounded-full bg-current" />
            {error ? 'Offline' : 'Live'}
          </span>
          {maxAgents !== '—' && (
            <span className="text-xs text-slate-500">
              Max {maxAgents} concurrent agents
            </span>
          )}
        </div>
      </header>

      {/* Error banner */}
      {error && (
        <div className="mb-6 px-5 py-4 rounded-2xl bg-red-900/30 border border-red-700/40 text-red-300">
          <strong>Error:</strong> {error}
        </div>
      )}

      {/* Metrics row */}
      <section className="grid grid-cols-2 sm:grid-cols-3 lg:grid-cols-5 gap-3.5 mb-6">
        <MetricCard
          label="Running"
          value={formatCount(counts.running)}
          hint="Active sessions"
        />
        <MetricCard
          label="Retrying"
          value={formatCount(counts.retrying)}
          hint="In retry backoff"
        />
        <MetricCard
          label="Blocked"
          value={formatCount(counts.blocked)}
          hint="Awaiting operator"
        />
        <MetricCard
          label="Completed"
          value={formatCount(counts.completed_total ?? counts.completed)}
          hint={`Recent window: ${formatCount(counts.completed)}`}
        />
        <MetricCard
          label="Failed"
          value={formatCount(counts.failed_total ?? counts.failed)}
          hint={`Suppression set: ${formatCount(counts.failed)}`}
        />
      </section>

      {/* Token / runtime metrics row */}
      <section className="grid grid-cols-2 sm:grid-cols-4 gap-3.5 mb-6">
        <MetricCard
          label="Total tokens"
          value={formatCount(totals.total_tokens)}
          hint={`In ${formatCount(totals.input_tokens)} / Out ${formatCount(totals.output_tokens)}`}
        />
        <MetricCard
          label="Runtime"
          value={formatRuntime(totalRuntime)}
          hint="Completed + active Codex time"
        />
        <MetricCard
          label="Poll interval"
          value={`${formatCount(state?.poll_interval_ms)}ms`}
          hint="Tracker poll cadence"
        />
        <MetricCard
          label="Snapshot"
          value={loadedAt ? loadedAt.toLocaleTimeString() : '—'}
          hint={state?.generated_at ? `API: ${formatDate(state.generated_at)}` : 'Fetching…'}
        />
      </section>

      {/* Rate limits */}
      <Panel title="Rate limits" subtitle="Latest upstream snapshot">
        <pre className="overflow-auto rounded-xl p-4 bg-slate-950/70 text-violet-300 text-xs leading-relaxed">
          {state?.rate_limits
            ? JSON.stringify(state.rate_limits, null, 2)
            : 'No rate-limit snapshot reported.'}
        </pre>
      </Panel>

      {/* Running sessions */}
      <div className="mt-4">
        <Panel title="Running sessions" subtitle="Current active issue work">
          <DataTable
            columns={['Issue', 'State', 'Session', 'Runtime / turns', 'Last update', 'Tokens']}
            rows={(state?.running || []).map((row) => (
              <RunningRow key={row.issue_id} row={row} />
            ))}
            emptyText="No active sessions."
          />
        </Panel>
      </div>

      {/* Blocked sessions */}
      <div className="mt-4">
        <Panel title="Blocked sessions" subtitle="Operator input and error indicators">
          <DataTable
            columns={['Issue', 'State', 'Session', 'Blocked at', 'Method', 'Error']}
            rows={(state?.blocked || []).map((row) => (
              <BlockedRow key={row.issue_id} row={row} />
            ))}
            emptyText="No blocked sessions."
          />
        </Panel>
      </div>

      {/* Retry queue */}
      <div className="mt-4">
        <Panel title="Retry queue" subtitle="Backoff delays before redispatch">
          <DataTable
            columns={['Issue', 'Attempt', 'Due at', 'Error']}
            rows={(state?.retrying || []).map((row) => (
              <RetryRow key={row.issue_id} row={row} />
            ))}
            emptyText="No issues are currently backing off."
          />
        </Panel>
      </div>

      <footer className="mt-8 text-center text-xs text-slate-600">
        Generated: {formatDate(state?.generated_at)} · Last refreshed:{' '}
        {formatDate(loadedAt)} · Poll interval: {formatCount(state?.poll_interval_ms)}ms
      </footer>
    </main>
  );
}
