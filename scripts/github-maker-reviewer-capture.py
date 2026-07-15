#!/usr/bin/env python3
"""Capture GitHub maker/reviewer E2E worker, forge, CI, and screenshot evidence."""

from __future__ import annotations

import argparse
import json
import os
import shlex
import subprocess
import sys
import urllib.request
from pathlib import Path
from typing import Any


PR_JSON_FIELDS = (
    "number,title,state,author,headRefName,headRefOid,baseRefName,mergeStateStatus,mergeable,"
    "autoMergeRequest,mergedAt,mergedBy,mergeCommit,closingIssuesReferences,reviewDecision,reviews,statusCheckRollup,"
    "url,body,createdAt,updatedAt"
)


def parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--run-root", required=True, type=Path)
    p.add_argument("--repo", required=True, help="OWNER/NAME")
    p.add_argument("--tag", required=True, help="snapshot tag, for example preflight or final")
    p.add_argument("--maker-url", default="")
    p.add_argument("--reviewer-url", default="")
    p.add_argument("--gh-config-dir", default=os.environ.get("GH_CONFIG_DIR", ""))
    p.add_argument("--screenshot", action="append", default=[], help="NAME=URL")
    p.add_argument("--include-github-pages", action="store_true", help="capture default GitHub issues/actions pages")
    p.add_argument("--browser-storage-state", type=Path, help="Playwright storage_state JSON for authenticated browser captures")
    p.add_argument("--skip-screenshots", action="store_true")
    p.add_argument("--command-timeout-seconds", type=int, default=300, help="timeout for each gh subprocess")
    return p


def command_text(args: list[str]) -> str:
    return shlex.join(args)


def run(
    args: list[str],
    *,
    env: dict[str, str] | None = None,
    check: bool = True,
    timeout_seconds: int,
    timeout_log: Path | None = None,
) -> subprocess.CompletedProcess[str]:
    try:
        return subprocess.run(args, text=True, capture_output=True, env=env, check=check, timeout=timeout_seconds)
    except subprocess.TimeoutExpired as exc:
        message = f"{command_text(args)} timed out after {timeout_seconds}s"
        if timeout_log:
            timeout_log.parent.mkdir(parents=True, exist_ok=True)
            with timeout_log.open("a", encoding="utf-8") as fh:
                fh.write(f"TIMEOUT after {timeout_seconds}s\n")
                fh.write(f"command: {command_text(args)}\n")
            message += f"; see {timeout_log}"
        raise SystemExit(message) from exc


def gh(args: argparse.Namespace, *cmd: str, check: bool = True) -> subprocess.CompletedProcess[str]:
    env = os.environ.copy()
    env.pop("GH_TOKEN", None)
    env.pop("GITHUB_TOKEN", None)
    if args.gh_config_dir:
        env["GH_CONFIG_DIR"] = args.gh_config_dir
    log = args.run_root / "logs" / f"capture-{args.tag}-commands.log"
    return run(["gh", *cmd], env=env, check=check, timeout_seconds=args.command_timeout_seconds, timeout_log=log)


def write_json(path: Path, raw: str) -> Any:
    path.parent.mkdir(parents=True, exist_ok=True)
    try:
        parsed = json.loads(raw)
    except json.JSONDecodeError:
        path.write_text(raw)
        return None
    path.write_text(json.dumps(parsed, indent=2, sort_keys=True) + "\n")
    return parsed


def fetch_worker_state(url: str, dest: Path) -> None:
    headers = {}
    token = os.environ.get("AIOPS_STATE_API_TOKEN")
    if token:
        headers["Authorization"] = f"Bearer {token}"
    req = urllib.request.Request(url.rstrip("/") + "/api/v1/state", headers=headers)
    with urllib.request.urlopen(req, timeout=15) as resp:
        raw = resp.read().decode()
    write_json(dest, raw)


def capture_gh_json(args: argparse.Namespace) -> None:
    out_dir = args.run_root / "forge-json"
    repo = args.repo
    tag = args.tag

    issues = write_json(
        out_dir / f"issues-{tag}.json",
        gh(
            args,
            "issue",
            "list",
            "--repo",
            repo,
            "--state",
            "all",
            "--limit",
            "100",
            "--json",
            "number,title,state,labels,closed,closedAt,url,updatedAt",
        ).stdout,
    )
    if isinstance(issues, list):
        for issue in issues:
            number = str(issue.get("number", ""))
            if not number:
                continue
            write_json(
                out_dir / f"issue-{number}-{tag}.json",
                gh(
                    args,
                    "issue",
                    "view",
                    number,
                    "--repo",
                    repo,
                    "--json",
                    "number,title,state,labels,closed,closedAt,url,updatedAt,body,comments",
                ).stdout,
            )
            write_json(
                out_dir / f"issue-{number}-events-{tag}.json",
                gh(args, "api", f"repos/{repo}/issues/{number}/events?per_page=100").stdout,
            )

    prs = write_json(
        out_dir / f"prs-{tag}.json",
        gh(
            args,
            "pr",
            "list",
            "--repo",
            repo,
            "--state",
            "all",
            "--limit",
            "100",
            "--json",
            PR_JSON_FIELDS,
        ).stdout,
    )
    if isinstance(prs, list):
        for pr in prs:
            number = str(pr.get("number", ""))
            if not number:
                continue
            write_json(
                out_dir / f"pr-{number}-{tag}.json",
                gh(
                    args,
                    "pr",
                    "view",
                    number,
                    "--repo",
                    repo,
                    "--json",
                    PR_JSON_FIELDS,
                ).stdout,
            )
            write_json(
                out_dir / f"pr-{number}-reviews-{tag}.json",
                gh(args, "api", f"repos/{repo}/pulls/{number}/reviews?per_page=100").stdout,
            )

    write_json(
        out_dir / f"actions-runs-{tag}.json",
        gh(
            args,
            "run",
            "list",
            "--repo",
            repo,
            "--limit",
            "100",
            "--json",
            "databaseId,displayTitle,event,headBranch,headSha,status,conclusion,workflowName,createdAt,updatedAt,url",
        ).stdout,
    )

    protection = gh(args, "api", f"repos/{repo}/branches/main/protection", check=False)
    if protection.returncode == 0:
        write_json(out_dir / f"branch-protection-{tag}.json", protection.stdout)
    else:
        (out_dir / f"branch-protection-{tag}.err").write_text(protection.stderr)


def screenshot_pairs(args: argparse.Namespace) -> list[tuple[str, str]]:
    pairs: list[tuple[str, str]] = []
    if args.maker_url:
        pairs.append(("maker-dashboard", args.maker_url))
    if args.reviewer_url:
        pairs.append(("reviewer-dashboard", args.reviewer_url))
    if args.include_github_pages:
        pairs.append(("github-issues", f"https://github.com/{args.repo}/issues"))
        pairs.append(("github-actions", f"https://github.com/{args.repo}/actions"))
    for item in args.screenshot:
        if "=" not in item:
            raise SystemExit(f"--screenshot must be NAME=URL, got {item!r}")
        name, url = item.split("=", 1)
        pairs.append((name, url))
    return pairs


def capture_screenshots(args: argparse.Namespace) -> None:
    if args.skip_screenshots:
        return
    pairs = screenshot_pairs(args)
    if not pairs:
        return
    try:
        from playwright.sync_api import sync_playwright
    except ImportError as exc:
        raise SystemExit("Playwright is required for requested screenshots") from exc

    out_dir = args.run_root / "screenshots"
    out_dir.mkdir(parents=True, exist_ok=True)
    if args.browser_storage_state and not args.browser_storage_state.exists():
        raise SystemExit(f"browser storage state not found: {args.browser_storage_state}")
    if not args.browser_storage_state and any(url.startswith("https://github.com/") for _, url in pairs):
        print("warning: GitHub screenshots use an unauthenticated browser; private repos need --browser-storage-state", file=sys.stderr)
    with sync_playwright() as pw:
        browser = pw.chromium.launch(headless=True)
        context_kwargs: dict[str, Any] = {"viewport": {"width": 1440, "height": 1000}}
        if args.browser_storage_state:
            context_kwargs["storage_state"] = str(args.browser_storage_state)
        context = browser.new_context(**context_kwargs)
        page = context.new_page()
        for name, url in pairs:
            path = out_dir / f"{name}-{args.tag}.png"
            page.goto(url, wait_until="domcontentloaded", timeout=30000)
            page.wait_for_timeout(1000)
            page.screenshot(path=str(path), full_page=True)
        context.close()
        browser.close()


def main() -> int:
    args = parser().parse_args()
    (args.run_root / "state").mkdir(parents=True, exist_ok=True)
    if args.maker_url:
        fetch_worker_state(args.maker_url, args.run_root / "state" / f"maker-state-{args.tag}.json")
    if args.reviewer_url:
        fetch_worker_state(args.reviewer_url, args.run_root / "state" / f"reviewer-state-{args.tag}.json")
    capture_gh_json(args)
    capture_screenshots(args)
    print(f"captured {args.tag} under {args.run_root}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
