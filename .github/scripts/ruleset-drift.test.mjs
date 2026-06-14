import test from 'node:test';
import assert from 'node:assert/strict';
import { chmod, mkdir, mkdtemp, readFile, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const repoRoot = path.resolve(fileURLToPath(new URL('../..', import.meta.url)));
const scriptPath = path.join(repoRoot, '.github/scripts/check-ruleset-drift.sh');
const workflowPath = path.join(repoRoot, '.github/workflows/ruleset-drift.yml');
const sourcePath = path.join(repoRoot, '.github/governance/main-ruleset.json');

async function readSourceRuleset() {
  return JSON.parse(await readFile(sourcePath, 'utf8'));
}

function liveRulesetFromSource(source, overrides = {}) {
  const withLiveDefaults = {
    ...source,
    _links: {
      self: { href: 'https://api.github.com/repos/xrf9268-hue/aiops-platform/rulesets/17166171' },
      html: { href: 'https://github.com/xrf9268-hue/aiops-platform/rules/17166171' },
    },
    bypass_actors: source.bypass_actors,
    created_at: '2026-06-02T12:19:17Z',
    current_user_can_bypass: 'pull_requests_only',
    id: 17166171,
    node_id: 'RRS_example',
    source: 'xrf9268-hue/aiops-platform',
    source_type: 'Repository',
    updated_at: '2026-06-14T02:55:16Z',
    rules: source.rules.map((rule) => {
      if (rule.type !== 'pull_request') {
        return rule;
      }
      return {
        ...rule,
        parameters: {
          ...rule.parameters,
          required_reviewers: [],
        },
      };
    }),
  };
  return {
    ...withLiveDefaults,
    ...overrides,
  };
}

async function runChecker({ source, live }) {
  const dir = await mkdtemp(path.join(tmpdir(), 'ruleset-drift-'));
  const expectedPath = path.join(dir, 'main-ruleset.json');
  const livePath = path.join(dir, 'live-ruleset.json');
  await writeFile(expectedPath, `${JSON.stringify(source, null, 2)}\n`);
  await writeFile(livePath, `${JSON.stringify(live, null, 2)}\n`);

  return new Promise((resolve, reject) => {
    const child = spawn('bash', [scriptPath], {
      cwd: repoRoot,
      env: {
        ...process.env,
        AIOPS_RULESET_DRIFT_LIVE_JSON: livePath,
        AIOPS_RULESET_SOURCE_FILE: expectedPath,
        GITHUB_REPOSITORY: 'xrf9268-hue/aiops-platform',
      },
    });

    let stdout = '';
    let stderr = '';
    child.stdout.on('data', (chunk) => {
      stdout += chunk;
    });
    child.stderr.on('data', (chunk) => {
      stderr += chunk;
    });
    child.on('error', reject);
    child.on('close', (code) => {
      resolve({ code, stdout, stderr, output: `${stdout}${stderr}` });
    });
  });
}

async function runCheckerWithMockGh({ source, list, detail }) {
  const dir = await mkdtemp(path.join(tmpdir(), 'ruleset-drift-gh-'));
  const binDir = path.join(dir, 'bin');
  const expectedPath = path.join(dir, 'main-ruleset.json');
  const listPath = path.join(dir, 'rulesets.json');
  const detailPath = path.join(dir, 'live-ruleset.json');
  const ghPath = path.join(binDir, 'gh');
  await mkdir(binDir);
  await writeFile(expectedPath, `${JSON.stringify(source, null, 2)}\n`);
  await writeFile(listPath, `${JSON.stringify(list, null, 2)}\n`);
  await writeFile(detailPath, `${JSON.stringify(detail, null, 2)}\n`);
  await writeFile(
    ghPath,
    `#!/usr/bin/env bash
set -euo pipefail
if [[ "$1" != "api" ]]; then
  echo "unexpected gh command: $*" >&2
  exit 64
fi
case "$2" in
  "repos/xrf9268-hue/aiops-platform/rulesets?per_page=100&includes_parents=false&targets=branch")
    cat "${listPath}"
    ;;
  "repos/xrf9268-hue/aiops-platform/rulesets/17166171?includes_parents=false")
    cat "${detailPath}"
    ;;
  *)
    echo "unexpected endpoint: $2" >&2
    exit 65
    ;;
esac
`,
  );
  await chmod(ghPath, 0o755);

  return new Promise((resolve, reject) => {
    const child = spawn('bash', [scriptPath], {
      cwd: repoRoot,
      env: {
        ...process.env,
        AIOPS_RULESET_SOURCE_FILE: expectedPath,
        GITHUB_REPOSITORY: 'xrf9268-hue/aiops-platform',
        PATH: `${binDir}${path.delimiter}${process.env.PATH}`,
      },
    });

    let stdout = '';
    let stderr = '';
    child.stdout.on('data', (chunk) => {
      stdout += chunk;
    });
    child.stderr.on('data', (chunk) => {
      stderr += chunk;
    });
    child.on('error', reject);
    child.on('close', (code) => {
      resolve({ code, stdout, stderr, output: `${stdout}${stderr}` });
    });
  });
}

test('checker accepts matching read-visible rulesets after normalization', async () => {
  const source = await readSourceRuleset();
  const live = liveRulesetFromSource(source);

  const result = await runChecker({ source, live });

  assert.equal(result.code, 0, result.output);
  assert.match(result.output, /main branch ruleset matches/);
});

test('checker fails with an annotation and diff when live settings drift', async () => {
  const source = await readSourceRuleset();
  const live = liveRulesetFromSource(source, { enforcement: 'evaluate' });

  const result = await runChecker({ source, live });

  assert.equal(result.code, 1, result.output);
  assert.match(result.output, /::error file=.github\/governance\/main-ruleset\.json/);
  assert.match(result.output, /Main branch ruleset drift/);
  assert.match(result.output, /"enforcement": "active"/);
  assert.match(result.output, /"enforcement": "evaluate"/);
});

test('checker preserves non-empty required reviewers as drift', async () => {
  const source = await readSourceRuleset();
  const sourceWithReviewer = {
    ...source,
    rules: source.rules.map((rule) => {
      if (rule.type !== 'pull_request') {
        return rule;
      }
      return {
        ...rule,
        parameters: {
          ...rule.parameters,
          required_reviewers: [
            {
              repository_id: 123,
              reviewer_id: 456,
              reviewer_type: 'Team',
            },
          ],
        },
      };
    }),
  };
  const live = liveRulesetFromSource(source);

  const result = await runChecker({ source: sourceWithReviewer, live });

  assert.equal(result.code, 1, result.output);
  assert.match(result.output, /required_reviewers/);
  assert.match(result.output, /reviewer_type/);
});

test('checker filters the list request by target instead of reading list target fields', async () => {
  const source = await readSourceRuleset();
  const listWithoutTargets = [
    {
      id: 17166171,
      name: source.name,
      enforcement: source.enforcement,
    },
  ];
  const detail = liveRulesetFromSource(source);

  const result = await runCheckerWithMockGh({ source, list: listWithoutTargets, detail });

  assert.equal(result.code, 0, result.output);
  assert.match(result.output, /main branch ruleset matches/);
});

test('checker and workflow remain read-only and never auto-apply rulesets', async () => {
  const script = await readFile(scriptPath, 'utf8');
  const workflow = await readFile(workflowPath, 'utf8');

  assert.doesNotMatch(`${script}\n${workflow}`, /\b(PUT|POST|DELETE)\b/);
  assert.doesNotMatch(`${script}\n${workflow}`, /--method\s+/);
  assert.match(script, /rulesets\?per_page=100&includes_parents=false&targets=\$\{ruleset_target\}/);
  assert.match(script, /rulesets\/\$\{ruleset_id\}\?includes_parents=false/);
  assert.match(workflow, /^permissions:\n  contents: read$/m);
  assert.match(workflow, /^  workflow_dispatch:$/m);
  assert.match(workflow, /^  schedule:$/m);
  assert.match(workflow, /^  push:$/m);
});
