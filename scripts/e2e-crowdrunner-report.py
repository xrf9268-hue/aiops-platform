#!/usr/bin/env python3
"""Generate a Crowd Runner product lifecycle E2E report pack from a run root."""

from __future__ import annotations

import argparse
import json
import time
from pathlib import Path
from typing import Any


def parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--run-root", required=True, type=Path)
    p.add_argument("--title", default="Crowd Runner Product Lifecycle E2E Report")
    p.add_argument("--date", default=time.strftime("%Y-%m-%d"))
    return p


def load_json(path: Path, default: Any) -> Any:
    if not path.exists():
        return default
    try:
        return json.loads(path.read_text())
    except json.JSONDecodeError:
        return default


def issue_label_names(issue: dict[str, Any]) -> list[str]:
    labels = issue.get("labels") or []
    names = []
    for label in labels:
        if isinstance(label, dict):
            names.append(str(label.get("name", "")))
        else:
            names.append(str(label))
    return [name for name in names if name]


def label_names(issue: dict[str, Any]) -> str:
    return ", ".join(issue_label_names(issue)) or "-"


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


def control_issue_number(expectation: dict[str, Any]) -> int:
    try:
        return int(expectation.get("issue_number", 16))
    except (TypeError, ValueError):
        return 16


def control_pr_refs(prs: list[dict[str, Any]], issue_number: int) -> list[str]:
    needle = f"#{issue_number}"
    branch = f"ai/{issue_number}"
    slug = "control-continuation-budget"
    refs = []
    for pr in prs:
        head = pr.get("head") or {}
        head_ref = str(head.get("ref", ""))
        text = " ".join(
            str(value)
            for value in [
                pr.get("title", ""),
                pr.get("body", ""),
                head_ref,
            ]
        )
        if (
            needle in text
            or slug in text.lower()
            or head_ref == branch
            or head_ref.startswith(f"{branch}-")
        ):
            refs.append(f"#{pr.get('number', '?')}")
    return refs


def control_issue_status_failure(
    issues: list[dict[str, Any]],
    expectation: dict[str, Any],
    issue_number: int,
) -> str:
    forbidden_labels = expectation.get("forbidden_terminal_labels") or [
        "aiops/done",
        "aiops/canceled",
        "aiops/human-review",
    ]
    forbidden = [str(label) for label in forbidden_labels]
    for issue in issues:
        try:
            number = int(issue.get("number", 0))
        except (TypeError, ValueError):
            continue
        if number != issue_number:
            continue
        labels = issue_label_names(issue)
        forbidden_found = [label for label in forbidden if label in labels]
        if forbidden_found:
            return (
                f"FAIL: control issue #{issue_number} has forbidden label(s) "
                f"{', '.join(forbidden_found)}."
            )
        state = str(issue.get("state", "")).lower()
        if state and state != "open":
            return f"FAIL: control issue #{issue_number} reached terminal state `{state}`."
        return ""
    return f"FAIL: control issue #{issue_number} missing from issues-final evidence."


def continuation_budget_rows(stress: dict[str, Any], method: str, issue_number: int) -> list[dict[str, Any]]:
    rows = stress.get("blocked") or []
    if not isinstance(rows, list):
        return []
    matches = []
    for row in rows:
        if not isinstance(row, dict) or row.get("method") != method:
            continue
        identifier = str(row.get("issue_identifier", ""))
        issue_id = str(row.get("issue_id", ""))
        issue_url = str(row.get("issue_url", "")).rstrip("/")
        if (
            identifier == f"#{issue_number}"
            or issue_id == str(issue_number)
            or issue_url.endswith(f"/issues/{issue_number}")
        ):
            matches.append(row)
    return matches


def continuation_control_verdict(
    stress: dict[str, Any],
    prs: list[dict[str, Any]],
    issues: list[dict[str, Any]],
    expectation: dict[str, Any],
) -> str:
    issue_number = control_issue_number(expectation)
    method = str(expectation.get("expected_blocked_method", "continuation_budget"))
    pr_refs = control_pr_refs(prs, issue_number)
    if pr_refs:
        return f"FAIL: control issue #{issue_number} produced PR(s) {', '.join(pr_refs)}."
    status_failure = control_issue_status_failure(issues, expectation, issue_number)
    if status_failure:
        return status_failure
    rows = continuation_budget_rows(stress, method, issue_number)
    if rows:
        return f"PASS: issue #{issue_number} blocked via `{method}` in stress worker state."
    if (stress.get("counts") or {}).get("blocked", 0):
        return f"FAIL: stress worker blocked, but no issue #{issue_number} `{method}` row was captured."
    return f"FAIL: stress worker did not capture issue #{issue_number} `{method}` exhaustion."


def collect_assets(run_root: Path) -> dict[str, list[Path]]:
    return {
        "promo_screenshots": sorted((run_root / "promo" / "screenshots").glob("**/*.png")),
        "final_screenshots": sorted((run_root / "final-verify" / "screenshots").glob("**/*.png")),
        "product_evidence": sorted((run_root / "final-verify" / "product-evidence").glob("*")),
        "videos": sorted((run_root / "final-verify" / "videos").glob("*.webm")),
        "traces": sorted((run_root / "final-verify" / "traces").glob("*")),
        "tui": sorted((run_root / "promo" / "pages").glob("tui-*.txt")),
        "logs": sorted((run_root / "logs").glob("*.log")) + sorted((run_root / "final-verify").glob("*.log")),
        "reports": sorted((run_root / "final-verify" / "playwright-report").glob("**/*")),
    }


def rel(path: Path, root: Path) -> str:
    try:
        return str(path.relative_to(root))
    except ValueError:
        return str(path)


def asset_bullets(paths: list[Path], root: Path) -> list[str]:
    files = [path for path in paths if path.is_file()]
    return [f"- `{rel(path, root)}`" for path in files] or ["- none captured"]


def load_milestone_freezes(run_root: Path) -> list[tuple[Path, dict[str, Any]]]:
    records = []
    for path in sorted((run_root / "state").glob("operator-milestone-freeze-*.json")):
        data = load_json(path, {})
        if isinstance(data, dict) and data.get("kind") == "operator_milestone_freeze":
            records.append((path, data))
    return records


def milestone_freeze_bullets(run_root: Path) -> list[str]:
    freezes = load_milestone_freezes(run_root)
    if not freezes:
        return ["- none recorded"]
    lines = []
    for path, freeze in freezes:
        removed = ", ".join(
            f"#{item.get('number')}" for item in freeze.get("ready_labels_removed", [])
        ) or "none"
        lines.append(
            "- operator milestone freeze after {stop_after}: "
            "{completed} completed, ready labels removed from {removed}; evidence `{path}`".format(
                stop_after=freeze.get("stop_after", "?"),
                completed=freeze.get("completed_product_issues", "?"),
                removed=removed,
                path=rel(path, run_root),
            )
        )
    return lines


def lifecycle_verdict(product_done: list[dict[str, Any]], merged_prs: list[dict[str, Any]]) -> str:
    if len(product_done) >= 10 and len(merged_prs) >= 10:
        return "READY FOR OPERATOR PASS REVIEW"
    return "INCOMPLETE"


def codex_delivery_verdict(product_done: list[dict[str, Any]]) -> str:
    if len(product_done) >= 10:
        return "Codex delivered the minimum product issue count."
    return "Codex delivery is incomplete."


def product_quality_verdict(run_root: Path) -> str:
    verification = run_root / "final-verify" / "final-verification.log"
    if not verification.exists():
        verification = run_root / "final-verify" / "verification.log"
    if not verification.exists():
        return "Product quality not yet verified: missing final verification log."
    text = verification.read_text(errors="replace")
    required = [
        "npm ci",
        "npm run lint",
        "npm run test -- --run",
        "npm run test:e2e",
        "npm run build",
    ]
    if all(item in text for item in required) and "failed" not in text.lower():
        return "Final product verification log contains the required npm gates."
    return "Final product verification log needs operator review."


def operator_checklist() -> list[str]:
    return [
        "- [ ] Release archive SHA256 and GitHub attestation were verified.",
        "- [ ] Maker and reviewer doctor logs passed in real mode.",
        "- [ ] At least ten product issues reached `aiops/done`.",
        "- [ ] At least ten product PRs merged through reviewer/CI-gated flow.",
        "- [ ] Rework was exercised and reviewer findings are linked.",
        "- [ ] Reconcile cancellation control was captured.",
        "- [ ] Continuation / turn-budget stress evidence was captured.",
        "- [ ] Operator milestone freeze evidence was captured when used.",
        "- [ ] Fresh-clone npm verification passed.",
        "- [ ] Product screenshots include gameplay, mobile, performance, and boss/finale evidence.",
        "- [ ] Maker, reviewer, and optional stress dashboards are idle or intentionally stopped.",
    ]


def write_report(args: argparse.Namespace) -> Path:
    run_root = args.run_root
    reports = run_root / "reports"
    reports.mkdir(parents=True, exist_ok=True)
    all_issues = load_json(run_root / "state" / "issues-final.json", [])
    issues = [issue for issue in all_issues if not issue.get("pull_request")]
    prs = load_json(run_root / "state" / "prs-final.json", [])
    maker = load_json(run_root / "state" / "maker-final.json", {})
    reviewer = load_json(run_root / "state" / "reviewer-final.json", {})
    stress = load_json(run_root / "state" / "stress-final.json", {})
    control_expectation = load_json(run_root / "state" / "continuation-control-expected.json", {})
    assets = collect_assets(run_root)

    product_done = [
        issue for issue in issues
        if 1 <= int(issue.get("number", 0)) <= 12 and "aiops/done" in label_names(issue)
    ]
    merged_prs = [pr for pr in prs if pr.get("merged")]
    verdict = lifecycle_verdict(product_done, merged_prs)

    lines = [
        f"# {args.title}",
        "",
        f"Run root: `{run_root}`",
        f"Date: {args.date}",
        "",
        "## Verdicts",
        "",
        f"- aiops-platform lifecycle: **{verdict}**",
        f"- Codex product delivery: {codex_delivery_verdict(product_done)}",
        f"- Product quality: {product_quality_verdict(run_root)}",
        f"- Continuation control: {continuation_control_verdict(stress, prs, issues, control_expectation)}",
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
        counts_line("Stress", stress),
        "",
        "## Control Scenario Assertions",
        "",
        f"- Continuation budget: {continuation_control_verdict(stress, prs, issues, control_expectation)}",
        "",
        "## Issue Results",
        "",
        *issue_rows(issues),
        "",
        "## PR Results",
        "",
        *pr_rows(prs),
        "",
        "## Operator Milestone Freezes",
        "",
        *milestone_freeze_bullets(run_root),
        "",
        "## Evidence Inventory",
        "",
        "Promotion screenshots:",
        *asset_bullets(assets["promo_screenshots"], run_root),
        "",
        "Final product screenshots:",
        *asset_bullets(assets["final_screenshots"], run_root),
        "",
        "Product evidence files:",
        *asset_bullets(assets["product_evidence"], run_root),
        "",
        "Videos:",
        *asset_bullets(assets["videos"], run_root),
        "",
        "Playwright traces:",
        *asset_bullets(assets["traces"], run_root),
        "",
        "TUI raw frames:",
        *asset_bullets(assets["tui"], run_root),
        "",
        "Logs:",
        *asset_bullets(assets["logs"], run_root),
        "",
        "Playwright report files:",
        *asset_bullets(assets["reports"], run_root),
        "",
        "## Notes",
        "",
        "- Review generated rows against the live run before committing an evidence pack.",
        "- Do not commit `env.local`, Codex auth files, downloaded binaries, cache directories, or credential-bearing workflow files.",
    ]
    path = reports / "report.md"
    path.write_text("\n".join(lines) + "\n")
    return path


def write_promo_notes(args: argparse.Namespace) -> Path:
    notes_dir = args.run_root / "promo" / "notes"
    notes_dir.mkdir(parents=True, exist_ok=True)
    path = notes_dir / "promotion-materials.md"
    lines = [
        "# Crowd Runner Product E2E Promotion Notes",
        "",
        f"Run root: `{args.run_root}`",
        f"Captured at: {args.date}",
        "",
        "## Headline Story",
        "",
        "- Latest aiops-platform release binary runs locally with real Codex app-server.",
        "- Maker and reviewer workers build and review a production-style 3D game through Gitea issues and PRs.",
        "- Rework, cancellation, and continuation-budget controls are first-class evidence, not afterthoughts.",
        "- Final product evidence should include gameplay, boss/finale, mobile, performance, dashboards, Gitea issue/PR pages, TUI frames, and verification logs.",
        "",
        "## Copy Points",
        "",
        "- The worker schedules and observes; agents own PRs, review comments, merges, and tracker handoff.",
        "- Local binary mode intentionally uses the operator's Codex configuration.",
        "- The product is a fresh crowd-runner design inspired by genre mechanics, not a copy of the previous private implementation.",
    ]
    path.write_text("\n".join(lines) + "\n")
    return path


def main() -> int:
    args = parser().parse_args()
    report = write_report(args)
    notes = write_promo_notes(args)
    print(f"wrote {report}")
    print(f"wrote {notes}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
