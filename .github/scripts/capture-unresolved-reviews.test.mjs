import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';

import {
  buildFollowUpIssue,
  captureUnresolvedReviewThreads,
  extractPriorityLabel,
  loadRecentlyMergedPullNumbers,
  loadRepositoryIssuesForTracking,
  normalizeDiscussionPermalink,
  parseArgs,
  searchTermsForDiscussionPermalink,
  buildTrackingIssueIndex,
  verifyTrackingForActionableThreads,
} from './capture-unresolved-reviews.mjs';

const repository = {
  owner: 'xrf9268-hue',
  name: 'aiops-platform',
};

const pullRequest = {
  number: 106,
  title: 'feat: reconcile workspaces on worker startup',
  url: 'https://github.com/xrf9268-hue/aiops-platform/pull/106',
  reviewThreads: [
    {
      id: 'PRRT_kwDOExample1',
      isResolved: false,
      isOutdated: false,
      path: 'internal/worker/startup_reconcile.go',
      line: 42,
      originalLine: 40,
      comments: [
        {
          url: 'https://github.com/xrf9268-hue/aiops-platform/pull/106#discussion_r3252794498',
          author: 'chatgpt-codex-connector',
          body: '[P1] Startup reconcile silently falls back to defaults when workflow config is missing.',
        },
      ],
    },
    {
      id: 'PRRT_kwDOExample2',
      isResolved: false,
      isOutdated: true,
      path: 'internal/worker/startup_reconcile.go',
      line: null,
      originalLine: 41,
      comments: [
        {
          url: 'https://github.com/xrf9268-hue/aiops-platform/pull/106#discussion_r3252781671',
          author: 'chatgpt-codex-connector',
          body: '[P2] This outdated comment should not file an issue.',
        },
      ],
    },
    {
      id: 'PRRT_kwDOExample3',
      isResolved: true,
      isOutdated: false,
      path: 'internal/worker/startup_reconcile.go',
      line: 45,
      originalLine: 45,
      comments: [
        {
          url: 'https://github.com/xrf9268-hue/aiops-platform/pull/106#discussion_r3252799999',
          author: 'human-reviewer',
          body: '[P0] Resolved feedback should not file an issue.',
        },
      ],
    },
  ],
};

test('extractPriorityLabel maps Codex-style priority badges and defaults to P2', () => {
  assert.equal(extractPriorityLabel('[P0] data loss risk'), 'priority:p0');
  assert.equal(extractPriorityLabel('nit: [p3] wording'), 'priority:p3');
  assert.equal(extractPriorityLabel('no priority badge'), 'priority:p2');
});

test('normalizeDiscussionPermalink keeps the discussion anchor as the idempotency key', () => {
  assert.equal(
    normalizeDiscussionPermalink('https://github.com/xrf9268-hue/aiops-platform/pull/106#discussion_r3252794498'),
    'https://github.com/xrf9268-hue/aiops-platform/pull/106#discussion_r3252794498',
  );
  assert.equal(
    normalizeDiscussionPermalink('https://github.com/xrf9268-hue/aiops-platform/pull/106/files#discussion_r3252794498'),
    'https://github.com/xrf9268-hue/aiops-platform/pull/106#discussion_r3252794498',
  );
});

test('buildFollowUpIssue includes PR link, discussion permalink, file line, author, and body', () => {
  const issue = buildFollowUpIssue({ repository, pullRequest, thread: pullRequest.reviewThreads[0] });

  assert.equal(issue.title, 'Follow up unresolved review thread from PR #106');
  assert.equal(issue.owner, repository.owner);
  assert.equal(issue.repo, repository.name);
  assert.deepEqual(issue.labels, ['priority:p1', 'type:chore']);
  assert.match(issue.body, /Merged PR: https:\/\/github\.com\/xrf9268-hue\/aiops-platform\/pull\/106/);
  assert.match(issue.body, /Discussion: https:\/\/github\.com\/xrf9268-hue\/aiops-platform\/pull\/106#discussion_r3252794498/);
  assert.match(issue.body, /Location: `internal\/worker\/startup_reconcile\.go:42`/);
  assert.match(issue.body, /Author: @chatgpt-codex-connector/);
  assert.match(issue.body, /Startup reconcile silently falls back to defaults/);
});

test('captureUnresolvedReviewThreads skips already tracked permalinks and files author-agnostic actionable threads', async () => {
  const created = [];
  const searches = [];
  const result = await captureUnresolvedReviewThreads({
    repository,
    pullRequest,
    dryRun: false,
    github: {
      async searchIssuesByDiscussionPermalink({ permalink }) {
        searches.push(permalink);
        return permalink.endsWith('discussion_r3252794498') ? [{ number: 107, state: 'closed' }] : [];
      },
      async createIssue(issue) {
        created.push(issue);
        return { number: 123, url: 'https://github.com/xrf9268-hue/aiops-platform/issues/123' };
      },
    },
  });

  assert.deepEqual(searches, [
    'https://github.com/xrf9268-hue/aiops-platform/pull/106#discussion_r3252794498',
    'https://github.com/xrf9268-hue/aiops-platform/pull/106#discussion_r3252781671',
  ]);
  assert.equal(created.length, 0);
  assert.equal(result.created.length, 0);
  assert.deepEqual(result.skippedAlreadyTracked, [
    {
      permalink: 'https://github.com/xrf9268-hue/aiops-platform/pull/106#discussion_r3252794498',
      issues: [{ number: 107, state: 'closed' }],
    },
  ]);
});

test('captureUnresolvedReviewThreads reports already tracked unresolved outdated threads without creating issues', async () => {
  const searches = [];
  const result = await captureUnresolvedReviewThreads({
    repository,
    pullRequest,
    dryRun: true,
    github: {
      async searchIssuesByDiscussionPermalink({ permalink }) {
        searches.push(permalink);
        if (permalink.endsWith('discussion_r3252781671')) {
          return [{ number: 108, state: 'open' }];
        }
        return [];
      },
      async createIssue() {
        throw new Error('dry-run should not create issues');
      },
    },
  });

  assert.deepEqual(searches, [
    'https://github.com/xrf9268-hue/aiops-platform/pull/106#discussion_r3252794498',
    'https://github.com/xrf9268-hue/aiops-platform/pull/106#discussion_r3252781671',
  ]);
  assert.deepEqual(result.skippedNonActionableAlreadyTracked, [
    {
      permalink: 'https://github.com/xrf9268-hue/aiops-platform/pull/106#discussion_r3252781671',
      issues: [{ number: 108, state: 'open' }],
      reason: 'outdated',
    },
  ]);
});

test('capture workflow uses pull_request_target so fork-origin merges can file follow-up issues', async () => {
  const workflow = await readFile(new URL('../workflows/capture-unresolved-reviews.yml', import.meta.url), 'utf8');

  assert.match(workflow, /^  pull_request_target:\n    types:\n      - closed$/m);
  assert.match(workflow, /github\.event_name == 'pull_request_target'/);
  assert.doesNotMatch(workflow, /^  pull_request:\n/m);
  assert.doesNotMatch(workflow, /github\.event_name == 'pull_request'/);
});

test('captureUnresolvedReviewThreads creates one issue per unique unresolved non-outdated discussion permalink', async () => {
  const humanThread = {
    id: 'PRRT_kwDOExample4',
    isResolved: false,
    isOutdated: false,
    path: 'README.md',
    line: null,
    originalLine: 12,
    comments: [
      {
        url: 'https://github.com/xrf9268-hue/aiops-platform/pull/106/files#discussion_r3252800000',
        author: 'human-reviewer',
        body: 'Please document this behavior.',
      },
    ],
  };
  const created = [];
  const result = await captureUnresolvedReviewThreads({
    repository,
    pullRequest: { ...pullRequest, reviewThreads: [pullRequest.reviewThreads[0], humanThread] },
    dryRun: false,
    github: {
      async searchIssuesByDiscussionPermalink() {
        return [];
      },
      async createIssue(issue) {
        created.push(issue);
        return { number: 124, url: 'https://github.com/xrf9268-hue/aiops-platform/issues/124' };
      },
    },
  });

  assert.equal(created.length, 2);
  assert.deepEqual(created.map((issue) => issue.labels), [
    ['priority:p1', 'type:chore'],
    ['priority:p2', 'type:chore'],
  ]);
  assert.match(created[1].body, /Author: @human-reviewer/);
  assert.match(created[1].body, /Location: `README\.md:12`/);
  assert.deepEqual(result.created.map((issue) => issue.number), [124, 124]);
});


test('verifyTrackingForActionableThreads fails loudly when an actionable thread has no tracking issue', async () => {
  await assert.rejects(
    verifyTrackingForActionableThreads({
      repository,
      pullRequest: { ...pullRequest, reviewThreads: [pullRequest.reviewThreads[0]] },
      github: {
        async searchIssuesByDiscussionPermalink() {
          return [];
        },
      },
    }),
    /Post-capture verification failed/,
  );
});

test('verifyTrackingForActionableThreads rechecks newly created issues after search indexing settles', async () => {
  const searches = [];

  await assert.rejects(
    verifyTrackingForActionableThreads({
      repository,
      pullRequest: { ...pullRequest, reviewThreads: [pullRequest.reviewThreads[0]] },
      createdPermalinks: ['https://github.com/xrf9268-hue/aiops-platform/pull/106#discussion_r3252794498'],
      github: {
        async searchIssuesByDiscussionPermalink({ permalink }) {
          searches.push(permalink);
          return [];
        },
      },
    }),
    /Post-capture verification failed/,
  );

  assert.deepEqual(searches, ['https://github.com/xrf9268-hue/aiops-platform/pull/106#discussion_r3252794498']);
});

test('verifyTrackingForActionableThreads passes when newly created issues are searchable', async () => {
  const searches = [];

  await verifyTrackingForActionableThreads({
    repository,
    pullRequest: { ...pullRequest, reviewThreads: [pullRequest.reviewThreads[0]] },
    createdPermalinks: ['https://github.com/xrf9268-hue/aiops-platform/pull/106#discussion_r3252794498'],
    github: {
      async searchIssuesByDiscussionPermalink({ permalink }) {
        searches.push(permalink);
        return [{ number: 123, state: 'open' }];
      },
    },
  });

  assert.deepEqual(searches, ['https://github.com/xrf9268-hue/aiops-platform/pull/106#discussion_r3252794498']);
});

test('searchTermsForDiscussionPermalink returns distinct terms including the discussion anchor', () => {
  assert.deepEqual(
    searchTermsForDiscussionPermalink('https://github.com/xrf9268-hue/aiops-platform/pull/112#discussion_r3253405490'),
    [
      'https://github.com/xrf9268-hue/aiops-platform/pull/112#discussion_r3253405490',
      'discussion_r3253405490',
    ],
  );
});

test('captureUnresolvedReviewThreads falls back to anchor-only search without duplicate URL searches', async () => {
  const searches = [];
  const result = await captureUnresolvedReviewThreads({
    repository,
    pullRequest: {
      ...pullRequest,
      reviewThreads: [
        {
          ...pullRequest.reviewThreads[0],
          comments: [
            {
              ...pullRequest.reviewThreads[0].comments[0],
              url: 'https://github.com/xrf9268-hue/aiops-platform/pull/112#discussion_r3253405490',
            },
          ],
        },
      ],
    },
    github: {
      async searchIssuesByDiscussionPermalink({ permalink }) {
        searches.push(...searchTermsForDiscussionPermalink(permalink));
        return [{ number: 121, state: 'open', url: 'https://github.com/xrf9268-hue/aiops-platform/issues/121' }];
      },
      async createIssue() {
        throw new Error('anchor-only discussion should already be tracked');
      },
    },
  });

  assert.deepEqual(searches, [
    'https://github.com/xrf9268-hue/aiops-platform/pull/112#discussion_r3253405490',
    'discussion_r3253405490',
  ]);
  assert.equal(result.created.length, 0);
});

test('captureUnresolvedReviewThreads treats anchor-only manual issues as existing tracking', async () => {
  const result = await captureUnresolvedReviewThreads({
    repository,
    pullRequest: {
      ...pullRequest,
      reviewThreads: [
        {
          ...pullRequest.reviewThreads[0],
          comments: [
            {
              ...pullRequest.reviewThreads[0].comments[0],
              url: 'https://github.com/xrf9268-hue/aiops-platform/pull/112#discussion_r3253405490',
            },
          ],
        },
      ],
    },
    github: {
      async searchIssuesByDiscussionPermalink({ permalink }) {
        assert.equal(permalink, 'https://github.com/xrf9268-hue/aiops-platform/pull/112#discussion_r3253405490');
        return [{ number: 121, state: 'open', url: 'https://github.com/xrf9268-hue/aiops-platform/issues/121' }];
      },
      async createIssue() {
        throw new Error('already tracked anchor-only discussion should not create a duplicate issue');
      },
    },
  });

  assert.deepEqual(result.skippedAlreadyTracked, [
    {
      permalink: 'https://github.com/xrf9268-hue/aiops-platform/pull/112#discussion_r3253405490',
      issues: [{ number: 121, state: 'open', url: 'https://github.com/xrf9268-hue/aiops-platform/issues/121' }],
    },
  ]);
});

test('captureUnresolvedReviewThreads reuses tracking lookups for duplicate verification', async () => {
  let searches = 0;
  const result = await captureUnresolvedReviewThreads({
    repository,
    pullRequest: { ...pullRequest, reviewThreads: [pullRequest.reviewThreads[0]] },
    github: {
      async searchIssuesByDiscussionPermalink() {
        searches += 1;
        return [{ number: 121, state: 'open', url: 'https://github.com/xrf9268-hue/aiops-platform/issues/121' }];
      },
      async createIssue() {
        throw new Error('existing tracked issue should avoid creation');
      },
    },
  });

  assert.equal(searches, 1);
  assert.equal(result.created.length, 0);
});

test('parseArgs allows explicit zero settle seconds but rejects invalid positive-only counts', () => {
  assert.deepEqual(parseArgs(['--settle-seconds', '0']), { dryRun: false, settleSeconds: 0 });
  assert.throws(() => parseArgs(['--pull-number', '0']), /--pull-number must be a positive number/);
  assert.throws(
    () => parseArgs(['--pull-number', '112', '--recent-merged-days', '3']),
    /Pass either --pull-number or --recent-merged-days/,
  );
});

test('loadRecentlyMergedPullNumbers paginates until the updated window is exhausted', async () => {
  const now = Date.now();
  const requests = [];
  const page1 = Array.from({ length: 100 }, (_, index) => ({
    number: index + 1,
    merged_at: index === 0 ? null : new Date(now - 60 * 60 * 1000).toISOString(),
    updated_at: new Date(now - 60 * 60 * 1000).toISOString(),
  }));
  const page2 = [
    {
      number: 200,
      merged_at: new Date(now - 2 * 60 * 60 * 1000).toISOString(),
      updated_at: new Date(now - 2 * 60 * 60 * 1000).toISOString(),
    },
    {
      number: 201,
      merged_at: new Date(now - 10 * 24 * 60 * 60 * 1000).toISOString(),
      updated_at: new Date(now - 10 * 24 * 60 * 60 * 1000).toISOString(),
    },
  ];

  const result = await loadRecentlyMergedPullNumbers({
    owner: repository.owner,
    repo: repository.name,
    days: 3,
    async request(path) {
      requests.push(path);
      return requests.length === 1 ? page1 : page2;
    },
  });

  assert.equal(requests.length, 2);
  assert.match(requests[0], /page=1/);
  assert.match(requests[1], /page=2/);
  assert.equal(result.includes(1), false);
  assert.equal(result.includes(200), true);
  assert.equal(result.includes(201), false);
});

test('capture workflow has scheduled retroactive sweep and failure notification job', async () => {
  const workflow = await readFile(new URL('../workflows/capture-unresolved-reviews.yml', import.meta.url), 'utf8');

  assert.match(workflow, /^  schedule:\n    - cron: /m);
  assert.match(workflow, /--recent-merged-days/);
  assert.match(workflow, /Notify capture workflow failure/);
});

test('buildTrackingIssueIndex indexes full permalinks and discussion anchors from issue bodies without Search API calls', () => {
  const index = buildTrackingIssueIndex([
    {
      number: 135,
      state: 'open',
      html_url: 'https://github.com/xrf9268-hue/aiops-platform/issues/135',
      body: 'Discussion: https://github.com/xrf9268-hue/aiops-platform/pull/101#discussion_r3251385489',
    },
    {
      number: 136,
      state: 'closed',
      html_url: 'https://github.com/xrf9268-hue/aiops-platform/issues/136',
      body: 'Manual backfill for discussion_r3251492691 only.',
    },
    {
      number: 137,
      state: 'open',
      html_url: 'https://github.com/xrf9268-hue/aiops-platform/issues/137',
      body: 'Unrelated issue body.',
    },
  ]);

  assert.deepEqual(index.get('https://github.com/xrf9268-hue/aiops-platform/pull/101#discussion_r3251385489'), [
    { number: 135, state: 'open', url: 'https://github.com/xrf9268-hue/aiops-platform/issues/135' },
  ]);
  assert.deepEqual(index.get('discussion_r3251385489'), [
    { number: 135, state: 'open', url: 'https://github.com/xrf9268-hue/aiops-platform/issues/135' },
  ]);
  assert.deepEqual(index.get('discussion_r3251492691'), [
    { number: 136, state: 'closed', url: 'https://github.com/xrf9268-hue/aiops-platform/issues/136' },
  ]);
  assert.equal(index.has('discussion_rdoesnotexist'), false);
});

test('loadRepositoryIssuesForTracking paginates issue bodies through the regular issues API', async () => {
  const requests = [];
  const page1 = Array.from({ length: 100 }, (_, index) => ({
    number: index + 1,
    state: 'open',
    html_url: `https://github.com/xrf9268-hue/aiops-platform/issues/${index + 1}`,
    body: index === 0 ? 'Discussion: https://github.com/xrf9268-hue/aiops-platform/pull/101#discussion_r3251385489' : '',
  }));
  const page2 = [
    {
      number: 101,
      state: 'closed',
      html_url: 'https://github.com/xrf9268-hue/aiops-platform/issues/101',
      body: 'Manual tracking issue for discussion_r3251492691.',
    },
  ];

  const issues = await loadRepositoryIssuesForTracking({
    repository,
    async request(path) {
      requests.push(path);
      return requests.length === 1 ? page1 : page2;
    },
  });

  assert.equal(requests.length, 2);
  assert.match(requests[0], /\/repos\/xrf9268-hue\/aiops-platform\/issues\?/);
  assert.match(requests[0], /state=all/);
  assert.match(requests[0], /page=1/);
  assert.match(requests[1], /page=2/);
  assert.equal(issues.length, 101);
  assert.equal(issues[100].number, 101);
});
