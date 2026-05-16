import test from 'node:test';
import assert from 'node:assert/strict';

import {
  buildFollowUpIssue,
  captureUnresolvedReviewThreads,
  extractPriorityLabel,
  normalizeDiscussionPermalink,
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
