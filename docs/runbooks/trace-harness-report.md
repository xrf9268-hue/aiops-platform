# Trace harness report

Use this read-only command to generate the first trace-driven harness report
from existing evidence. It implements the Milestone 1 grouped-report path plus
the later proposal surfaces from
`docs/design/trace-driven-harness-improvement.md`: issue/draft-PR proposals and
advisory evaluator candidates. It does not mutate tracker state. It does not
open PRs. It does not edit prompts. It does not create a worker phase, merge
gate, runtime gate, or evaluator gate.

```bash
python3 scripts/trace-harness-report.py \
  --worker-log /path/to/worker.log \
  --json-out /tmp/trace-harness-report.json
```

## Supported inputs

- worker process logs emitted by the existing worker event emitter, including
  `runner_timeout`, failed `runner_end`, `turn_input_required`,
  `unsupported_tool_call`, and malformed protocol runtime events.
- durable trace-evidence manifests (`--evidence-manifest`) produced by
  [`trace-evidence-manifest.py`](trace-evidence-manifest.md) from retained
  worker logs. A manifest carries the same already-redacted, byte-bounded
  worker-event evidence, so the report re-clusters it by failure class without
  re-reading raw logs and records it as an `evidence_manifest` input.

Multiple `--worker-log` and `--evidence-manifest` flags are allowed, and the two
input kinds may be combined.

The command writes compact JSON so the emitted report uses the same byte
accounting as the documented bounds. For local inspection, pipe the file through
`python3 -m json.tool` or `jq`.

## Unsupported inputs

This first subset does not fetch tracker comments, PR review threads, Codex
review comments, human reviews, CI logs, or workspace `.aiops` artifacts such as
`.aiops/CODEX_APP_SERVER_OUTPUT.txt`, `.aiops/PLAN.md`, or `.aiops/TASK.md`.
Those are still valid design inputs, but this command only reports from local
worker logs already collected by the operator. It also does not automatically
open issues or draft PRs; proposal text is rendered for an explicit operator or
agent action.

## How the importer reads a worker log

Each worker event is one log record beginning with the Go log timestamp and
`event=<kind>`. The structured prefix up to ` payload=` is single line and
`%q`-escaped, so the importer reads event kind, `task_id`/`issue_id`,
`issue_identifier`, `session_id`, and PR ids from it directly. The
`payload=map[...]` region is Go's `%v` map rendering: unescaped, lexically
key-sorted, and free-form values can span multiple physical lines and contain
arbitrary brackets, timestamps, and `event=` text. That rendering is a one-way
diagnostic format, not a parseable one, so the importer never tries to find
where a `map[...]` ends. It reads trusted scalar metadata from the record's
first line only, as a contiguous left-to-right run of space-free `key:value`
chunks, and stops at the first chunk that is not a recognized safe scalar — an
opaque payload key, an agent-controlled key (the chosen `tool` name, whose value
Go renders unquoted and may contain spaces), an unrecognized key, or free-form
text. The rest of the payload — including continuation lines — is treated as
opaque and never parsed. Stopping at that boundary is what keeps text inside an
earlier unquoted value from being promoted as if it were a later top-level key.

### Known limitation

Because a scalar Go sorts behind that boundary lands past it (often on a skipped
continuation line), its value is not recovered: `unsupported_tool_call` exposes
`arguments` (opaque) and the agent-controlled `tool`, `turn_input_required`
sorts `params` before `session_id`, and `runner_timeout` sorts
`output_head`/`output_tail` before `timeout_ms`. The
cluster still reports the failure class and the affected run/issue ids from the
prefix. The harness fix is for the worker to emit a structured payload (for
example a `%q`-quoted JSON object) so those scalars become recoverable; that is
exactly the kind of improvement this report is meant to surface. A related,
unavoidable consequence of the unescaped format is that opaque output which
reproduces the full record-start grammar (a log timestamp plus `event=<known
kind>`) is indistinguishable from a real record and may be surfaced.

## Output schema

The JSON output uses schema `trace-harness-report/v3`. Each cluster contains:

- cluster id and short title
- symptom class
- affected issue, issue identifier, PR, run, and session ids read from the
  structured prefix or from trusted top-level scalar payload fields before the
  first opaque payload key;
  pathological clusters may include `affected.omitted` counts when the cluster
  byte cap requires truncating these id lists
- evidence references with bounded excerpts and the affected ids recovered for
  each retained evidence entry
- suspected harness surface to change
- proposed next action, currently `issue, draft-PR, or advisory evaluator
  proposal` for supported worker-log failure clusters
- acceptance criteria for the proposed harness change
- redaction note naming what was omitted
- `proposals.github_issue.body`, a ready-to-open issue body for promoting the
  reviewed cluster without hand-writing the issue
- `proposals.draft_pr.plan`, a draft-PR plan a normal coding agent can use for
  follow-through after the operator approves the intent
- `proposals.advisory_evaluator`, a report-only evaluator candidate with
  recovered affected ids, bounded fixtures/examples, expected true-positive and
  false-positive behavior, a future report-output contract, and the evidence
  required before a later gate-promotion PR

The proposal fields repeat references and bounded metadata, not raw unbounded
logs. They preserve the same redaction note and SPEC boundary as the cluster:
the worker does not open the issue or PR, mutate tracker state, rewrite harness
files, or create evaluator gates.

## Promoting a reviewed cluster

After reviewing a cluster, extract the generated issue body or draft-PR plan and
pass it to the normal GitHub or coding-agent workflow:

```bash
jq -r '.clusters[] | select(.id == "runner-timeout").proposals.github_issue.body' \
  ./trace-report.json > /tmp/runner-timeout-issue.md

jq -r '.clusters[] | select(.id == "runner-timeout").proposals.draft_pr.plan' \
  ./trace-report.json > /tmp/runner-timeout-pr-plan.md
```

Opening the issue, starting the draft PR, or running a coding agent remains an
explicit workflow action outside this report command.
Use [`trace-harness-follow-through.md`](trace-harness-follow-through.md) for
the approved-proposal handoff into a normal coding-agent branch, PR, or no-op
closure.

## Advisory evaluator candidates

Each cluster also renders an advisory evaluator candidate:

```bash
jq '.clusters[] | select(.id == "runner-timeout").proposals.advisory_evaluator' \
  ./trace-report.json > /tmp/runner-timeout-evaluator-candidate.json
```

The candidate is report-only. Its `execution` block explicitly records that it
does not block CI, runtime, or merge. Its `recovered_affected_ids` block is an
independently bounded issue/run/session/PR summary with omitted counts, not a
full duplicate of the cluster's top-level affected arrays. It carries enough of
the references used to compute `current_signal` for the candidate to remain
reviewable when it is extracted on its own. `current_signal` counts independent
retained evidence occurrences by
merging entries that share a recovered affected id; same-column omitted counts
from byte-bound clusters still count as recurrence evidence. Its fixtures are
bounded examples copied from the cluster's
already-redacted evidence references, not raw logs or prompt text. Its
`expected_signal_behavior.false_positive` section names cases the candidate must
not treat as signal, including successful or unrelated events that only mention
the failure inside opaque payload text.

The candidate's `future_report_output` schema is stable enough for a future L4
report importer to cite evaluator results, but this command does not install a
gate. Promotion to a required CI/runtime/merge gate needs a separate reviewed PR
after multiple reviewed reports show stable true positives and documented false
positive handling.

## Advisory evaluator result artifacts

Milestone 6 closes the report-only evaluator feedback loop without adding a
worker phase or tracker writer. Add `--evaluator-results-out` to a report run to
emit a durable result artifact:

```bash
python3 scripts/trace-harness-report.py \
  --evidence-manifest ./trace-evidence-manifest.json \
  --json-out ./trace-report.json \
  --evaluator-results-out ./trace-evaluator-results.json
```

The artifact uses schema `trace-harness-advisory-evaluator-results/v1`. Its
`results[]` entries keep the M4-declared record shape
`trace-harness-advisory-evaluator-result/v1`:

- `schema`
- `evaluator_id`
- `source_cluster_id`
- `mode`
- `signal`
- `evidence_refs`
- `false_positive_notes`

The artifact is metadata-first and bounded like the trace evidence manifest: it
stores input digests, byte bounds, bounded redacted evidence refs, and bounded
false-positive notes. It does not store raw prompts, raw worker streams, forge
comments, GraphQL payloads, CI logs, tokens, unmasked clone URLs, or full
payload text.

## Consuming prior evaluator results

A later manual report run can consume one or more prior result artifacts:

```bash
python3 scripts/trace-harness-report.py \
  --evidence-manifest ./new-trace-evidence-manifest.json \
  --prior-evaluator-results ./trace-evaluator-results.json \
  --json-out ./new-trace-report.json
```

If the current report and a prior artifact both show a
`positive-recurring-cluster` signal for the same `source_cluster_id`, the report
adds a top-level `recurrence_escalations[]` entry. When the affected cluster has
room under its byte cap, the report also mirrors the same proposal at
`clusters[].proposals.recurrence_escalation`; high-volume clusters may omit that
mirror to preserve the cluster bound, but the top-level proposal remains the
canonical M6 ratchet output. The report remembers that the same failure class
returned and produces a reviewable forge/agent action proposal.

The recurrence proposal is idempotent by construction. It carries a stable
dedupe marker:

```text
trace-harness-recurrence:<source_cluster_id>:<evaluator_id>
```

Use that marker when reopening or bumping the existing tracking issue through
ordinary forge tooling. For example, search the issue comments/body for the
marker first; if absent, reopen the issue if closed or add the generated
`forge_comment` once. The report command itself remains read-only: it does not
open issues, post comments, mutate tracker state, rewrite harness files, or
promote an evaluator to a CI/runtime/merge gate. The later scheduled M7 driver
can reuse the same marker when it automates this explicit forge step.

## Redaction and bounds

The report stores metadata first: event kind, timestamp, issue/run/session ids,
source path, line number, and known scalar payload fields. Text evidence is the
record's structured prefix only; the entire `payload=map[...]` region is omitted
from excerpts and replaced with `payload=[redacted-payload]`, so arbitrary
agent, protocol, or tool text in `output_head`, `output_tail`, `error`,
`arguments`, `arguments_raw`, `raw`, and `params` is never reproduced. The
importer keeps safe scalar metadata before the first opaque payload key, which
is a hard boundary for the rest of that payload; it does not parse GraphQL or
other embedded protocol text.

Bounds are enforced at 64 KiB per run and 256 KiB per cluster. Individual
evidence excerpts are smaller, known scalar metadata is byte-bounded per field
and in aggregate, and oversized affected id lists are truncated with omitted
counts, so a reviewer can inspect the cluster without opening full prompts,
full agent streams, full CI logs, raw GraphQL payloads, tokens, clone URLs with
userinfo, or complete tracker comments. Clone URL userinfo that reaches the
prefix or trusted scalar metadata is masked with the same `MaskCloneURL`
convention used by worker diagnostics, and token-like prefixes are replaced with
`[redacted-token]`.

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

Durable evidence manifest (see
[`trace-evidence-manifest.md`](trace-evidence-manifest.md)):

```bash
python3 scripts/trace-harness-report.py \
  --evidence-manifest ./trace-evidence-manifest.json \
  --json-out ./trace-report.json
```
