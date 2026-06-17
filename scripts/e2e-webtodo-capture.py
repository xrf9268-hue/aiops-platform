#!/usr/bin/env python3
"""Capture reusable evidence for the local Web Todo lifecycle E2E."""

from __future__ import annotations

import argparse
import json
import os
import signal
import subprocess
import time
import urllib.error
import urllib.request
from pathlib import Path


def parse_screenshot(value: str) -> tuple[str, str]:
    if "=" not in value:
        raise argparse.ArgumentTypeError("expected NAME=URL")
    name, url = value.split("=", 1)
    if not name or not url:
        raise argparse.ArgumentTypeError("expected non-empty NAME=URL")
    return name, url


def parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--run-root", required=True, type=Path)
    p.add_argument("--maker-url", default=os.getenv("AIOPS_WEBTODO_MAKER_DASHBOARD_URL", ""))
    p.add_argument("--reviewer-url", default=os.getenv("AIOPS_WEBTODO_REVIEWER_DASHBOARD_URL", ""))
    p.add_argument("--gitea-url", default=os.getenv("AIOPS_WEBTODO_GITEA_URL", ""))
    p.add_argument("--repo-owner", default=os.getenv("AIOPS_WEBTODO_REPO_OWNER", "aiops-bot"))
    p.add_argument("--repo-name", default=os.getenv("AIOPS_WEBTODO_REPO_NAME", "web-todo"))
    p.add_argument("--tui-bin", default=os.getenv("AIOPS_WEBTODO_TUI_BIN", "tui"))
    p.add_argument("--state-token", default=os.getenv("AIOPS_STATE_API_TOKEN", ""))
    p.add_argument("--tag", default=time.strftime("%H%M%S"))
    p.add_argument("--tui-seconds", type=float, default=2.0)
    p.add_argument("--screenshot", action="append", type=parse_screenshot, default=[])
    p.add_argument("--no-default-screenshots", action="store_true")
    return p


def ensure_dirs(run_root: Path) -> dict[str, Path]:
    dirs = {
        "state": run_root / "state",
        "screenshots": run_root / "promo" / "screenshots",
        "pages": run_root / "promo" / "pages",
        "promo": run_root / "promo",
    }
    for path in dirs.values():
        path.mkdir(parents=True, exist_ok=True)
    return dirs


def fetch_json(url: str, dest: Path, token: str = "") -> bool:
    if not url:
        return False
    req = urllib.request.Request(url.rstrip("/") + "/api/v1/state")
    if token:
        req.add_header("Authorization", f"Bearer {token}")
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            data = json.loads(resp.read().decode("utf-8"))
    except (urllib.error.URLError, TimeoutError, json.JSONDecodeError) as exc:
        dest.write_text(json.dumps({"error": str(exc)}, indent=2) + "\n")
        return False
    dest.write_text(json.dumps(data, indent=2, sort_keys=True) + "\n")
    return True


def capture_tui(tui_bin: str, url: str, dest: Path, seconds: float, token: str) -> bool:
    if not url:
        return False
    env = os.environ.copy()
    if token:
        env["AIOPS_STATE_API_TOKEN"] = token
    cmd = [tui_bin, "--url", url, "--interval", "1s", "--raw"]
    try:
        proc = subprocess.Popen(
            cmd,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            env=env,
        )
    except OSError as exc:
        dest.write_text(f"failed to start {' '.join(cmd)}: {exc}\n")
        return False
    time.sleep(max(seconds, 0.5))
    proc.send_signal(signal.SIGTERM)
    try:
        stdout, stderr = proc.communicate(timeout=5)
    except subprocess.TimeoutExpired:
        proc.kill()
        stdout, stderr = proc.communicate()
    dest.write_text(stdout + ("\n--- stderr ---\n" + stderr if stderr else ""))
    return proc.returncode in (0, -signal.SIGTERM)


def default_screenshots(args: argparse.Namespace) -> list[tuple[str, str]]:
    shots: list[tuple[str, str]] = []
    if args.gitea_url:
        base = args.gitea_url.rstrip("/")
        repo = f"{base}/{args.repo_owner}/{args.repo_name}"
        shots.extend([
            ("gitea-repo", repo),
            ("gitea-issues", f"{repo}/issues"),
            ("gitea-pulls", f"{repo}/pulls"),
        ])
    if args.maker_url:
        shots.append(("maker-dashboard", args.maker_url))
    if args.reviewer_url:
        shots.append(("reviewer-dashboard", args.reviewer_url))
    return shots


def safe_screenshot_stem(name: str) -> str:
    stem = "".join(ch if ch.isalnum() or ch in "._-" else "-" for ch in name)
    return stem.strip(".-") or "page"


def capture_screenshots(
    shots: list[tuple[str, str]],
    dest_dir: Path,
    tag: str,
    required: bool,
) -> list[tuple[str, str, str]]:
    if not shots:
        return []
    try:
        from playwright.sync_api import sync_playwright
    except ImportError as exc:
        if required:
            raise SystemExit(
                "Playwright is required for requested screenshots. Install it or omit --screenshot."
            ) from exc
        print("Playwright is not installed; skipped default screenshots")
        return []
    captured: list[tuple[str, str, str]] = []
    with sync_playwright() as pw:
        browser = pw.chromium.launch()
        page = browser.new_page(viewport={"width": 1440, "height": 1100})
        for name, url in shots:
            path = dest_dir / f"{safe_screenshot_stem(name)}-{tag}.png"
            page.goto(url, wait_until="networkidle", timeout=30_000)
            page.screenshot(path=str(path), full_page=True)
            captured.append((name, url, str(path)))
        browser.close()
    return captured


def append_index(run_root: Path, tag: str, entries: list[tuple[str, str, str]]) -> None:
    index = run_root / "promo" / "capture-index.md"
    existing = index.read_text() if index.exists() else "# Page Capture Index\n"
    lines = [existing.rstrip(), "", f"## Capture {tag}", ""]
    for name, url, path in entries:
        rel = Path(path).relative_to(run_root)
        lines.append(f"- `{name}`: {url} -> `{rel}`")
    index.write_text("\n".join(lines) + "\n")


def main() -> int:
    args = parser().parse_args()
    dirs = ensure_dirs(args.run_root)

    fetch_json(args.maker_url, dirs["state"] / f"maker-{args.tag}.json", args.state_token)
    fetch_json(args.reviewer_url, dirs["state"] / f"reviewer-{args.tag}.json", args.state_token)

    capture_tui(
        args.tui_bin,
        args.maker_url,
        dirs["pages"] / f"tui-maker-{args.tag}.txt",
        args.tui_seconds,
        args.state_token,
    )
    capture_tui(
        args.tui_bin,
        args.reviewer_url,
        dirs["pages"] / f"tui-reviewer-{args.tag}.txt",
        args.tui_seconds,
        args.state_token,
    )

    explicit_shots = list(args.screenshot)
    shots = list(explicit_shots)
    if not args.no_default_screenshots:
        shots = default_screenshots(args) + shots
    entries = (
        capture_screenshots(shots, dirs["screenshots"], args.tag, bool(explicit_shots))
        if shots
        else []
    )
    if entries:
        append_index(args.run_root, args.tag, entries)
    print(f"captured evidence under {args.run_root}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
