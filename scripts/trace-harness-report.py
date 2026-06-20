#!/usr/bin/env python3
"""Generate grouped trace-driven harness reports from worker logs.

The worker emits one structured log line per event:

    <timestamp> event=<kind> task_id=<id> issue_id=<id> [issue_identifier=<id>]
        [session_id=<id>] msg=<%q-quoted> payload=<%v map rendering>

Everything up to ` payload=` is a single line of Go `key=value` logging with a
`%q`-escaped `msg`, so it is safe to parse. Everything after ` payload=` is Go's
`%v` rendering of a map: it is unescaped, lexically key-sorted, and free-form
values (output, errors, tool arguments, params) can span multiple physical lines
and contain arbitrary brackets, timestamps, and `event=` text. That rendering is
a one-way diagnostic format, not a parseable one, so this importer never tries to
find where a `map[...]` ends. It reads trusted scalar metadata from the first
physical line only, as a contiguous left-to-right run of space-free `key:value`
chunks, stopping at the first chunk that is not a recognized safe scalar (an
opaque key, an agent-controlled key such as the chosen `tool` name, an
unrecognized key, or free-form text). The rest of the payload — including
continuation lines — is treated as opaque. The documented cost is that scalars Go
sorts behind such a boundary (e.g. `tool`, `timeout_ms`, payload `session_id`)
are not recovered; the harness fix is to make the worker emit a structured
payload, which this report is meant to surface as an improvement.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import re
import sys
from collections.abc import Iterator
from datetime import datetime, timezone
from pathlib import Path
from urllib.parse import urlsplit, urlunsplit

SCHEMA_VERSION = "trace-harness-report/v3"
# Single source of truth for the manifest schema id, shared by the producer
# (trace-evidence-manifest.py) and the --evidence-manifest consumer's validation.
EVIDENCE_MANIFEST_SCHEMA_VERSION = "trace-evidence-manifest/v1"
MAX_RUN_EVIDENCE_BYTES, MAX_CLUSTER_BYTES, MAX_EVIDENCE_EXCERPT_BYTES = 64 * 1024, 256 * 1024, 4 * 1024
MAX_METADATA_BYTES, MAX_METADATA_VALUE_BYTES = 16 * 1024, 4 * 1024
MAX_PROPOSAL_EVIDENCE_REFS, MAX_PROPOSAL_AFFECTED_VALUES = 5, 5
MAX_PROPOSAL_INPUT_REFS, MAX_PROPOSAL_FIELD_BYTES = 3, 512
MAX_EVALUATOR_FIXTURES, MAX_EVALUATOR_FIXTURE_EXCERPT_BYTES = 3, 512

FIELD_RE = re.compile(r'\b(event|task_id|issue_id|issue_identifier|session_id|pr|pr_number|pr_url|pull_request|pull_request_url)=("[^"]*"|\S+)')
CLONE_URL_SCHEME_RE = re.compile(r"\b[A-Za-z][A-Za-z0-9+.-]*://")
TIMESTAMP_RE = re.compile(r"^(\d{4}/\d{2}/\d{2}\s+\d{2}:\d{2}:\d{2}(?:\.\d+)?)\b")
WORKER_RECORD_START_RE = re.compile(r"^\d{4}/\d{2}/\d{2}\s+\d{2}:\d{2}:\d{2}(?:\.\d+)?\s+event=")
TOKEN_RE = re.compile(
    r"\b(?:gh[opurs]_[A-Za-z0-9_]{20,}|github_pat_[A-Za-z0-9_]{20,}|"
    r"glpat-[A-Za-z0-9_-]{20,}|xox[baprs]-[A-Za-z0-9-]{20,}|"
    r"lin_api_[A-Za-z0-9_]{8,}|sk-(?:ant-)?[A-Za-z0-9_-]{20,})\b"
)
PR_KEYS = ("pr", "pr_number", "pr_url", "pull_request", "pull_request_url")
AFFECTED_KEYS = ("issues", "issue_identifiers", "pull_requests", "runs", "sessions")
# Free-form payload values: unescaped, possibly multi-line, never parsed. They
# are named in the redaction note; the first of these (or any unrecognized key)
# on a record's first line ends trusted scalar extraction.
OPAQUE_PAYLOAD_KEYS = ("arguments", "arguments_raw", "error", "output_head", "output_tail", "params", "raw")
# Trusted scalars: worker/tracker-generated and space-free by construction. Go
# renders map values unquoted, so a key whose value can contain a space (e.g. the
# agent-chosen `tool` name) is NOT listed here — its value tail would otherwise be
# misread as later top-level keys. Such keys, like any unrecognized key, stop the
# scan (see scalar_payload).
SAFE_PAYLOAD_KEYS = set(
    "elapsed_ms timeout_ms duration_ms output_bytes output_dropped model method task_id issue_id "
    "issue_identifier session_id pr pr_number pr_url pull_request pull_request_url exit_code ok".split()
)
CLASS_BY_EVENT = {
    "runner_timeout": ("runner-timeout", "Runner timeouts", "runner timeout"),
    "turn_input_required": ("input-required", "Input required stops", "input required"),
    "unsupported_tool_call": ("tool-unsupported", "Unsupported tool calls", "tool unsupported"),
    "malformed": ("malformed-protocol", "Malformed protocol output", "malformed protocol output"),
}
# runner_end is the one kind classified from its prefix message, not its kind, so
# its (id, title, symptom) lives here as the single source for both classify()
# and the cluster-id lookup CLASS_BY_ID below.
RUNNER_FAILURE_CLASS = ("runner-failure", "Runner failures", "runner failure")
# cluster id -> (id, title, symptom), so a durable manifest event that already
# carries its resolved failure class is folded without re-running classify() on a
# byte-truncated, redacted excerpt (which could lose the runner_end prefix match).
CLASS_BY_ID = {value[0]: value for value in CLASS_BY_EVENT.values()}
CLASS_BY_ID[RUNNER_FAILURE_CLASS[0]] = RUNNER_FAILURE_CLASS


def parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--worker-log", action="append", default=[], type=Path)
    p.add_argument(
        "--evidence-manifest",
        action="append",
        default=[],
        type=Path,
        help="durable trace-evidence manifest (scripts/trace-evidence-manifest.py) to consume instead of, or alongside, raw worker logs",
    )
    p.add_argument("--json-out", type=Path, help="write JSON report to this path; default stdout")
    return p


def main(argv: list[str]) -> int:
    args = parser().parse_args(argv)
    try:
        report = generate(args.worker_log, args.evidence_manifest)
        raw = report_json(report)
        if args.json_out:
            args.json_out.write_text(raw)
        else:
            sys.stdout.write(raw)
    except Exception as exc:  # argparse has already handled usage errors.
        print(mask(str(exc)), file=sys.stderr)
        return 1
    return 0


def generate(worker_logs: list[Path], manifests: list[Path] | None = None) -> dict:
    manifests = manifests or []
    if not worker_logs and not manifests:
        raise ValueError("at least one --worker-log or --evidence-manifest is required")
    clusters: dict[str, dict] = {}
    run_bytes: dict[str, int] = {}
    inputs = [input_ref(path) for path in worker_logs]
    inputs += [input_ref(path, "evidence_manifest") for path in manifests]
    report_ref = proposal_report_reference(inputs)
    for path in worker_logs:
        for finding in parse_worker_log(path):
            add_finding(clusters, run_bytes, finding)
    for path in manifests:
        fold_manifest(clusters, run_bytes, path)
    return {
        "schema_version": SCHEMA_VERSION,
        "generated_at": datetime.now(timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z"),
        "inputs": inputs,
        "bounds": {
            "max_run_evidence_bytes": MAX_RUN_EVIDENCE_BYTES,
            "max_cluster_bytes": MAX_CLUSTER_BYTES,
        },
        "clusters": [finalize_cluster(clusters[key], report_ref) for key in sorted(clusters)],
    }


def input_ref(path: Path, kind: str = "worker_log") -> dict:
    # Stream the digest/byte count so a multi-GB production log is never read
    # whole into memory; the parser below already reads it line by line.
    digest, size = hashlib.sha256(), 0
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
            size += len(chunk)
    return {
        "type": kind,
        "path": mask(str(path)),
        "bytes": size,
        "sha256": digest.hexdigest(),
    }


def proposal_report_reference(inputs: list[dict]) -> str:
    refs = []
    for item in inputs[:MAX_PROPOSAL_INPUT_REFS]:
        sha = item.get("sha256", "")
        refs.append(
            f"{item.get('type', 'input')} {md_code(str(item.get('path', '')))} "
            f"sha256:{sha[:12]} bytes:{item.get('bytes', 0)}"
        )
    if len(inputs) > MAX_PROPOSAL_INPUT_REFS:
        refs.append(f"plus {len(inputs) - MAX_PROPOSAL_INPUT_REFS} more input(s) in report.inputs")
    return f"{SCHEMA_VERSION}; inputs: " + "; ".join(refs)


def parse_worker_log(path: Path) -> Iterator[dict]:
    # Yield findings so generate() folds them into bounded cluster state one at a
    # time; a large log never materializes every hit in memory at once.
    with path.open(encoding="utf-8", errors="replace") as handle:
        for line_no, line in enumerate(handle, 1):
            if parsed := parse_line(path, line_no, line.rstrip("\n")):
                yield parsed


def parse_line(path: Path, line_no: int, line: str) -> dict | None:
    if not WORKER_RECORD_START_RE.match(line):
        return None
    prefix, payload, has_payload = split_payload(line)
    fields = prefix_field_values(prefix)
    kind = fields.get("event", "")
    event_class = classify(kind, prefix)
    if not event_class:
        return None
    cid, title, symptom = event_class
    metadata = scalar_payload(payload)
    if timestamp := line_timestamp(prefix):
        metadata.setdefault("timestamp", timestamp)
    return {
        "id": cid,
        "title": title,
        "symptom": symptom,
        "ref": f"{mask(str(path))}:{line_no}",
        "kind": kind,
        "excerpt": evidence_excerpt(prefix, has_payload),
        "metadata": metadata,
        "issue": fields.get("issue_id", "") or metadata.get("issue_id", ""),
        "identifier": fields.get("issue_identifier", "") or metadata.get("issue_identifier", ""),
        "run": fields.get("task_id", "") or metadata.get("task_id", ""),
        "session": fields.get("session_id", "") or metadata.get("session_id", ""),
        "pull_request": first_mapped_value(fields, metadata, PR_KEYS),
    }


def classify(kind: str, prefix: str) -> tuple[str, str, str] | None:
    if kind in CLASS_BY_EVENT:
        return CLASS_BY_EVENT[kind]
    if kind == "runner_end" and "runner failed" in prefix.lower():
        return RUNNER_FAILURE_CLASS
    return None


def fold_manifest(clusters: dict, run_bytes: dict, path: Path) -> None:
    # A trace-evidence manifest (scripts/trace-evidence-manifest.py) is the
    # durable, pre-redacted form of the same worker-event evidence
    # parse_worker_log() yields. Its events are already masked and byte-bounded,
    # so consuming one re-clusters by failure class without re-reading raw logs.
    data = json.loads(path.read_text(encoding="utf-8"))
    # Reject a wrong artifact (e.g. a trace-harness-report/v3 output) loudly:
    # silently treating an unrecognized JSON dict as an empty manifest would hide
    # real evidence behind a clean-looking zero-cluster report.
    if not isinstance(data, dict) or data.get("schema_version") != EVIDENCE_MANIFEST_SCHEMA_VERSION:
        raise ValueError(f"{mask(str(path))}: not a {EVIDENCE_MANIFEST_SCHEMA_VERSION} evidence manifest")
    runs = data.get("runs", [])
    if not isinstance(runs, list):
        raise ValueError(f"{mask(str(path))}: evidence manifest 'runs' must be a list")
    for run in runs:
        if isinstance(run, dict):
            fold_manifest_run(clusters, run_bytes, run)


def fold_manifest_run(clusters: dict, run_bytes: dict, run: dict) -> None:
    classes = set()
    for event in run.get("events", []):
        finding = manifest_event_finding(event)
        if not finding:
            continue
        classes.add(finding["id"])
        add_finding(clusters, run_bytes, finding)
    # The manifest records the full run-level affected summary before its 64 KiB
    # event cap drops later events. When every retained event shares one failure
    # class, fold that summary into the cluster so a chatty capped run does not
    # undercount affected ids and recurrence/advisory signal matches the raw
    # --worker-log path. A multi-class run is left to its retained events to
    # avoid attributing a dropped event's ids to the wrong class.
    if len(classes) == 1:
        fold_run_affected(clusters[next(iter(classes))], run.get("affected", {}))


def fold_run_affected(cluster: dict, affected: dict) -> None:
    if not isinstance(affected, dict):
        return
    for key in AFFECTED_KEYS:
        for value in affected.get(key, []) or []:
            add_unique(cluster, key, value)


def manifest_event_finding(event: dict) -> dict | None:
    if not isinstance(event, dict):
        return None
    # The manifest stores the resolved failure class, so trust it through a fixed
    # set lookup instead of re-parsing the redacted excerpt; an unknown class is
    # skipped rather than guessed.
    event_class = CLASS_BY_ID.get(event.get("class", ""))
    if not event_class:
        return None
    cid, title, symptom = event_class
    affected = event.get("affected", {})
    affected = affected if isinstance(affected, dict) else {}
    metadata = event.get("metadata", {})
    return {
        "id": cid,
        "title": title,
        "symptom": symptom,
        "ref": event.get("ref", ""),
        "kind": event.get("kind", ""),
        "excerpt": event.get("excerpt", ""),
        "metadata": metadata if isinstance(metadata, dict) else {},
        "issue": first_affected(affected, "issues"),
        "identifier": first_affected(affected, "issue_identifiers"),
        # Use the event's own run id; the manifest's run group key is an
        # issue-id fallback when the source event had no task_id, and promoting
        # it here would fabricate an affected run the raw path never reports.
        "run": first_affected(affected, "runs"),
        "session": first_affected(affected, "sessions"),
        "pull_request": first_affected(affected, "pull_requests"),
    }


def first_affected(affected: dict, key: str) -> str:
    values = affected.get(key) or []
    return values[0] if isinstance(values, list) and values else ""


def split_payload(line: str) -> tuple[str, str, bool]:
    if " payload=" not in line:
        return line, "", False
    prefix, payload = line.split(" payload=", 1)
    return prefix, payload, True


def line_timestamp(prefix: str) -> str:
    match = TIMESTAMP_RE.match(prefix)
    return mask(match.group(1)) if match else ""


def prefix_field_values(prefix: str) -> dict:
    # Cut at ` msg=` so the %q-quoted message body is never scanned for fields.
    head = prefix.split(" msg=", 1)[0]
    return {m.group(1): mask(m.group(2).strip('"')) for m in FIELD_RE.finditer(head)}


def scalar_payload(payload: str) -> dict:
    # Scan the first line's map body as a left-to-right run of space-separated
    # `key:value` chunks. Stop at the first chunk that is not a recognized safe
    # scalar — an opaque key, an agent-controlled key, an unrecognized key, or
    # free-form text. Because Go renders values unquoted, this contiguous-run
    # boundary is the only sound way to avoid promoting text inside an earlier
    # value as if it were a later top-level key.
    if not payload.startswith("map["):
        return {}
    fields: dict[str, str] = {}
    for chunk in map_payload_first_line_body(payload).split():
        key, sep, value = chunk.partition(":")
        if not sep or key not in SAFE_PAYLOAD_KEYS:
            break
        if key not in fields:
            fields[key] = mask(value)
    return fields


def map_payload_first_line_body(payload: str) -> str:
    # `payload` is the first physical line only. Drop `map[` and, when this is a
    # single-line payload, the matching trailing `]`. A multi-line payload's
    # first line has no trailing `]`, and its continuation lines are skipped by
    # parse_line because they do not start a worker record.
    body = payload[len("map["):]
    return body[:-1] if body.endswith("]") else body


def evidence_excerpt(prefix: str, has_payload: bool) -> str:
    # The payload region is never reproduced: trusted scalars already live in
    # metadata, and opaque values are unescaped free-form text.
    excerpt = mask(prefix)
    return f"{excerpt} payload=[redacted-payload]" if has_payload else excerpt


def add_finding(clusters: dict[str, dict], run_bytes: dict[str, int], finding: dict) -> None:
    cluster = clusters.setdefault(finding["id"], new_cluster(finding))
    add_unique(cluster, "issues", finding["issue"])
    add_unique(cluster, "issue_identifiers", finding["identifier"])
    add_unique(cluster, "pull_requests", finding["pull_request"])
    add_unique(cluster, "runs", finding["run"])
    add_unique(cluster, "sessions", finding["session"])
    entry, entry_size = bounded_evidence_entry(cluster, run_bytes, finding)
    if not entry:
        return
    cluster["evidence"].append(entry)
    cluster["_evidence_bytes"] = evidence_array_bytes_after(cluster, entry_size)
    cluster["_evidence_count"] += 1


def new_cluster(finding: dict) -> dict:
    return {
        "id": finding["id"],
        "title": finding["title"],
        "symptom_class": finding["symptom"],
        "affected": {"issues": [], "issue_identifiers": [], "pull_requests": [], "runs": [], "sessions": []},
        "evidence": [],
        # O(1) dedup membership per affected key; emitted arrays stay sorted.
        "_seen": {key: set() for key in AFFECTED_KEYS},
        "_evidence_bytes": byte_len("[]"),
        "_evidence_count": 0,
        "suspected_harness_surface": "WORKFLOW.md, reviewer rubrics, skills, hooks, tests, CI, or docs",
        "proposed_next_action": "issue, draft-PR, or advisory evaluator proposal",
        "acceptance_criteria": [
            "Future runs cover this failure class with a reviewed harness surface or a documented no-op decision.",
            "The change remains report/agent/workflow-owned and does not add a worker-side gate or tracker writer.",
        ],
        "redaction_note": (
            "Every payload=map[...] region is omitted from excerpts because Go's %v map rendering is unescaped "
            "and not reliably parseable. Only trusted top-level scalar metadata before the first opaque payload "
            "key is kept; output_head, output_tail, error, arguments, arguments_raw, raw, and params are treated "
            "as opaque. Clone URL userinfo follows workflow.MaskCloneURL, and token-like values are redacted."
        ),
    }


def bounded_evidence_entry(cluster: dict, run_bytes: dict[str, int], finding: dict) -> tuple[dict | None, int]:
    run = finding["run"] or finding["issue"] or "unknown"
    used = run_bytes.get(run, 0)
    text = mask(finding["excerpt"])
    best, best_size = None, 0
    low, high = 1, min(MAX_EVIDENCE_EXCERPT_BYTES, byte_len(text))
    while low <= high:
        limit = (low + high) // 2
        entry = evidence_entry(finding, truncate(text, limit))
        entry_size = evidence_entry_bytes(entry)
        if entry_size <= MAX_RUN_EVIDENCE_BYTES - used and evidence_array_bytes_after(cluster, entry_size) <= MAX_CLUSTER_BYTES:
            best, best_size = entry, entry_size
            low = limit + 1
        else:
            high = limit - 1
    if best:
        run_bytes[run] = used + best_size
    return best, best_size


def evidence_entry(finding: dict, excerpt: str) -> dict:
    return {
        "source": "worker_log",
        "ref": finding["ref"],
        "kind": finding["kind"],
        "excerpt": excerpt,
        "affected": finding_affected(finding),
        "metadata": bounded_metadata(finding["metadata"]),
    }


def finding_affected(finding: dict) -> dict:
    keys = (
        ("issues", "issue"),
        ("issue_identifiers", "identifier"),
        ("pull_requests", "pull_request"),
        ("runs", "run"),
        ("sessions", "session"),
    )
    return {affected_key: [finding[finding_key]] for affected_key, finding_key in keys if finding.get(finding_key)}


def bounded_metadata(metadata: dict) -> dict:
    bounded = {key: truncate(value, MAX_METADATA_VALUE_BYTES) for key, value in metadata.items()}
    while encoded_bytes(bounded) > MAX_METADATA_BYTES and bounded:
        key = max(bounded, key=lambda item: byte_len(bounded[item]))
        value = bounded[key]
        if not value:
            break
        overage = encoded_bytes(bounded) - MAX_METADATA_BYTES
        limit = max(0, byte_len(value) - overage - byte_len("... [truncated]"))
        next_value = truncate(value, limit)
        bounded[key] = "" if next_value == value else next_value
    return bounded


def evidence_entry_bytes(entry: dict) -> int:
    return byte_len(json.dumps(entry, separators=(",", ":"), sort_keys=True))


def evidence_array_bytes_after(cluster: dict, entry_size: int) -> int:
    if cluster["_evidence_count"] == 0:
        return entry_size + byte_len("[]")
    return cluster["_evidence_bytes"] + byte_len(",") + entry_size


def finalize_cluster(cluster: dict, report_ref: str) -> dict:
    for key in AFFECTED_KEYS:
        cluster["affected"][key].sort()
    cluster.pop("_seen", None)  # drop before any encoded_bytes call: sets are not JSON-serializable.
    cluster.pop("_evidence_bytes", None)
    cluster.pop("_evidence_count", None)
    enforce_cluster_bound(cluster)
    sync_proposals(cluster, report_ref)
    return cluster


def sync_proposals(cluster: dict, report_ref: str) -> None:
    while True:
        before = proposal_source_signature(cluster)
        cluster["proposals"] = render_proposals(cluster, report_ref)
        enforce_cluster_bound(cluster)
        if proposal_source_signature(cluster) == before:
            return


def proposal_source_signature(cluster: dict) -> str:
    return json.dumps(
        {"affected": cluster.get("affected", {}), "evidence": cluster.get("evidence", [])},
        separators=(",", ":"),
        sort_keys=True,
    )


def render_proposals(cluster: dict, report_ref: str) -> dict:
    return {
        "github_issue": {
            "title": f"Trace harness: address {cluster['id']} cluster",
            "body": render_issue_body(cluster, report_ref),
        },
        "draft_pr": {
            "title": f"fix(harness): address {cluster['id']} trace cluster",
            "plan": render_draft_pr_plan(cluster, report_ref),
        },
        "advisory_evaluator": render_advisory_evaluator_candidate(cluster, report_ref),
    }


def render_issue_body(cluster: dict, report_ref: str) -> str:
    lines = [
        "## Summary",
        "",
        f"Trace harness cluster {md_code(cluster['id'])} reports {cluster['symptom_class']}.",
        "",
        "Part of #937. Design source: `docs/design/trace-driven-harness-improvement.md`.",
        "",
        "## Source",
        "",
        f"- Report: {report_ref}",
        f"- Cluster: {md_code(cluster['id'])}",
        f"- Failure class: {cluster['symptom_class']}",
        "",
        "## Observed evidence",
        "",
        *affected_lines(cluster),
        *evidence_reference_lines(cluster),
        "",
        "## Suspected harness surface",
        "",
        cluster["suspected_harness_surface"],
        "",
        "## Proposed scope",
        "",
        proposed_scope(cluster),
        "",
        "## Non-goals / SPEC boundary",
        "",
        *non_goal_lines(),
        "",
        "## Acceptance criteria",
        "",
        *checkbox_lines(cluster["acceptance_criteria"]),
        "",
        "## Verification expectations",
        "",
        *verification_lines(),
        "",
        "## Redaction",
        "",
        cluster["redaction_note"],
    ]
    return "\n".join(lines) + "\n"


def render_draft_pr_plan(cluster: dict, report_ref: str) -> str:
    lines = [
        "## Goal",
        "",
        proposed_scope(cluster),
        "",
        "## Source cluster",
        "",
        f"- Report: {report_ref}",
        f"- Cluster: {md_code(cluster['id'])}",
        f"- Failure class: {cluster['symptom_class']}",
        "",
        "## Evidence to review",
        "",
        *affected_lines(cluster),
        *evidence_reference_lines(cluster),
        "",
        "## Implementation plan",
        "",
        "1. Inspect the suspected harness surface and confirm the smallest repo-owned change.",
        "2. Implement that change through normal coding-agent workflow on a branch and draft PR.",
        "3. Add or update focused tests, docs, or fixtures that pin the harness behavior.",
        "4. Record a reviewed no-op decision instead if the cluster is not actionable.",
        "",
        "## Non-goals / SPEC boundary",
        "",
        *non_goal_lines(),
        "",
        "## Acceptance criteria",
        "",
        *checkbox_lines(cluster["acceptance_criteria"]),
        "",
        "## Verification expectations",
        "",
        *verification_lines(),
        "",
        "## Redaction",
        "",
        cluster["redaction_note"],
    ]
    return "\n".join(lines) + "\n"


def proposed_scope(cluster: dict) -> str:
    return (
        f"Make a reviewed harness change, or a reviewed no-op decision, that addresses "
        f"{md_code(cluster['id'])} ({cluster['symptom_class']}) at the suspected harness surface. "
        "Keep the worker as scheduler/runner/tracker reader and keep issue/PR creation as an explicit "
        "operator or coding-agent action."
    )


def affected_lines(cluster: dict) -> list[str]:
    lines = ["Affected ids recovered from trusted metadata:"]
    affected = cluster.get("affected", {})
    omitted = affected.get("omitted", {})
    emitted = False
    for key in AFFECTED_KEYS:
        values = affected.get(key, [])
        if not values and not omitted.get(key):
            continue
        sample = ", ".join(md_code(value) for value in values[:MAX_PROPOSAL_AFFECTED_VALUES])
        if len(values) > MAX_PROPOSAL_AFFECTED_VALUES:
            sample += f", plus {len(values) - MAX_PROPOSAL_AFFECTED_VALUES} more"
        if omitted.get(key):
            sample = f"{sample}; {omitted[key]} omitted by cluster byte cap" if sample else f"{omitted[key]} omitted by cluster byte cap"
        lines.append(f"- {key}: {sample}")
        emitted = True
    if not emitted:
        lines.append("- No affected ids were recovered beyond the evidence references.")
    return lines


def evidence_reference_lines(cluster: dict) -> list[str]:
    lines = ["Evidence references:"]
    evidence = cluster.get("evidence", [])
    for entry in evidence[:MAX_PROPOSAL_EVIDENCE_REFS]:
        lines.append(f"- {md_code(entry.get('ref', ''))} ({entry.get('kind', 'event')}{metadata_suffix(entry.get('metadata', {}))})")
    remaining = len(evidence) - MAX_PROPOSAL_EVIDENCE_REFS
    if remaining > 0:
        lines.append(f"- plus {remaining} additional bounded evidence entries in the report cluster.")
    if not evidence:
        lines.append("- No bounded evidence entry was retained; inspect the source report inputs.")
    return lines


def metadata_suffix(metadata: dict) -> str:
    parts = []
    for key in ("timestamp", "method", "model", "exit_code", "timeout_ms", "elapsed_ms", "duration_ms", "output_bytes", "output_dropped"):
        if value := metadata.get(key):
            parts.append(f"{key}={md_code(value)}")
    return "; " + ", ".join(parts) if parts else ""


def checkbox_lines(values: list[str]) -> list[str]:
    return [f"- [ ] {value}" for value in values]


def non_goal_lines() -> list[str]:
    return [
        "- Do not automatically open issues or PRs from the worker.",
        "- Do not mutate tracker state as worker business logic.",
        "- Do not edit WORKFLOW.md, rubrics, skills, tests, CI, or docs unless a normal reviewed coding-agent change is approved.",
        "- Do not create worker-owned verifier or evaluator gates.",
    ]


def verification_lines() -> list[str]:
    return [
        "- Add deterministic tests or fixtures for the changed harness surface.",
        "- Run the local gate appropriate to the touched files before opening or updating the PR.",
        "- Confirm the change preserves the redaction and SPEC-boundary constraints from the trace-driven harness design.",
    ]


def render_advisory_evaluator_candidate(cluster: dict, report_ref: str) -> dict:
    return {
        "id": f"{cluster['id']}-advisory-evaluator",
        "title": f"Advisory evaluator candidate for {cluster['id']} trace cluster",
        "mode": "report-only/advisory",
        "target_failure_class": cluster["symptom_class"],
        "source": {
            "report": report_ref,
            "cluster_id": cluster["id"],
        },
        "current_signal": evaluator_current_signal(cluster),
        "recovered_affected_ids": evaluator_recovered_affected_ids(cluster),
        "fixtures": evaluator_fixtures(cluster),
        "expected_signal_behavior": {
            "true_positive": [
                f"Report {cluster['symptom_class']} when a future grouped report has at least two independent recovered issue, run, session, or PR references for {cluster['id']}.",
                "Use only trusted metadata and redacted evidence references already present in trace harness reports.",
            ],
            "false_positive": [
                "Do not fire on successful or unrelated events that only mention the failure in opaque payload text.",
                "Do not parse raw logs, forge comments, prompts, model output, GraphQL payloads, or other natural language as a machine contract.",
            ],
        },
        "execution": {
            "mode": "report-only",
            "blocks_ci": False,
            "blocks_runtime": False,
            "blocks_merge": False,
        },
        "future_report_output": {
            "schema": "trace-harness-advisory-evaluator-result/v1",
            "fields": [
                "evaluator_id",
                "source_cluster_id",
                "mode",
                "signal",
                "evidence_refs",
                "false_positive_notes",
            ],
        },
        "gate_promotion_evidence": [
            "Multiple reviewed reports show stable true positives for the same failure class.",
            "False positives are triaged and documented with fixtures or examples.",
            "A separate PR explicitly proposes gate promotion after review history exists.",
        ],
    }


def evaluator_current_signal(cluster: dict) -> str:
    if independent_occurrence_count(cluster) >= 2:
        return "positive-recurring-cluster"
    return "candidate-only-needs-more-evidence"


def evaluator_recovered_affected_ids(cluster: dict) -> dict:
    affected = cluster.get("affected", {})
    existing_omitted = affected.get("omitted", {})
    recovered = {}
    omitted = {}
    for key in AFFECTED_KEYS:
        values = list(affected.get(key, []))
        recovered[key] = values[:MAX_PROPOSAL_AFFECTED_VALUES]
        omitted_count = len(values) - len(recovered[key]) + int(existing_omitted.get(key, 0))
        if omitted_count > 0:
            omitted[key] = omitted_count
    if omitted:
        recovered["omitted"] = omitted
    return recovered


def independent_occurrence_count(cluster: dict) -> int:
    # Retained evidence is overlap-merged so records sharing a recovered affected id
    # count once; byte-bound trimmed records survive only as omitted counts, so add
    # them rather than letting a single column max override the overlap merge (#941).
    return independent_evidence_occurrence_count(cluster) + trimmed_affected_occurrence_count(cluster)


def independent_evidence_occurrence_count(cluster: dict) -> int:
    components: list[set[str]] = []
    for entry in cluster.get("evidence", []):
        tokens = evidence_affected_tokens(entry.get("affected", {}))
        if tokens:
            add_occurrence_component(components, tokens)
    return len(components)


def evidence_affected_tokens(affected: dict) -> set[str]:
    tokens = set()
    for key in AFFECTED_KEYS:
        for value in affected.get(key, []):
            if value:
                tokens.add(f"{key}:{value}")
    return tokens


def add_occurrence_component(components: list[set[str]], tokens: set[str]) -> None:
    overlaps = [idx for idx, component in enumerate(components) if component & tokens]
    if not overlaps:
        components.append(tokens)
        return
    merged = set(tokens)
    for idx in reversed(overlaps):
        merged.update(components.pop(idx))
    components.append(merged)


def trimmed_affected_occurrence_count(cluster: dict) -> int:
    # Only counts affected ids dropped by the byte cap: retained ids are already
    # represented in (overlap-merged) evidence, so counting them here would let the
    # cluster cap inflate recurrence beyond independent records.
    omitted = cluster.get("affected", {}).get("omitted", {})
    return max((int(omitted.get(key, 0)) for key in AFFECTED_KEYS), default=0)


def evaluator_fixtures(cluster: dict) -> list[dict]:
    fixtures = []
    for idx, entry in enumerate(cluster.get("evidence", [])[:MAX_EVALUATOR_FIXTURES], 1):
        fixtures.append(
            {
                "name": f"{cluster['id']}-positive-{idx}",
                "source_ref": entry.get("ref", ""),
                "event_kind": entry.get("kind", ""),
                "expected": "match",
                "bounded_excerpt": truncate(entry.get("excerpt", ""), MAX_EVALUATOR_FIXTURE_EXCERPT_BYTES),
                "metadata": evaluator_fixture_metadata(entry.get("metadata", {})),
            }
        )
    return fixtures


def evaluator_fixture_metadata(metadata: dict) -> dict:
    keep = ("timestamp", "method", "model", "exit_code", "timeout_ms", "elapsed_ms", "duration_ms", "output_bytes", "output_dropped")
    return {key: metadata[key] for key in keep if metadata.get(key)}


def enforce_cluster_bound(cluster: dict) -> None:
    if encoded_bytes(cluster) <= MAX_CLUSTER_BYTES:
        return

    trim_evidence_to_cluster_bound(cluster)
    if encoded_bytes(cluster) <= MAX_CLUSTER_BYTES:
        return

    omitted = cluster["affected"].setdefault("omitted", {})
    base_omitted = {key: int(omitted.get(key, 0)) for key in AFFECTED_KEYS}
    originals = {key: list(cluster["affected"][key]) for key in AFFECTED_KEYS}
    for key in sorted(AFFECTED_KEYS, key=lambda name: len(originals[name]), reverse=True):
        if encoded_bytes(cluster) <= MAX_CLUSTER_BYTES:
            break
        values = originals[key]
        if not values:
            continue
        keep = max_affected_keep_count(cluster, key, values, base_omitted[key])
        cluster["affected"][key] = values[:keep]
        set_affected_omitted(cluster, key, base_omitted[key] + len(values) - keep)

    for key, count in list(omitted.items()):
        if count <= 0:
            omitted.pop(key)
    if not omitted:
        cluster["affected"].pop("omitted", None)
    trim_evidence_to_cluster_bound(cluster)


def max_affected_keep_count(cluster: dict, key: str, values: list[str], base_omitted: int) -> int:
    low, high, best = 0, len(values), 0
    while low <= high:
        keep = (low + high) // 2
        cluster["affected"][key] = values[:keep]
        set_affected_omitted(cluster, key, base_omitted + len(values) - keep)
        if encoded_bytes(cluster) <= MAX_CLUSTER_BYTES:
            best = keep
            low = keep + 1
        else:
            high = keep - 1
    return best


def set_affected_omitted(cluster: dict, key: str, count: int) -> None:
    omitted = cluster["affected"].setdefault("omitted", {})
    if count > 0:
        omitted[key] = count
    else:
        omitted.pop(key, None)
    if not omitted:
        cluster["affected"].pop("omitted", None)


def trim_evidence_to_cluster_bound(cluster: dict) -> None:
    while len(cluster["evidence"]) > 1 and encoded_bytes(cluster) > MAX_CLUSTER_BYTES:
        cluster["evidence"].pop()


def add_unique(cluster: dict, key: str, value: str) -> None:
    value = value.strip()
    if not value:
        return
    seen = cluster["_seen"][key]
    if value not in seen:  # set membership keeps large-log accumulation linear.
        seen.add(value)
        cluster["affected"][key].append(value)


def first_mapped_value(primary: dict, secondary: dict, keys: tuple[str, ...]) -> str:
    for source in (primary, secondary):
        for key in keys:
            if value := source.get(key):
                return value
    return ""


def mask(text: str) -> str:
    masked = []
    cursor = 0
    for match in CLONE_URL_SCHEME_RE.finditer(text):
        start = match.start()
        if start < cursor:
            continue
        url_end = clone_url_end(text, start)
        candidate = text[start:url_end]
        replacement = mask_clone_url(candidate)
        if replacement == candidate:
            continue
        masked.append(text[cursor:start])
        masked.append(replacement)
        cursor = url_end
    masked.append(text[cursor:])
    return TOKEN_RE.sub("[redacted-token]", "".join(masked))


def clone_url_end(text: str, start: int) -> int:
    auth_start = text.find("://", start) + len("://")
    scan_end = first_index(text, "/?#", auth_start)
    at = text.rfind("@", auth_start, scan_end)
    if at < 0:
        return scan_end
    return first_index(text, " \t\r\n]", at + 1)


def first_index(text: str, chars: str, start: int) -> int:
    found = [idx for ch in chars if (idx := text.find(ch, start)) >= 0]
    return min(found, default=len(text))


def mask_clone_url(raw: str) -> str:
    try:
        parsed = urlsplit(raw)
        if not parsed.username and parsed.password is None:
            return raw
        host = parsed.hostname or ""
        if parsed.port:
            host += f":{parsed.port}"
        return urlunsplit((parsed.scheme, host, parsed.path, parsed.query, parsed.fragment))
    except ValueError:
        scheme, rest = raw.split("://", 1)
        end = min((idx for ch in "/?#" if (idx := rest.find(ch)) >= 0), default=len(rest))
        authority, tail = rest[:end], rest[end:]
        if "@" not in authority:
            return raw
        return scheme + "://" + authority.rsplit("@", 1)[1] + tail


def truncate(text: str, limit: int) -> str:
    suffix = "... [truncated]"
    data = text.encode()
    if len(data) <= limit:
        return text
    suffix_data = suffix.encode()
    if limit <= len(suffix_data):
        return data[:limit].decode(errors="ignore")
    head = data[: limit - len(suffix_data)].decode(errors="ignore")
    return head + suffix


def md_code(text: str) -> str:
    return "`" + truncate(text, MAX_PROPOSAL_FIELD_BYTES).replace("`", "'") + "`"


def byte_len(text: str) -> int:
    return len(text.encode())


def encoded_bytes(value: dict | list) -> int:
    return byte_len(json.dumps(value, separators=(",", ":"), sort_keys=True))


def report_json(report: dict) -> str:
    return json.dumps(report, separators=(",", ":"), sort_keys=False) + "\n"


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
