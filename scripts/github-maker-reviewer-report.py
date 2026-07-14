#!/usr/bin/env python3
"""Generate GitHub maker/reviewer auto-merge reports from captured JSON."""

from __future__ import annotations

import argparse
import json
import os
import time
from pathlib import Path
from typing import Any


def parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--run-root", required=True, type=Path)
    p.add_argument("--repo", required=True)
    p.add_argument("--reviewer-login", default=os.environ.get("AIOPS_GHMR_REVIEWER_LOGIN", ""))
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


def load_json(path: Path) -> Any:
    try:
        return json.loads(path.read_text())
    except (OSError, json.JSONDecodeError):
        return None


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


def issue_event_candidates(forge_json: Path, issue_number: int | str) -> list[Path]:
    number = str(issue_number)
    preferred = [
        forge_json / f"issue-{number}-events-final.json",
        forge_json / f"final-issue-{number}-events.json",
    ]
    discovered = newest_json(list(forge_json.glob(f"issue-{number}-events-*.json")))
    return preferred + [path for path in discovered if path not in preferred]


def pr_review_candidates(forge_json: Path, pr_number: int | str) -> list[Path]:
    number = str(pr_number)
    if not number:
        return []
    preferred = [
        forge_json / f"pr-{number}-reviews-final.json",
        forge_json / f"final-pr-{number}-reviews.json",
    ]
    discovered = newest_json(list(forge_json.glob(f"pr-{number}-reviews-*.json")))
    return preferred + [path for path in discovered if path not in preferred]


def newest_prefixed_json(directory: Path, prefix: str) -> list[Path]:
    return newest_json(list(directory.glob(f"{prefix}-*.json")))


def latest_branch_protection_artifact(forge_json: Path) -> Path | None:
    final_err = forge_json / "branch-protection-final.err"
    if final_err.exists():
        return None
    final_json = forge_json / "branch-protection-final.json"
    if final_json.exists():
        return final_json
    artifacts = [
        path
        for path in forge_json.glob("branch-protection-*")
        if path.suffix in (".err", ".json")
    ]
    for path in sorted(artifacts, key=lambda item: item.stat().st_mtime, reverse=True):
        if path.suffix == ".err":
            return None
        if path.suffix == ".json":
            return path
    return None


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


def review_author(review: dict[str, Any]) -> str:
    return user_login(review.get("author") or review.get("user"))


def review_commit(review: dict[str, Any]) -> str:
    for key in ("commitOid", "commitId", "commit_id"):
        if review.get(key):
            return str(review.get(key))
    commit = review.get("commit")
    if isinstance(commit, dict):
        return str(commit.get("oid") or commit.get("sha") or "")
    return str(commit or "")


def append_review_records(records: list[dict[str, Any]], value: Any) -> None:
    if isinstance(value, list):
        records.extend(item for item in value if isinstance(item, dict))
    elif isinstance(value, dict):
        for key in ("reviews", "nodes"):
            child = value.get(key)
            if isinstance(child, list):
                records.extend(item for item in child if isinstance(item, dict))


def review_records(pr: dict[str, Any], forge_json: Path) -> list[dict[str, Any]]:
    records: list[dict[str, Any]] = []
    append_review_records(records, pr.get("reviews") or [])
    for path in pr_review_candidates(forge_json, pr.get("number", "")):
        append_review_records(records, load_json(path))
    return records


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


def closed_issue_titles(issues: list[dict[str, Any]]) -> list[str]:
    titles: list[str] = []
    for issue in issues:
        if str(issue.get("state", "")).lower() == "closed":
            titles.append(str(issue.get("title", "")).lower())
    return titles


def required_issue_scenarios_closed(issues: list[dict[str, Any]]) -> bool:
    titles = closed_issue_titles(issues)
    required = ("happy path", "rework candidate", "dependency:")
    return all(any(marker in title for title in titles) for marker in required)


def closed_issue_by_title(issues: list[dict[str, Any]], marker: str) -> dict[str, Any] | None:
    for issue in issues:
        title = str(issue.get("title", "")).lower()
        if marker in title and str(issue.get("state", "")).lower() == "closed":
            return issue
    return None


def event_label_name(event: dict[str, Any]) -> str:
    label = event.get("label")
    if isinstance(label, dict):
        return str(label.get("name") or "")
    return str(label or "")


def event_created_at(event: dict[str, Any]) -> str:
    return str(event.get("created_at") or event.get("createdAt") or "")


def dependency_sequencing_evidence_present(forge_json: Path, issues: list[dict[str, Any]]) -> bool:
    happy = closed_issue_by_title(issues, "happy path")
    rework = closed_issue_by_title(issues, "rework candidate")
    dependency = closed_issue_by_title(issues, "dependency:")
    if not happy or not rework or not dependency:
        return False
    prerequisite_closed_at = max(str(happy.get("closedAt") or ""), str(rework.get("closedAt") or ""))
    if not prerequisite_closed_at:
        return False
    events: list[Any] = []
    for path in issue_event_candidates(forge_json, dependency.get("number", "")):
        loaded = load_json(path)
        if isinstance(loaded, list):
            events = loaded
            break
    todo_label_times = [
        event_created_at(event)
        for event in events
        if isinstance(event, dict) and event.get("event") == "labeled" and event_label_name(event) == "aiops:todo"
    ]
    return bool(todo_label_times) and all(when and when > prerequisite_closed_at for when in todo_label_times)


def merged_prs(prs: list[dict[str, Any]]) -> list[dict[str, Any]]:
    return [p for p in prs if p.get("mergedAt") or str(p.get("state", "")).upper() == "MERGED"]


def reviewer_owned_merges(prs: list[dict[str, Any]], reviewer_login: str) -> bool:
    reviewer = reviewer_login.strip()
    if not reviewer or reviewer.startswith("REPLACE_ME"):
        return False
    merged = merged_prs(prs)
    if len(merged) < 3:
        return False
    for pr in merged:
        author = user_login(pr.get("author"))
        merged_by = user_login(pr.get("mergedBy"))
        if not author or merged_by != reviewer or author == reviewer:
            return False
    return True


def reviewer_approved_merges(prs: list[dict[str, Any]], reviewer_login: str, forge_json: Path) -> bool:
    reviewer = reviewer_login.strip()
    if not reviewer or reviewer.startswith("REPLACE_ME"):
        return False
    merged = merged_prs(prs)
    if not merged:
        return False
    for pr in merged:
        head_sha = str(pr.get("headRefOid") or "")
        if not head_sha:
            return False
        approved = False
        for review in review_records(pr, forge_json):
            if not isinstance(review, dict) or str(review.get("state", "")).upper() != "APPROVED":
                continue
            commit_sha = review_commit(review)
            if review_author(review) == reviewer and commit_sha and commit_sha == head_sha:
                approved = True
                break
        if not approved:
            return False
    return True


def branch_protection_requires_build_test_and_review(forge_json: Path) -> bool:
    path = latest_branch_protection_artifact(forge_json)
    if not path:
        return False
    protection = load_json(path)
    if not isinstance(protection, dict):
        return False
    status_checks = protection.get("required_status_checks") or {}
    contexts = [str(item) for item in status_checks.get("contexts") or []]
    for check in status_checks.get("checks") or []:
        if isinstance(check, dict):
            contexts.append(str(check.get("context") or check.get("name") or ""))
    pull_request_reviews = protection.get("required_pull_request_reviews") or {}
    try:
        review_count = int(pull_request_reviews.get("required_approving_review_count") or 0)
    except (TypeError, ValueError):
        review_count = 0
    return "build-test" in contexts and review_count >= 1


def has_build_test_success(value: Any) -> bool:
    if isinstance(value, dict):
        names = {str(value.get(key) or "") for key in ("name", "context", "workflowName", "displayTitle", "title")}
        states = {str(value.get(key) or "").upper() for key in ("conclusion", "status", "state")}
        if "build-test" in names and "SUCCESS" in states:
            return True
        return any(has_build_test_success(child) for child in value.values())
    if isinstance(value, list):
        return any(has_build_test_success(child) for child in value)
    return False


def actions_runs(forge_json: Path) -> list[dict[str, Any]]:
    runs: list[dict[str, Any]] = []
    for path in newest_prefixed_json(forge_json, "actions-runs"):
        loaded = load_json(path)
        if isinstance(loaded, list):
            runs.extend(item for item in loaded if isinstance(item, dict))
    return runs


def action_run_has_build_test_success(run: dict[str, Any], head_sha: str) -> bool:
    if head_sha and str(run.get("headSha") or "") not in ("", head_sha):
        return False
    names = {str(run.get(key) or "") for key in ("workflowName", "displayTitle", "name", "title")}
    return "build-test" in names and str(run.get("conclusion") or "").upper() == "SUCCESS"


def build_test_success_for_merged_prs(prs: list[dict[str, Any]], forge_json: Path) -> bool:
    merged = merged_prs(prs)
    if not merged:
        return False
    runs = actions_runs(forge_json)
    for pr in merged:
        head_sha = str(pr.get("headRefOid") or "")
        if has_build_test_success(pr.get("statusCheckRollup")):
            continue
        if any(action_run_has_build_test_success(run, head_sha) for run in runs):
            continue
        return False
    return True


FINAL_VERIFY_EXIT_STATUS_MARKER = "AIOPS_FINAL_VERIFY_EXIT_STATUS"


def final_verify_log_succeeded(text: str) -> bool:
    return any(line.strip() == f"{FINAL_VERIFY_EXIT_STATUS_MARKER}: 0" for line in text.splitlines())


def fresh_clone_verification_present(run_root: Path) -> bool:
    logs = run_root / "final-verify" / "logs"
    required_logs = {
        "npm-ci.log": "npm ci",
        "npm-test.log": "npm test",
        "npm-build.log": "npm run build",
        "npm-e2e.log": "npm run test:e2e",
    }
    for name, command in required_logs.items():
        try:
            text = (logs / name).read_text()
        except OSError:
            return False
        if command not in text or "TIMEOUT" in text or not final_verify_log_succeeded(text):
            return False
    return True


def automated_verdict(
    issues: list[dict[str, Any]],
    prs: list[dict[str, Any]],
    sequencing_evidence: bool,
    reviewer_merge_evidence: bool,
    reviewer_approval_evidence: bool,
    branch_protection_evidence: bool,
    build_test_evidence: bool,
    fresh_clone_evidence: bool,
) -> str:
    reworked = any("CHANGES_REQUESTED" in review_states(p) for p in prs)
    reviewer_evidence = reviewer_merge_evidence and reviewer_approval_evidence
    if (
        required_issue_scenarios_closed(issues)
        and reviewer_evidence
        and reworked
        and sequencing_evidence
        and branch_protection_evidence
        and build_test_evidence
        and fresh_clone_evidence
    ):
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
    forge_json = args.run_root / "forge-json"
    sequencing_evidence = dependency_sequencing_evidence_present(forge_json, issues)
    reviewer_merge_evidence = reviewer_owned_merges(prs, args.reviewer_login)
    reviewer_approval_evidence = reviewer_approved_merges(prs, args.reviewer_login, forge_json)
    branch_protection_evidence = branch_protection_requires_build_test_and_review(forge_json)
    build_test_evidence = build_test_success_for_merged_prs(prs, forge_json)
    fresh_clone_evidence = fresh_clone_verification_present(args.run_root)
    verdict = automated_verdict(
        issues,
        prs,
        sequencing_evidence,
        reviewer_merge_evidence,
        reviewer_approval_evidence,
        branch_protection_evidence,
        build_test_evidence,
        fresh_clone_evidence,
    )
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
        "- [ ] Maker opened PRs with non-closing `Refs #N` and handed issues to `aiops:human-review` without review, merge, or close writes.",
        "- [ ] Reviewer did not edit, commit, or push code.",
        "- [ ] At least one PR received reviewer Rework before a new maker head passed.",
        "- [ ] GitHub branch protection required the `build-test` check and an approving review.",
        "- [ ] Closed issues have merged PR, exact-head reviewer approval, and required-check evidence.",
        "- [ ] Dependency issue was activated only after prerequisite issues were closed.",
        "- [ ] Fresh clone verification passed `npm ci`, `npm test`, `npm run build`, and `npm run test:e2e`.",
        "",
        "## Issue / PR Table",
        "",
        f"Dependency sequencing evidence: {'present' if sequencing_evidence else 'missing'}",
        f"Reviewer merge identity evidence: {'matched' if reviewer_merge_evidence else 'missing'}",
        f"Reviewer approval evidence: {'matched' if reviewer_approval_evidence else 'missing'}",
        f"Branch protection evidence: {'present' if branch_protection_evidence else 'missing'}",
        f"Merged PR build-test evidence: {'present' if build_test_evidence else 'missing'}",
        f"Fresh clone verification evidence: {'present' if fresh_clone_evidence else 'missing'}",
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
        "- PR review/merge actor metadata: `forge-json/prs-*.json`, `forge-json/pr-*-*.json`, and `forge-json/pr-*-reviews-*.json`.",
        "- Reviewer approvals must include reviewed-head commit evidence matching each merged `headRefOid`.",
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
        "| Maker + reviewer-automerge | Separation of duties | Strong | Maker opens a non-closing PR; reviewer approval enables a squash commit that closes the issue. |",
        "| Worker/orchestrator merge | Centralized automation | Not recommended | It crosses the aiops-platform boundary; the worker must not become PR writer, merger, or terminal tracker writer. |",
        "",
        "## GitHub Lessons",
        "",
        "- Use distinct bot accounts or users with distinct `GH_CONFIG_DIR` homes.",
        "- Do not pass `GITHUB_TOKEN` to the agent; the worker uses it for tracker reads and denies it from env passthrough.",
        "- Require `build-test` and one approval on `main`, enable repository auto-merge, and use squash-only merges for clean evidence.",
        "- `gh pr merge --auto --squash --delete-branch --match-head-commit <sha> --body \"Closes #<N>\"` pins the head and closes the issue when the squash commit lands.",
        "- Durable evidence is closed issue state, exact-head reviewer approval, required check success, merge actor, and `mergedAt`.",
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
