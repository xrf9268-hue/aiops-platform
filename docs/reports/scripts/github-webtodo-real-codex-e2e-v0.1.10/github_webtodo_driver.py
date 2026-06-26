#!/usr/bin/env python3
from __future__ import annotations

import html
import json
import os
import subprocess
import sys
import time
import urllib.request
from pathlib import Path
from typing import Any


RUN_ROOT = Path("/tmp/aiops-github-webtodo-e2e-20260626-093809")
REPO = "zjlgdx/aiops-e2e-vite-react-webtodo-20260626-0938"
DASHBOARD_URL = "http://127.0.0.1:4101"
READY_LABEL = "aiops:ready"
CANCELED_LABEL = "aiops:canceled"
FEATURE_ISSUES = list(range(1, 13))
CONTROL_IDLE_ISSUE = 13
CONTROL_CANCEL_ISSUE = 14
MAX_SECONDS = int(os.environ.get("AIOPS_E2E_MAX_SECONDS", "28800"))
POLL_SECONDS = int(os.environ.get("AIOPS_E2E_DRIVER_POLL_SECONDS", "20"))


def run(cmd: list[str], *, cwd: Path | None = None, check: bool = True) -> subprocess.CompletedProcess[str]:
    env = os.environ.copy()
    env.setdefault("NO_COLOR", "1")
    env.setdefault("npm_config_cache", str(RUN_ROOT / ".npm-cache"))
    return subprocess.run(
        cmd,
        cwd=str(cwd) if cwd else None,
        env=env,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        check=check,
    )


def gh_json(args: list[str]) -> Any:
    result = run(["gh", *args])
    return json.loads(result.stdout)


def gh_text(args: list[str], *, check: bool = True) -> str:
    return run(["gh", *args], check=check).stdout


def labels(issue: dict[str, Any]) -> set[str]:
    return {label["name"] for label in issue.get("labels", [])}


def issue_map() -> dict[int, dict[str, Any]]:
    rows = gh_json([
        "issue",
        "list",
        "--repo",
        REPO,
        "--state",
        "all",
        "--limit",
        "100",
        "--json",
        "number,title,state,labels,url,closed",
    ])
    return {int(row["number"]): row for row in rows}


def pr_rows() -> list[dict[str, Any]]:
    return gh_json([
        "pr",
        "list",
        "--repo",
        REPO,
        "--state",
        "all",
        "--limit",
        "100",
        "--json",
        "number,title,state,isDraft,mergedAt,headRefName,url,closingIssuesReferences",
    ])


def actions_rows() -> list[dict[str, Any]]:
    return gh_json([
        "run",
        "list",
        "--repo",
        REPO,
        "--limit",
        "50",
        "--json",
        "databaseId,workflowName,event,status,conclusion,headSha,createdAt,url",
    ])


def fetch_state() -> dict[str, Any]:
    with urllib.request.urlopen(f"{DASHBOARD_URL}/api/v1/state", timeout=10) as resp:
        return json.loads(resp.read().decode("utf-8"))


def refresh_worker() -> None:
    req = urllib.request.Request(
        f"{DASHBOARD_URL}/api/v1/refresh",
        method="POST",
        headers={"X-AIOPS-Refresh": "true"},
    )
    with urllib.request.urlopen(req, timeout=10):
        pass


def write_json(path: Path, data: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(data, indent=2, sort_keys=True) + "\n")


def render_evidence(tag: str, state: dict[str, Any], issues: dict[int, dict[str, Any]], prs: list[dict[str, Any]], runs: list[dict[str, Any]]) -> Path:
    rows = []
    for number in sorted(issues):
        issue = issues[number]
        rows.append(
            "<tr>"
            f"<td>#{number}</td>"
            f"<td>{html.escape(issue['title'])}</td>"
            f"<td>{html.escape(issue['state'])}</td>"
            f"<td>{html.escape(', '.join(sorted(labels(issue))))}</td>"
            "</tr>"
        )
    pr_lines = []
    for pr in prs:
        refs = ", ".join(f"#{ref['number']}" for ref in pr.get("closingIssuesReferences", []))
        pr_lines.append(
            "<tr>"
            f"<td>#{pr['number']}</td>"
            f"<td>{html.escape(pr['state'])}</td>"
            f"<td>{html.escape(pr.get('mergedAt') or '-')}</td>"
            f"<td>{html.escape(refs or '-')}</td>"
            f"<td>{html.escape(pr['title'])}</td>"
            "</tr>"
        )
    run_lines = []
    for run_row in runs[:12]:
        run_lines.append(
            "<tr>"
            f"<td>{html.escape(run_row.get('workflowName') or '')}</td>"
            f"<td>{html.escape(run_row.get('status') or '')}</td>"
            f"<td>{html.escape(run_row.get('conclusion') or '-')}</td>"
            f"<td>{html.escape(run_row.get('createdAt') or '')}</td>"
            "</tr>"
        )
    counts = html.escape(json.dumps(state.get("counts", {}), sort_keys=True))
    out = RUN_ROOT / "screenshots" / f"evidence-{tag}.html"
    out.write_text(
        "<!doctype html><meta charset='utf-8'>"
        "<title>aiops GitHub E2E Evidence</title>"
        "<style>body{font-family:system-ui;margin:32px;color:#172033}"
        "table{border-collapse:collapse;width:100%;margin:18px 0}"
        "th,td{border:1px solid #d7dfec;padding:7px;text-align:left;font-size:13px}"
        "th{background:#eef4ff}code{background:#f2f5fa;padding:2px 4px}</style>"
        f"<h1>GitHub Web Todo E2E Evidence: {html.escape(tag)}</h1>"
        f"<p>Repo: <code>{html.escape(REPO)}</code></p>"
        f"<p>Worker counts: <code>{counts}</code></p>"
        "<h2>Issues</h2><table><thead><tr><th>Issue</th><th>Title</th><th>State</th><th>Labels</th></tr></thead><tbody>"
        + "".join(rows)
        + "</tbody></table><h2>Pull Requests</h2><table><thead><tr><th>PR</th><th>State</th><th>Merged</th><th>Closes</th><th>Title</th></tr></thead><tbody>"
        + "".join(pr_lines)
        + "</tbody></table><h2>Recent Actions</h2><table><thead><tr><th>Workflow</th><th>Status</th><th>Conclusion</th><th>Created</th></tr></thead><tbody>"
        + "".join(run_lines)
        + "</tbody></table>"
    )
    return out


def screenshot(url: str, dest: Path) -> None:
    result = run([
        "npx",
        "playwright",
        "screenshot",
        "--full-page",
        url,
        str(dest),
    ], cwd=RUN_ROOT / "repo", check=False)
    if result.returncode != 0:
        dest.with_suffix(".log").write_text(result.stdout)


def capture(tag: str) -> None:
    safe = "".join(ch if ch.isalnum() or ch in "._-" else "-" for ch in tag)
    state = fetch_state()
    issues = issue_map()
    prs = pr_rows()
    runs = actions_rows()
    write_json(RUN_ROOT / "state" / f"worker-{safe}.json", state)
    write_json(RUN_ROOT / "github-json" / f"issues-{safe}.json", list(issues.values()))
    write_json(RUN_ROOT / "github-json" / f"prs-{safe}.json", prs)
    write_json(RUN_ROOT / "github-json" / f"runs-{safe}.json", runs)
    evidence = render_evidence(safe, state, issues, prs, runs)
    screenshot(DASHBOARD_URL, RUN_ROOT / "screenshots" / f"dashboard-{safe}.png")
    screenshot(evidence.as_uri(), RUN_ROOT / "screenshots" / f"evidence-{safe}.png")


def add_label(number: int, label: str) -> None:
    gh_text(["issue", "edit", str(number), "--repo", REPO, "--add-label", label])


def remove_label(number: int, label: str) -> None:
    gh_text(["issue", "edit", str(number), "--repo", REPO, "--remove-label", label], check=False)


def ready(number: int) -> None:
    issues = issue_map()
    if READY_LABEL not in labels(issues[number]) and issues[number]["state"] == "OPEN":
        add_label(number, READY_LABEL)
        refresh_worker()
        capture(f"issue-{number:02d}-activated")


def issue_closed(issues: dict[int, dict[str, Any]], number: int) -> bool:
    return issues[number]["state"] == "CLOSED"


def pr_claims() -> dict[int, list[dict[str, Any]]]:
    claims: dict[int, list[dict[str, Any]]] = {}
    for pr in pr_rows():
        for ref in pr.get("closingIssuesReferences", []):
            claims.setdefault(int(ref["number"]), []).append(pr)
    return claims


def running_mentions(issue_number: int, state: dict[str, Any]) -> bool:
    needle = str(issue_number)
    for item in state.get("running") or []:
        blob = json.dumps(item, sort_keys=True)
        if needle in blob:
            return True
    return False


def main() -> int:
    started = time.monotonic()
    next_feature = 1
    cancel_started = False
    cancel_done = False
    captured_even: set[int] = set()
    log = RUN_ROOT / "logs" / "driver.log"

    def note(message: str) -> None:
        line = f"{time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime())} {message}"
        print(line, flush=True)
        with log.open("a") as fh:
            fh.write(line + "\n")

    capture("pre-activation")
    ready(1)
    note("activated issue #1")

    while time.monotonic() - started < MAX_SECONDS:
        issues = issue_map()
        claims = pr_claims()
        state = fetch_state()

        if READY_LABEL in labels(issues[CONTROL_IDLE_ISSUE]):
            note("FAIL: control idle issue received ready label")
            capture("fail-control-idle-ready")
            return 2
        if CONTROL_IDLE_ISSUE in claims:
            note("FAIL: control idle issue has a PR claim")
            capture("fail-control-idle-pr")
            return 2

        closed_features = [number for number in FEATURE_ISSUES if issue_closed(issues, number)]
        if len(closed_features) >= 2 and len(closed_features) % 2 == 0 and len(closed_features) not in captured_even:
            captured_even.add(len(closed_features))
            capture(f"{len(closed_features):02d}-features-closed")

        if not cancel_done and issue_closed(issues, 1):
            if not cancel_started:
                ready(CONTROL_CANCEL_ISSUE)
                cancel_started = True
                note("activated cancel control issue #14")
            elif running_mentions(CONTROL_CANCEL_ISSUE, state):
                capture("cancel-control-running")
                remove_label(CONTROL_CANCEL_ISSUE, READY_LABEL)
                add_label(CONTROL_CANCEL_ISSUE, CANCELED_LABEL)
                refresh_worker()
                cancel_done = True
                note("canceled control issue #14 after observing running state")
                capture("cancel-control-canceled")

        if cancel_done or not issue_closed(issues, 1):
            while next_feature <= 12 and issue_closed(issues, next_feature):
                next_feature += 1
            if next_feature <= 12:
                prev_ok = next_feature == 1 or issue_closed(issues, next_feature - 1)
                if prev_ok and issues[next_feature]["state"] == "OPEN" and READY_LABEL not in labels(issues[next_feature]):
                    ready(next_feature)
                    note(f"activated feature issue #{next_feature}")

        issues = issue_map()
        all_features_closed = all(issue_closed(issues, number) for number in FEATURE_ISSUES)
        cancel_terminal = CANCELED_LABEL in labels(issues[CONTROL_CANCEL_ISSUE]) or issue_closed(issues, CONTROL_CANCEL_ISSUE)
        if all_features_closed and cancel_terminal:
            capture("final-success")
            note("PASS: all feature issues closed and cancel control terminal")
            return 0

        time.sleep(POLL_SECONDS)

    note("FAIL: driver timeout")
    capture("fail-timeout")
    return 124


if __name__ == "__main__":
    raise SystemExit(main())
