#!/usr/bin/env python3
"""Capture GitHub maker/reviewer E2E worker, forge, CI, and screenshot evidence."""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import urllib.request
from pathlib import Path
from typing import Any


def parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--run-root", required=True, type=Path)
    p.add_argument("--repo", required=True, help="OWNER/NAME")
    p.add_argument("--tag", required=True, help="snapshot tag, for example preflight or final")
    p.add_argument("--maker-url", default="")
    p.add_argument("--reviewer-url", default="")
    p.add_argument("--gh-config-dir", default=os.environ.get("GH_CONFIG_DIR", ""))
    p.add_argument("--screenshot", action="append", default=[], help="NAME=URL")
    p.add_argument("--skip-screenshots", action="store_true")
    return p


def run(args: list[str], *, env: dict[str, str] | None = None, check: bool = True) -> subprocess.CompletedProcess[str]:
    return subprocess.run(args, text=True, capture_output=True, env=env, check=check)


def gh(args: argparse.Namespace, *cmd: str, check: bool = True) -> subprocess.CompletedProcess[str]:
    env = os.environ.copy()
    if args.gh_config_dir:
        env["GH_CONFIG_DIR"] = args.gh_config_dir
    return run(["gh", *cmd], env=env, check=check)


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
            "number,title,state,author,headRefName,headRefOid,baseRefName,mergeStateStatus,mergeable,autoMergeRequest,mergedAt,mergedBy,reviewDecision,reviews,statusCheckRollup,url,body,createdAt,updatedAt",
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
                    "number,title,state,author,headRefName,headRefOid,baseRefName,mergeStateStatus,mergeable,autoMergeRequest,mergedAt,mergedBy,reviewDecision,reviews,statusCheckRollup,url,body,createdAt,updatedAt",
                ).stdout,
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
        if args.screenshot:
            raise SystemExit("Playwright is required for requested screenshots") from exc
        print("skipped screenshots: Playwright is not installed", file=sys.stderr)
        return

    out_dir = args.run_root / "screenshots"
    out_dir.mkdir(parents=True, exist_ok=True)
    with sync_playwright() as pw:
        browser = pw.chromium.launch(headless=True)
        page = browser.new_page(viewport={"width": 1440, "height": 1000})
        for name, url in pairs:
            path = out_dir / f"{name}-{args.tag}.png"
            page.goto(url, wait_until="domcontentloaded", timeout=30000)
            page.wait_for_timeout(1000)
            page.screenshot(path=str(path), full_page=True)
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
