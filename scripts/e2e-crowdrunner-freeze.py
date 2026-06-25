#!/usr/bin/env python3
"""Freeze Crowd Runner lifecycle dispatch at an operator-selected milestone."""

from __future__ import annotations

import argparse
import json
import os
import time
import urllib.error
import urllib.parse
import urllib.request
from datetime import datetime, timezone
from pathlib import Path
from typing import Any


NOT_FOUND = object()


def parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--run-root", required=True, type=Path)
    p.add_argument("--gitea-url", default=os.getenv("AIOPS_CROWDRUNNER_GITEA_URL", ""))
    p.add_argument("--repo-owner", default=os.getenv("AIOPS_CROWDRUNNER_REPO_OWNER", "aiops-bot"))
    p.add_argument("--repo-name", default=os.getenv("AIOPS_CROWDRUNNER_REPO_NAME", "crowd-runner-product"))
    p.add_argument("--token", default=os.getenv("GITEA_TOKEN", ""))
    p.add_argument("--stop-after", required=True, type=int)
    p.add_argument("--product-start", type=int, default=1)
    p.add_argument("--product-end", type=int, default=12)
    p.add_argument("--ready-label", default="aiops/todo")
    p.add_argument("--done-label", default="aiops/done")
    p.add_argument("--poll-interval-seconds", type=float, default=15.0)
    p.add_argument("--timeout-seconds", type=float, default=0.0)
    return p


def api_url(args: argparse.Namespace, path: str, query: str = "") -> str:
    owner = urllib.parse.quote(args.repo_owner, safe="")
    repo = urllib.parse.quote(args.repo_name, safe="")
    url = f"{args.gitea_url.rstrip('/')}/api/v1/repos/{owner}/{repo}{path}"
    if query:
        url += "?" + query
    return url


def request(args: argparse.Namespace, method: str, url: str, *, allow_not_found: bool = False) -> Any:
    req = urllib.request.Request(url, method=method)
    if args.token:
        req.add_header("Authorization", f"token {args.token}")
    try:
        with urllib.request.urlopen(req, timeout=20) as resp:
            body = resp.read()
    except urllib.error.HTTPError as exc:
        if allow_not_found and exc.code == 404:
            return NOT_FOUND
        detail = exc.read().decode("utf-8", errors="replace")
        raise SystemExit(f"{method} {url} failed: {exc.code} {detail}") from exc
    except urllib.error.URLError as exc:
        raise SystemExit(f"{method} {url} failed: {exc.reason}") from exc
    if not body:
        return None
    try:
        return json.loads(body.decode("utf-8"))
    except json.JSONDecodeError as exc:
        raise SystemExit(f"{method} {url} returned invalid JSON: {exc}") from exc


def load_issues(args: argparse.Namespace) -> list[dict[str, Any]]:
    url = api_url(args, "/issues", "state=all&limit=100")
    data = request(args, "GET", url)
    if not isinstance(data, list):
        raise SystemExit("Gitea issues response was not a list")
    return data


def label_names(issue: dict[str, Any]) -> list[str]:
    names: list[str] = []
    for label in issue.get("labels") or []:
        if isinstance(label, dict):
            name = str(label.get("name", ""))
        else:
            name = str(label)
        if name:
            names.append(name)
    return names


def find_label(issue: dict[str, Any], name: str) -> dict[str, Any] | None:
    for label in issue.get("labels") or []:
        if isinstance(label, dict) and label.get("name") == name:
            return label
    return None


def product_issues(args: argparse.Namespace, issues: list[dict[str, Any]]) -> list[dict[str, Any]]:
    products = []
    for issue in issues:
        try:
            number = int(issue.get("number", 0))
        except (TypeError, ValueError):
            continue
        if args.product_start <= number <= args.product_end:
            products.append(issue)
    return sorted(products, key=lambda item: int(item.get("number", 0)))


def delete_ready_label(args: argparse.Namespace, issue: dict[str, Any], label: dict[str, Any]) -> bool:
    label_id = label.get("id")
    if label_id is None:
        raise SystemExit(f"issue #{issue.get('number')} has ready label without an id")
    url = api_url(args, f"/issues/{issue.get('number')}/labels/{label_id}")
    return request(args, "DELETE", url, allow_not_found=True) is not NOT_FOUND


def freeze(args: argparse.Namespace, issues: list[dict[str, Any]]) -> dict[str, Any] | None:
    products = product_issues(args, issues)
    completed = [issue for issue in products if args.done_label in label_names(issue)]
    if len(completed) < args.stop_after:
        return None

    removed = []
    already_inactive = []
    for issue in products:
        if args.done_label in label_names(issue):
            continue
        ready = find_label(issue, args.ready_label)
        if ready is None:
            already_inactive.append(issue.get("number"))
            continue
        if not delete_ready_label(args, issue, ready):
            already_inactive.append(issue.get("number"))
            continue
        removed.append({
            "number": issue.get("number"),
            "title": issue.get("title", ""),
            "removed_label": args.ready_label,
            "label_id": ready.get("id"),
        })

    return {
        "kind": "operator_milestone_freeze",
        "created_at": datetime.now(timezone.utc).isoformat(),
        "stop_after": args.stop_after,
        "product_issue_range": [args.product_start, args.product_end],
        "completed_product_issues": len(completed),
        "ready_labels_removed": removed,
        "already_inactive_product_issues": already_inactive,
        "note": "Operator milestone freeze: ready labels were removed without stopping workers or dashboards.",
    }


def write_evidence(args: argparse.Namespace, record: dict[str, Any]) -> tuple[Path, Path]:
    state_dir = args.run_root / "state"
    reports_dir = args.run_root / "reports"
    state_dir.mkdir(parents=True, exist_ok=True)
    reports_dir.mkdir(parents=True, exist_ok=True)
    stem = f"operator-milestone-freeze-after-{args.stop_after}"
    state_path = state_dir / f"{stem}.json"
    report_path = reports_dir / f"{stem}.md"
    state_path.write_text(json.dumps(record, indent=2, sort_keys=True) + "\n")
    removed = ", ".join(f"#{item['number']}" for item in record["ready_labels_removed"]) or "none"
    inactive = ", ".join(f"#{number}" for number in record["already_inactive_product_issues"]) or "none"
    lines = [
        "# Operator Milestone Freeze",
        "",
        f"- Stop after: {record['stop_after']} completed product issues",
        f"- Completed product issues observed: {record['completed_product_issues']}",
        f"- Ready labels removed: {removed}",
        f"- Already inactive product issues: {inactive}",
        "- Classification: operator milestone freeze, not a worker failure",
        "- Workers and dashboards remain running for evidence capture.",
    ]
    report_path.write_text("\n".join(lines) + "\n")
    return state_path, report_path


def wait_for_freeze(args: argparse.Namespace) -> dict[str, Any]:
    deadline = time.monotonic() + args.timeout_seconds if args.timeout_seconds > 0 else None
    while True:
        record = freeze(args, load_issues(args))
        if record is not None:
            return record
        if deadline is not None and time.monotonic() >= deadline:
            raise SystemExit(f"timed out waiting for {args.stop_after} completed product issues")
        time.sleep(max(args.poll_interval_seconds, 0.1))


def main() -> int:
    args = parser().parse_args()
    if not args.gitea_url:
        raise SystemExit("set --gitea-url or AIOPS_CROWDRUNNER_GITEA_URL")
    if not args.token:
        raise SystemExit("set --token or GITEA_TOKEN so the helper can remove ready labels")
    if args.stop_after < 1:
        raise SystemExit("--stop-after must be positive")
    if args.product_start > args.product_end:
        raise SystemExit("--product-start must be <= --product-end")
    record = wait_for_freeze(args)
    state_path, report_path = write_evidence(args, record)
    print(f"operator milestone freeze recorded: {state_path}")
    print(f"report fragment written: {report_path}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
