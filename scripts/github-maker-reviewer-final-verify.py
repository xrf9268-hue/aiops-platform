#!/usr/bin/env python3
"""Fresh-clone verify a GitHub Web Todo repo and capture desktop/mobile screenshots."""

from __future__ import annotations

import argparse
import os
import shutil
import subprocess
import sys
import time
import urllib.request
from pathlib import Path


def parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--run-root", required=True, type=Path)
    p.add_argument("--repo", required=True, help="OWNER/NAME")
    p.add_argument("--gh-config-dir", default=os.environ.get("GH_CONFIG_DIR", ""))
    p.add_argument("--app-dir", type=Path)
    p.add_argument("--port", type=int, default=5189)
    p.add_argument("--skip-screenshots", action="store_true")
    return p


def run_logged(cmd: list[str], cwd: Path, log: Path, env: dict[str, str]) -> None:
    log.parent.mkdir(parents=True, exist_ok=True)
    with log.open("w") as fh:
        fh.write(f"$ {' '.join(cmd)}\n")
        fh.flush()
        proc = subprocess.run(cmd, cwd=cwd, text=True, stdout=fh, stderr=subprocess.STDOUT, env=env)
    if proc.returncode != 0:
        raise SystemExit(f"{' '.join(cmd)} failed; see {log}")


def clone_repo(args: argparse.Namespace, app_dir: Path, env: dict[str, str]) -> None:
    if app_dir.exists():
        shutil.rmtree(app_dir)
    app_dir.parent.mkdir(parents=True, exist_ok=True)
    clone = subprocess.run(
        ["gh", "repo", "clone", args.repo, str(app_dir)],
        text=True,
        capture_output=True,
        env=env,
    )
    if clone.returncode != 0:
        raise SystemExit(f"gh repo clone failed:\n{clone.stdout}\n{clone.stderr}")


def wait_for(url: str) -> None:
    deadline = time.time() + 60
    while time.time() < deadline:
        try:
            with urllib.request.urlopen(url, timeout=2):
                return
        except OSError:
            time.sleep(1)
    raise SystemExit(f"app did not become ready at {url}")


def capture_app(url: str, screenshots: Path) -> None:
    try:
        from playwright.sync_api import sync_playwright
    except ImportError as exc:
        raise SystemExit("Playwright is required for final app screenshots") from exc

    screenshots.mkdir(parents=True, exist_ok=True)
    with sync_playwright() as pw:
        browser = pw.chromium.launch(headless=True)
        for name, viewport in {
            "desktop": {"width": 1440, "height": 1000},
            "mobile": {"width": 390, "height": 844},
        }.items():
            page = browser.new_page(viewport=viewport)
            page.goto(url, wait_until="networkidle", timeout=30000)
            page.screenshot(path=str(screenshots / f"final-app-{name}.png"), full_page=True)
            page.close()
        browser.close()


def main() -> int:
    args = parser().parse_args()
    app_dir = args.app_dir or args.run_root / "final-verify" / "app"
    logs = args.run_root / "final-verify" / "logs"
    screenshots = args.run_root / "final-verify" / "screenshots"
    env = os.environ.copy()
    if args.gh_config_dir:
        env["GH_CONFIG_DIR"] = args.gh_config_dir

    clone_repo(args, app_dir, env)
    for name, cmd in [
        ("npm-ci.log", ["npm", "ci"]),
        ("npm-test.log", ["npm", "test"]),
        ("npm-build.log", ["npm", "run", "build"]),
        ("npm-e2e.log", ["npm", "run", "test:e2e"]),
    ]:
        run_logged(cmd, app_dir, logs / name, env)

    if not args.skip_screenshots:
        url = f"http://127.0.0.1:{args.port}"
        log = (logs / "npm-dev.log").open("w")
        proc = subprocess.Popen(
            ["npm", "run", "dev", "--", "--host", "127.0.0.1", "--port", str(args.port)],
            cwd=app_dir,
            text=True,
            stdout=log,
            stderr=subprocess.STDOUT,
            env=env,
        )
        try:
            wait_for(url)
            capture_app(url, screenshots)
        finally:
            proc.terminate()
            try:
                proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                proc.kill()
            log.close()

    print(f"fresh clone verification complete under {args.run_root / 'final-verify'}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
