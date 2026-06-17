#!/usr/bin/env python3
"""Generate a Web Todo lifecycle E2E report pack from a run root."""

from __future__ import annotations

import argparse
import json
import time
from pathlib import Path
from typing import Any


def parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--run-root", required=True, type=Path)
    p.add_argument("--title", default="Web Todo E2E Lifecycle Test Report")
    p.add_argument("--date", default=time.strftime("%Y-%m-%d"))
    return p


def load_json(path: Path, default: Any) -> Any:
    if not path.exists():
        return default
    try:
        return json.loads(path.read_text())
    except json.JSONDecodeError:
        return default


def label_names(issue: dict[str, Any]) -> str:
    labels = issue.get("labels") or []
    names = []
    for label in labels:
        if isinstance(label, dict):
            names.append(str(label.get("name", "")))
        else:
            names.append(str(label))
    return ", ".join(name for name in names if name) or "-"


def issue_rows(issues: list[dict[str, Any]]) -> list[str]:
    rows = ["| Issue | Title | State | Labels |", "|---|---|---|---|"]
    for issue in sorted(issues, key=lambda item: int(item.get("number", 0))):
        rows.append(
            "| #{number} | {title} | {state} | {labels} |".format(
                number=issue.get("number", ""),
                title=str(issue.get("title", "")).replace("|", "\\|"),
                state=issue.get("state", ""),
                labels=label_names(issue).replace("|", "\\|"),
            )
        )
    return rows


def pr_rows(prs: list[dict[str, Any]]) -> list[str]:
    rows = ["| PR | Title | Branch | State | Merged |", "|---|---|---|---|---|"]
    for pr in sorted(prs, key=lambda item: int(item.get("number", 0))):
        head = pr.get("head") or {}
        rows.append(
            "| #{number} | {title} | {branch} | {state} | {merged} |".format(
                number=pr.get("number", ""),
                title=str(pr.get("title", "")).replace("|", "\\|"),
                branch=head.get("ref", ""),
                state=pr.get("state", ""),
                merged="yes" if pr.get("merged") else "no",
            )
        )
    return rows


def counts_line(name: str, state: dict[str, Any]) -> str:
    counts = state.get("counts", {})
    if not counts:
        return f"- {name}: no final state snapshot found"
    interesting = {
        "running": counts.get("running", 0),
        "blocked": counts.get("blocked", 0),
        "retrying": counts.get("retrying", 0),
        "agent_handoff_reconcile_stopped": counts.get("agent_handoff_reconcile_stopped", 0),
        "operator_terminal_stops": counts.get("operator_terminal_stops", 0),
    }
    return f"- {name}: `{interesting}`"


def collect_assets(run_root: Path) -> dict[str, list[Path]]:
    return {
        "promo_screenshots": sorted((run_root / "promo" / "screenshots").glob("*.png")),
        "final_screenshots": sorted((run_root / "final-verify" / "screenshots").glob("*.png")),
        "videos": sorted((run_root / "final-verify" / "videos").glob("*.webm")),
        "tui": sorted((run_root / "promo" / "pages").glob("tui-*.txt")),
        "logs": sorted((run_root / "final-verify").glob("*.log")),
    }


def rel(path: Path, root: Path) -> str:
    try:
        return str(path.relative_to(root))
    except ValueError:
        return str(path)


def asset_bullets(paths: list[Path], root: Path) -> list[str]:
    return [f"- `{rel(path, root)}`" for path in paths] or ["- none captured"]


def automated_verdict(primary_done: list[dict[str, Any]], merged_prs: list[dict[str, Any]]) -> str:
    if len(primary_done) == 10 and len(merged_prs) >= 10:
        return "READY FOR OPERATOR PASS REVIEW"
    return "INCOMPLETE"


def operator_checklist() -> list[str]:
    return [
        "- [ ] Control no-ready issue never dispatched and has no PR.",
        "- [ ] Control blocked/dependency issue stayed out of dispatch until terminal blockers.",
        "- [ ] Control cancel-running issue stopped without creating a PR.",
        "- [ ] Maker and reviewer dashboards are idle at closeout.",
        "- [ ] Fresh-clone Go verification log passes.",
        "- [ ] Final browser smoke passes with empty console output.",
        "- [ ] Required screenshots, TUI frames, logs, and video evidence are present.",
    ]


def write_report(args: argparse.Namespace) -> Path:
    run_root = args.run_root
    reports = run_root / "reports"
    reports.mkdir(parents=True, exist_ok=True)
    issues = load_json(run_root / "state" / "issues-final.json", [])
    prs = load_json(run_root / "state" / "prs-final.json", [])
    maker = load_json(run_root / "state" / "maker-final.json", {})
    reviewer = load_json(run_root / "state" / "reviewer-final.json", {})
    assets = collect_assets(run_root)

    primary_done = [
        issue for issue in issues
        if 1 <= int(issue.get("number", 0)) <= 10 and "aiops/done" in label_names(issue)
    ]
    merged_prs = [pr for pr in prs if pr.get("merged")]
    verdict = automated_verdict(primary_done, merged_prs)

    lines = [
        f"# {args.title}",
        "",
        f"Run root: `{run_root}`",
        f"Date: {args.date}",
        "",
        "## Verdict",
        "",
        verdict,
        "",
        "The helper does not self-certify a full pass. Mark the checklist below",
        "against the live evidence before promoting or committing the report.",
        "",
        "## Operator Pass Checklist",
        "",
        *operator_checklist(),
        "",
        "## Final Worker State",
        "",
        counts_line("Maker", maker),
        counts_line("Reviewer", reviewer),
        "",
        "## Primary Issue Results",
        "",
        *issue_rows(issues),
        "",
        "## PR Results",
        "",
        *pr_rows(prs),
        "",
        "## Evidence Inventory",
        "",
        "Promotion screenshots:",
        *asset_bullets(assets["promo_screenshots"], run_root),
        "",
        "Final Web UI screenshots:",
        *asset_bullets(assets["final_screenshots"], run_root),
        "",
        "Videos:",
        *asset_bullets(assets["videos"], run_root),
        "",
        "TUI raw frames:",
        *asset_bullets(assets["tui"], run_root),
        "",
        "Verification / console logs:",
        *asset_bullets(assets["logs"], run_root),
        "",
        "## Notes",
        "",
        "- Review generated rows against the live run before committing an evidence pack.",
        "- Do not commit `env.local`, Codex auth files, downloaded binaries, or cache directories.",
    ]
    path = reports / "report.md"
    path.write_text("\n".join(lines) + "\n")
    return path


def write_promo_notes(args: argparse.Namespace) -> Path:
    notes_dir = args.run_root / "promo" / "notes"
    notes_dir.mkdir(parents=True, exist_ok=True)
    path = notes_dir / "promotion-materials.md"
    lines = [
        "# Promotion Material Notes",
        "",
        f"Run root: `{args.run_root}`",
        f"Captured at: {args.date}",
        "",
        "## Headline Story",
        "",
        "- Latest binary runs locally against Gitea with real Codex app-server.",
        "- Maker implements one issue per PR; reviewer independently verifies and merges.",
        "- Rework is a first-class path, not a failure of the lifecycle.",
        "- Final smoke evidence should include dashboard, Gitea issue/PR pages, TUI raw frames, Web UI screenshots, and a browser video.",
        "",
        "## Copy Points",
        "",
        "- The worker schedules and observes; agents own PRs, review comments, merges, and tracker handoff.",
        "- Local binary mode intentionally uses the operator's Codex configuration.",
        "- Production-style reproducibility comes from a dedicated `CODEX_HOME` and explicit workflow config.",
        "",
        "## Caveats",
        "",
        "- Token usage is environment-dependent and should not be marketed as a benchmark.",
        "- Keep raw credentials and auth homes out of committed evidence packs.",
    ]
    path.write_text("\n".join(lines) + "\n")
    return path


def main() -> int:
    args = parser().parse_args()
    report = write_report(args)
    promo = write_promo_notes(args)
    print(f"wrote {report}")
    print(f"wrote {promo}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
