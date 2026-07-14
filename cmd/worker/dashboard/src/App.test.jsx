import { fireEvent, render, screen, within } from '@testing-library/react';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import App, { stateAPIURL } from './App';

const iso = (msAgo) => new Date(Date.now() - msAgo).toISOString();
const unixIn = (s) => Math.floor(Date.now() / 1000) + s;

// Shaped exactly like GET /api/v1/state (internal/stateapi · StateResponse).
function busyState(overrides = {}) {
  return {
    generated_at: iso(0),
    poll_interval_ms: 20000,
    agent_default: 'codex-app-server',
    max_concurrent_agents: 3,
    max_concurrent_agents_by_state: { 'In Progress': 3 },
    counts: {
      running: 2, retrying: 1, blocked: 1,
      completed: 14, completed_total: 137,
      reconcile_stopped_with_progress: 1, reconcile_stopped_with_progress_total: 4,
      agent_handoff_reconcile_stopped: 1, agent_handoff_reconcile_stopped_total: 9,
      active_success_no_handoff: 1, active_success_no_handoff_total: 6,
    },
    running: [
      {
        issue_id: '9f2c8a', issue_identifier: 'MT-614', state: 'In Progress',
        session_id: 'thread-7af3-turn-3', turn_count: 6, last_event: 'turn_completed',
        last_message: 'Wired counter into reconciler.',
        started_at: iso(372000), runtime_seconds: 372, last_event_at: iso(12000),
        workspace_path: '/tmp/aiops_workspaces/MT-614',
        tokens: { input_tokens: 286400, output_tokens: 41200, total_tokens: 327600 },
        codex_app_server_pid: 48213,
        agent_provider: 'codex-app-server', agent_model: 'gpt-5.3-codex-spark',
        workflow_source: 'file', workflow_path: '/srv/reviewer/WORKFLOW.md',
      },
    ],
    retrying: [
      {
        issue_id: '5d3177', issue_identifier: 'MT-613', attempt: 2,
        due_at: new Date(Date.now() + 46000).toISOString(),
        error: 'context deadline exceeded', kind: 'runner_error',
        startup_failure: { phase: 'thread/start', error: 'codex app-server read timeout after 5000ms' },
      },
    ],
    blocked: [
      {
        issue_id: '8e9041', issue_identifier: 'MT-590', state: 'Blocked',
        blocked_at: iso(5340000), session_id: 'thread-2d51-turn-2',
        workspace_path: '/tmp/aiops_workspaces/MT-590', method: 'agent_blocked',
        runtime_seconds: 918,
        tokens: { input_tokens: 44000, output_tokens: 6100, total_tokens: 50100 },
        error: 'Waiting on KMS key-grant approval.',
      },
    ],
    completed_session_usage: [
      {
        issue_id: 'aa0112', issue_identifier: 'MT-612', state: 'Done',
        session_id: 'thread-done-turn-5', workflow_source: 'file', workflow_path: '/srv/maker/WORKFLOW.md',
        agent_provider: 'codex-app-server', agent_model: 'gpt-5.3-codex-spark',
        tokens: { input_tokens: 102000, output_tokens: 18000, total_tokens: 120000 },
        runtime_seconds: 2400, completed_at: iso(60000), outcome: 'completed',
      },
      {
        issue_id: 'aa0113', issue_identifier: 'MT-613F', state: 'In Progress',
        session_id: 'thread-failed-turn-2', workflow_source: 'file', workflow_path: '/srv/maker/WORKFLOW.md',
        agent_provider: 'codex-app-server', agent_model: 'gpt-5.6-sol',
        tokens: { input_tokens: 21000, output_tokens: 3000, total_tokens: 25000 },
        runtime_seconds: 120, completed_at: iso(30000), outcome: 'failed',
      },
      {
        issue_id: 'aa0114', issue_identifier: 'MT-614R', state: 'In Review',
        session_id: 'thread-reconcile-turn-1', workflow_source: 'file', workflow_path: '/srv/reviewer/WORKFLOW.md',
        agent_provider: 'codex-app-server', agent_model: 'gpt-5.6-sol',
        tokens: { input_tokens: 11000, output_tokens: 1000, total_tokens: 12000 },
        runtime_seconds: 60, completed_at: iso(15000), outcome: 'reconcile_ineligible',
      },
    ],
    budget_guardrails: { max_tokens_per_claim: 20000000, max_runtime_seconds_per_claim: 7200 },
    completed: ['aa0112', 'aa0245', 'aa0388'],
    reconcile_stopped_with_progress: ['bb0190'],
    agent_handoff_reconcile_stopped: ['cc0271'],
    active_success_no_handoff: ['dd0442'],
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
    active_success_no_handoff: 0, active_success_no_handoff_total: 5,
  },
  running: [], retrying: [], blocked: [], completed_session_usage: [],
  completed: [], reconcile_stopped_with_progress: [], agent_handoff_reconcile_stopped: [], active_success_no_handoff: [],
  budget_guardrails: {},
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

  it('renders the Worker status title and delivered handoff KPI', async () => {
    render(<App />);
    expect(await screen.findByRole('heading', { name: /Worker status/i })).toBeTruthy();

    const delivered = screen.getByText('Delivered').closest('.kpi');
    expect(within(delivered).getByText('1')).toBeTruthy();
    expect(within(delivered).getByText(/9 state handoffs lifetime/)).toBeTruthy();
    expect(within(delivered).getByText(/137 completed exits/)).toBeTruthy();
    expect(within(delivered).getByText(/1 no handoff/)).toBeTruthy();
    expect(screen.queryByText(/^Completed$/)).toBeNull();

    // process token totals strip (5,443,400 → 5.4M) and a compacted running token cell.
    expect(screen.getByText('Process total tokens')).toBeTruthy();
    expect(screen.getByText('may include older issues')).toBeTruthy();
    expect(screen.getByText('5.4M')).toBeTruthy();
    expect(screen.getByText('286k')).toBeTruthy();
    expect(screen.getByText('20M tokens/claim · 2h 0m/claim')).toBeTruthy();
    expect(screen.getByText('Ended usage')).toBeTruthy();
    expect(screen.getByText('MT-612')).toBeTruthy();
    expect(screen.getByText('120k')).toBeTruthy();
    expect(screen.getByText('failed').classList.contains('failed')).toBe(true);
    expect(screen.getByText('reconcile_ineligible').classList.contains('blocked')).toBe(true);
  });

  it('links each issue id to its per-issue detail endpoint', async () => {
    render(<App />);
    const link = await screen.findByRole('link', { name: 'MT-614' });
    expect(link.getAttribute('href')).toBe('/api/v1/MT-614');
    expect(screen.getByText('Wired counter into reconciler.')).toBeTruthy();
  });

  it('shows the active agent model per running claim and the worker default provider', async () => {
    const { container } = render(<App />);
    await screen.findByRole('link', { name: 'MT-614' });
    const modelCell = container.querySelector('td[data-label="Model"]');
    expect(modelCell).toBeTruthy();
    expect(modelCell.textContent).toContain('gpt-5.3-codex-spark');
    expect(modelCell.textContent).toContain('codex-app-server'); // provider sub-line
    // worker default provider in the title meta; model is resolved per-run.
    const titleMeta = container.querySelector('.title-meta');
    expect(titleMeta.textContent).toContain('default agentcodex-app-server');
    expect(titleMeta.textContent).not.toContain('model unknown');
  });

  it('renders a missing agent model as "unknown", never blank', async () => {
    current = busyState({
      running: [{
        issue_id: 'no-model', issue_identifier: 'MT-700', state: 'In Progress',
        session_id: 'thread-x', turn_count: 1, last_event: 'turn_started',
        started_at: iso(1000), last_event_at: iso(1000), workspace_path: '/tmp/w',
        tokens: { input_tokens: 0, output_tokens: 0, total_tokens: 0 },
      }],
    });
    const { container } = render(<App />);
    await screen.findByRole('link', { name: 'MT-700' });
    const modelCell = container.querySelector('td[data-label="Model"]');
    expect(modelCell.textContent).toContain('unknown');
    expect(modelCell.textContent.trim()).not.toBe('');
  });

  it('shows which WORKFLOW.md (the profile) produced each running claim', async () => {
    const { container } = render(<App />);
    await screen.findByRole('link', { name: 'MT-614' });
    const detail = container.querySelector('tbody .wp');
    expect(detail).toBeTruthy();
    expect(detail.textContent).toContain('workflow /srv/reviewer/WORKFLOW.md');
  });

  it('falls back to the workflow source when no path is reported (default workflow)', async () => {
    current = busyState({
      running: [{
        issue_id: 'wf-default', issue_identifier: 'MT-983D', state: 'In Progress',
        session_id: 'thread-x', turn_count: 1, last_event: 'turn_started',
        started_at: iso(1000), last_event_at: iso(1000), workspace_path: '/tmp/w',
        tokens: { input_tokens: 0, output_tokens: 0, total_tokens: 0 },
        workflow_source: 'default',
      }],
    });
    const { container } = render(<App />);
    await screen.findByRole('link', { name: 'MT-983D' });
    const detail = container.querySelector('tbody .wp');
    expect(detail.textContent).toContain('workflow default');
  });

  it('renders a missing workflow profile as "unknown", never blank', async () => {
    current = busyState({
      running: [{
        issue_id: 'no-wf', issue_identifier: 'MT-983', state: 'In Progress',
        session_id: 'thread-x', turn_count: 1, last_event: 'turn_started',
        started_at: iso(1000), last_event_at: iso(1000), workspace_path: '/tmp/w',
        tokens: { input_tokens: 0, output_tokens: 0, total_tokens: 0 },
      }],
    });
    const { container } = render(<App />);
    await screen.findByRole('link', { name: 'MT-983' });
    const detail = container.querySelector('tbody .wp');
    expect(detail.textContent).toContain('workflow unknown');
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

  it('labels observed Codex window_duration_mins windows with human-scale units', async () => {
    current = busyState({
      rate_limits: {
        limit_name: 'codex', plan_type: 'pro',
        primary: { used_percent: 31, window_duration_mins: 300, resets_at: unixIn(41) },
        secondary: { used_percent: 12, window_duration_mins: 10080, resets_at: unixIn(1800) },
      },
    });
    render(<App />);
    await screen.findByText('Primary window');

    expect(screen.getByText(/5-hour window/)).toBeTruthy();
    expect(screen.getByText(/7-day window/)).toBeTruthy();
  });

  it('formats long rate-limit reset countdowns with day units', async () => {
    const dateNow = vi.spyOn(Date, 'now').mockReturnValue(Date.UTC(2026, 0, 2, 3, 4, 5));
    try {
      current = busyState({
        rate_limits: {
          limit_name: 'codex', plan_type: 'pro',
          primary: { used_percent: 31, window_minutes: 300, resets_at: unixIn(3 * 3600 + 51 * 60) },
          secondary: { used_percent: 12, window_minutes: 10080, resets_at: unixIn(49 * 3600 + 10 * 60) },
        },
      });
      render(<App />);
      await screen.findByText('Primary window');

      expect(screen.getByText(/5-hour window · resets in 3h 51m/)).toBeTruthy();
      expect(screen.getByText(/7-day window · resets in 2d 1h/)).toBeTruthy();
    } finally {
      dateNow.mockRestore();
    }
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

  it('surfaces the worker build version as the top-bar chip when stamped', async () => {
    // The worker stamps `version` into /api/v1/state (resolveVersion); the topbar
    // threads s.version into the .brand-ver chip next to the mark. Mutation seam:
    // dropping version={s.version} from the Topbar call drops this chip.
    current = busyState({ version: 'v1.4.2' });
    const { container } = render(<App />);
    await screen.findByRole('heading', { name: /Worker status/i });

    const ver = container.querySelector('.brand-ver');
    expect(ver).not.toBeNull();
    expect(ver.textContent).toBe('v1.4.2');
  });

  it('omits the version chip when the build is unstamped (no version field)', async () => {
    // omitempty drops `version` for un-stamped dev builds — render no empty chip.
    current = busyState(); // default fixture carries no `version`
    const { container } = render(<App />);
    await screen.findByRole('heading', { name: /Worker status/i });
    expect(container.querySelector('.brand-ver')).toBeNull();
  });

  it('shows the no-snapshot empty state when rate_limits is null', async () => {
    current = idleState;
    const { container } = render(<App />);
    await screen.findByText('No rate-limit data yet');
    expect(container.querySelector('pre.rate-raw')).toBeNull();
  });

  it('explains that ended usage includes every terminal run path', async () => {
    current = idleState;
    render(<App />);
    expect(await screen.findByText('No ended usage yet')).toBeTruthy();
    expect(screen.getByText('Session token totals appear after completed, failed, or reconcile-cancelled runs in this process.')).toBeTruthy();
  });

  it('groups all live-work panels in the main column in KPI order (Worker Status v2)', async () => {
    const { container } = render(<App />);
    await screen.findByRole('heading', { name: /Worker status/i });

    // v1 scattered the buckets: Retrying + Blocked sat in a full-width .grid-2
    // row at the very bottom, below the reference gauges. v2 groups all three
    // in-flight buckets in the main column, directly under the matching KPIs.
    // Read only the title's own text nodes so the structural assertion is not
    // coupled to the fixture's count-badge values.
    const titleText = (el) => [...el.childNodes]
      .filter((n) => n.nodeType === Node.TEXT_NODE).map((n) => n.textContent).join('');
    const mainTitles = [...container.querySelectorAll('.body-main .panel-title')].map(titleText);
    expect(mainTitles).toEqual(['Running', 'Retrying', 'Blocked']);
    const sideTitles = [...container.querySelectorAll('.body-side .panel-title')].map(titleText);
    expect(sideTitles).toEqual(['Rate limits', 'Ended usage', 'Reconcile roll-up']);
    expect(container.querySelector('.grid-2')).toBeNull(); // the bottom row is gone

    // tokens header renders as one line: bold "Tokens" + dim "in / out" suffix
    const tokHeader = container.querySelector('th.tok-h');
    expect(tokHeader).not.toBeNull();
    expect(tokHeader.textContent).toBe('Tokens in / out');
    expect(tokHeader.querySelector('.th-sub').textContent).toBe(' in / out');
  });

  it('marks the Running table with overflow-safe layout hooks', async () => {
    const { container } = render(<App />);
    await screen.findByRole('link', { name: 'MT-614' });

    const runningPanel = [...container.querySelectorAll('.body-main .panel')]
      .find((panel) => panel.querySelector('.panel-title')?.textContent.includes('Running'));
    expect(runningPanel).toBeTruthy();
    expect(runningPanel.querySelector('.running-table-wrap')).not.toBeNull();
    expect(runningPanel.querySelector('table.running-sessions')).not.toBeNull();
  });

  it('keeps reconcile-stopped details in the roll-up under the delivered KPI', async () => {
    render(<App />);
    const rollup = (await screen.findByText('Reconcile roll-up')).closest('.panel');

    const noHandoffCell = within(rollup).getByText('No handoff').closest('.rollup-cell');
    expect(within(noHandoffCell).getByText('dd0442')).toBeTruthy();
    expect(noHandoffCell.querySelector('.rollup-v b').textContent).toBe('1');
    expect(noHandoffCell.querySelector('.rollup-v .tot').textContent).toBe('· 6 total');
    expect(noHandoffCell.textContent).toContain('no guarded handoff');

    // all outcome buckets render as compact rows inside the single roll-up panel
    const stoppedCell = within(rollup).getByText('Stopped w/ progress').closest('.rollup-cell');
    expect(within(stoppedCell).getByText('bb0190')).toBeTruthy();
    // v2: the window count is the serif-numeric anchor (<b>), the lifetime
    // total is the dimmed .tot suffix — not one undifferentiated mono run.
    expect(stoppedCell.querySelector('.rollup-v b').textContent).toBe('1');
    expect(stoppedCell.querySelector('.rollup-v .tot').textContent).toBe('· 4 total');
    // sr-only explanation present (the prose lives in textContent, not just title)
    expect(stoppedCell.textContent).toContain('worth inspecting');

    const handoffCell = within(rollup).getByText('Agent handoff').closest('.rollup-cell');
    expect(within(handoffCell).getByText('cc0271')).toBeTruthy();
    expect(handoffCell.textContent).toContain('9 total'); // lifetime total
    expect(handoffCell.textContent).toContain('informational, not an error');

    // the handoff details stay in the roll-up; the headline KPI uses a
    // plain-language Delivered label instead of duplicating this bucket label.
    expect(screen.getAllByText('Agent handoff')).toHaveLength(1);
  });

  it('hides the reconcile roll-up when all buckets are empty', async () => {
    current = idleState;
    render(<App />);
    await screen.findByRole('heading', { name: /Worker status/i });
    expect(screen.queryByText('Reconcile roll-up')).toBeNull();
    expect(screen.queryByText('Agent handoff')).toBeNull();
    expect(screen.queryByText('Stopped w/ progress')).toBeNull();
    expect(screen.queryByText('No handoff')).toBeNull();
  });

  it('shows the retry queue with attempt count, backoff kind, and startup phase', async () => {
    render(<App />);
    expect(await screen.findByRole('link', { name: 'MT-613' })).toBeTruthy();
    expect(screen.getByText('attempt 2')).toBeTruthy();
    const kind = screen.getByText('runner_error · startup_phase=thread/start');
    expect(kind).toBeTruthy();
    expect(kind.getAttribute('title')).toBe('codex app-server read timeout after 5000ms');
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
