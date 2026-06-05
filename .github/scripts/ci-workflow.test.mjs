import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';

function jobBlock(workflow, jobName) {
  const start = workflow.match(new RegExp(`^  ${jobName}:\\n`, 'm'));
  assert.ok(start, `missing ${jobName} job`);

  const rest = workflow.slice(start.index + start[0].length);
  const nextJob = rest.search(/^  [a-zA-Z0-9_-]+:\n/m);
  return nextJob === -1 ? rest : rest.slice(0, nextJob);
}

test('CI keeps standalone go vet step in security job', async () => {
  const workflow = await readFile(new URL('../workflows/ci.yml', import.meta.url), 'utf8');
  const security = jobBlock(workflow, 'security');

  assert.match(security, /^\s+name: Security and supply-chain$/m);
  assert.match(security, /^\s+- name: go vet\n\s+run: go vet \.\/\.\.\.$/m);
});
