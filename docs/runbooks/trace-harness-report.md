# Trace harness report

Use this read-only command to generate the first trace-driven harness report
from existing evidence. It implements the Milestone 1 subset from
`docs/design/trace-driven-harness-improvement.md`: group bounded evidence into
reviewable clusters. It does not mutate tracker state. It does not open PRs.
It does not edit prompts. It does not create a worker phase, merge gate, or
evaluator gate.

```bash
python3 scripts/trace-harness-report.py \
  --worker-log /path/to/worker.log \
  --json-out /tmp/trace-harness-report.json
```

## Supported inputs

- worker process logs emitted by the existing worker event emitter, including
  `runner_timeout`, failed `runner_end`, `turn_input_required`,
  `unsupported_tool_call`, and malformed protocol runtime events.

Multiple `--worker-log` flags are allowed.

The command writes compact JSON so the emitted report uses the same byte
accounting as the documented bounds. For local inspection, pipe the file through
`python3 -m json.tool` or `jq`.

## Unsupported inputs

This first subset does not fetch tracker comments, PR review threads, Codex
review comments, human reviews, CI logs, or workspace `.aiops` artifacts such as
`.aiops/CODEX_APP_SERVER_OUTPUT.txt`, `.aiops/PLAN.md`, or `.aiops/TASK.md`.
Those are still valid design inputs, but this command only reports from local
worker logs already collected by the operator. It also does not automatically
open issues or draft PRs; that belongs to the later proposal-rendering
milestone.

## Output schema

The JSON output uses schema `trace-harness-report/v1`. Each cluster contains:

- cluster id and short title
- symptom class
- affected issue, issue identifier, PR, run, and session ids when present in
  the structured prefix or in trusted top-level scalar payload fields before the
  first opaque payload key;
  pathological clusters may include `affected.omitted` counts when the cluster
  byte cap requires truncating these id lists
- evidence references with bounded excerpts
- suspected harness surface to change
- proposed next action, currently `issue proposal` for supported worker-log
  failure clusters
- acceptance criteria for the proposed harness change
- redaction note naming what was omitted

## Redaction and bounds

The report stores metadata first: event kind, timestamp, issue/run/session ids,
source path, line number, and known scalar payload fields. Text evidence is
excerpted only to prove grouping. Payload fields that can contain arbitrary
agent, protocol, or tool text are opaque: `output_head`, `output_tail`, `error`,
`arguments`, `arguments_raw`, `raw`, and `params` are redacted from excerpts
instead of parsed. The importer keeps safe scalar metadata before the first
opaque payload key, then treats the rest of that payload as opaque; it does not
parse GraphQL or other embedded protocol text. This hard boundary applies even
when an opaque value looks bracket-balanced, because Go's human-readable
`map[...]` formatting does not escape arbitrary string values and cannot prove
that later key-shaped text is a real top-level field. The same rule applies to
ambiguous multiline closures: a single trailing `]` inside an opaque value does
not prove that a following event-shaped line is a real worker log record, so the
importer prefers omission over fabricating affected issues.

Bounds are enforced at 64 KiB per run and 256 KiB per cluster. Individual
evidence excerpts are smaller, known scalar metadata is byte-bounded per field
and in aggregate, and oversized affected id lists are truncated with omitted
counts, so a reviewer can inspect the cluster without opening full prompts,
full agent streams, full CI logs, raw GraphQL payloads, tokens, clone URLs with
userinfo, or complete tracker comments. Clone URL userinfo that appears in
trusted scalar metadata is masked with the same `MaskCloneURL` convention used
by worker diagnostics, and token-like prefixes are replaced with
`[redacted-token]`; clone URLs inside opaque text are omitted with the rest of
the opaque payload.

## Examples

Worker-log only:

```bash
python3 scripts/trace-harness-report.py \
  --worker-log ./worker.log \
  --json-out ./trace-report.json
```

Multiple worker logs:

```bash
python3 scripts/trace-harness-report.py \
  --worker-log ./maker.log \
  --worker-log ./reviewer.log \
  --json-out ./trace-report.json
```
