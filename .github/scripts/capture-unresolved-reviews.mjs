import { readFile } from 'node:fs/promises';
import { pathToFileURL } from 'node:url';

const apiVersion = '2022-11-28';
const defaultLabels = ['type:chore'];

function requiredEnv(name) {
  const value = process.env[name];
  if (!value) {
    throw new Error(`Missing required environment variable: ${name}`);
  }
  return value;
}

function githubToken() {
  return process.env.GITHUB_TOKEN ?? process.env.GH_TOKEN ?? requiredEnv('GITHUB_TOKEN');
}

function sleep(milliseconds) {
  return new Promise((resolve) => setTimeout(resolve, milliseconds));
}

async function githubRequest(path, { method = 'GET', body } = {}) {
  const response = await fetch(`https://api.github.com${path}`, {
    method,
    headers: {
      Accept: 'application/vnd.github+json',
      Authorization: `Bearer ${githubToken()}`,
      'Content-Type': 'application/json',
      'User-Agent': 'aiops-platform-capture-unresolved-reviews',
      'X-GitHub-Api-Version': apiVersion,
    },
    body: body ? JSON.stringify(body) : undefined,
  });

  const text = await response.text();
  const payload = text ? JSON.parse(text) : null;
  if (!response.ok) {
    const details = JSON.stringify(payload ?? text, null, 2);
    throw new Error(`GitHub REST request failed ${method} ${path}: ${details}`);
  }
  return payload;
}

async function graphqlRequest(query, variables) {
  const response = await fetch('https://api.github.com/graphql', {
    method: 'POST',
    headers: {
      Accept: 'application/vnd.github+json',
      Authorization: `Bearer ${githubToken()}`,
      'Content-Type': 'application/json',
      'User-Agent': 'aiops-platform-capture-unresolved-reviews',
      'X-GitHub-Api-Version': apiVersion,
    },
    body: JSON.stringify({ query, variables }),
  });

  const payload = await response.json();
  if (!response.ok || payload.errors?.length) {
    const details = JSON.stringify(payload.errors ?? payload, null, 2);
    throw new Error(`GraphQL request failed: ${details}`);
  }
  return payload.data;
}

export function extractPriorityLabel(body) {
  const match = body.match(/\[(P[0-3])\]/i);
  if (!match) {
    return 'priority:p2';
  }
  return `priority:${match[1].toLowerCase()}`;
}

export function normalizeDiscussionPermalink(url) {
  const parsed = new URL(url);
  const discussion = parsed.hash.match(/^#discussion_r\d+$/)?.[0];
  if (!discussion) {
    throw new Error(`Review comment URL is missing a discussion anchor: ${url}`);
  }
  parsed.hash = discussion;
  parsed.search = '';
  parsed.pathname = parsed.pathname.replace(/\/files$/, '');
  return parsed.toString();
}

function firstComment(thread) {
  if (Array.isArray(thread.comments)) {
    return thread.comments[0] ?? {};
  }
  return thread.comments?.nodes?.[0] ?? {};
}

function authorLogin(comment) {
  return comment.author?.login ?? comment.author ?? 'ghost';
}

function commentBody(comment) {
  return comment.bodyText ?? comment.body ?? '';
}

function commentUrl(comment) {
  return comment.url ?? comment.htmlUrl ?? comment.html_url;
}

function threadLine(thread) {
  return thread.line ?? thread.originalLine ?? thread.startLine ?? thread.originalStartLine ?? null;
}

function classifyThreads(reviewThreads) {
  const seen = new Set();
  const actionable = [];
  const nonActionable = [];

  for (const thread of reviewThreads) {
    const comment = firstComment(thread);
    const url = commentUrl(comment);
    if (!url) {
      console.warn(`Skipping review thread ${thread.id ?? '<unknown>'}: first comment has no URL.`);
      continue;
    }

    const permalink = normalizeDiscussionPermalink(url);
    if (seen.has(permalink)) {
      continue;
    }
    seen.add(permalink);

    if (!thread.isResolved && !thread.isOutdated) {
      actionable.push({ ...thread, discussionPermalink: permalink });
    } else if (!thread.isResolved && thread.isOutdated) {
      nonActionable.push({ ...thread, discussionPermalink: permalink, nonActionableReason: 'outdated' });
    }
  }

  return { actionable, nonActionable };
}

export function buildFollowUpIssue({ repository, pullRequest, thread }) {
  const comment = firstComment(thread);
  const permalink = thread.discussionPermalink ?? normalizeDiscussionPermalink(commentUrl(comment));
  const line = threadLine(thread);
  const location = line ? `${thread.path}:${line}` : thread.path;
  const body = commentBody(comment);
  const priorityLabel = extractPriorityLabel(body);
  const prUrl = pullRequest.url ?? `https://github.com/${repository.owner}/${repository.name}/pull/${pullRequest.number}`;

  return {
    title: `Follow up unresolved review thread from PR #${pullRequest.number}`,
    labels: [priorityLabel, ...defaultLabels],
    owner: repository.owner,
    repo: repository.name,
    body: [
      'A merged PR still had an unresolved, non-outdated review discussion. This issue was filed automatically so the feedback is not lost after merge.',
      '',
      `Merged PR: ${prUrl}`,
      `Discussion: ${permalink}`,
      `Location: \`${location}\``,
      `Author: @${authorLogin(comment)}`,
      '',
      'Original review body:',
      '',
      '```',
      body.trim(),
      '```',
    ].join('\n'),
  };
}

export async function verifyTrackingForActionableThreads({
  repository,
  pullRequest,
  github,
  createdPermalinks = [],
  existingTrackedPermalinks = [],
}) {
  const created = new Set(createdPermalinks);
  const existingTracked = new Set(existingTrackedPermalinks);
  const { actionable } = classifyThreads(pullRequest.reviewThreads ?? []);
  const missing = [];

  for (const thread of actionable) {
    const permalink = thread.discussionPermalink;
    if (existingTracked.has(permalink)) {
      continue;
    }

    if (!created.has(permalink)) {
      missing.push(permalink);
      continue;
    }

    const existingIssues = await github.searchIssuesByDiscussionPermalink({ repository, permalink });
    if (existingIssues.length === 0) {
      missing.push(permalink);
    }
  }

  if (missing.length > 0) {
    throw new Error(
      `Post-capture verification failed: ${missing.length} actionable review thread(s) have no created or existing tracking issue: ${missing.join(', ')}`,
    );
  }

  return { actionableCount: actionable.length, missingCount: missing.length };
}

export async function captureUnresolvedReviewThreads({ repository, pullRequest, github, dryRun = false }) {
  const created = [];
  const createdPermalinks = [];
  const existingTrackedPermalinks = [];
  const skippedAlreadyTracked = [];
  const skippedNonActionableAlreadyTracked = [];
  const { actionable, nonActionable } = classifyThreads(pullRequest.reviewThreads ?? []);

  for (const thread of actionable) {
    const permalink = thread.discussionPermalink;
    const existingIssues = await github.searchIssuesByDiscussionPermalink({ repository, permalink });
    if (existingIssues.length > 0) {
      console.log(`Skipping ${permalink}; already tracked by ${existingIssues.map((issue) => `#${issue.number}`).join(', ')}.`);
      skippedAlreadyTracked.push({ permalink, issues: existingIssues });
      existingTrackedPermalinks.push(permalink);
      continue;
    }

    const issue = buildFollowUpIssue({ repository, pullRequest, thread });
    if (dryRun) {
      console.log(`Dry run: would create follow-up issue for ${permalink}.`);
      created.push({ ...issue, dryRun: true });
      createdPermalinks.push(permalink);
      existingTrackedPermalinks.push(permalink);
      continue;
    }

    const response = await github.createIssue(issue);
    console.log(`Created follow-up issue #${response.number} for ${permalink}.`);
    created.push(response);
    createdPermalinks.push(permalink);
    if (response?.url || response?.html_url) {
      existingTrackedPermalinks.push(permalink);
    }
  }

  for (const thread of nonActionable) {
    const permalink = thread.discussionPermalink;
    const existingIssues = await github.searchIssuesByDiscussionPermalink({ repository, permalink });
    if (existingIssues.length > 0) {
      console.log(
        `Non-actionable ${thread.nonActionableReason} thread ${permalink} is already tracked by ${existingIssues
          .map((issue) => `#${issue.number}`)
          .join(', ')}.`,
      );
      skippedNonActionableAlreadyTracked.push({
        permalink,
        issues: existingIssues,
        reason: thread.nonActionableReason,
      });
    }
  }

  await verifyTrackingForActionableThreads({
    repository,
    pullRequest,
    github,
    createdPermalinks,
    existingTrackedPermalinks,
  });

  return {
    created,
    skippedAlreadyTracked,
    skippedNonActionableAlreadyTracked,
    actionableCount: actionable.length,
    createdPermalinks,
  };
}

async function loadPullRequest({ owner, repo, pullNumber }) {
  const reviewThreads = [];
  let after = null;
  let pullRequest = null;

  do {
    const data = await graphqlRequest(
      `
      query ReviewThreads($owner: String!, $repo: String!, $pullNumber: Int!, $after: String) {
        repository(owner: $owner, name: $repo) {
          pullRequest(number: $pullNumber) {
            number
            title
            url
            reviewThreads(first: 100, after: $after) {
              pageInfo {
                hasNextPage
                endCursor
              }
              nodes {
                id
                isResolved
                isOutdated
                path
                line
                originalLine
                startLine
                originalStartLine
                comments(first: 1) {
                  nodes {
                    url
                    author {
                      login
                    }
                    bodyText
                  }
                }
              }
            }
          }
        }
      }
      `,
      { owner, repo, pullNumber, after },
    );

    pullRequest = data.repository.pullRequest;
    if (!pullRequest) {
      throw new Error(`Pull request #${pullNumber} was not found.`);
    }

    reviewThreads.push(...pullRequest.reviewThreads.nodes);
    after = pullRequest.reviewThreads.pageInfo.hasNextPage ? pullRequest.reviewThreads.pageInfo.endCursor : null;
  } while (after);

  return { number: pullRequest.number, title: pullRequest.title, url: pullRequest.url, reviewThreads };
}


function parseTimestampOrOldest(value) {
  const timestamp = Date.parse(value ?? '');
  return Number.isFinite(timestamp) ? timestamp : -Infinity;
}

export async function loadRecentlyMergedPullNumbers({ owner, repo, days, request = githubRequest }) {
  const since = Date.now() - days * 24 * 60 * 60 * 1000;
  const merged = [];
  for (let page = 1; ; page += 1) {
    const payload = await request(
      `/repos/${owner}/${repo}/pulls?${new URLSearchParams({
        state: 'closed',
        sort: 'updated',
        direction: 'desc',
        per_page: '100',
        page: String(page),
      })}`,
    );
    const pulls = payload ?? [];
    merged.push(...pulls.filter((pull) => parseTimestampOrOldest(pull.merged_at) >= since));
    if (pulls.length < 100 || pulls.every((pull) => parseTimestampOrOldest(pull.updated_at ?? pull.closed_at ?? pull.merged_at) < since)) {
      break;
    }
  }
  return merged.map((pull) => pull.number).sort((left, right) => left - right);
}

export function searchTermsForDiscussionPermalink(permalink) {
  const parsed = new URL(permalink);
  const discussion = parsed.hash.slice(1);
  return [...new Set([permalink, discussion])];
}

function uniqueIssues(issues) {
  const seen = new Set();
  const result = [];
  for (const issue of issues) {
    const key = issue.url ?? issue.number;
    if (seen.has(key)) {
      continue;
    }
    seen.add(key);
    result.push(issue);
  }
  return result;
}

export function buildTrackingIssueIndex(issues) {
  const index = new Map();
  for (const issue of issues ?? []) {
    const body = issue.body ?? '';
    const trackedIssue = {
      number: issue.number,
      state: issue.state,
      url: issue.html_url ?? issue.url,
    };
    const terms = new Set(body.match(/discussion_r\d+/g) ?? []);
    for (const discussion of [...terms]) {
      const permalink = body.match(new RegExp(`https://github\\.com/[^\\s)]+#${discussion}`))?.[0];
      if (permalink) {
        terms.add(permalink);
      }
    }
    for (const term of terms) {
      index.set(term, uniqueIssues([...(index.get(term) ?? []), trackedIssue]));
    }
  }
  return index;
}

export async function loadRepositoryIssuesForTracking({ repository, request = githubRequest }) {
  const issues = [];
  for (let page = 1; ; page += 1) {
    const query = new URLSearchParams({
      state: 'all',
      per_page: '100',
      page: String(page),
    });
    const payload = await request(`/repos/${repository.owner}/${repository.name}/issues?${query.toString()}`);
    const pageIssues = (payload ?? []).filter((issue) => !issue.pull_request);
    issues.push(...pageIssues);
    if ((payload ?? []).length < 100) {
      break;
    }
  }
  return issues;
}

function createGitHubClient() {
  let trackingIssueIndex;
  return {
    async searchIssuesByDiscussionPermalink({ repository, permalink }) {
      if (!trackingIssueIndex) {
        trackingIssueIndex = buildTrackingIssueIndex(await loadRepositoryIssuesForTracking({ repository }));
      }
      const issues = [];
      for (const term of searchTermsForDiscussionPermalink(permalink)) {
        issues.push(...(trackingIssueIndex.get(term) ?? []));
      }
      return uniqueIssues(issues);
    },
    async createIssue({ title, body, labels, owner = requiredEnv('REPO_OWNER'), repo = requiredEnv('REPO_NAME') }) {
      const issue = await githubRequest(`/repos/${owner}/${repo}/issues`, {
        method: 'POST',
        body: { title, body, labels },
      });
      trackingIssueIndex = undefined;
      return { number: issue.number, url: issue.html_url };
    },
  };
}

function parsePositiveNumber(value, name) {
  const number = Number(value);
  if (!Number.isFinite(number) || number <= 0) {
    throw new Error(`${name} must be a positive number.`);
  }
  return number;
}

function parseNonNegativeNumber(value, name) {
  const number = Number(value);
  if (!Number.isFinite(number) || number < 0) {
    throw new Error(`${name} must be a non-negative number.`);
  }
  return number;
}

export function parseArgs(argv) {
  const args = { dryRun: false, settleSeconds: 0 };
  for (let index = 0; index < argv.length; index += 1) {
    const arg = argv[index];
    if (arg === '--dry-run') {
      args.dryRun = true;
    } else if (arg === '--pull-number') {
      args.pullNumber = parsePositiveNumber(argv[++index], '--pull-number');
    } else if (arg.startsWith('--pull-number=')) {
      args.pullNumber = parsePositiveNumber(arg.slice('--pull-number='.length), '--pull-number');
    } else if (arg === '--recent-merged-days') {
      args.recentMergedDays = parsePositiveNumber(argv[++index], '--recent-merged-days');
    } else if (arg.startsWith('--recent-merged-days=')) {
      args.recentMergedDays = parsePositiveNumber(arg.slice('--recent-merged-days='.length), '--recent-merged-days');
    } else if (arg === '--settle-seconds') {
      args.settleSeconds = parseNonNegativeNumber(argv[++index], '--settle-seconds');
    } else if (arg.startsWith('--settle-seconds=')) {
      args.settleSeconds = parseNonNegativeNumber(arg.slice('--settle-seconds='.length), '--settle-seconds');
    } else {
      throw new Error(`Unknown argument: ${arg}`);
    }
  }
  if (args.pullNumber && args.recentMergedDays) {
    throw new Error('Pass either --pull-number or --recent-merged-days, not both.');
  }
  return args;
}

async function resolveEventPullNumber(args) {
  if (args.pullNumber) {
    return args.pullNumber;
  }

  const eventPath = process.env.GITHUB_EVENT_PATH;
  if (!eventPath) {
    throw new Error('Pass --pull-number or run inside a GitHub pull_request/workflow_dispatch event.');
  }

  const event = JSON.parse(await readFile(eventPath, 'utf8'));
  const pullNumber = event.pull_request?.number ?? Number(event.inputs?.pull_number);
  if (!pullNumber) {
    throw new Error('Could not resolve pull request number from the GitHub event payload.');
  }
  return pullNumber;
}

async function capturePullRequest({ owner, repo, pullNumber, repository, github, dryRun }) {
  const pullRequest = await loadPullRequest({ owner, repo, pullNumber });
  const result = await captureUnresolvedReviewThreads({
    repository,
    pullRequest,
    github,
    dryRun,
  });

  console.log(
    `Capture complete for PR #${pullNumber}: ${result.actionableCount} actionable thread(s), ${result.created.length} created/would-create, ${result.skippedAlreadyTracked.length} already tracked.`,
  );
  return result;
}

export async function main(argv = process.argv.slice(2)) {
  const args = parseArgs(argv);
  const owner = process.env.REPO_OWNER ?? process.env.GITHUB_REPOSITORY_OWNER;
  const repo = process.env.REPO_NAME ?? process.env.GITHUB_REPOSITORY?.split('/')[1];
  if (!owner || !repo) {
    throw new Error('Repository owner/name not found; set REPO_OWNER and REPO_NAME.');
  }

  if (args.settleSeconds > 0) {
    console.log(`Waiting ${args.settleSeconds}s for asynchronous review threads to settle before capture.`);
    await sleep(args.settleSeconds * 1000);
  }

  const repository = { owner, name: repo };
  const github = createGitHubClient();
  if (args.recentMergedDays) {
    const pullNumbers = await loadRecentlyMergedPullNumbers({ owner, repo, days: args.recentMergedDays });
    console.log(`Retroactive sweep found ${pullNumbers.length} merged PR(s) in the last ${args.recentMergedDays} day(s).`);
    for (const pullNumber of pullNumbers) {
      await capturePullRequest({ owner, repo, pullNumber, repository, github, dryRun: args.dryRun });
    }
    return;
  }

  const pullNumber = await resolveEventPullNumber(args);
  await capturePullRequest({ owner, repo, pullNumber, repository, github, dryRun: args.dryRun });
}

if (import.meta.url === pathToFileURL(process.argv[1]).href) {
  main().catch((error) => {
    console.error(`::error::${error.message}`);
    process.exitCode = 1;
  });
}
