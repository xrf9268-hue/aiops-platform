#!/usr/bin/env python3
"""Generate grouped trace-driven harness reports from worker logs."""

from __future__ import annotations

import argparse
import hashlib
import json
import re
import sys
from datetime import datetime, timezone
from pathlib import Path
from urllib.parse import urlsplit, urlunsplit

SCHEMA_VERSION = "trace-harness-report/v1"
MAX_RUN_EVIDENCE_BYTES, MAX_CLUSTER_BYTES, MAX_EVIDENCE_EXCERPT_BYTES = 64 * 1024, 256 * 1024, 4 * 1024
MAX_METADATA_BYTES, MAX_METADATA_VALUE_BYTES = 16 * 1024, 4 * 1024

FIELD_RE = re.compile(r'\b(event|task_id|issue_id|issue_identifier|session_id|pr|pr_number|pr_url|pull_request|pull_request_url)=("[^"]*"|\S+)')
PAYLOAD_KEY_RE = re.compile(r"(?:^|\s)([A-Za-z0-9_]+):")
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
OPAQUE_PAYLOAD_KEYS = (
    "arguments",
    "arguments_raw",
    "error",
    "output_head",
    "output_tail",
    "params",
    "raw",
)
FREEFORM_PAYLOAD_KEYS = OPAQUE_PAYLOAD_KEYS
SAFE_PAYLOAD_KEYS = set(
    "elapsed_ms timeout_ms duration_ms output_bytes output_dropped model method task_id issue_id "
    "issue_identifier session_id pr pr_number pr_url pull_request pull_request_url exit_code ok tool".split()
)
PAYLOAD_KEYS = SAFE_PAYLOAD_KEYS | set(OPAQUE_PAYLOAD_KEYS)
ERROR_REDACTION = "[redacted-error]"
CLASS_BY_EVENT = {
    "runner_timeout": ("runner-timeout", "Runner timeouts", "runner timeout"),
    "turn_input_required": ("input-required", "Input required stops", "input required"),
    "unsupported_tool_call": ("tool-unsupported", "Unsupported tool calls", "tool unsupported"),
    "malformed": ("malformed-protocol", "Malformed protocol output", "malformed protocol output"),
}

def parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--worker-log", action="append", default=[], type=Path)
    p.add_argument("--json-out", type=Path, help="write JSON report to this path; default stdout")
    return p


def main(argv: list[str]) -> int:
    args = parser().parse_args(argv)
    try:
        report = generate(args.worker_log)
        raw = report_json(report)
        if args.json_out:
            args.json_out.write_text(raw)
        else:
            sys.stdout.write(raw)
    except Exception as exc:  # argparse has already handled usage errors.
        print(mask(str(exc)), file=sys.stderr)
        return 1
    return 0


def generate(paths: list[Path]) -> dict:
    if not paths:
        raise ValueError("at least one --worker-log is required")
    clusters: dict[str, dict] = {}
    run_bytes: dict[str, int] = {}
    inputs = [input_ref(path) for path in paths]
    for path in paths:
        for finding in parse_worker_log(path):
            add_finding(clusters, run_bytes, finding)
    return {
        "schema_version": SCHEMA_VERSION,
        "generated_at": datetime.now(timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z"),
        "inputs": inputs,
        "bounds": {
            "max_run_evidence_bytes": MAX_RUN_EVIDENCE_BYTES,
            "max_cluster_bytes": MAX_CLUSTER_BYTES,
        },
        "clusters": [finalize_cluster(clusters[key]) for key in sorted(clusters)],
    }


def input_ref(path: Path) -> dict:
    data = path.read_bytes()
    return {
        "type": "worker_log",
        "path": mask(str(path)),
        "bytes": len(data),
        "sha256": hashlib.sha256(data).hexdigest(),
    }


def parse_worker_log(path: Path) -> list[dict]:
    findings = []
    with path.open(encoding="utf-8", errors="replace") as handle:
        for line_no, record in worker_log_records(handle):
            if parsed := parse_line(path, line_no, record):
                findings.append(parsed)
    return findings


def worker_log_records(lines):
    current, start_line = [], 0
    for line_no, line in enumerate(lines, 1):
        text = line.rstrip("\n")
        if current:
            if record_payload_open(current):
                if event_after_freeform_bracket(current, text):
                    yield start_line, "\n".join(current)
                    current, start_line = [text], line_no
                    continue
                if timestamped_log_after_freeform_bracket(current, text):
                    yield start_line, "\n".join(current)
                    current, start_line = [], 0
                else:
                    current.append(text)
                    continue
            yield start_line, "\n".join(current)
            current, start_line = [], 0
        if is_worker_log_record_start(text):
            current, start_line = [text], line_no
        else:
            yield line_no, text
    if current:
        yield start_line, "\n".join(current)


def is_worker_log_record_start(line: str) -> bool:
    return bool(WORKER_RECORD_START_RE.match(line))


def record_payload_open(lines: list[str]) -> bool:
    if not lines:
        return False
    _, payload = split_payload("\n".join(lines))
    return payload.startswith("map[") and not record_map_payload_closed(payload)


def event_after_freeform_bracket(lines: list[str], text: str) -> bool:
    if not is_worker_log_record_start(text):
        return False
    return freeform_bracket_closes_record(lines)


def timestamped_log_after_freeform_bracket(lines: list[str], text: str) -> bool:
    if not TIMESTAMP_RE.match(text) or is_worker_log_record_start(text):
        return False
    return freeform_bracket_closes_record(lines)


def freeform_bracket_closes_record(lines: list[str]) -> bool:
    _, payload = split_payload("\n".join(lines))
    if not payload.startswith("map["):
        return False
    body = map_payload_body(payload)
    if freeform_payload_start(body) == len(body):
        return False
    return freeform_terminal_bracket_closes_map(lines[-1])


def parse_line(path: Path, line_no: int, line: str) -> dict | None:
    if not is_worker_log_record_start(line):
        return None
    prefix, payload = split_payload(line)
    fields = prefix_field_values(prefix)
    kind = fields.get("event", "")
    metadata = scalar_payload(payload, kind)
    if timestamp := line_timestamp(prefix):
        metadata.setdefault("timestamp", timestamp)
    event_class = classify(kind, prefix, metadata)
    if not event_class:
        return None
    cid, title, symptom = event_class
    return {
        "id": cid,
        "title": title,
        "symptom": symptom,
        "ref": f"{mask(str(path))}:{line_no}",
        "kind": kind,
        "excerpt": evidence_excerpt(kind, line),
        "metadata": metadata,
        "issue": fields.get("issue_id", "") or metadata.get("issue_id", ""),
        "identifier": fields.get("issue_identifier", "") or metadata.get("issue_identifier", ""),
        "run": fields.get("task_id", "") or metadata.get("task_id", ""),
        "session": fields.get("session_id", "") or metadata.get("session_id", ""),
        "pull_request": first_mapped_value(fields, metadata, PR_KEYS),
    }


def classify(kind: str, line: str, metadata: dict) -> tuple[str, str, str] | None:
    if kind in CLASS_BY_EVENT:
        return CLASS_BY_EVENT[kind]
    lower = line.lower()
    if kind == "runner_end" and runner_end_failed(lower, metadata):
        return ("runner-failure", "Runner failures", "runner failure")
    return None


def runner_end_failed(line: str, metadata: dict) -> bool:
    ok = metadata.get("ok", "").lower()
    if ok == "true":
        return False
    if ok == "false":
        return True
    if "runner failed" in line:
        return True
    return bool(metadata.get("error"))


def split_payload(line: str) -> tuple[str, str]:
    if " payload=" not in line:
        return line, ""
    prefix, payload = line.split(" payload=", 1)
    return prefix, payload


def line_timestamp(prefix: str) -> str:
    match = TIMESTAMP_RE.match(prefix)
    return mask(match.group(1)) if match else ""


def prefix_fields(prefix: str) -> str:
    return prefix.split(" msg=", 1)[0]


def prefix_field_values(prefix: str) -> dict:
    return {m.group(1): mask(m.group(2).strip('"')) for m in FIELD_RE.finditer(prefix_fields(prefix))}


def scalar_payload(payload: str, kind: str) -> dict:
    if not payload:
        return {}
    payload = mask(payload)
    if payload.startswith("map["):
        payload = map_payload_body(payload)
    return scalar_payload_fields(payload)


def map_payload_body(payload: str) -> str:
    body = payload[4:]
    return body[:-1] if map_payload_closed(payload) else body


def scalar_payload_fields(payload: str) -> dict:
    fields = {}
    for key, value in top_level_payload_fields(payload):
        if key in fields:
            continue
        if key in SAFE_PAYLOAD_KEYS and value:
            fields[key] = metadata_value(value)
            continue
        if key == "error":
            fields[key] = ERROR_REDACTION
    return fields


def metadata_value(value: str) -> str:
    return mask(value.strip().strip('"'))


def map_payload_closed(payload: str) -> bool:
    return payload.rstrip().endswith("]")


def record_map_payload_closed(payload: str) -> bool:
    if not map_payload_closed(payload):
        return False
    body = map_payload_body(payload)
    if "\n" not in payload:
        return single_line_map_payload_closed(payload)
    if freeform_payload_start(body) < len(body) and not freeform_terminal_bracket_strongly_closes_map(payload):
        return False
    if freeform_payload_start(body) == len(body):
        return True
    last_line = payload.rsplit("\n", 1)[-1]
    return freeform_terminal_bracket_strongly_closes_map(last_line)


def single_line_map_payload_closed(payload: str) -> bool:
    raw_body = payload[4:].rstrip() if payload.startswith("map[") else payload.rstrip()
    start = freeform_payload_start(raw_body)
    if start == len(raw_body):
        return True
    if freeform_terminal_bracket_strongly_closes_map(payload):
        return True
    return not terminal_bracket_may_close_payload_text(raw_body[start:])


def terminal_bracket_may_close_payload_text(value: str) -> bool:
    text = value.rstrip()
    if not text.endswith("]"):
        return False
    before_terminal = text[:-1]
    return before_terminal.count("[") > before_terminal.count("]")


def freeform_terminal_bracket_closes_map(line: str) -> bool:
    return freeform_terminal_bracket_strongly_closes_map(line)


def freeform_terminal_bracket_strongly_closes_map(line: str) -> bool:
    text = line.rstrip()
    return text.endswith("]]") or text.endswith("] ]")


def evidence_excerpt(kind: str, line: str) -> str:
    if kind == "malformed":
        return redact_malformed_payload(line)
    prefix, payload = split_payload(line)
    if not payload:
        return mask(line)
    if payload_has_opaque_field(payload):
        return f"{mask(prefix)} payload=[redacted-payload]"
    return f"{mask(prefix)} payload={mask(payload)}"


def redact_malformed_payload(line: str) -> str:
    prefix, payload = split_payload(line)
    if not payload:
        return mask(line)
    return f"{mask(prefix)} payload=[redacted-malformed-payload]"


def freeform_payload_start(payload: str) -> int:
    starts = [idx for key in FREEFORM_PAYLOAD_KEYS if (idx := payload.find(f"{key}:")) >= 0]
    return min(starts) if starts else len(payload)


def payload_has_opaque_field(payload: str) -> bool:
    if payload.startswith("map["):
        payload = map_payload_body(payload)
    return any(key in OPAQUE_PAYLOAD_KEYS for key, _ in top_level_payload_fields(payload))


def top_level_payload_fields(payload: str) -> list[tuple[str, str]]:
    fields, cursor = [], 0
    while cursor < len(payload):
        match = next_payload_field(payload, cursor)
        if not match:
            break
        key = match.group(1)
        value_start = match.end()
        value_end, keep_scanning = top_level_value_end(payload, value_start, key)
        fields.append((key, payload[value_start:value_end].strip()))
        if not keep_scanning:
            break
        cursor = value_end
    return fields


def next_payload_field(payload: str, start: int) -> re.Match | None:
    for match in PAYLOAD_KEY_RE.finditer(payload, start):
        if match.group(1) in PAYLOAD_KEYS:
            return match
    return None


def top_level_value_end(payload: str, start: int, key: str) -> tuple[int, bool]:
    value_start = first_non_space(payload, start)
    if key in OPAQUE_PAYLOAD_KEYS:
        return len(payload), False
    bracket_end = bracketed_value_end(payload, value_start)
    if bracket_end > value_start:
        return bracket_end, True
    next_key = next_payload_field_boundary(payload, value_start)
    return (next_key, True) if next_key >= 0 else (len(payload), False)


def first_non_space(text: str, start: int) -> int:
    while start < len(text) and text[start].isspace():
        start += 1
    return start


def bracketed_value_end(text: str, start: int) -> int:
    if text.startswith("map[", start):
        return scan_balanced(text, start + len("map"), "[", "]")
    if start < len(text) and text[start] in "[{":
        return scan_balanced(text, start, text[start], "]" if text[start] == "[" else "}")
    return -1


def scan_balanced(text: str, start: int, opener: str, closer: str) -> int:
    depth, in_string, escape = 0, False, False
    for idx in range(start, len(text)):
        ch = text[idx]
        if in_string:
            if escape:
                escape = False
            elif ch == "\\":
                escape = True
            elif ch == '"':
                in_string = False
            continue
        if ch == '"':
            in_string = True
            continue
        if ch == opener:
            depth += 1
            continue
        if ch == closer and depth:
            depth -= 1
            if depth == 0:
                return idx + 1
    return -1


def next_payload_field_boundary(payload: str, start: int) -> int:
    for match in PAYLOAD_KEY_RE.finditer(payload, start):
        if match.group(1) in PAYLOAD_KEYS:
            return match.start()
    return -1


def add_finding(clusters: dict[str, dict], run_bytes: dict[str, int], finding: dict) -> None:
    cluster = clusters.setdefault(finding["id"], new_cluster(finding))
    add_unique(cluster["affected"]["issues"], finding["issue"])
    add_unique(cluster["affected"]["issue_identifiers"], finding["identifier"])
    add_unique(cluster["affected"]["pull_requests"], finding["pull_request"])
    add_unique(cluster["affected"]["runs"], finding["run"])
    add_unique(cluster["affected"]["sessions"], finding["session"])
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
        "_evidence_bytes": byte_len("[]"),
        "_evidence_count": 0,
        "suspected_harness_surface": "WORKFLOW.md, reviewer rubrics, skills, hooks, tests, CI, or docs",
        "proposed_next_action": "issue proposal",
        "acceptance_criteria": [
            "Future runs cover this failure class with a reviewed harness surface or a documented no-op decision.",
            "The change remains report/agent/workflow-owned and does not add a worker-side gate or tracker writer.",
        ],
        "redaction_note": "Evidence stores trusted metadata before opaque payloads; output_head, output_tail, error, arguments, arguments_raw, raw, and params are omitted from excerpts. Clone URL userinfo follows workflow.MaskCloneURL, and token-like values are redacted.",
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
        "metadata": bounded_metadata(finding["metadata"]),
    }


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


def finalize_cluster(cluster: dict) -> dict:
    for key in AFFECTED_KEYS:
        values = cluster["affected"][key]
        values.sort()
    cluster.pop("_evidence_bytes", None)
    cluster.pop("_evidence_count", None)
    enforce_cluster_bound(cluster)
    return cluster


def enforce_cluster_bound(cluster: dict) -> None:
    if encoded_bytes(cluster) <= MAX_CLUSTER_BYTES:
        return

    trim_evidence_to_cluster_bound(cluster)
    if encoded_bytes(cluster) <= MAX_CLUSTER_BYTES:
        return

    omitted = cluster["affected"].setdefault("omitted", {})
    originals = {key: list(cluster["affected"][key]) for key in AFFECTED_KEYS}
    for key in sorted(AFFECTED_KEYS, key=lambda name: len(originals[name]), reverse=True):
        if encoded_bytes(cluster) <= MAX_CLUSTER_BYTES:
            break
        values = originals[key]
        if not values:
            continue
        keep = max_affected_keep_count(cluster, key, values)
        cluster["affected"][key] = values[:keep]
        if len(values) > keep:
            omitted[key] = len(values) - keep
        else:
            omitted.pop(key, None)

    for key, count in list(omitted.items()):
        if count <= 0:
            omitted.pop(key)
    if not omitted:
        cluster["affected"].pop("omitted", None)
    trim_evidence_to_cluster_bound(cluster)


def max_affected_keep_count(cluster: dict, key: str, values: list[str]) -> int:
    low, high, best = 0, len(values), 0
    while low <= high:
        keep = (low + high) // 2
        cluster["affected"][key] = values[:keep]
        cluster["affected"]["omitted"][key] = len(values) - keep
        if encoded_bytes(cluster) <= MAX_CLUSTER_BYTES:
            best = keep
            low = keep + 1
        else:
            high = keep - 1
    return best


def trim_evidence_to_cluster_bound(cluster: dict) -> None:
    while len(cluster["evidence"]) > 1 and encoded_bytes(cluster) > MAX_CLUSTER_BYTES:
        cluster["evidence"].pop()


def add_unique(values: list[str], value: str) -> None:
    value = value.strip()
    if value and value not in values:
        values.append(value)


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


def byte_len(text: str) -> int:
    return len(text.encode())


def encoded_bytes(value: dict | list) -> int:
    return byte_len(json.dumps(value, separators=(",", ":"), sort_keys=True))


def report_json(report: dict) -> str:
    return json.dumps(report, separators=(",", ":"), sort_keys=False) + "\n"


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
