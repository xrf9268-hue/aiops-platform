import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';

import {
  classifySpecSensitivity,
  extractClosingIssueNumbers,
  hasElixirCitation,
  parseSpecAlignmentChecklist,
  touchesDeviations,
  validatePullRequestMetadata,
} from './validate-pr-metadata.mjs';

// checklist renders the three SPEC-alignment options with `selected` (0..2)
// checked, matching the pull_request_template.md anchors the parser keys on.
function checklist(selected) {
  const lines = [
    'No new top-level `WORKFLOW.md`/`Config` key and no new worker/orchestrator phase/gate/artifact.',
    'Adds one, justified by an upstream Elixir reference cited below.',
    'Adds one, tracked as a DEVIATIONS.md row + `area:spec-alignment` issue.',
  ];
  return lines.map((line, i) => `- [${i === selected ? 'x' : ' '}] ${line}`).join('\n');
}

function body({ closes = 'Closes #573', selected = 0, extra = '' } = {}) {
  return `${closes}\n\n## SPEC alignment\n${checklist(selected)}\n${extra}`;
}

const CONFIG = [{ filename: 'internal/workflow/config.go', status: 'modified' }];
const NON_SENSITIVE = [{ filename: 'internal/tracker/linear.go', status: 'modified' }];

test('extractClosingIssueNumbers parses every closing keyword and dedupes', () => {
  assert.deepEqual(
    extractClosingIssueNumbers('Closes #1, fixes #2\nResolved #2 Fix #3'),
    [1, 2, 3],
  );
  assert.deepEqual(extractClosingIssueNumbers('mentions #9 without a keyword'), []);
});

test('parseSpecAlignmentChecklist reports present + checked options', () => {
  const all = parseSpecAlignmentChecklist(checklist(1));
  assert.equal(all.presentOptions.length, 3);
  assert.deepEqual(all.checkedOptions, ['elixir']);
  assert.equal(all.selection, 'elixir');

  const none = parseSpecAlignmentChecklist('- [x] No new top-level keys here');
  assert.equal(none.presentOptions.length, 1);
  assert.equal(none.selection, 'none');

  const ambiguous = parseSpecAlignmentChecklist(`- [x] No new top-level x\n- [x] Elixir reference y`);
  assert.equal(ambiguous.selection, null, 'two checked → no single selection');
});

test('classifySpecSensitivity flags config.go and newly-added scheduler files only', () => {
  assert.equal(classifySpecSensitivity(CONFIG).sensitive, true);
  assert.deepEqual(
    classifySpecSensitivity([{ filename: 'internal/orchestrator/new_phase.go', status: 'added' }]).matches,
    ['internal/orchestrator/new_phase.go'],
  );
  assert.equal(
    classifySpecSensitivity([{ filename: 'internal/worker/new_phase.go', status: 'modified' }]).sensitive,
    false,
    'modifying (not adding) a worker file is not a new phase',
  );
  assert.equal(
    classifySpecSensitivity([{ filename: 'internal/orchestrator/new_phase_test.go', status: 'added' }]).sensitive,
    false,
    'added test files do not trip the gate',
  );
  assert.deepEqual(
    classifySpecSensitivity([{ filename: 'internal/orchestrator/sub/new_phase.go', status: 'added' }]).matches,
    ['internal/orchestrator/sub/new_phase.go'],
    'a nested subpackage phase still trips the gate',
  );
  assert.equal(
    classifySpecSensitivity([{ filename: 'internal/worker/moved_phase.go', status: 'renamed' }]).sensitive,
    true,
    'a git mv/rename into the sensitive tree is a new surface',
  );
  assert.equal(
    classifySpecSensitivity([{ filename: 'internal/worker/sub/new_phase_test.go', status: 'added' }]).sensitive,
    false,
    'a nested test file does not trip the gate',
  );
  assert.equal(classifySpecSensitivity(NON_SENSITIVE).sensitive, false);
});

test('hasElixirCitation requires a concrete openai/symphony Elixir-tree path', () => {
  assert.equal(hasElixirCitation('see elixir/lib/symphony_elixir/orchestrator.ex:1142'), true);
  assert.equal(hasElixirCitation('elixir/lib/symphony_elixir/config/schema.ex'), true);
  assert.equal(hasElixirCitation('a bare schema.ex name'), false, 'bare .ex filename is not a tree path');
  assert.equal(hasElixirCitation('not_a_real.ex'), false, 'a fabricated .ex name cannot satisfy the gate');
  assert.equal(hasElixirCitation('this matches the Elixir reference conceptually'), false);
});

test('the shipped PR template cannot self-satisfy the citation gate', async () => {
  const template = await readFile(new URL('../pull_request_template.md', import.meta.url), 'utf8');
  assert.equal(
    parseSpecAlignmentChecklist(template).presentOptions.length,
    3,
    'template must keep all three SPEC-alignment options',
  );
  assert.equal(
    hasElixirCitation(template),
    false,
    'an author copying the template must not pass the citation gate without adding a real reference',
  );
});

test('touchesDeviations needs a non-removed DEVIATIONS.md change', () => {
  assert.equal(touchesDeviations([{ filename: 'DEVIATIONS.md', status: 'modified' }]), true);
  assert.equal(touchesDeviations([{ filename: 'DEVIATIONS.md', status: 'added' }]), true);
  assert.equal(
    touchesDeviations([{ filename: 'DEVIATIONS.md', status: 'removed' }]),
    false,
    'deleting DEVIATIONS.md must not satisfy deviation tracking',
  );
  assert.equal(touchesDeviations(CONFIG), false);
});

test('clean non-sensitive PR claiming no-new-key passes', () => {
  assert.deepEqual(
    validatePullRequestMetadata({ body: body({ selected: 0 }), files: NON_SENSITIVE }).errors,
    [],
  );
});

test('SPEC-sensitive PR claiming no-new-key is rejected', () => {
  const { errors } = validatePullRequestMetadata({ body: body({ selected: 0 }), files: CONFIG });
  assert.equal(errors.length, 1);
  assert.match(errors[0], /SPEC-sensitive paths changed/);
});

test('SPEC-sensitive PR with Elixir option passes only with a citation', () => {
  assert.deepEqual(
    validatePullRequestMetadata({
      body: body({ selected: 1, extra: 'Upstream: elixir/lib/symphony_elixir/orchestrator.ex:1142' }),
      files: CONFIG,
    }).errors,
    [],
  );
  const missing = validatePullRequestMetadata({ body: body({ selected: 1 }), files: CONFIG });
  assert.equal(missing.errors.length, 1);
  assert.match(missing.errors[0], /no citation/);
});

test('SPEC-sensitive PR with DEVIATIONS option passes only when DEVIATIONS.md changes', () => {
  assert.deepEqual(
    validatePullRequestMetadata({
      body: body({ selected: 2 }),
      files: [...CONFIG, { filename: 'DEVIATIONS.md', status: 'modified' }],
    }).errors,
    [],
  );
  const missing = validatePullRequestMetadata({ body: body({ selected: 2 }), files: CONFIG });
  assert.equal(missing.errors.length, 1);
  assert.match(missing.errors[0], /does not modify DEVIATIONS\.md/);
});

test('missing closing issue and ambiguous checklist are each reported', () => {
  const noIssue = validatePullRequestMetadata({
    body: body({ closes: 'no link here', selected: 0 }),
    files: NON_SENSITIVE,
  });
  assert.equal(noIssue.errors.length, 1);
  assert.match(noIssue.errors[0], /closing keyword/);

  const none = validatePullRequestMetadata({
    body: `Closes #1\n${checklist(0).replace('- [x]', '- [ ]')}`,
    files: NON_SENSITIVE,
  });
  assert.ok(none.errors.some((e) => /exactly one/.test(e)));
});
