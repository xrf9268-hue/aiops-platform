import { fireEvent, render, screen, within } from '@testing-library/react';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import App, { stateAPIURL } from './App';

const iso = (msAgo) => new Date(Date.now() - msAgo).toISOString();
const unixIn = (s) => Math.floor(Date.now() / 1000) + s;

// Shaped exactly like GET /api/v1/state (cmd/worker/stateapi.go · apiStateResponse).
function busyState(overrides = {}) {
  return {
    generated_at: iso(0),
    poll_interval_ms: 20000,
    max_concurrent_agents: 3,
    max_concurrent_agents_by_state: { 'In Progress': 3 },
    counts: {
      running: 2, retrying: 1, blocked: 1,
      completed: 14, completed_total: 137,
      reconcile_stopped_with_progress: 1, reconcile_stopped_with_progress_total: 4,
      agent_handoff_reconcile_stopped: 1, agent_handoff_reconcile_stopped_total: 9,
    },
    running: [
      {
        issue_id: '9f2c8a', issue_identifier: 'MT-614', state: 'In Progress',
        session_id: 'thread-7af3-turn-3', turn_count: 6, last_event: 'turn_completed',
        last_message: 'Wired counter into reconciler.',
        started_at: iso(372000), last_event_at: iso(12000),
        workspace_path: '/tmp/aiops_workspaces/MT-614',
        tokens: { input_tokens: 286400, output_tokens: 41200, total_tokens: 327600 },
        codex_app_server_pid: 48213,
      },
    ],
    retrying: [
      {
        issue_id: '5d3177', issue_identifier: 'MT-613', attempt: 2,
        due_at: new Date(Date.now() + 46000).toISOString(),
        error: 'context deadline exceeded', kind: 'runner_error',
      },
    ],
    blocked: [
      {
        issue_id: '8e9041', issue_identifier: 'MT-590', state: 'Blocked',
        blocked_at: iso(5340000), session_id: 'thread-2d51-turn-2',
        workspace_path: '/tmp/aiops_workspaces/MT-590', method: 'agent_blocked',
        error: 'Waiting on KMS key-grant approval.',
      },
    ],
    completed: ['aa0112', 'aa0245', 'aa0388'],
    reconcile_stopped_with_progress: ['bb0190'],
    agent_handoff_reconcile_stopped: ['cc0271'],
    codex_totals: { input_tokens: 4812000, output_tokens: 631400, total_tokens: 5443400, seconds_running: 7338 },
    rate_limits: {
      limit_name: 'codex',
      primary: { used_percent: 31.0, window_minutes: 1, resets_at: unixIn(41) },
      secondary: { used_percent: 12.4, window_minutes: 60, resets_at: unixIn(1800) },
      plan_type: 'pro',
    },
    ...overrides,
  };
}

const idleState = {
  generated_at: iso(0),
  poll_interval_ms: 20000,
  max_concurrent_agents: 3,
  counts: {
    running: 0, retrying: 0, blocked: 0, completed: 9, completed_total: 132,
    reconcile_stopped_with_progress: 0, reconcile_stopped_with_progress_total: 3,
    agent_handoff_reconcile_stopped: 0, agent_handoff_reconcile_stopped_total: 8,
  },
  running: [], retrying: [], blocked: [],
  completed: [], reconcile_stopped_with_progress: [], agent_handoff_reconcile_stopped: [],
  codex_totals: { input_tokens: 2140000, output_tokens: 288900, total_tokens: 2428900, seconds_running: 4292 },
  rate_limits: null,
};

let current = busyState();
function stubFetchOK() {
  vi.stubGlobal('fetch', vi.fn(() => Promise.resolve({ ok: true, json: () => Promise.resolve(current) })));
}

beforeEach(() => {
  current = busyState();
  stubFetchOK();
  window.localStorage.clear();
  document.documentElement.removeAttribute('data-brand');
  document.documentElement.removeAttribute('data-theme');
  document.documentElement.removeAttribute('data-density');
  document.documentElement.classList.remove('theme-swapping');
});

describe('Worker status dashboard', () => {
  it('builds state API requests without URL credentials', () => {
    const url = stateAPIURL({
      origin: 'http://127.0.0.1:4000',
      href: 'http://aiops:state-token@127.0.0.1:4000/',
    });
    expect(url).toBe('http://127.0.0.1:4000/api/v1/state');
    expect(url).not.toContain('aiops:state-token@');
  });

  it('falls back to a relative state path when the origin is unavailable', () => {
    // file:// / sandboxed iframes report origin === 'null'.
    expect(stateAPIURL({ origin: 'null' })).toBe('/api/v1/state');
    expect(stateAPIURL({})).toBe('/api/v1/state');
  });

  it('renders the Worker status title and live KPI counts', async () => {
    render(<App />);
    expect(await screen.findByRole('heading', { name: /Worker status/i })).toBeTruthy();

    const completed = screen.getByText('Completed').closest('.kpi');
    expect(within(completed).getByText('14')).toBeTruthy();
    expect(within(completed).getByText(/137 lifetime/)).toBeTruthy();

    // codex token totals strip (5,443,400 → 5.4M) and a compacted running token cell.
    expect(screen.getByText('5.4M')).toBeTruthy();
    expect(screen.getByText('286k')).toBeTruthy();
  });

  it('links each issue id to its per-issue detail endpoint', async () => {
    render(<App />);
    const link = await screen.findByRole('link', { name: 'MT-614' });
    expect(link.getAttribute('href')).toBe('/api/v1/MT-614');
    expect(screen.getByText('Wired counter into reconciler.')).toBeTruthy();
  });

  it('renders Codex rate-limit windows as percent bars, not raw JSON', async () => {
    const { container } = render(<App />);
    await screen.findByText('Primary window');

    expect(container.querySelector('pre.rate-raw')).toBeNull();
    expect(screen.getByText('Secondary window')).toBeTruthy();
    expect(screen.getByText('pro')).toBeTruthy();
    const rateValues = [...container.querySelectorAll('.rate-v')].map((el) => el.textContent);
    expect(rateValues.some((t) => t.includes('31'))).toBe(true);
    expect(rateValues.some((t) => t.includes('12'))).toBe(true);
  });

  it('falls back to a raw JSON dump for an unrecognized rate-limit shape', async () => {
    // No used_percent windows → keep the data visible rather than mis-rendering it.
    current = busyState({ rate_limits: { limit_name: 'codex', primary: { remaining: 450, limit: 500 } } });
    const { container } = render(<App />);
    await screen.findByRole('heading', { name: /Worker status/i });

    const raw = container.querySelector('pre.rate-raw');
    expect(raw).not.toBeNull();
    expect(raw.textContent).toContain('"remaining": 450');
  });

  it('falls back to raw JSON when only some windows are percent-shaped (no silent drop)', async () => {
    // primary is the Codex percent shape, secondary is an unfamiliar shape:
    // render the whole snapshot raw rather than dropping secondary.
    current = busyState({
      rate_limits: {
        limit_name: 'codex',
        primary: { used_percent: 31, window_minutes: 1 },
        secondary: { remaining: 18, limit: 100 },
      },
    });
    const { container } = render(<App />);
    await screen.findByRole('heading', { name: /Worker status/i });

    const raw = container.querySelector('pre.rate-raw');
    expect(raw).not.toBeNull();
    expect(raw.textContent).toContain('"remaining": 18');
    // the percent-shaped primary must NOT be half-rendered as a window
    expect(container.querySelector('.rate-v')).toBeNull();
  });

  it('preserves extra rate-limit fields (e.g. credits) alongside percent windows', async () => {
    current = busyState({
      rate_limits: {
        limit_name: 'codex', plan_type: 'pro',
        primary: { used_percent: 31, window_minutes: 1, resets_at: unixIn(41) },
        secondary: { used_percent: 12, window_minutes: 60, resets_at: unixIn(1800) },
        credits: { has_credits: true, balance: 999 },
      },
    });
    const { container } = render(<App />);
    await screen.findByText('Primary window');

    expect(container.querySelectorAll('.rate-v')).toHaveLength(2); // windows still render
    const raw = container.querySelector('pre.rate-raw');
    expect(raw).not.toBeNull();
    expect(raw.textContent).toContain('"credits"');
    expect(raw.textContent).toContain('"balance": 999');
    expect(raw.textContent).not.toContain('used_percent'); // known keys aren't duplicated
  });

  it('shows the state endpoint crumb in the top bar and the tracker poll in the title meta', async () => {
    // busy fixture: poll_interval_ms=20000 (worker→tracker poll, in the title meta).
    // The one-screen redesign dropped the footer that restated the client refresh
    // cadence; the endpoint now lives in the top-bar crumb instead.
    const { container } = render(<App />);
    await screen.findByRole('heading', { name: /Worker status/i });

    const crumb = container.querySelector('.crumb');
    expect(crumb).not.toBeNull();
    expect(crumb.textContent).toContain('/api/v1/state');
    expect(screen.getByText('every 20s')).toBeTruthy(); // tracker poll, in the title meta
    expect(screen.queryByText(/refreshed every/)).toBeNull(); // footer was removed
  });

  it('shows the no-snapshot empty state when rate_limits is null', async () => {
    current = idleState;
    const { container } = render(<App />);
    await screen.findByText('No rate-limit data yet');
    expect(container.querySelector('pre.rate-raw')).toBeNull();
  });

  it('folds stopped-with-progress and agent-handoff into the reconcile roll-up, not extra KPIs', async () => {
    render(<App />);
    const rollup = (await screen.findByText('Reconcile roll-up')).closest('.panel');

    // both buckets render as compact rows inside the single roll-up panel
    const stoppedCell = within(rollup).getByText('Stopped w/ progress').closest('.rollup-cell');
    expect(within(stoppedCell).getByText('bb0190')).toBeTruthy();
    expect(stoppedCell.textContent).toContain('4 total'); // lifetime total
    // sr-only explanation present (the prose lives in textContent, not just title)
    expect(stoppedCell.textContent).toContain('worth inspecting');

    const handoffCell = within(rollup).getByText('Agent handoff').closest('.rollup-cell');
    expect(within(handoffCell).getByText('cc0271')).toBeTruthy();
    expect(handoffCell.textContent).toContain('9 total'); // lifetime total
    expect(handoffCell.textContent).toContain('informational, not an error');

    // neither bucket leaks into the lean 4-KPI strip
    expect(screen.getAllByText('Agent handoff')).toHaveLength(1);
  });

  it('hides the reconcile roll-up when both buckets are empty', async () => {
    current = idleState;
    render(<App />);
    await screen.findByRole('heading', { name: /Worker status/i });
    expect(screen.queryByText('Reconcile roll-up')).toBeNull();
    expect(screen.queryByText('Agent handoff')).toBeNull();
    expect(screen.queryByText('Stopped w/ progress')).toBeNull();
  });

  it('shows the retry queue with attempt count and backoff kind', async () => {
    render(<App />);
    expect(await screen.findByRole('link', { name: 'MT-613' })).toBeTruthy();
    expect(screen.getByText('attempt 2')).toBeTruthy();
    expect(screen.getByText('runner_error')).toBeTruthy();
    // the 2-line clamp can truncate long errors; the full text stays available
    // via a hover title so it is never lost.
    const err = screen.getByText('context deadline exceeded');
    expect(err.getAttribute('title')).toBe('context deadline exceeded');
  });

  it('switches brand from the settings popover and persists it', async () => {
    render(<App />);
    await screen.findByRole('heading', { name: /Worker status/i });
    expect(document.documentElement.dataset.brand).toBe('linen');

    fireEvent.click(screen.getByRole('button', { name: 'Display settings' }));
    fireEvent.click(screen.getByRole('button', { name: 'Anthropic' }));

    expect(document.documentElement.dataset.brand).toBe('anthropic');
    expect(window.localStorage.getItem('dashboard.brand')).toBe('anthropic');
  });

  it('reports a disconnected pill when the state fetch fails', async () => {
    vi.stubGlobal('fetch', vi.fn(() => Promise.reject(new Error('network down'))));
    render(<App />);
    expect(await screen.findByText('Disconnected')).toBeTruthy();
    expect(screen.getByRole('alert').textContent).toContain('network down');
  });

  it('matches the rate-limit panel render snapshot', async () => {
    render(<App />);
    await screen.findByText('Primary window');
    const panel = screen.getByText('Rate limits').closest('.panel');
    // Normalize the live "resets in …" countdown — it is derived from Date.now()
    // at render time, so leaving it in the snapshot makes the test flaky when the
    // wall clock crosses a second between fixture build and render.
    const html = panel.innerHTML.replace(/resets in [^<]*/g, 'resets in <relative>');
    expect(html).toMatchSnapshot();
  });
});
