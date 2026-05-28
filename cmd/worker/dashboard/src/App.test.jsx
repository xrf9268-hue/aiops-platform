import { render, screen, within } from '@testing-library/react';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import App, { stateAPIURL } from './App';

const sampleState = {
  generated_at: '2026-05-24T00:00:00.000Z',
  poll_interval_ms: 2000,
  max_concurrent_agents: 10,
  counts: { running: 2, retrying: 1, blocked: 1, completed_total: 142, completed: 8, failed_total: 3, failed: 1 },
  codex_totals: { input_tokens: 1234567, output_tokens: 234567, total_tokens: 1469134, seconds_running: 252 },
  rate_limits: {
    limit_id: 'pro-tier',
    primary: { remaining: 450, limit: 500, reset_in_seconds: 30 },
    secondary: { remaining: 18, limit: 100, reset_in_seconds: 600 },
    credits: { has_credits: true, balance: 999.0 },
  },
  running: [
    {
      issue_id: 'ENG-123', issue_identifier: 'ENG-123', state: 'running',
      session_id: 'abc1234567890def123456', turn_count: 7,
      last_message: 'analyzing repository structure',
      last_event: 'codex/event/task_started',
      started_at: '2026-05-23T23:58:30.000Z', last_event_at: '2026-05-23T23:59:56.000Z',
      tokens: { input_tokens: 1000, output_tokens: 234, total_tokens: 1234 },
    },
  ],
  blocked: [],
  retrying: [
    {
      issue_id: 'ENG-124',
      issue_identifier: 'ENG-124',
      attempt: 1,
      kind: 'quota_backoff',
      due_at: '2026-05-24T00:01:30.000Z',
      error: 'quota backoff',
    },
  ],
};

beforeEach(() => {
  vi.stubGlobal(
    'fetch',
    vi.fn(() => Promise.resolve({ ok: true, json: () => Promise.resolve(sampleState) })),
  );
  window.localStorage.clear();
  document.documentElement.classList.remove('dark');
});

describe('Operations Dashboard', () => {
  it('builds state API requests without URL credentials', () => {
    const url = stateAPIURL({
      origin: 'http://127.0.0.1:4000',
      href: 'http://aiops:state-token@127.0.0.1:4000/',
    });

    expect(url).toBe('http://127.0.0.1:4000/api/v1/state');
    expect(url).not.toContain('aiops:state-token@');
  });

  it('formats rate limits as cards instead of dumping raw JSON', async () => {
    const { container } = render(<App />);
    await screen.findByText('pro-tier');

    expect(container.querySelector('pre')).toBeNull();
    expect(screen.getByText('450 / 500')).toBeTruthy();
    expect(screen.getByText('18 / 100')).toBeTruthy();
    expect(screen.getByText('999.00')).toBeTruthy();
  });

  it('compacts long session ids to 4…6', async () => {
    render(<App />);
    const matches = await screen.findAllByText('abc1…123456');
    expect(matches.length).toBeGreaterThan(0);
  });

  it('tones the Failed metric as danger when greater than zero', async () => {
    render(<App />);
    await screen.findByText('pro-tier');

    const failedCard = screen.getByText('Failed').closest('article');
    const value = within(failedCard).getByText('3');
    expect(value.className).toContain('text-danger');
  });

  it('shows quota backoff kind in the retry queue', async () => {
    render(<App />);
    const kinds = await screen.findAllByText('quota_backoff');

    expect(kinds.length).toBeGreaterThan(0);
    expect(screen.getAllByText('ENG-124').length).toBeGreaterThan(0);
    expect(screen.getAllByText('quota backoff').length).toBeGreaterThan(0);
  });

  it('tones the Completed metric as good', async () => {
    render(<App />);
    await screen.findByText('pro-tier');

    const completedCard = screen.getByText('Completed').closest('article');
    const value = within(completedCard).getByText('142');
    expect(value.className).toContain('text-good');
  });

  it('renders a theme toggle defaulting from system preference', async () => {
    render(<App />);
    const toggle = await screen.findByRole('button', { name: /switch to .* theme/i });
    expect(toggle).toBeTruthy();
  });

  it('matches the rate-limit panel render snapshot', async () => {
    render(<App />);
    await screen.findByText('pro-tier');

    const panel = screen.getByText('Rate limits').closest('section');
    expect(panel.innerHTML).toMatchSnapshot();
  });
});
