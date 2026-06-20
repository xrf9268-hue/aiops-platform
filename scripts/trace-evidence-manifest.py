#!/usr/bin/env python3
"""Normalize retained worker logs into a durable, redacted trace-evidence manifest.

This is the Milestone 5 (#952) durable input for the trace-driven harness loop.
It is the design's pre-specified "bounded per-run evidence manifest" — explicitly
**not** a trace database, queue, metrics store, or scheduler/recovery state
(`docs/design/trace-driven-harness-improvement.md`, §"Redaction, retention, and
bounds"). It is a standalone tool that reads worker logs the operator already
retained and writes a metadata-first manifest the trace harness report can
consume directly (`trace-harness-report.py --evidence-manifest`). It does not
touch the worker, so restart recovery stays tracker/filesystem-driven (#73/#407);
it does not mutate tracker state, open PRs, or persist scheduler state.

Parsing, redaction (clone-URL userinfo via workflow.MaskCloneURL, token-like
values, wholesale opaque payload omission), and the 64 KiB-per-run / 256 KiB
cluster-equivalent byte bounds are reused verbatim from trace-harness-report.py
so the manifest's redaction and bounds match the report (M1) exactly, with no
second redaction implementation to drift.
"""

from __future__ import annotations

import argparse
import importlib.util
import sys
from datetime import datetime, timezone
from pathlib import Path

# (manifest affected key, finding scalar key) pairs, mirroring the report's
# finding_affected() so per-run affected id arrays use one vocabulary.
AFFECTED_FROM_FINDING = (
    ("issues", "issue"),
    ("issue_identifiers", "identifier"),
    ("pull_requests", "pull_request"),
    ("runs", "run"),
    ("sessions", "session"),
)


def load_report():
    # The canonical report script name carries a hyphen, so it cannot be imported
    # with a normal `import`. Load it by path and reuse its parsing, redaction,
    # and byte bounds as the single source of truth (no second redaction).
    path = Path(__file__).resolve().with_name("trace-harness-report.py")
    spec = importlib.util.spec_from_file_location("trace_harness_report", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


report = load_report()
# Shared with the report so the producer and the --evidence-manifest consumer's
# validation agree on one schema id.
MANIFEST_SCHEMA_VERSION = report.EVIDENCE_MANIFEST_SCHEMA_VERSION


def parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--worker-log", action="append", default=[], type=Path)
    p.add_argument("--json-out", type=Path, help="write manifest JSON to this path; default stdout")
    return p


def main(argv: list[str]) -> int:
    args = parser().parse_args(argv)
    try:
        manifest = build_manifest(args.worker_log)
        raw = report.report_json(manifest)
        if args.json_out:
            args.json_out.write_text(raw)
        else:
            sys.stdout.write(raw)
    except Exception as exc:  # argparse has already handled usage errors.
        print(report.mask(str(exc)), file=sys.stderr)
        return 1
    return 0


def build_manifest(paths: list[Path]) -> dict:
    if not paths:
        raise ValueError("at least one --worker-log is required")
    inputs = [report.input_ref(path, "worker_log") for path in paths]
    runs: dict[str, dict] = {}
    for path in paths:
        for finding in report.parse_worker_log(path):
            fold_event(runs, finding)
    return {
        "schema_version": MANIFEST_SCHEMA_VERSION,
        "generated_at": datetime.now(timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z"),
        "bounds": {
            "max_run_evidence_bytes": report.MAX_RUN_EVIDENCE_BYTES,
            "max_cluster_bytes": report.MAX_CLUSTER_BYTES,
        },
        "inputs": inputs,
        "runs": [finalize_run(runs[key]) for key in sorted(runs)],
    }


def fold_event(runs: dict[str, dict], finding: dict) -> None:
    run_id = finding["run"] or finding["issue"] or "unknown"
    group = runs.setdefault(run_id, new_run_group(run_id))
    record_affected(group, finding)
    excerpt = report.truncate(report.mask(finding["excerpt"]), report.MAX_EVIDENCE_EXCERPT_BYTES)
    entry = report.evidence_entry(finding, excerpt)
    # Record the resolved failure class so consumers re-cluster without re-parsing
    # the redacted excerpt. Added before sizing so the byte accounting is exact.
    entry["class"] = finding["id"]
    size = report.evidence_entry_bytes(entry)
    if group["_bytes"] + size > report.MAX_RUN_EVIDENCE_BYTES:
        group["_dropped"] += 1  # per-run 64 KiB cap reached; whole events are dropped, not truncated.
        return
    group["events"].append(entry)
    group["_bytes"] += size


def new_run_group(run_id: str) -> dict:
    return {
        "run": run_id,
        "affected": {key: [] for key in report.AFFECTED_KEYS},
        # O(1) dedup membership per affected key; emitted arrays stay sorted.
        "_seen": {key: set() for key in report.AFFECTED_KEYS},
        "events": [],
        "_bytes": 0,
        "_dropped": 0,
    }


def record_affected(group: dict, finding: dict) -> None:
    for affected_key, finding_key in AFFECTED_FROM_FINDING:
        value = (finding.get(finding_key) or "").strip()
        if value and value not in group["_seen"][affected_key]:
            group["_seen"][affected_key].add(value)
            group["affected"][affected_key].append(value)


def finalize_run(group: dict) -> dict:
    for key in report.AFFECTED_KEYS:
        group["affected"][key].sort()
    group.pop("_seen", None)  # drop before any encoded_bytes call: sets are not JSON-serializable.
    group.pop("_bytes", None)
    # Keep dropped_events in the dict while enforcing the cap so its bytes are
    # accounted for; remove it only when nothing was dropped.
    group["dropped_events"] = group.pop("_dropped", 0)
    enforce_run_group_bound(group)
    if not group["dropped_events"]:
        group.pop("dropped_events")
    return group


def enforce_run_group_bound(group: dict) -> None:
    # Cluster-equivalent cap (#952): keep the whole run group under the same
    # 256 KiB ceiling, matching the report's enforce_cluster_bound on both axes —
    # trim events first, then the affected-id arrays (recording omitted counts) so
    # an affected-id explosion cannot push the group over the cap.
    trim_events_to_cap(group)
    if report.encoded_bytes(group) <= report.MAX_CLUSTER_BYTES:
        return
    affected = group["affected"]
    omitted = affected.setdefault("omitted", {})
    base = {key: int(omitted.get(key, 0)) for key in report.AFFECTED_KEYS}
    originals = {key: list(affected[key]) for key in report.AFFECTED_KEYS}
    for key in sorted(report.AFFECTED_KEYS, key=lambda name: len(originals[name]), reverse=True):
        if report.encoded_bytes(group) <= report.MAX_CLUSTER_BYTES:
            break
        values = originals[key]
        if not values:
            continue
        keep = report.max_affected_keep_count(group, key, values, base[key])
        affected[key] = values[:keep]
        report.set_affected_omitted(group, key, base[key] + len(values) - keep)
    for key, count in list(omitted.items()):
        if count <= 0:
            omitted.pop(key)
    if not omitted:
        affected.pop("omitted", None)
    trim_events_to_cap(group)


def trim_events_to_cap(group: dict) -> None:
    while len(group["events"]) > 1 and report.encoded_bytes(group) > report.MAX_CLUSTER_BYTES:
        group["events"].pop()
        group["dropped_events"] += 1


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
