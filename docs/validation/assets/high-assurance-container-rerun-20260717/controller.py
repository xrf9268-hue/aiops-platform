#!/usr/bin/env python3
"""One-run host controller for issue #1128's frozen Docker experiment."""

from __future__ import annotations

import argparse
from dataclasses import dataclass
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import tempfile
import time
from typing import Any, Mapping, Sequence


STATE_URL = "http://127.0.0.1:4000/api/v1/state"
STATE_SECRET = "/run/secrets/aiops_state_api_token"
STATE_REQUEST_CONFIG = "/run/secrets/state_wgetrc"
ASSET_DIR = Path(__file__).resolve().parent
REPOSITORY = "zjlgdx/aiops-workflow-bench-high-20260717-1128-v1"
MAKER_LOGIN = "xrf-9527"


@dataclass(frozen=True)
class ContainerRef:
    role: str
    container_id: str

    def __post_init__(self) -> None:
        if self.role not in {"maker", "reviewer"}:
            raise ValueError(f"unknown container role: {self.role}")
        if not re.fullmatch(r"[0-9a-f]{64}", self.container_id):
            raise ValueError(
                f"{self.role} container ID must be the full 64-byte hex ID"
            )


@dataclass(frozen=True)
class BindMount:
    name: str
    source: Path
    destination: str
    empty_required: bool
    writable: bool = True


class ForgeObservationError(RuntimeError):
    def __init__(
        self,
        stage_id: str,
        reason: str,
        partial_results: Mapping[str, Any],
    ) -> None:
        super().__init__(f"forge observation {reason} at {stage_id}")
        self.stage_id = stage_id
        self.reason = reason
        self.partial_results = dict(partial_results)


def utc_now() -> str:
    return (
        datetime.now(timezone.utc)
        .isoformat(timespec="microseconds")
        .replace("+00:00", "Z")
    )


def append_event(path: Path, event: Mapping[str, Any]) -> None:
    payload = dict(event)
    payload.setdefault("timestamp", utc_now())
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("a", encoding="utf-8") as stream:
        stream.write(json.dumps(payload, sort_keys=True, separators=(",", ":")) + "\n")
        stream.flush()
        os.fsync(stream.fileno())


def validate_bind_sources(mounts: tuple[BindMount, ...]) -> tuple[BindMount, ...]:
    resolved: list[tuple[BindMount, Path]] = []
    for mount in mounts:
        if not mount.source.exists():
            raise ValueError(f"missing bind source for {mount.name}: {mount.source}")
        if not mount.source.is_dir():
            raise ValueError(
                f"bind source is not a directory for {mount.name}: {mount.source}"
            )
        source = mount.source.resolve(strict=True)
        if mount.empty_required and any(source.iterdir()):
            raise ValueError(f"nonempty bind source for {mount.name}: {source}")
        if not mount.destination.startswith("/"):
            raise ValueError(
                f"bind destination is not absolute for {mount.name}: {mount.destination}"
            )
        resolved.append((mount, source))

    for index, (left_mount, left) in enumerate(resolved):
        for right_mount, right in resolved[index + 1 :]:
            if left == right:
                raise ValueError(
                    f"equal bind sources: {left_mount.name} and {right_mount.name}: {left}"
                )
            if left in right.parents or right in left.parents:
                raise ValueError(
                    f"nested bind sources: {left_mount.name}={left}, {right_mount.name}={right}"
                )
    return mounts


_PROBE_SCRIPT = r"""
set -eu
destination="$1"
[ "$(id -un)" = aiops ]
probe="$destination/.aiops-1128-probe-$$"
payload="aiops-1128-mount-probe"
trap 'rm -f "$probe"' EXIT HUP INT TERM
printf '%s' "$payload" | dd of="$probe" conv=fsync status=none
actual="$(cat "$probe")"
[ "$actual" = "$payload" ]
rm -f "$probe"
[ ! -e "$probe" ]
trap - EXIT HUP INT TERM
printf '%s\t%s\t%s\t%s\n' "$(id -un)" "$(id -u)" "$(id -g)" "$destination"
""".strip()


def inspect_and_probe_mounts(
    container: ContainerRef,
    mounts: tuple[BindMount, ...],
    *,
    timeout_seconds: float,
) -> dict[str, Any]:
    inspected = subprocess.run(
        ["docker", "inspect", container.container_id],
        capture_output=True,
        text=True,
        timeout=timeout_seconds,
        check=False,
    )
    if inspected.returncode != 0:
        raise RuntimeError(
            f"docker inspect failed for {container.role}: {inspected.stderr.strip()}"
        )
    try:
        rows = json.loads(inspected.stdout)
        detail = rows[0]
    except (json.JSONDecodeError, IndexError, TypeError) as error:
        raise ValueError(
            f"invalid docker inspect payload for {container.role}"
        ) from error
    if detail.get("Id") != container.container_id:
        raise ValueError(f"docker inspect returned wrong ID for {container.role}")

    config_env = (detail.get("Config") or {}).get("Env") or []
    configured_names = {entry.split("=", 1)[0] for entry in config_env}
    forbidden_names = {
        "GITHUB_TOKEN",
        "GH_TOKEN",
        "AIOPS_TRACKER_SECRET",
        "AIOPS_STATE_API_TOKEN",
    }
    exposed = sorted(configured_names & forbidden_names)
    if exposed:
        raise ValueError(
            f"secret-bearing names present in Docker Config.Env: {exposed}"
        )

    bindings = (detail.get("HostConfig") or {}).get("PortBindings") or {}
    if bindings.get("4000/tcp"):
        raise ValueError(f"state API port is published for {container.role}")

    actual_mounts = detail.get("Mounts") or []
    mount_proof: list[dict[str, Any]] = []
    for expected in mounts:
        matches = [
            row
            for row in actual_mounts
            if row.get("Destination") == expected.destination
        ]
        if len(matches) != 1:
            raise ValueError(
                f"expected one {container.role} mount at {expected.destination}; got {len(matches)}"
            )
        actual = matches[0]
        if actual.get("Type") != "bind":
            raise ValueError(f"{container.role} {expected.name} mount is not a bind")
        if Path(str(actual.get("Source", ""))).resolve() != expected.source.resolve(
            strict=True
        ):
            raise ValueError(f"{container.role} {expected.name} bind source mismatch")
        if actual.get("RW") is not expected.writable:
            raise ValueError(
                f"{container.role} {expected.name} bind RW={actual.get('RW')!r}; "
                f"want {expected.writable}"
            )
        mount_proof.append(
            {
                "name": expected.name,
                "source": str(expected.source.resolve(strict=True)),
                "destination": expected.destination,
                "rw": expected.writable,
            }
        )

    probes: list[dict[str, Any]] = []
    for expected in (mount for mount in mounts if mount.writable):
        args = [
            "docker",
            "exec",
            container.container_id,
            "sh",
            "-ceu",
            _PROBE_SCRIPT,
            "--",
            expected.destination,
        ]
        result = subprocess.run(
            args,
            capture_output=True,
            text=True,
            timeout=timeout_seconds,
            check=False,
        )
        if result.returncode != 0:
            raise RuntimeError(
                f"{container.role} {expected.name} write/fsync/read/delete probe failed: "
                f"{result.stderr.strip()}"
            )
        fields = result.stdout.rstrip("\n").split("\t")
        if (
            len(fields) != 4
            or fields[0] != "aiops"
            or fields[3] != expected.destination
        ):
            raise ValueError(
                f"invalid {container.role} {expected.name} probe identity/result"
            )
        probes.append(
            {
                "name": expected.name,
                "destination": expected.destination,
                "user": fields[0],
                "uid": int(fields[1]),
                "gid": int(fields[2]),
                "write_fsync_read_delete": "passed",
            }
        )
    return {
        "role": container.role,
        "container_id": container.container_id,
        "forbidden_config_env_absent": sorted(forbidden_names),
        "state_api_host_port_published": False,
        "mounts": mount_proof,
        "probes": probes,
    }


_UNAUTH_STATE_SCRIPT = rf"""
set -eu
headers="$(mktemp)"
trap 'rm -f "$headers"' EXIT HUP INT TERM
if wget --timeout=2 --tries=1 --server-response -O /dev/null {STATE_URL} 2>"$headers"; then
  printf 'HTTP/1.1 200 OK\n'
  exit 0
fi
if grep -Eq 'HTTP/[0-9.]+ 401' "$headers"; then
  printf 'HTTP/1.1 401 Unauthorized\n'
  exit 0
fi
cat "$headers" >&2
exit 1
""".strip()

_STATE_SECRET_MATCH_SCRIPT = rf"""
set -eu
token="$(cat {STATE_SECRET})"
expected="header = Authorization: Bearer $token"
[ "$(cat {STATE_REQUEST_CONFIG})" = "$expected" ]
""".strip()


def _validate_state_schema(state: Mapping[str, Any]) -> None:
    totals = state.get("codex_totals")
    if not isinstance(totals, dict):
        raise ValueError("state schema requires codex_totals object")
    total_tokens = totals.get("total_tokens")
    if (
        isinstance(total_tokens, bool)
        or not isinstance(total_tokens, int)
        or total_tokens < 0
    ):
        raise ValueError(
            "state schema requires non-negative codex_totals.total_tokens integer"
        )

    claim_fields = ("completed_session_usage", "running", "blocked")
    for field in (*claim_fields, "retrying"):
        rows = state.get(field)
        if not isinstance(rows, list):
            raise ValueError(f"state schema requires {field} array")
        for index, row in enumerate(rows):
            if not isinstance(row, dict):
                raise ValueError(f"state schema requires {field}[{index}] object")
            if not re.fullmatch(r"#\d+", str(row.get("issue_identifier", ""))):
                raise ValueError(
                    f"state schema requires {field}[{index}].issue_identifier"
                )
            if field in claim_fields:
                tokens = row.get("tokens")
                row_total = (
                    tokens.get("total_tokens") if isinstance(tokens, dict) else None
                )
                if (
                    isinstance(row_total, bool)
                    or not isinstance(row_total, int)
                    or row_total < 0
                ):
                    raise ValueError(
                        f"state schema requires non-negative {field}[{index}].tokens.total_tokens"
                    )


def read_state_via_exec(
    container: ContainerRef,
    *,
    timeout_seconds: float,
) -> dict[str, Any]:
    args = [
        "docker",
        "exec",
        container.container_id,
        "/usr/bin/timeout",
        "--signal=TERM",
        "--kill-after=1s",
        "5s",
        "/usr/bin/wget",
        f"--config={STATE_REQUEST_CONFIG}",
        "--timeout=3",
        "--tries=1",
        "--quiet",
        "--output-document=-",
        STATE_URL,
    ]
    result = subprocess.run(
        args,
        capture_output=True,
        text=True,
        timeout=timeout_seconds,
        check=False,
    )
    if result.returncode != 0:
        raise RuntimeError(
            f"authenticated state read failed for {container.role}: {result.stderr.strip()}"
        )
    if len(result.stdout.encode("utf-8")) > 4 * 1024 * 1024:
        raise ValueError(f"state API response exceeded 4 MiB for {container.role}")
    try:
        state = json.loads(result.stdout)
    except json.JSONDecodeError as error:
        raise ValueError(
            f"state API returned invalid JSON for {container.role}"
        ) from error
    if not isinstance(state, dict):
        raise ValueError(f"state API returned non-object JSON for {container.role}")
    _validate_state_schema(state)
    return state


def prove_state_exec_boundary(
    container: ContainerRef,
    *,
    timeout_seconds: float,
) -> dict[str, Any]:
    secret_match = subprocess.run(
        [
            "docker",
            "exec",
            container.container_id,
            "sh",
            "-ceu",
            _STATE_SECRET_MATCH_SCRIPT,
        ],
        capture_output=True,
        text=True,
        timeout=timeout_seconds,
        check=False,
    )
    if secret_match.returncode != 0:
        raise RuntimeError(f"secret request config mismatch for {container.role}")
    args = [
        "docker",
        "exec",
        container.container_id,
        "sh",
        "-ceu",
        _UNAUTH_STATE_SCRIPT,
    ]
    result = subprocess.run(
        args,
        capture_output=True,
        text=True,
        timeout=timeout_seconds,
        check=False,
    )
    if result.returncode != 0:
        raise RuntimeError(
            f"unauthenticated state control failed for {container.role}: {result.stderr.strip()}"
        )
    match = re.search(r"HTTP/[0-9.]+\s+(\d{3})", result.stdout)
    status = int(match.group(1)) if match else None
    if status != 401:
        raise ValueError(
            f"unauthenticated state control for {container.role} returned {status}; want 401"
        )
    state = read_state_via_exec(container, timeout_seconds=timeout_seconds)
    return {
        "role": container.role,
        "container_id": container.container_id,
        "transport": "bounded docker exec to container loopback",
        "state_url": STATE_URL,
        "token_secret_path": STATE_SECRET,
        "request_config_secret_path": STATE_REQUEST_CONFIG,
        "unauthenticated_status": 401,
        "authenticated_read": "passed",
        "state": state,
    }


_PR_FIELDS = ",".join(
    (
        "number",
        "state",
        "isDraft",
        "author",
        "headRefOid",
        "baseRefOid",
        "baseRefName",
        "headRefName",
        "mergedAt",
        "mergeStateStatus",
        "mergeable",
        "autoMergeRequest",
        "statusCheckRollup",
        "url",
        "updatedAt",
    )
)

_THREAD_QUERY = """
query($owner:String!,$name:String!,$number:Int!,$endCursor:String) {
  repository(owner:$owner,name:$name) {
    pullRequest(number:$number) {
      reviewThreads(first:100,after:$endCursor) {
        nodes {
          id isResolved isOutdated path line originalLine
          comments(first:100) {
            nodes { id author { login } body createdAt updatedAt url }
            pageInfo { hasNextPage endCursor }
          }
        }
        pageInfo { hasNextPage endCursor }
      }
    }
  }
}
""".strip()


def _flatten_pages(value: Any) -> Any:
    if (
        isinstance(value, list)
        and value
        and all(isinstance(item, list) for item in value)
    ):
        return [entry for page in value for entry in page]
    return value


def _newest_pr_number(comments: Any, repo: str) -> int | None:
    if not isinstance(comments, list):
        return None
    pattern = re.compile(rf"https://github\.com/{re.escape(repo)}/pull/(\d+)")
    ordered = sorted(
        (
            item
            for item in comments
            if isinstance(item, dict)
            and isinstance(item.get("user"), dict)
            and item["user"].get("login") == MAKER_LOGIN
        ),
        key=lambda item: str(item.get("created_at", "")),
    )
    for comment in reversed(ordered):
        matches = pattern.findall(str(comment.get("body", "")))
        if matches:
            return int(matches[-1])
    return None


def _review_thread_comments_complete(value: Any) -> bool:
    pages = value if isinstance(value, list) else [value]
    for page in pages:
        try:
            threads = page["data"]["repository"]["pullRequest"]["reviewThreads"]
            nodes = threads["nodes"]
            outer_page = threads["pageInfo"]
        except (KeyError, TypeError):
            return False
        if not isinstance(nodes, list) or not isinstance(outer_page, dict):
            return False
        for thread in nodes:
            comments = thread.get("comments") if isinstance(thread, dict) else None
            if not isinstance(comments, dict) or not isinstance(
                comments.get("nodes"), list
            ):
                return False
            page_info = comments.get("pageInfo")
            if (
                not isinstance(page_info, dict)
                or page_info.get("hasNextPage") is not False
            ):
                return False
    return True


def _forge_command(
    stage: str,
    *,
    gh_binary: str,
    repo: str,
    issue_number: int,
    pr_number: int | None,
) -> list[str]:
    if stage == "01-issue":
        return [gh_binary, "api", f"repos/{repo}/issues/{issue_number}"]
    if stage == "02-issue-comments":
        return [
            gh_binary,
            "api",
            "--paginate",
            "--slurp",
            f"repos/{repo}/issues/{issue_number}/comments",
        ]
    if pr_number is None:
        raise ValueError(f"{stage} requires a PR number")
    if stage == "03-pr":
        return [
            gh_binary,
            "pr",
            "view",
            str(pr_number),
            "--repo",
            repo,
            "--json",
            _PR_FIELDS,
        ]
    if stage == "04-pr-comments":
        return [
            gh_binary,
            "api",
            "--paginate",
            "--slurp",
            f"repos/{repo}/issues/{pr_number}/comments",
        ]
    if stage == "05-reviews":
        return [
            gh_binary,
            "api",
            "--paginate",
            "--slurp",
            f"repos/{repo}/pulls/{pr_number}/reviews",
        ]
    if stage == "06-review-threads":
        owner, name = repo.split("/", 1)
        return [
            gh_binary,
            "api",
            "graphql",
            "--paginate",
            "--slurp",
            "-f",
            f"query={_THREAD_QUERY}",
            "-F",
            f"owner={owner}",
            "-F",
            f"name={name}",
            "-F",
            f"number={pr_number}",
        ]
    raise ValueError(f"unknown forge stage: {stage}")


def _stop_stage_process(
    proc: subprocess.Popen[Any], cleanup_grace_seconds: float
) -> None:
    try:
        os.killpg(proc.pid, signal.SIGTERM)
    except ProcessLookupError:
        pass
    try:
        proc.wait(timeout=cleanup_grace_seconds)
        return
    except subprocess.TimeoutExpired:
        pass
    try:
        os.killpg(proc.pid, signal.SIGKILL)
    except ProcessLookupError:
        pass
    proc.wait(timeout=cleanup_grace_seconds)


def observe_forge_stages(
    *,
    repo: str,
    issue_number: int,
    poll_id: str,
    gh_config_dir: Path,
    event_log: Path,
    timeout_seconds: float,
    gh_binary: str = "gh",
    env: Mapping[str, str] | None = None,
) -> dict[str, Any]:
    process_env = dict(os.environ if env is None else env)
    process_env["GH_CONFIG_DIR"] = str(gh_config_dir)
    for secret_env in (
        "GH_TOKEN",
        "GITHUB_TOKEN",
        "AIOPS_BENCH_MAKER_GITHUB_TOKEN",
        "AIOPS_BENCH_REVIEWER_GITHUB_TOKEN",
    ):
        process_env.pop(secret_env, None)

    partial: dict[str, Any] = {}
    pr_number: int | None = None
    stages = (
        ("01-issue", "issue"),
        ("02-issue-comments", "issue_comments"),
        ("03-pr", "pr"),
        ("04-pr-comments", "pr_comments"),
        ("05-reviews", "reviews"),
        ("06-review-threads", "review_threads"),
    )
    for stage, result_key in stages:
        if stage == "03-pr" and pr_number is None:
            break
        stage_id = f"{poll_id}/{stage}"
        command = _forge_command(
            stage,
            gh_binary=gh_binary,
            repo=repo,
            issue_number=issue_number,
            pr_number=pr_number,
        )
        append_event(
            event_log,
            {"event": "forge_stage_started", "poll_id": poll_id, "stage_id": stage_id},
        )
        reason: str | None = None
        stderr_text = ""
        returncode: int | None = None
        process_reaped = False
        with (
            tempfile.TemporaryFile() as stdout_file,
            tempfile.TemporaryFile() as stderr_file,
        ):
            proc = subprocess.Popen(
                command,
                stdout=stdout_file,
                stderr=stderr_file,
                env=process_env,
                start_new_session=True,
            )
            try:
                returncode = proc.wait(timeout=timeout_seconds)
            except subprocess.TimeoutExpired:
                reason = "timeout"
                _stop_stage_process(proc, min(1.0, max(0.1, timeout_seconds)))
                returncode = proc.returncode
            process_reaped = proc.poll() is not None
            stdout_file.seek(0)
            stderr_file.seek(0)
            stdout_text = stdout_file.read().decode("utf-8", errors="replace")
            stderr_text = stderr_file.read().decode("utf-8", errors="replace")

        if reason is None and returncode != 0:
            reason = "exit"
        parsed: Any = None
        if reason is None:
            try:
                parsed = json.loads(stdout_text)
            except json.JSONDecodeError:
                reason = "decode"
        if reason is None:
            parsed = _flatten_pages(parsed)
            if stage == "03-pr" and (
                not isinstance(parsed, dict)
                or not isinstance(parsed.get("author"), dict)
                or parsed["author"].get("login") != MAKER_LOGIN
            ):
                reason = "identity"
                stderr_text = "PR author is not the frozen maker identity"
            elif stage == "06-review-threads" and not _review_thread_comments_complete(
                parsed
            ):
                reason = "incomplete"
                stderr_text = "review-thread schema incomplete or nested comments require pagination"
        if reason is not None:
            append_event(
                event_log,
                {
                    "event": "forge_stage_failed",
                    "poll_id": poll_id,
                    "stage_id": stage_id,
                    "reason": reason,
                    "exit_code": returncode,
                    "stderr": stderr_text[-4000:],
                    "partial_results": partial,
                    "process_reaped": process_reaped,
                    "controller_processes_active": 0 if process_reaped else 1,
                },
            )
            raise ForgeObservationError(stage_id, reason, partial)
        partial[result_key] = parsed
        append_event(
            event_log,
            {
                "event": "forge_stage_completed",
                "poll_id": poll_id,
                "stage_id": stage_id,
                "result": parsed,
                "process_reaped": process_reaped,
            },
        )
        if stage == "02-issue-comments":
            pr_number = _newest_pr_number(parsed, repo)

    append_event(
        event_log,
        {"event": "forge_poll_completed", "poll_id": poll_id, "snapshot": partial},
    )
    return partial


def _row_issue_number(row: Mapping[str, Any]) -> int | None:
    match = re.fullmatch(r"#(\d+)", str(row.get("issue_identifier", "")))
    return int(match.group(1)) if match else None


def _row_tokens(row: Mapping[str, Any]) -> int:
    tokens = row.get("tokens") or {}
    return int(tokens.get("total_tokens") or 0)


def summarize_issue_usage(
    states: Mapping[str, Mapping[str, Any]],
    *,
    issue_number: int,
    baseline_totals: Mapping[str, int],
) -> dict[str, Any]:
    claim_rows: list[dict[str, Any]] = []
    retrying_count = 0
    active_issue_numbers: set[int] = set()
    worker_total_delta = 0
    counter_regressions: list[str] = []
    for role, state in states.items():
        current_total = int((state.get("codex_totals") or {}).get("total_tokens") or 0)
        delta = current_total - int(baseline_totals[role])
        worker_total_delta += delta
        if delta < 0:
            counter_regressions.append(role)
        for field in ("completed_session_usage", "running", "blocked"):
            for raw_row in state.get(field) or []:
                row = dict(raw_row)
                row_issue = _row_issue_number(row)
                if row_issue == issue_number:
                    claim_rows.append({"role": role, "source": field, **row})
                if field in {"running", "blocked"} and row_issue is not None:
                    active_issue_numbers.add(row_issue)
        for raw_row in state.get("retrying") or []:
            row_issue = _row_issue_number(raw_row)
            if row_issue == issue_number:
                retrying_count += 1
            if row_issue is not None:
                active_issue_numbers.add(row_issue)
    attributed = sum(_row_tokens(row) for row in claim_rows)
    return {
        "issue_number": issue_number,
        "claim_count": len(claim_rows),
        "claim_rows": claim_rows,
        "retrying_count": retrying_count,
        "active_issue_numbers": sorted(active_issue_numbers),
        "worker_total_delta": worker_total_delta,
        "issue_attributed_tokens": attributed,
        "accounting_matches": worker_total_delta == attributed,
        "token_counter_regressions": counter_regressions,
    }


def detect_limit_breach(
    usage: Mapping[str, Any],
    *,
    wall_seconds: float,
    external_evidence: Mapping[str, Any],
) -> dict[str, Any] | None:
    if usage.get("token_counter_regressions"):
        return {
            "reason": "token_counter_regression",
            "limit": "monotonic worker token totals",
            "observed": usage["token_counter_regressions"],
        }
    if not usage.get("accounting_matches"):
        return {
            "reason": "token_accounting_mismatch",
            "limit": "process delta equals issue-attributed usage",
            "observed": {
                "worker_total_delta": usage.get("worker_total_delta"),
                "issue_attributed_tokens": usage.get("issue_attributed_tokens"),
            },
        }
    issue_number = int(usage["issue_number"])
    unexpected = [
        number
        for number in usage.get("active_issue_numbers", [])
        if number != issue_number
    ]
    if unexpected:
        return {
            "reason": "unexpected_active_issue",
            "limit": "one preregistered issue at a time",
            "observed": unexpected,
        }
    claim_count = int(usage.get("claim_count") or 0)
    if claim_count > 4:
        return {"reason": "claim_limit", "limit": 4, "observed": claim_count}
    if claim_count == 4 and int(usage.get("retrying_count") or 0) > 0:
        return {
            "reason": "claim_five_pending",
            "limit": 4,
            "observed": {"claims": 4, "retrying": usage["retrying_count"]},
        }
    tokens = int(usage.get("worker_total_delta") or 0)
    if tokens > 3_500_000:
        return {"reason": "token_limit", "limit": 3_500_000, "observed": tokens}
    if external_evidence.get("breach"):
        return dict(external_evidence["breach"])
    if wall_seconds > 1_800:
        return {"reason": "wall_limit", "limit": 1_800, "observed": wall_seconds}
    return None


def _parse_timestamp(value: str) -> datetime:
    return datetime.fromisoformat(value.replace("Z", "+00:00"))


def evaluate_external_review(
    snapshot: Mapping[str, Any],
    seen_triggers: dict[str, dict[str, Any]],
    *,
    now: str,
) -> dict[str, Any]:
    pr = snapshot.get("pr") or {}
    head = str(pr.get("headRefOid") or "")
    base = str(pr.get("baseRefOid") or "")
    base_name = str(pr.get("baseRefName") or "")
    if not head or not base or not base_name:
        return {"triggers": [], "reliable_signal": False, "breach": None}
    tuple_key = f"{head}:{base}:{base_name}"

    comments = snapshot.get("pr_comments") or []
    for comment in comments:
        if not isinstance(comment, dict):
            continue
        user = comment.get("user") or {}
        body = str(comment.get("body") or "")
        if user.get("login") != "zjlgdx" or not re.search(
            r"(^|\s)@codex\s+review\b", body
        ):
            continue
        comment_id = str(comment.get("id") or comment.get("node_id") or "")
        created_at = str(comment.get("created_at") or "")
        if comment_id and created_at and comment_id not in seen_triggers:
            seen_triggers[comment_id] = {
                "comment_id": comment_id,
                "tuple": tuple_key,
                "headRefOid": head,
                "baseRefOid": base,
                "baseRefName": base_name,
                "created_at": created_at,
            }

    triggers = [
        record for record in seen_triggers.values() if record["tuple"] == tuple_key
    ]
    triggers.sort(key=lambda record: record["created_at"])
    if len(triggers) > 1:
        return {
            "triggers": triggers,
            "reliable_signal": False,
            "breach": {
                "reason": "duplicate_exact_tuple_trigger",
                "limit": 1,
                "observed": len(triggers),
            },
        }
    if not triggers:
        return {"triggers": [], "reliable_signal": False, "breach": None}

    trigger = triggers[0]
    expected_checkpoint = (
        f"Reviewer checkpoint: headRefOid={head} baseRefOid={base} "
        f"baseRefName={base_name} local-rubric=PASS"
    )
    reviews = [item for item in snapshot.get("reviews") or [] if isinstance(item, dict)]
    trigger_time = _parse_timestamp(trigger["created_at"])
    checkpoint_found = False
    for review in reviews:
        user = review.get("user") or {}
        submitted_at = str(review.get("submitted_at") or "")
        if (
            user.get("login") == "zjlgdx"
            and review.get("state") == "COMMENTED"
            and review.get("body") == expected_checkpoint
            and review.get("commit_id") == head
            and submitted_at
            and _parse_timestamp(submitted_at) < trigger_time
        ):
            checkpoint_found = True
            break
    if not checkpoint_found:
        return {
            "triggers": triggers,
            "reliable_signal": False,
            "breach": {
                "reason": "trigger_without_checkpoint",
                "limit": "checkpoint strictly precedes trigger",
                "observed": trigger,
            },
        }

    reliable_reviews = []
    for review in reviews:
        user = review.get("user") or {}
        submitted_at = str(review.get("submitted_at") or "")
        if (
            user.get("id") == 199175422
            and review.get("commit_id") == head
            and submitted_at
            and _parse_timestamp(submitted_at) >= trigger_time
        ):
            reliable_reviews.append(review)
    if reliable_reviews:
        return {
            "triggers": triggers,
            "reliable_signal": True,
            "reliable_reviews": reliable_reviews,
            "breach": None,
        }
    elapsed = (_parse_timestamp(now) - trigger_time).total_seconds()
    if elapsed > 600:
        return {
            "triggers": triggers,
            "reliable_signal": False,
            "breach": {
                "reason": "external_review_timeout",
                "limit": 600,
                "observed": elapsed,
            },
        }
    return {
        "triggers": triggers,
        "reliable_signal": False,
        "elapsed_seconds": elapsed,
        "breach": None,
    }


def _best_effort_kill(
    containers: Sequence[ContainerRef], timeout_seconds: float
) -> None:
    for container in containers:
        try:
            subprocess.run(
                ["docker", "kill", container.container_id],
                capture_output=True,
                text=True,
                timeout=timeout_seconds,
                check=False,
            )
        except (OSError, subprocess.TimeoutExpired):
            pass


def stop_both_and_prove(
    maker: ContainerRef,
    reviewer: ContainerRef,
    event_log: Path,
    first_event: Mapping[str, Any],
    *,
    grace_seconds: int,
    timeout_seconds: float,
) -> list[dict[str, Any]]:
    containers = (maker, reviewer)
    processes: list[tuple[ContainerRef, subprocess.Popen[Any]]] = []
    requests: list[dict[str, Any]] = []
    try:
        append_event(event_log, first_event)
        for container in containers:
            requested_at = utc_now()
            proc = subprocess.Popen(
                [
                    "docker",
                    "stop",
                    "--signal",
                    "TERM",
                    "--timeout",
                    str(grace_seconds),
                    container.container_id,
                ],
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                text=True,
            )
            processes.append((container, proc))
            requests.append(
                {
                    "role": container.role,
                    "container_id": container.container_id,
                    "requested_at": requested_at,
                }
            )
        append_event(event_log, {"event": "stop_requests", "requests": requests})

        for container, proc in processes:
            try:
                _, stderr = proc.communicate(timeout=timeout_seconds)
            except subprocess.TimeoutExpired as error:
                proc.kill()
                proc.communicate()
                raise RuntimeError(
                    f"docker stop timed out for {container.role}"
                ) from error
            if proc.returncode != 0:
                raise RuntimeError(
                    f"docker stop failed for {container.role}: {stderr.strip()}"
                )
    except Exception as error:
        _best_effort_kill(containers, timeout_seconds)
        try:
            append_event(event_log, {"event": "stop_failed", "error": str(error)})
        except OSError:
            pass
        raise

    proof: list[dict[str, Any]] = []
    for container in containers:
        waited = subprocess.run(
            ["docker", "wait", container.container_id],
            capture_output=True,
            text=True,
            timeout=timeout_seconds,
            check=False,
        )
        if waited.returncode != 0:
            raise RuntimeError(
                f"docker wait failed for {container.role}: {waited.stderr.strip()}"
            )
        try:
            wait_exit = int(waited.stdout.strip())
        except ValueError as error:
            raise ValueError(
                f"invalid docker wait output for {container.role}"
            ) from error

        inspected = subprocess.run(
            ["docker", "inspect", container.container_id],
            capture_output=True,
            text=True,
            timeout=timeout_seconds,
            check=False,
        )
        if inspected.returncode != 0:
            raise RuntimeError(f"terminal docker inspect failed for {container.role}")
        try:
            detail = json.loads(inspected.stdout)[0]
            state = detail["State"]
        except (json.JSONDecodeError, IndexError, KeyError, TypeError) as error:
            raise ValueError(
                f"invalid terminal inspect payload for {container.role}"
            ) from error
        if detail.get("Id") != container.container_id:
            raise ValueError(f"terminal inspect returned wrong ID for {container.role}")
        if state.get("Running") is not False:
            raise ValueError(f"terminal inspect says {container.role} is still running")
        if state.get("Pid") != 0:
            raise ValueError(f"terminal inspect says {container.role} PID is not zero")
        if state.get("ExitCode") != wait_exit:
            raise ValueError(f"docker wait/inspect exit mismatch for {container.role}")
        terminal = {
            "role": container.role,
            "container_id": container.container_id,
            "docker_wait_exit": wait_exit,
            "running": False,
            "pid": 0,
            "inspect_exit_code": state["ExitCode"],
            "terminal_at": utc_now(),
            "proof_boundary": "exact container ID",
        }
        proof.append(terminal)
        append_event(event_log, {"event": "container_terminal", **terminal})
    return proof


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def write_json(path: Path, value: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + ".tmp")
    descriptor = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o644)
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
            json.dump(value, stream, indent=2, sort_keys=True)
            stream.write("\n")
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
        os.chmod(path, 0o644)
    except Exception:
        temporary.unlink(missing_ok=True)
        raise


def _clean_env(**updates: str) -> dict[str, str]:
    env = dict(os.environ)
    for name in (
        "GH_TOKEN",
        "GITHUB_TOKEN",
        "AIOPS_TRACKER_SECRET",
        "AIOPS_STATE_API_TOKEN",
        "AIOPS_BENCH_MAKER_GITHUB_TOKEN",
        "AIOPS_BENCH_REVIEWER_GITHUB_TOKEN",
    ):
        env.pop(name, None)
    env.update(updates)
    return env


def _run_checked(
    args: Sequence[str],
    *,
    timeout_seconds: float = 30,
    env: Mapping[str, str] | None = None,
) -> str:
    result = subprocess.run(
        list(args),
        capture_output=True,
        text=True,
        timeout=timeout_seconds,
        env=None if env is None else dict(env),
        check=False,
    )
    if result.returncode != 0:
        raise RuntimeError(
            f"command failed ({result.returncode}): {args[0]}: {result.stderr.strip()}"
        )
    return result.stdout


def _gh_json(config_dir: Path, args: Sequence[str]) -> Any:
    output = _run_checked(
        ["gh", *args],
        timeout_seconds=30,
        env=_clean_env(GH_CONFIG_DIR=str(config_dir)),
    )
    try:
        return json.loads(output)
    except json.JSONDecodeError as error:
        raise ValueError(f"gh returned invalid JSON for {args[0]}") from error


def _role_mounts(runtime: Mapping[str, Any], role: str) -> tuple[BindMount, ...]:
    role_paths = runtime["roles"][role]
    return (
        BindMount("workspace", Path(role_paths["workspace"]), "/workspaces", True),
        BindMount("mirror", Path(role_paths["mirror"]), "/mirrors", True),
        BindMount(
            "codex_home", Path(role_paths["codex_home"]), "/home/aiops/.codex", False
        ),
        BindMount(
            "home_config", Path(role_paths["gh_config"]), "/home/aiops/.config", False
        ),
    )


def _role_secret_mounts(runtime: Mapping[str, Any], role: str) -> tuple[BindMount, ...]:
    role_paths = runtime["roles"][role]
    mounts = (
        BindMount(
            "github_token",
            Path(role_paths["github_token"]),
            "/run/secrets/github_token",
            False,
            writable=False,
        ),
        BindMount(
            "state_token",
            Path(role_paths["state_token"]),
            STATE_SECRET,
            False,
            writable=False,
        ),
        BindMount(
            "state_wgetrc",
            Path(role_paths["state_wgetrc"]),
            STATE_REQUEST_CONFIG,
            False,
            writable=False,
        ),
    )
    for mount in mounts:
        if not mount.source.is_file() or mount.source.stat().st_mode & 0o777 != 0o600:
            raise ValueError(
                f"{role} secret source is not a 0600 regular file: {mount.name}"
            )
    return mounts


def _wait_for_state(
    container: ContainerRef, timeout_seconds: float = 60
) -> dict[str, Any]:
    deadline = time.monotonic() + timeout_seconds
    last_error: Exception | None = None
    while time.monotonic() < deadline:
        try:
            return prove_state_exec_boundary(container, timeout_seconds=7)
        except (OSError, RuntimeError, ValueError, subprocess.TimeoutExpired) as error:
            last_error = error
            time.sleep(1)
    raise RuntimeError(
        f"state API did not become ready for {container.role}: {last_error}"
    )


def _require_quiescent(state_proof: Mapping[str, Any]) -> None:
    state = state_proof["state"]
    for field in ("running", "retrying", "blocked", "completed_session_usage"):
        if state.get(field):
            raise ValueError(
                f"{state_proof['role']} is not fresh/quiescent: {field} is non-empty"
            )
    if int((state.get("codex_totals") or {}).get("total_tokens") or 0) != 0:
        raise ValueError(f"{state_proof['role']} has a non-zero token baseline")
    if state.get("version") != "v0.1.16":
        raise ValueError(
            f"{state_proof['role']} worker version is {state.get('version')!r}"
        )
    if state.get("agent_default") != "codex-app-server":
        raise ValueError(f"{state_proof['role']} agent_default drifted")
    if (
        state.get("max_concurrent_agents") != 1
        or state.get("poll_interval_ms") != 30000
    ):
        raise ValueError(f"{state_proof['role']} concurrency/poll profile drifted")


_TIMEOUT_GH_SCRIPT = rf"""#!/bin/sh
set -eu
joined="$*"
case "$joined" in
  *"/issues/1/comments"*)
    printf '%s\n' '[{{"user":{{"login":"{MAKER_LOGIN}"}},"body":"https://github.com/{REPOSITORY}/pull/7","created_at":"2026-07-17T00:00:00Z"}}]'
    ;;
  *"/issues/1"*)
    printf '%s\n' '{{"number":1,"state":"open"}}'
    ;;
  *"pr view 7"*)
    trap '' TERM HUP
    while :; do sleep 30; done
    ;;
  *)
    printf '%s\n' '[]'
    ;;
esac
"""


def _forge_timeout_injection(runtime_root: Path, event_log: Path) -> dict[str, Any]:
    fake_gh = runtime_root / "forge-timeout-gh"
    fake_gh.write_text(_TIMEOUT_GH_SCRIPT, encoding="utf-8")
    fake_gh.chmod(0o700)
    try:
        observe_forge_stages(
            repo=REPOSITORY,
            issue_number=1,
            poll_id="preactivation-timeout-injection",
            gh_config_dir=runtime_root,
            event_log=event_log,
            timeout_seconds=2,
            gh_binary=str(fake_gh),
            env=_clean_env(),
        )
    except ForgeObservationError as error:
        if (
            error.reason != "timeout"
            or error.stage_id != "preactivation-timeout-injection/03-pr"
        ):
            raise ValueError(
                f"forge timeout injection failed at unexpected boundary: {error}"
            ) from error
        proof = {
            "stage_id": error.stage_id,
            "reason": error.reason,
            "partial_keys": sorted(error.partial_results),
            "controller_processes_active": 0,
        }
        append_event(event_log, {"event": "forge_timeout_injection_passed", **proof})
        return proof
    raise ValueError("forge timeout injection unexpectedly completed")


_TERM_RESISTANT_SCRIPT = r"""
set -eu
ready="$1"
rm -f "$ready"
command -v setsid >/dev/null
exec setsid sh -ceu 'trap "" TERM HUP; printf "ready\n" >"$1"; while :; do sleep 60; done' -- "$ready"
""".strip()


def _start_term_resistant_descendant(
    container: ContainerRef,
    source_workspace: Path,
    event_log: Path,
) -> Path:
    destination = f"/workspaces/.aiops-1128-{container.role}-shutdown-ready"
    source = source_workspace / Path(destination).name
    result = subprocess.run(
        [
            "docker",
            "exec",
            "--detach",
            container.container_id,
            "sh",
            "-ceu",
            _TERM_RESISTANT_SCRIPT,
            "--",
            destination,
        ],
        capture_output=True,
        text=True,
        timeout=10,
        check=False,
    )
    if result.returncode != 0:
        raise RuntimeError(
            f"failed to start {container.role} shutdown injection: {result.stderr.strip()}"
        )
    deadline = time.monotonic() + 10
    while time.monotonic() < deadline and not source.is_file():
        time.sleep(0.05)
    if source.read_text(encoding="utf-8") != "ready\n":
        raise ValueError(
            f"{container.role} TERM-resistant descendant did not become ready"
        )
    append_event(
        event_log,
        {
            "event": "shutdown_injection_ready",
            "role": container.role,
            "container_id": container.container_id,
            "term_resistant": True,
            "separate_session_and_process_group": "setsid",
            "proof_boundary": "exact container ID; no host PID inventory",
        },
    )
    return source


def _secret_scan(
    values: Sequence[str], paths: Sequence[Path], pending_json: Any
) -> None:
    blobs = [json.dumps(pending_json, sort_keys=True).encode("utf-8")]
    for path in paths:
        if path.is_file():
            blobs.append(path.read_bytes())
    for value in values:
        for candidate in dict.fromkeys((value, value.rstrip("\r\n"))):
            if candidate and any(candidate.encode("utf-8") in blob for blob in blobs):
                raise ValueError("secret scan found a credential in persisted evidence")


def _load_runtime(runtime_root: Path) -> dict[str, Any]:
    with (runtime_root / "runtime.json").open(encoding="utf-8") as stream:
        return json.load(stream)


def _container_refs(runtime: Mapping[str, Any]) -> dict[str, ContainerRef]:
    return {
        role: ContainerRef(role, runtime["containers"][role])
        for role in ("maker", "reviewer")
    }


def _current_states(
    containers: Mapping[str, ContainerRef],
) -> dict[str, dict[str, Any]]:
    return {
        role: read_state_via_exec(containers[role], timeout_seconds=7)
        for role in ("maker", "reviewer")
    }


def _baseline_totals(states: Mapping[str, Mapping[str, Any]]) -> dict[str, int]:
    return {
        role: int((state.get("codex_totals") or {}).get("total_tokens") or 0)
        for role, state in states.items()
    }


def _stable_state_signature(states: Mapping[str, Any]) -> str:
    volatile = {"generated_at", "runtime_seconds", "seconds_running"}

    def scrub(value: Any) -> Any:
        if isinstance(value, dict):
            return {
                key: scrub(child) for key, child in value.items() if key not in volatile
            }
        if isinstance(value, list):
            return [scrub(child) for child in value]
        return value

    encoded = json.dumps(scrub(states), sort_keys=True, separators=(",", ":")).encode(
        "utf-8"
    )
    return hashlib.sha256(encoded).hexdigest()


def _activate_issue(number: int, operator_config: Path, event_log: Path) -> str:
    if number not in {1, 2}:
        raise ValueError(f"unregistered issue activation: {number}")
    requested_at = utc_now()
    append_event(
        event_log,
        {
            "event": "issue_activation_requested",
            "issue_number": number,
            "requested_at": requested_at,
        },
    )
    _run_checked(
        [
            "gh",
            "issue",
            "edit",
            str(number),
            "--repo",
            REPOSITORY,
            "--add-label",
            "aiops:todo",
        ],
        timeout_seconds=30,
        env=_clean_env(GH_CONFIG_DIR=str(operator_config)),
    )
    issue = _gh_json(operator_config, ["api", f"repos/{REPOSITORY}/issues/{number}"])
    labels = {label["name"] for label in issue.get("labels") or []}
    if labels != {"aiops:todo"} or issue.get("state") != "open":
        raise ValueError(
            f"issue {number} activation did not converge: state={issue.get('state')}, labels={labels}"
        )
    completed_at = utc_now()
    append_event(
        event_log,
        {
            "event": "issue_activated",
            "issue_number": number,
            "completed_at": completed_at,
        },
    )
    return requested_at


def _live_rows_for_issue(
    states: Mapping[str, Mapping[str, Any]], issue_number: int
) -> int:
    total = 0
    for state in states.values():
        for field in ("running", "retrying", "blocked"):
            total += sum(
                1
                for row in state.get(field) or []
                if _row_issue_number(row) == issue_number
            )
    return total


def _completion_state(
    snapshot: Mapping[str, Any] | None,
    states: Mapping[str, Mapping[str, Any]],
    issue_number: int,
    external_evidence: Mapping[str, Any],
) -> tuple[bool, dict[str, Any] | None]:
    if not snapshot:
        return False, None
    issue = snapshot.get("issue") or {}
    if issue.get("number") != issue_number:
        return False, {
            "reason": "forge_issue_mismatch",
            "observed": issue.get("number"),
        }
    pr = snapshot.get("pr") or {}
    if issue.get("state") == "closed" and not pr.get("mergedAt"):
        return False, {
            "reason": "closed_without_merged_pr",
            "limit": "native close requires a merged PR",
            "observed": {"issue_state": "closed", "pr": pr.get("number")},
        }
    complete = (
        issue.get("state") == "closed"
        and bool(pr.get("mergedAt"))
        and _live_rows_for_issue(states, issue_number) == 0
        and external_evidence.get("reliable_signal") is True
    )
    return complete, None


def _runtime_secret_values(runtime: Mapping[str, Any]) -> tuple[str, ...]:
    values: list[str] = []
    for role in ("maker", "reviewer"):
        paths = runtime["roles"][role]
        values.append(Path(paths["github_token"]).read_text(encoding="utf-8"))
        values.append(Path(paths["state_token"]).read_text(encoding="utf-8"))
        auth = json.loads(
            (Path(paths["codex_home"]) / "auth.json").read_text(encoding="utf-8")
        )

        def collect(value: Any, key: str = "") -> None:
            if isinstance(value, dict):
                for child_key, child in value.items():
                    collect(child, str(child_key))
            elif isinstance(value, list):
                for child in value:
                    collect(child, key)
            elif (
                isinstance(value, str)
                and len(value) >= 20
                and ("token" in key.lower() or "api_key" in key.lower())
            ):
                values.append(value)

        collect(auth)
    return tuple(dict.fromkeys(values))


def _verify_activation_gate(
    runtime: Mapping[str, Any],
    preflight: Mapping[str, Any],
    containers: Mapping[str, ContainerRef],
    event_log: Path,
) -> dict[str, dict[str, Any]]:
    if preflight.get("decision") != "GO":
        raise ValueError("preflight decision is not GO")
    for name, evidence in preflight["frozen_files"].items():
        if sha256_file(Path(evidence["path"])) != evidence["sha256"]:
            raise ValueError(f"frozen file changed after GO: {name}")
    if runtime.get("image") != preflight["image"]["tag"]:
        raise ValueError("runtime image tag changed after GO")
    for role in ("maker", "reviewer"):
        proof = inspect_and_probe_mounts(
            containers[role],
            _role_mounts(runtime, role) + _role_secret_mounts(runtime, role),
            timeout_seconds=10,
        )
        if proof["container_id"] != preflight["containers"][role]["container_id"]:
            raise ValueError(f"{role} container ID changed after GO")
    states = {
        role: _wait_for_state(containers[role], timeout_seconds=20)
        for role in ("maker", "reviewer")
    }
    for proof in states.values():
        _require_quiescent(proof)
    operator_config = Path(runtime["roles"]["reviewer"]["gh_config"])
    for number in (1, 2):
        issue = _gh_json(
            operator_config, ["api", f"repos/{REPOSITORY}/issues/{number}"]
        )
        labels = [label["name"] for label in issue.get("labels") or []]
        if issue.get("state") != "open" or labels:
            raise ValueError(
                f"issue {number} changed after GO: state={issue.get('state')}, labels={labels}"
            )
    append_event(
        event_log,
        {
            "event": "activation_gate_rechecked",
            "decision": "GO",
            "container_ids": {
                role: containers[role].container_id for role in containers
            },
        },
    )
    return {role: dict(states[role]["state"]) for role in states}


def _probe_runtime(runtime: Mapping[str, Any], event_log: Path) -> dict[str, Any]:
    mounts = _role_mounts(runtime, "maker") + _role_mounts(runtime, "reviewer")
    validate_bind_sources(mounts)
    containers = _container_refs(runtime)
    proof = {}
    for role in ("maker", "reviewer"):
        boundary = inspect_and_probe_mounts(
            containers[role],
            _role_mounts(runtime, role) + _role_secret_mounts(runtime, role),
            timeout_seconds=10,
        )
        state = _wait_for_state(containers[role], timeout_seconds=60)
        _require_quiescent(state)
        proof[role] = {"boundary": boundary, "state": state}
    append_event(event_log, {"event": "preactivation_container_probe", "proof": proof})
    return proof


def _shutdown_injection(runtime: Mapping[str, Any], event_log: Path) -> dict[str, Any]:
    containers = _container_refs(runtime)
    readiness = {
        role: _start_term_resistant_descendant(
            containers[role], Path(runtime["roles"][role]["workspace"]), event_log
        )
        for role in ("maker", "reviewer")
    }
    proof = stop_both_and_prove(
        containers["maker"],
        containers["reviewer"],
        event_log,
        {
            "event": "shutdown_injection_started",
            "term_resistant_descendant": True,
            "separate_session_and_process_group": "setsid",
        },
        grace_seconds=1,
        timeout_seconds=15,
    )
    for path in readiness.values():
        path.unlink()
    return {"terminal_proof": proof, "readiness_files_removed": True}


def _run_holdout(runtime_root: Path) -> dict[str, Any]:
    clone = runtime_root / "final-holdout-clone"
    if clone.exists():
        raise ValueError(f"holdout clone path already exists: {clone}")
    _run_checked(
        [
            "git",
            "clone",
            "--branch",
            "main",
            f"https://github.com/{REPOSITORY}.git",
            str(clone),
        ],
        timeout_seconds=120,
    )
    result = subprocess.run(
        [str(ASSET_DIR / "holdout.sh"), str(clone)],
        capture_output=True,
        text=True,
        timeout=120,
        check=False,
    )
    proof = {
        "exit_code": result.returncode,
        "stdout": result.stdout,
        "stderr": result.stderr,
        "commit": _run_checked(["git", "-C", str(clone), "rev-parse", "HEAD"]).strip(),
    }
    if result.returncode != 0 or result.stdout.strip() != "HOLDOUT PASS":
        raise ValueError(f"fixed holdout failed: {proof}")
    return proof


def _abort_run(
    *,
    reason: Mapping[str, Any],
    containers: Mapping[str, ContainerRef],
    event_log: Path,
    runtime: Mapping[str, Any],
    issues: Sequence[Mapping[str, Any]],
) -> None:
    stop_error = None
    stop_proof: Any = None
    try:
        stop_proof = stop_both_and_prove(
            containers["maker"],
            containers["reviewer"],
            event_log,
            {"event": "experiment_breach", **dict(reason)},
            grace_seconds=5,
            timeout_seconds=20,
        )
    except Exception as error:
        stop_error = str(error)
    summary = {
        "outcome": "aborted",
        "allowed_verdict": "keep disabled pending a named defect",
        "named_defect": dict(reason),
        "issues": list(issues),
        "container_stop_proof": stop_proof,
        "container_stop_error": stop_error,
        "known_unmeasured": [
            "external GitHub Codex review tokens",
            "any external reviewer tokens",
            "otherwise unreported nested or subagent tokens",
        ],
    }
    _secret_scan(_runtime_secret_values(runtime), [event_log], summary)
    write_json(ASSET_DIR / "run-summary.json", summary)
    raise RuntimeError(f"experiment aborted: {reason}")


def execute_run(runtime_root: Path) -> dict[str, Any]:
    event_log = ASSET_DIR / "events.jsonl"
    with (ASSET_DIR / "preflight.json").open(encoding="utf-8") as stream:
        preflight = json.load(stream)
    runtime = _load_runtime(runtime_root)
    containers = _container_refs(runtime)
    states = _verify_activation_gate(runtime, preflight, containers, event_log)
    operator_config = Path(runtime["roles"]["reviewer"]["gh_config"])

    issue_results: list[dict[str, Any]] = []
    issue_number = 1
    seen_triggers: dict[str, dict[str, Any]] = {}
    last_external_signature = ""
    last_state_signature = ""
    latest_forge: dict[str, Any] | None = None
    next_forge_at = 0.0
    issue_started_at = 0.0
    issue_activation_timestamp: str | None = None
    baseline_totals = _baseline_totals(states)

    while True:
        try:
            if issue_activation_timestamp is None:
                issue_started_at = time.monotonic()
                issue_activation_timestamp = _activate_issue(
                    issue_number, operator_config, event_log
                )
            states = _current_states(containers)
            signature = _stable_state_signature(states)
            if signature != last_state_signature:
                append_event(
                    event_log,
                    {
                        "event": "state_snapshot",
                        "issue_number": issue_number,
                        "signature": signature,
                        "states": states,
                    },
                )
                last_state_signature = signature

            now_monotonic = time.monotonic()
            if now_monotonic >= next_forge_at:
                poll_id = f"issue-{issue_number}-forge-{time.time_ns()}"
                latest_forge = observe_forge_stages(
                    repo=REPOSITORY,
                    issue_number=issue_number,
                    poll_id=poll_id,
                    gh_config_dir=operator_config,
                    event_log=event_log,
                    timeout_seconds=5,
                )
                next_forge_at = time.monotonic() + 15

            external = evaluate_external_review(
                latest_forge or {},
                seen_triggers,
                now=utc_now(),
            )
            external_signature = hashlib.sha256(
                json.dumps(external, sort_keys=True, default=str).encode("utf-8")
            ).hexdigest()
            if external_signature != last_external_signature:
                append_event(
                    event_log,
                    {
                        "event": "external_review_evidence",
                        "issue_number": issue_number,
                        "evidence": external,
                    },
                )
                last_external_signature = external_signature

            usage = summarize_issue_usage(
                states,
                issue_number=issue_number,
                baseline_totals=baseline_totals,
            )
            wall_seconds = time.monotonic() - issue_started_at
            breach = detect_limit_breach(
                usage,
                wall_seconds=wall_seconds,
                external_evidence=external,
            )
            complete, completion_breach = _completion_state(
                latest_forge,
                states,
                issue_number,
                external,
            )
            breach = breach or completion_breach
            if breach:
                _abort_run(
                    reason={
                        **breach,
                        "issue_number": issue_number,
                        "wall_seconds": wall_seconds,
                        "usage": usage,
                    },
                    containers=containers,
                    event_log=event_log,
                    runtime=runtime,
                    issues=issue_results,
                )
            if complete:
                result = {
                    "issue_number": issue_number,
                    "activation_timestamp": issue_activation_timestamp,
                    "completed_timestamp": utc_now(),
                    "wall_seconds": wall_seconds,
                    "usage": usage,
                    "exact_tuple": {
                        key: (latest_forge.get("pr") or {}).get(key)
                        for key in ("headRefOid", "baseRefOid", "baseRefName")
                    },
                    "merged_at": (latest_forge.get("pr") or {}).get("mergedAt"),
                    "external_review": external,
                }
                issue_results.append(result)
                append_event(event_log, {"event": "issue_completed", **result})
                if issue_number == 2:
                    break
                issue_number = 2
                seen_triggers = {}
                last_external_signature = ""
                latest_forge = None
                next_forge_at = 0.0
                baseline_totals = _baseline_totals(states)
                issue_activation_timestamp = None
                continue
            time.sleep(1)
        except ForgeObservationError as error:
            _abort_run(
                reason={
                    "reason": "forge_observation_failure",
                    "stage_id": error.stage_id,
                    "failure": error.reason,
                    "partial_results": error.partial_results,
                    "issue_number": issue_number,
                },
                containers=containers,
                event_log=event_log,
                runtime=runtime,
                issues=issue_results,
            )
        except (OSError, RuntimeError, ValueError, subprocess.TimeoutExpired) as error:
            if (ASSET_DIR / "run-summary.json").exists():
                raise
            _abort_run(
                reason={
                    "reason": "controller_evidence_failure",
                    "error": str(error),
                    "issue_number": issue_number,
                },
                containers=containers,
                event_log=event_log,
                runtime=runtime,
                issues=issue_results,
            )

    stop_proof = stop_both_and_prove(
        containers["maker"],
        containers["reviewer"],
        event_log,
        {"event": "experiment_complete", "accepted_issues": 2},
        grace_seconds=5,
        timeout_seconds=20,
    )
    try:
        holdout = _run_holdout(runtime_root)
        verdict = "operationally ready"
        outcome = "completed"
        named_defect = None
    except (OSError, RuntimeError, ValueError, subprocess.TimeoutExpired) as error:
        holdout = {"status": "failed", "error": str(error)}
        verdict = "keep disabled pending a named defect"
        outcome = "holdout_failed"
        named_defect = {"reason": "fixed_holdout_failed", "error": str(error)}
    summary = {
        "outcome": outcome,
        "allowed_verdict": verdict,
        "named_defect": named_defect,
        "issues": issue_results,
        "aggregate": {
            "accepted_issues": len(issue_results),
            "worker_observed_tokens": sum(
                int(issue["usage"]["worker_total_delta"]) for issue in issue_results
            ),
            "wall_seconds": sum(
                float(issue["wall_seconds"]) for issue in issue_results
            ),
            "rework_claims": sum(
                max(0, int(issue["usage"]["claim_count"]) - 2)
                for issue in issue_results
            ),
        },
        "holdout": holdout,
        "container_stop_proof": stop_proof,
        "comparison": preflight["comparison"]["issue_1089_standard_arm"],
        "known_unmeasured": preflight["known_unmeasured"],
    }
    _secret_scan(_runtime_secret_values(runtime), [event_log], summary)
    write_json(ASSET_DIR / "run-summary.json", summary)
    append_event(
        event_log,
        {
            "event": "run_summary_written",
            "outcome": outcome,
            "allowed_verdict": verdict,
        },
    )
    return summary


def parse_args(argv: Sequence[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    for name in (
        "validate-roots",
        "probe",
        "forge-injection",
        "shutdown-injection",
        "run",
    ):
        command = commands.add_parser(name)
        command.add_argument("--runtime-root", type=Path, required=True)
    return parser.parse_args(argv)


def main(argv: Sequence[str] | None = None) -> int:
    args = parse_args(argv)
    runtime_root = args.runtime_root.resolve()
    runtime = _load_runtime(runtime_root)
    event_log = ASSET_DIR / "events.jsonl"
    if args.command == "validate-roots":
        validate_bind_sources(
            _role_mounts(runtime, "maker") + _role_mounts(runtime, "reviewer")
        )
        append_event(
            event_log, {"event": "bind_sources_validated_before_container_create"}
        )
        result = {"status": "passed"}
    elif args.command == "probe":
        result = _probe_runtime(runtime, event_log)
    elif args.command == "forge-injection":
        result = _forge_timeout_injection(runtime_root, event_log)
    elif args.command == "shutdown-injection":
        result = _shutdown_injection(runtime, event_log)
    else:
        result = execute_run(runtime_root)
    print(json.dumps(result, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
