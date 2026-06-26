#!/usr/bin/env python3
"""Generate GitHub maker/reviewer auto-merge reports from captured JSON."""

from __future__ import annotations

import argparse
import json
import time
from pathlib import Path
from typing import Any


def parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--run-root", required=True, type=Path)
    p.add_argument("--repo", required=True)
    p.add_argument("--date", default=time.strftime("%Y-%m-%d"))
    return p


def load_first(paths: list[Path], default: Any) -> Any:
    for path in paths:
        if path.exists():
            try:
                return json.loads(path.read_text())
            except json.JSONDecodeError:
                return default
    return default


def newest_json(paths: list[Path]) -> list[Path]:
    existing = [path for path in paths if path.exists()]
    return sorted(existing, key=lambda path: path.stat().st_mtime, reverse=True)


def evidence_candidates(forge_json: Path, state: Path, kind: str) -> list[Path]:
    preferred = [
        forge_json / f"{kind}-final.json",
        forge_json / f"final-{kind}-all.json",
        state / f"{kind}-final.json",
    ]
    discovered = newest_json(list(forge_json.glob(f"{kind}-*.json")))
    return preferred + [path for path in discovered if path not in preferred]


def label_names(issue: dict[str, Any]) -> list[str]:
    names: list[str] = []
    for label in issue.get("labels") or []:
        if isinstance(label, dict):
            name = label.get("name")
        else:
            name = label
        if name:
            names.append(str(name))
    return names


def user_login(value: Any) -> str:
    if isinstance(value, dict):
        return str(value.get("login") or value.get("name") or "")
    return str(value or "")


def review_states(pr: dict[str, Any]) -> list[str]:
    states: list[str] = []
    for review in pr.get("reviews") or []:
        state = review.get("state") if isinstance(review, dict) else None
        if state:
            states.append(str(state))
    return states


def issue_rows(issues: list[dict[str, Any]]) -> list[str]:
    rows = ["| Issue | Title | State | Labels | Closed at |", "|---|---|---|---|---|"]
    for issue in sorted(issues, key=lambda item: int(item.get("number", 0))):
        rows.append(
            "| #{number} | {title} | {state} | {labels} | {closed} |".format(
                number=issue.get("number", ""),
                title=str(issue.get("title", "")).replace("|", "\\|"),
                state=issue.get("state", ""),
                labels=", ".join(label_names(issue)) or "-",
                closed=issue.get("closedAt") or "-",
            )
        )
    return rows


def pr_rows(prs: list[dict[str, Any]]) -> list[str]:
    rows = [
        "| PR | Title | Author | Head | Reviews | Merged by | Merged at |",
        "|---|---|---|---|---|---|---|",
    ]
    for pr in sorted(prs, key=lambda item: int(item.get("number", 0))):
        rows.append(
            "| #{number} | {title} | {author} | {head} | {reviews} | {merged_by} | {merged_at} |".format(
                number=pr.get("number", ""),
                title=str(pr.get("title", "")).replace("|", "\\|"),
                author=user_login(pr.get("author")) or "-",
                head=str(pr.get("headRefOid") or "")[:12] or "-",
                reviews=", ".join(review_states(pr)) or pr.get("reviewDecision") or "-",
                merged_by=user_login(pr.get("mergedBy")) or "-",
                merged_at=pr.get("mergedAt") or "-",
            )
        )
    return rows


def automated_verdict(issues: list[dict[str, Any]], prs: list[dict[str, Any]]) -> str:
    done = [i for i in issues if "aiops:done" in label_names(i) and str(i.get("state", "")).lower() == "closed"]
    merged = [p for p in prs if p.get("mergedAt") or str(p.get("state", "")).upper() == "MERGED"]
    reworked = any("CHANGES_REQUESTED" in review_states(p) for p in prs)
    if len(done) >= 3 and len(merged) >= 3 and reworked:
        return "READY FOR OPERATOR PASS REVIEW"
    return "INCOMPLETE - review the evidence before claiming PASS"


def asset_bullets(root: Path, glob: str) -> list[str]:
    paths = sorted(root.glob(glob))
    if not paths:
        return ["- none captured"]
    return [f"- `{path.relative_to(root)}`" for path in paths]


def write_main_report(args: argparse.Namespace, issues: list[dict[str, Any]], prs: list[dict[str, Any]]) -> Path:
    reports = args.run_root / "reports"
    reports.mkdir(parents=True, exist_ok=True)
    verdict = automated_verdict(issues, prs)
    lines = [
        "# GitHub maker + reviewer-automerge E2E Report",
        "",
        f"Run root: `{args.run_root}`",
        f"Repository: `{args.repo}`",
        f"Date: {args.date}",
        "",
        "## Verdict",
        "",
        verdict,
        "",
        "This helper summarizes captured machine evidence. The operator still",
        "checks screenshots, raw logs, and live forge state before marking a",
        "release-validation run PASS.",
        "",
        "## Pass Criteria Checklist",
        "",
        "- [ ] Latest release binary downloaded, checksum verified, SBOM captured, and attestation verified.",
        "- [ ] `worker --doctor --deploy=binary --mode=real` passed for maker and reviewer workflows.",
        "- [ ] Maker and reviewer used distinct GitHub identities and distinct workspace roots.",
        "- [ ] Maker opened PRs and handed issues to `aiops:human-review` without review, merge, Done, or close writes.",
        "- [ ] Reviewer did not edit, commit, or push code.",
        "- [ ] At least one PR received reviewer Rework before a new maker head passed.",
        "- [ ] GitHub branch protection required the `build-test` check and an approving review.",
        "- [ ] Reviewer confirmed `mergedAt`/merged state before adding `aiops:done` and closing issues.",
        "- [ ] Dependency issue was activated only after prerequisite issues were Done/closed.",
        "- [ ] Fresh clone verification passed `npm ci`, `npm test`, `npm run build`, and `npm run test:e2e`.",
        "",
        "## Issue / PR Table",
        "",
        "Issues:",
        *issue_rows(issues),
        "",
        "Pull requests:",
        *pr_rows(prs),
        "",
        "## Auto-Merge Evidence",
        "",
        "- Required check and branch-protection JSON: `forge-json/branch-protection-*.json`.",
        "- Actions/check summaries: `forge-json/actions-runs-*.json`.",
        "- PR review/merge actor metadata: `forge-json/prs-*.json` and `forge-json/pr-*-*.json`.",
        "- Durable GitHub evidence is reviewer approval, required check success, reviewer merge actor, and non-empty `mergedAt`.",
        "",
        "## Rework Evidence",
        "",
        "- Rework is present when a PR has a `CHANGES_REQUESTED` review or the issue timeline shows `aiops:rework` before a later passed head.",
        "- Maker must push a new head and include `Rework response:` before handing off again.",
        "- Reviewer may approve only the new reviewed head, ideally with `--match-head-commit` on auto-merge.",
        "",
        "## Screenshot Index",
        "",
        *asset_bullets(args.run_root, "screenshots/*.png"),
        "",
        "Final app screenshots:",
        *asset_bullets(args.run_root, "final-verify/screenshots/*.png"),
        "",
        "## Machine Evidence Index",
        "",
        "- `artifacts/release-view-summary.json`",
        "- `artifacts/sha256.log`",
        "- `artifacts/attestation.log`",
        "- `artifacts/sbom-summary.json`",
        "- `logs/maker-doctor.log`",
        "- `logs/reviewer-doctor.log`",
        "- `logs/maker-worker.log`",
        "- `logs/reviewer-worker.log`",
        "- `state/maker-state-*.json`",
        "- `state/reviewer-state-*.json`",
        "- `forge-json/*.json`",
        "- `final-verify/logs/*.log`",
        "",
        "## Notes",
        "",
        "- Do not commit `env.local`, `secrets/`, GitHub auth homes, downloaded binaries, or npm/browser caches.",
        "- If any required preflight fails, mark the run BLOCKED rather than downgrading to single-agent merge.",
    ]
    path = reports / "report.md"
    path.write_text("\n".join(lines) + "\n")
    return path


def write_retro(args: argparse.Namespace) -> Path:
    lines = [
        "# Merge Mechanism Retro",
        "",
        f"Run root: `{args.run_root}`",
        f"Repository: `{args.repo}`",
        "",
        "## Verdict",
        "",
        "The maker + reviewer-automerge pattern is the production-governance default for this flow. It keeps the worker/orchestrator as scheduler, runner, and tracker reader while GitHub branch protection remains the merge gate.",
        "",
        "## Pattern Comparison",
        "",
        "| Pattern | What it optimizes | Governance strength | Finding |",
        "|---|---|---|---|",
        "| Single-agent agent-side merge | Speed and simplicity | Weak | One agent can implement, judge, and merge its own work. |",
        "| Maker + reviewer-automerge | Separation of duties | Strong | Maker writes and opens PRs; reviewer independently approves, enables CI-gated auto-merge, confirms merged, then closes. |",
        "| Worker/orchestrator merge | Centralized automation | Not recommended | It crosses the aiops-platform boundary; the worker must not become PR writer, merger, or terminal tracker writer. |",
        "",
        "## GitHub Lessons",
        "",
        "- Use distinct bot accounts or users with distinct `GH_CONFIG_DIR` homes.",
        "- Do not pass `GITHUB_TOKEN` to the agent; the worker uses it for tracker reads and denies it from env passthrough.",
        "- Require `build-test` and one approval on `main`, enable repository auto-merge, and use squash-only merges for clean evidence.",
        "- `gh pr merge --auto --squash --delete-branch --match-head-commit <sha>` protects against approving one head and merging another.",
        "- Auto-merge enablement can be transient if checks are already green; durable evidence is reviewer approval, required check success, merge actor, `mergedAt`, and Done-after-merged issue comment.",
        "",
        "## Reusable Assets",
        "",
        "- `examples/github-maker-WORKFLOW.md`",
        "- `examples/github-reviewer-automerge-WORKFLOW.md`",
        "- `docs/runbooks/github-maker-reviewer-automerge-e2e.md`",
        "- `scripts/github-maker-reviewer-e2e-bootstrap.sh`",
        "- `scripts/github-maker-reviewer-release-preflight.sh`",
        "- `scripts/github-maker-reviewer-capture.py`",
        "- `scripts/github-maker-reviewer-final-verify.py`",
        "- `scripts/github-maker-reviewer-report.py`",
    ]
    path = args.run_root / "reports" / "merge-mechanism-retro.md"
    path.write_text("\n".join(lines) + "\n")
    return path


def main() -> int:
    args = parser().parse_args()
    fj = args.run_root / "forge-json"
    state = args.run_root / "state"
    issues = load_first(evidence_candidates(fj, state, "issues"), [])
    prs = load_first(evidence_candidates(fj, state, "prs"), [])
    if not isinstance(issues, list):
        issues = []
    if not isinstance(prs, list):
        prs = []
    report = write_main_report(args, issues, prs)
    retro = write_retro(args)
    print(f"wrote {report}")
    print(f"wrote {retro}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
