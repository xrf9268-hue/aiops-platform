import React, { useEffect, useMemo, useState } from 'react';
import { createRoot } from 'react-dom/client';
import './styles.css';

const REFRESH_MS = 5000;

function formatInt(value) {
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

function MetricCard({ label, value, hint }) {
  return (
    <article className="metric-card">
      <span>{label}</span>
      <strong>{value}</strong>
      <small>{hint}</small>
    </article>
  );
}

function EmptyRow({ children, colSpan }) {
  return (
    <tr className="empty-row">
      <td colSpan={colSpan}>{children}</td>
    </tr>
  );
}

function App() {
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

  const runningRuntime = useMemo(() => {
    const now = Date.now();
    return (state?.running || []).reduce((total, row) => {
      if (!row.started_at) return total;
      const started = new Date(row.started_at).getTime();
      return Number.isNaN(started) ? total : total + Math.max(0, (now - started) / 1000);
    }, 0);
  }, [state, loadedAt]);

  const totals = state?.codex_totals || {};
  const counts = state?.counts || {};
  const totalRuntime = Number(totals.seconds_running || 0) + runningRuntime;

  return (
    <main className="app-shell">
      <header className="hero">
        <div>
          <p className="eyebrow">aiops-platform</p>
          <h1>Operations Dashboard</h1>
          <p>
            Human-readable runtime state from the canonical <a href="/api/v1/state">/api/v1/state</a> API.
          </p>
        </div>
        <div className={error ? 'status status--error' : 'status status--ok'}>
          {error ? 'Snapshot unavailable' : 'Live snapshot'}
        </div>
      </header>

      {error ? <section className="error-panel"><strong>Health/error indicator:</strong> {error}</section> : null}

      <section className="metrics" aria-label="Runtime metrics">
        <MetricCard label="Running" value={formatInt(counts.running)} hint="Active sessions" />
        <MetricCard label="Retrying" value={formatInt(counts.retrying)} hint="Issues in retry backoff" />
        <MetricCard label="Blocked" value={formatInt(counts.blocked)} hint="Waiting for operator input" />
        <MetricCard label="Total tokens" value={formatInt(totals.total_tokens)} hint={`In ${formatInt(totals.input_tokens)} / Out ${formatInt(totals.output_tokens)}`} />
        <MetricCard label="Runtime" value={formatRuntime(totalRuntime)} hint="Completed plus active Codex time" />
      </section>

      <section className="panel">
        <div className="panel-heading">
          <h2>Rate limits</h2>
          <span>Latest upstream snapshot</span>
        </div>
        <pre>{state?.rate_limits ? JSON.stringify(state.rate_limits, null, 2) : 'No rate-limit snapshot reported.'}</pre>
      </section>

      <section className="panel">
        <div className="panel-heading">
          <h2>Running sessions</h2>
          <span>Current active issue work</span>
        </div>
        <table>
          <thead><tr><th>Issue</th><th>State</th><th>Session</th><th>Runtime / turns</th><th>Codex update</th><th>Tokens</th></tr></thead>
          <tbody>
            {(state?.running || []).length === 0 ? <EmptyRow colSpan={6}>No active sessions.</EmptyRow> : state.running.map((row) => (
              <tr key={row.issue_id}>
                <td><a href={jsonDetailsPath(row)}>{issueLabel(row)}</a></td>
                <td>{row.state || 'running'}</td>
                <td><code>{row.session_id || 'n/a'}</code></td>
                <td>{formatRuntime(row.started_at ? (Date.now() - new Date(row.started_at).getTime()) / 1000 : 0)} / {formatInt(row.turn_count)}</td>
                <td>{row.last_message || row.last_event || 'n/a'}<br /><small>{formatDate(row.last_codex_at)}</small></td>
                <td>Total {formatInt(row.tokens?.total_tokens)}<br /><small>In {formatInt(row.tokens?.input_tokens)} / Out {formatInt(row.tokens?.output_tokens)}</small></td>
              </tr>
            ))}
          </tbody>
        </table>
      </section>

      <section className="panel">
        <div className="panel-heading">
          <h2>Blocked sessions</h2>
          <span>Operator input and error indicators</span>
        </div>
        <table>
          <thead><tr><th>Issue</th><th>State</th><th>Session</th><th>Blocked at</th><th>Method</th><th>Error</th></tr></thead>
          <tbody>
            {(state?.blocked || []).length === 0 ? <EmptyRow colSpan={6}>No blocked sessions.</EmptyRow> : state.blocked.map((row) => (
              <tr key={row.issue_id}>
                <td><a href={jsonDetailsPath(row)}>{issueLabel(row)}</a></td>
                <td>{row.state || 'blocked'}</td>
                <td><code>{row.session_id || 'n/a'}</code></td>
                <td>{formatDate(row.blocked_at)}</td>
                <td>{row.method || 'n/a'}</td>
                <td>{row.error || 'n/a'}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </section>

      <section className="panel">
        <div className="panel-heading">
          <h2>Retry queue</h2>
          <span>Backoff delays before redispatch</span>
        </div>
        <table>
          <thead><tr><th>Issue</th><th>Attempt</th><th>Due at</th><th>Error</th></tr></thead>
          <tbody>
            {(state?.retrying || []).length === 0 ? <EmptyRow colSpan={4}>No issues are currently backing off.</EmptyRow> : state.retrying.map((row) => (
              <tr key={row.issue_id}>
                <td><a href={jsonDetailsPath(row)}>{issueLabel(row)}</a></td>
                <td>{formatInt(row.attempt)}</td>
                <td>{formatDate(row.due_at)}</td>
                <td>{row.error || 'n/a'}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </section>

      <footer>
        Generated: {formatDate(state?.generated_at)} · Last refreshed: {formatDate(loadedAt)} · Poll interval: {formatInt(state?.poll_interval_ms)}ms
      </footer>
    </main>
  );
}

createRoot(document.getElementById('root')).render(<App />);
