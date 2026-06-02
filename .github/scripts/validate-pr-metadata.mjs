import { readFile } from 'node:fs/promises';

// validate-pr-metadata.mjs enforces the SPEC-deviation merge gate (AGENTS.md
// principle 6/7): when a PR changes a SPEC-sensitive path it must NOT claim it
// adds no new key/phase — it must instead cite an upstream Elixir reference or
// track the deviation in DEVIATIONS.md. This is the author-time mechanical gate
// from #588: it makes a new SPEC deviation cost something *before* merge instead
// of being caught later by judgment review (the #73/#74/#76/#557/#561/D25
// recurrence). Modeled on the Wink PR-metadata gate: path classification + an
// honest PR-body checklist, enforced as a required status check.

const apiVersion = '2022-11-28';

// specSensitivePatterns triggers the gate. Kept path-level (no AST parse),
// matching the Wink model: the surfaces where a SPEC deviation actually enters.
//   - internal/workflow/config.go: new top-level WORKFLOW.md/Config keys.
//   - new (added) non-test files under internal/orchestrator or internal/worker:
//     new scheduler/runner phases, gates, or artifacts.
const specSensitivePatterns = [
  { test: (file) => file.filename === 'internal/workflow/config.go' },
  {
    test: (file) =>
      file.status === 'added' &&
      /^internal\/(orchestrator|worker)\/[^/]+\.go$/.test(file.filename) &&
      !file.filename.endsWith('_test.go'),
  },
];

const SPEC_OPTION_NONE = 'none';
const SPEC_OPTION_ELIXIR = 'elixir';
const SPEC_OPTION_DEVIATION = 'deviation';

// specOptionMarkers identifies each SPEC-alignment checklist option by a stable
// substring of its template line, so the template wording can evolve without
// breaking the parser as long as these anchors survive.
const specOptionMarkers = [
  { key: SPEC_OPTION_NONE, marker: 'no new top-level' },
  { key: SPEC_OPTION_ELIXIR, marker: 'elixir reference' },
  { key: SPEC_OPTION_DEVIATION, marker: 'deviations.md row' },
];

const closingKeywordPattern =
  /\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\b\s+#(\d+)/gi;

// elixirCitationPattern matches a concrete upstream reference: an Elixir source
// path (optionally with a line) or an elixir/ tree path. A bare "Elixir" word
// does not count — the citation must point somewhere a reviewer can open.
const elixirCitationPattern = /(?:elixir\/[\w./-]+|[\w./-]+\.ex)(?::\d+)?/i;

export function extractClosingIssueNumbers(body) {
  const numbers = new Set();
  for (const match of String(body ?? '').matchAll(closingKeywordPattern)) {
    numbers.add(Number(match[1]));
  }
  return [...numbers];
}

export function parseSpecAlignmentChecklist(body) {
  const present = [];
  const checked = [];
  for (const line of String(body ?? '').split('\n')) {
    const checkbox = line.match(/^\s*-\s*\[( |x|X)\]\s*(.*)$/);
    if (!checkbox) {
      continue;
    }
    const isChecked = checkbox[1].toLowerCase() === 'x';
    const text = checkbox[2].toLowerCase();
    const option = specOptionMarkers.find((entry) => text.includes(entry.marker));
    if (!option) {
      continue;
    }
    present.push(option.key);
    if (isChecked) {
      checked.push(option.key);
    }
  }
  return {
    presentOptions: present,
    checkedOptions: checked,
    selection: checked.length === 1 ? checked[0] : null,
  };
}

export function classifySpecSensitivity(files) {
  const matches = [];
  for (const file of files ?? []) {
    if (specSensitivePatterns.some((pattern) => pattern.test(file))) {
      matches.push(file.filename);
    }
  }
  return { sensitive: matches.length > 0, matches };
}

export function hasElixirCitation(body) {
  return elixirCitationPattern.test(String(body ?? ''));
}

export function touchesDeviations(files) {
  return (files ?? []).some((file) => file.filename === 'DEVIATIONS.md');
}

// validatePullRequestMetadata returns the list of blocking errors for a PR. It
// is pure (no I/O) so the test suite drives it directly.
export function validatePullRequestMetadata({ body, files }) {
  const errors = [];
  const issueNumbers = extractClosingIssueNumbers(body);
  const checklist = parseSpecAlignmentChecklist(body);
  const sensitivity = classifySpecSensitivity(files);

  if (issueNumbers.length === 0) {
    errors.push(
      'PR body must include a closing keyword such as `Closes #123` so merge closes the linked issue automatically.',
    );
  }

  if (checklist.presentOptions.length !== specOptionMarkers.length) {
    errors.push(
      'PR body must keep all three `SPEC alignment` checklist options from the template.',
    );
  } else if (checklist.checkedOptions.length !== 1) {
    errors.push('PR body must check exactly one `SPEC alignment` option.');
  }

  if (sensitivity.sensitive) {
    if (checklist.selection === SPEC_OPTION_NONE) {
      errors.push(
        `SPEC-sensitive paths changed (${sensitivity.matches.join(
          ', ',
        )}); you cannot claim "no new key/phase". Cite an upstream Elixir reference or track a DEVIATIONS.md row (AGENTS.md principle 6/7).`,
      );
    } else if (checklist.selection === SPEC_OPTION_ELIXIR && !hasElixirCitation(body)) {
      errors.push(
        'The Elixir-reference option is checked but the PR body has no citation (e.g. `elixir/lib/symphony_elixir/orchestrator.ex:1142`).',
      );
    } else if (checklist.selection === SPEC_OPTION_DEVIATION && !touchesDeviations(files)) {
      errors.push(
        'The DEVIATIONS.md option is checked but this PR does not modify DEVIATIONS.md.',
      );
    }
  }

  return {
    errors,
    summary: [
      `Linked issues: ${
        issueNumbers.length > 0 ? issueNumbers.map((n) => `#${n}`).join(', ') : 'none'
      }`,
      `SPEC alignment selection: ${checklist.selection ?? 'missing/ambiguous'}`,
      `SPEC-sensitive paths: ${
        sensitivity.sensitive ? sensitivity.matches.join(', ') : 'none'
      }`,
    ],
  };
}

async function githubRequest(pathname) {
  const response = await fetch(`https://api.github.com${pathname}`, {
    headers: {
      Accept: 'application/vnd.github+json',
      Authorization: `Bearer ${process.env.GITHUB_TOKEN}`,
      'User-Agent': 'aiops-pr-metadata-validator',
      'X-GitHub-Api-Version': apiVersion,
    },
  });
  if (!response.ok) {
    const body = await response.text();
    throw new Error(`GitHub API ${pathname} failed (${response.status}): ${body}`);
  }
  return response.json();
}

async function listPullRequestFiles(owner, repo, number) {
  if (process.env.PR_FILES) {
    return JSON.parse(process.env.PR_FILES);
  }
  const files = [];
  let page = 1;
  for (;;) {
    const result = await githubRequest(
      `/repos/${owner}/${repo}/pulls/${number}/files?per_page=100&page=${page}`,
    );
    files.push(...result);
    if (result.length < 100) {
      return files;
    }
    page += 1;
  }
}

async function main() {
  const event = JSON.parse(await readFile(process.env.GITHUB_EVENT_PATH, 'utf8'));
  const pullRequest = event.pull_request;
  if (!pullRequest) {
    throw new Error('This script must run from a pull_request_target workflow event.');
  }
  const owner = process.env.REPO_OWNER ?? event.repository.owner.login;
  const repo = process.env.REPO_NAME ?? event.repository.name;
  const body = process.env.PR_BODY ?? pullRequest.body ?? '';
  const files = await listPullRequestFiles(owner, repo, pullRequest.number);

  const { errors, summary } = validatePullRequestMetadata({ body, files });
  console.log(summary.join('\n'));
  if (errors.length > 0) {
    for (const error of errors) {
      console.error(`::error::${error}`);
    }
    process.exitCode = 1;
  }
}

// Only run main() when executed as a script, not when imported by the tests.
if (process.argv[1] && import.meta.url === `file://${process.argv[1]}`) {
  main().catch((error) => {
    console.error(`::error::${error.message}`);
    process.exitCode = 1;
  });
}
