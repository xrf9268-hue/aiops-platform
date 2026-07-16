#!/usr/bin/env python3
"""One-shot, fail-closed supervisor for the #1117 validation replay.

This is an operator-side validation asset. It starts two published workers,
performs only the two pre-registered label activations, records read-only state,
and stops both process groups at the first crossed limit. It is not imported by
the worker and is not a recurring benchmark service.
"""

from __future__ import annotations

import argparse
import contextlib
import dataclasses
import hashlib
import json
import os
import queue
import re
import signal
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Iterable, Iterator


CODEX_BOT_ID = 199175422
MAX_CLAIMS_PER_ISSUE = 4
MAX_TOKENS_PER_ISSUE = 3_500_000
MAX_EXTERNAL_WAIT_SECONDS = 600
MAX_ISSUE_WALL_SECONDS = 1_800
ACTIVE_LABELS = {"aiops:todo", "aiops:rework", "aiops:human-review"}
CHECKPOINT_RE = re.compile(
    r"^Reviewer checkpoint: headRefOid=(\S+) baseRefOid=(\S+) "
    r"baseRefName=(\S+) local-rubric=PASS$"
)
PROC_ROOT = Path("/proc")
PROC_SCAN_ATTEMPTS = 4
TERMINAL_PROC_STATES = {"Z", "X", "x"}


@dataclasses.dataclass(frozen=True)
class ReviewTuple:
    head: str
    base_oid: str
    base_name: str


@dataclasses.dataclass(frozen=True)
class LimitBreach:
    reason: str
    observed: float | int
    limit: float | int


class CounterRegressionError(ValueError):
    """A monotonic worker token counter moved backwards."""


class SupervisorSignal(RuntimeError):
    """The supervisor received an OS termination signal."""

    def __init__(self, signum: int):
        self.signum = signum
        super().__init__(f"received {signal.Signals(signum).name}")


class WorkersStoppedError(RuntimeError):
    """Worker shutdown completed, but cleanup evidence is incomplete."""


class TerminationEvidenceError(WorkersStoppedError):
    """Workers stopped, but one or more shutdown events could not be persisted."""

    def __init__(self, failures: list[BaseException]):
        self.failures = tuple(failures)
        super().__init__(
            f"failed to persist termination evidence ({len(failures)} writes): "
            f"{failures[0]}"
        )


class TerminationOperationError(WorkersStoppedError):
    """Workers stopped, but one or more shutdown operations failed."""

    def __init__(self, failures: list[BaseException]):
        self.failures = tuple(failures)
        super().__init__(
            f"worker shutdown recovered from {len(failures)} operation errors: "
            f"{failures[0]}"
        )


class WorkersStillRunningError(RuntimeError):
    """Worker shutdown exhausted its signal sequence with live process groups."""

    def __init__(self, pids: list[int], failures: list[BaseException]):
        self.pids = tuple(pids)
        self.failures = tuple(failures)
        super().__init__(f"worker process groups still alive after shutdown: {pids}")


class WorkerLogCloseError(WorkersStoppedError):
    """Workers stopped, but one or more worker log handles failed to close."""

    def __init__(self, failures: list[BaseException]):
        self.failures = tuple(failures)
        super().__init__(
            f"worker log close failed ({len(failures)} handles): {failures[0]}"
        )


@dataclasses.dataclass(frozen=True)
class WorkerSpec:
    role: str
    port: int
    workflow: Path
    workflow_sha256: str
    gh_config_dir: Path
    expected_login: str
    mirror_root: Path
    token_env: str


@dataclasses.dataclass
class WorkerProcess:
    spec: WorkerSpec
    process: subprocess.Popen[str]
    log_handle: Any


@dataclasses.dataclass
class TerminationAttempt:
    processes: list[subprocess.Popen[str]]
    event_log: Path
    event_errors: list[BaseException] = dataclasses.field(default_factory=list)
    operation_errors: list[BaseException] = dataclasses.field(default_factory=list)

    def record_event(self, event: dict[str, Any]) -> None:
        try:
            append_event(self.event_log, event)
        except BaseException as exc:
            # A failed evidence sink must not keep breached workers alive.
            self.event_errors.append(exc)

    def poll(self, process: subprocess.Popen[str]) -> int | None:
        try:
            return process.poll()
        except BaseException as exc:
            self.operation_errors.append(exc)
            return None

    def group_alive(self, process: subprocess.Popen[str]) -> bool:
        try:
            return process_group_alive(process.pid)
        except BaseException as exc:
            self.operation_errors.append(exc)
            return True

    def group_exists(self, process: subprocess.Popen[str]) -> bool:
        try:
            return process_group_exists(process.pid)
        except BaseException as exc:
            self.operation_errors.append(exc)
            return True

    def signal_groups(self, signum: signal.Signals) -> list[int]:
        signaled: list[int] = []
        for process in self.processes:
            self.poll(process)
            try:
                os.killpg(process.pid, signum)
                signaled.append(process.pid)
            except ProcessLookupError:
                pass
            except BaseException as exc:
                self.operation_errors.append(exc)
        return signaled

    def wait_for_grace(self, grace_seconds: float) -> None:
        deadline = time.monotonic() + grace_seconds
        while time.monotonic() < deadline:
            for process in self.processes:
                self.poll(process)
            if not any(self.group_exists(process) for process in self.processes):
                return
            time.sleep(min(0.02, max(0.0, deadline - time.monotonic())))

    def kill_survivors(self) -> list[int]:
        killed: list[int] = []
        for process in self.processes:
            self.poll(process)
            if not self.group_exists(process):
                continue
            try:
                os.killpg(process.pid, signal.SIGKILL)
                killed.append(process.pid)
            except ProcessLookupError:
                pass
            except BaseException as exc:
                self.operation_errors.append(exc)
                try:
                    process.kill()
                except BaseException as fallback_error:
                    self.operation_errors.append(fallback_error)
        return killed

    def wait_for_exit(self) -> None:
        for process in self.processes:
            try:
                process.wait(timeout=1)
            except subprocess.TimeoutExpired:
                pass
            except BaseException as exc:
                self.operation_errors.append(exc)

    def final_status(self) -> tuple[dict[str, int | None], list[int]]:
        exit_codes = {
            str(process.pid): self.poll(process) for process in self.processes
        }
        alive = [process.pid for process in self.processes if self.group_alive(process)]
        return exit_codes, alive


class ForgePoller:
    """Run forge snapshots off the stop-contract sampling path."""

    def __init__(
        self,
        client: GhClient,
        repo: str,
        issue_number: int,
        *,
        interval_seconds: float,
        timeout_seconds: float,
    ):
        self.client = client
        self.repo = repo
        self.issue_number = issue_number
        self.interval_seconds = interval_seconds
        self.timeout_seconds = timeout_seconds
        self.next_due = 0.0
        self.started_at: float | None = None
        self.results: queue.SimpleQueue[
            tuple[dict[str, Any] | None, BaseException | None]
        ] = queue.SimpleQueue()

    def maybe_start(self, now: float) -> None:
        if self.started_at is not None or now < self.next_due:
            return
        self.started_at = now
        threading.Thread(target=self._fetch, daemon=True).start()

    def _fetch(self) -> None:
        try:
            self.results.put(
                (forge_snapshot(self.client, self.repo, self.issue_number), None)
            )
        except BaseException as exc:
            self.results.put((None, exc))

    def take(self, now: float) -> dict[str, Any] | None:
        if self.started_at is None:
            return None
        try:
            snapshot, error = self.results.get_nowait()
        except queue.Empty:
            if now - self.started_at >= self.timeout_seconds:
                raise TimeoutError(
                    f"forge snapshot exceeded {self.timeout_seconds:.3f}s"
                )
            return None
        started_at = self.started_at
        self.started_at = None
        self.next_due = max(now, started_at + self.interval_seconds)
        if error is not None:
            raise RuntimeError(f"forge snapshot failed: {error}") from error
        if snapshot is None:
            raise RuntimeError("forge snapshot returned no data")
        return snapshot


def utc_now() -> str:
    return datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")


def parse_time(value: str) -> datetime:
    return datetime.fromisoformat(value.replace("Z", "+00:00"))


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def issue_matches(row: dict[str, Any], issue_number: int) -> bool:
    identifier = str(row.get("issue_identifier") or "")
    if identifier:
        return identifier == f"#{issue_number}"
    url = str(row.get("issue_url") or "")
    return url.rstrip("/").endswith(f"/issues/{issue_number}")


def claim_count(states: Iterable[dict[str, Any]], issue_number: int) -> int:
    count = 0
    for state in states:
        for field in ("completed_session_usage", "running", "blocked"):
            count += sum(
                1
                for row in (state.get(field) or [])
                if issue_matches(row, issue_number)
            )
    return count


def continuation_pending(states: Iterable[dict[str, Any]], issue_number: int) -> bool:
    return any(
        issue_matches(row, issue_number) and row.get("kind") == "continuation"
        for state in states
        for row in (state.get("retrying") or [])
    )


def issue_active(states: Iterable[dict[str, Any]], issue_number: int) -> bool:
    return any(
        issue_matches(row, issue_number)
        for state in states
        for field in ("running", "blocked", "retrying")
        for row in (state.get(field) or [])
    )


def canonical_issue_hash(issue: dict[str, Any]) -> str:
    canonical = {
        "body": issue.get("body") or "",
        "number": issue.get("number"),
        "title": issue.get("title") or "",
    }
    encoded = (
        json.dumps(canonical, sort_keys=True, separators=(",", ":")) + "\n"
    ).encode()
    return hashlib.sha256(encoded).hexdigest()


def workflow_workspace_root(workflow: Path) -> Path:
    lines = workflow.read_text(encoding="utf-8").splitlines()
    if not lines or lines[0] != "---":
        raise ValueError(f"workflow lacks YAML front matter: {workflow}")
    try:
        end = lines.index("---", 1)
    except ValueError as exc:
        raise ValueError(f"workflow front matter is not closed: {workflow}") from exc
    roots: list[str] = []
    in_workspace = False
    for line in lines[1:end]:
        if line == "workspace:":
            in_workspace = True
            continue
        if in_workspace and line and not line[0].isspace():
            in_workspace = False
        if not in_workspace:
            continue
        match = re.fullmatch(r"  root:\s*(.+?)\s*", line)
        if match:
            value = match.group(1)
            if len(value) >= 2 and value[0] == value[-1] and value[0] in "'\"":
                value = value[1:-1]
            roots.append(value)
    if len(roots) != 1:
        raise ValueError(
            f"workflow must declare exactly one workspace.root: {workflow}"
        )
    root = Path(roots[0])
    if not root.is_absolute():
        raise ValueError(f"workflow workspace.root must be absolute: {root}")
    return root.resolve()


def validate_fresh_directories(
    directories: dict[str, Path],
) -> dict[str, dict[str, Any]]:
    resolved = {name: path.resolve() for name, path in directories.items()}
    if len(set(resolved.values())) != len(resolved):
        raise ValueError(f"run directories must be pairwise distinct: {resolved}")
    items = list(resolved.items())
    for index, (left_name, left_path) in enumerate(items):
        for right_name, right_path in items[index + 1 :]:
            if left_path in right_path.parents or right_path in left_path.parents:
                raise ValueError(
                    "run directories must not overlap: "
                    f"{left_name}={left_path}, {right_name}={right_path}"
                )
    manifest: dict[str, dict[str, Any]] = {}
    for name, path in resolved.items():
        existed = path.exists()
        if existed and not path.is_dir():
            raise ValueError(f"{name} is not a directory: {path}")
        entries = sorted(item.name for item in path.iterdir()) if existed else []
        if entries:
            raise ValueError(f"{name} must be empty: {path} contains {entries}")
        manifest[name] = {"path": str(path), "existed": existed, "empty": True}
    return manifest


def token_total(state: dict[str, Any]) -> int:
    totals = state.get("codex_totals")
    value = totals.get("total_tokens") if isinstance(totals, dict) else None
    if isinstance(value, bool) or not isinstance(value, int) or value < 0:
        raise ValueError("state codex_totals.total_tokens is invalid")
    return value


def token_delta(states: list[dict[str, Any]], baselines: list[int]) -> int:
    if len(states) != len(baselines):
        raise ValueError("state/baseline cardinality differs")
    total = 0
    for index, (state, baseline) in enumerate(zip(states, baselines, strict=True)):
        current = token_total(state)
        if current < baseline:
            raise CounterRegressionError(
                f"worker {index} token counter regressed from baseline {baseline} to {current}"
            )
        total += current - baseline
    return total


def issue_token_total(states: Iterable[dict[str, Any]], issue_number: int) -> int:
    total = 0
    for state in states:
        for field in ("completed_session_usage", "running", "blocked"):
            for usage in state[field]:
                if not issue_matches(usage, issue_number):
                    continue
                value = int(usage["tokens"]["total_tokens"])
                if value < 0:
                    raise ValueError(f"negative issue-attributed token value {value}")
                total += value
    return total


def validate_token_accounting(
    states: list[dict[str, Any]], baselines: list[int], issue_number: int
) -> int:
    process_delta = token_delta(states, baselines)
    attributed = issue_token_total(states, issue_number)
    if process_delta != attributed:
        raise ValueError(
            "token accounting mismatch: "
            f"process_delta={process_delta} issue_attributed={attributed}"
        )
    return process_delta


def validate_state_payload(state: dict[str, Any]) -> None:
    if state.get("version") != "v0.1.16":
        raise ValueError(f"state version is {state.get('version')!r}; want 'v0.1.16'")
    totals = state.get("codex_totals")
    if not isinstance(totals, dict):
        raise ValueError("state codex_totals must be an object")
    total_tokens = totals.get("total_tokens")
    if isinstance(total_tokens, bool) or not isinstance(total_tokens, int):
        raise ValueError("state codex_totals.total_tokens must be an integer")
    if total_tokens < 0:
        raise ValueError("state codex_totals.total_tokens must be non-negative")
    for field in ("completed_session_usage", "running", "blocked", "retrying"):
        rows = state.get(field)
        if not isinstance(rows, list):
            raise ValueError(f"state {field} must be an array")
        for index, row in enumerate(rows):
            if not isinstance(row, dict):
                raise ValueError(f"state {field}[{index}] must be an object")
            if not row.get("issue_identifier") and not row.get("issue_url"):
                raise ValueError(f"state {field}[{index}] lacks issue identity")
            if field == "retrying":
                continue
            tokens = row.get("tokens")
            value = tokens.get("total_tokens") if isinstance(tokens, dict) else None
            if isinstance(value, bool) or not isinstance(value, int) or value < 0:
                raise ValueError(
                    f"state {field}[{index}].tokens.total_tokens must be a non-negative integer"
                )


def validate_role_identities(
    observed: dict[str, str], expected: dict[str, str]
) -> None:
    if observed != expected:
        raise RuntimeError(
            f"role identity mismatch: observed={observed}, expected={expected}"
        )
    if observed["maker"] == observed["reviewer"]:
        raise RuntimeError("maker and reviewer GitHub identities must differ")
    if observed["operator"] != observed["reviewer"]:
        raise RuntimeError(
            "personal-repository operator must reuse the comparison reviewer identity"
        )


def evaluate_limits(
    states: list[dict[str, Any]],
    baselines: list[int],
    *,
    issue_number: int,
    elapsed_seconds: float,
    issue_closed: bool,
) -> LimitBreach | None:
    tokens = token_delta(states, baselines)
    if tokens > MAX_TOKENS_PER_ISSUE:
        return LimitBreach("worker_tokens_exceeded", tokens, MAX_TOKENS_PER_ISSUE)
    claims = claim_count(states, issue_number)
    if claims > MAX_CLAIMS_PER_ISSUE:
        return LimitBreach("worker_sessions_exceeded", claims, MAX_CLAIMS_PER_ISSUE)
    if claims >= MAX_CLAIMS_PER_ISSUE and continuation_pending(states, issue_number):
        return LimitBreach("worker_sessions_exhausted", claims, MAX_CLAIMS_PER_ISSUE)
    if elapsed_seconds >= MAX_ISSUE_WALL_SECONDS:
        return LimitBreach(
            "issue_wall_exceeded", elapsed_seconds, MAX_ISSUE_WALL_SECONDS
        )
    return None


def reliable_external_review(
    reviews: Iterable[dict[str, Any]],
    review_tuple: ReviewTuple,
    triggered_at: datetime,
) -> dict[str, Any] | None:
    for review in reviews:
        user = review.get("user") or {}
        submitted = review.get("submitted_at")
        if (
            user.get("id") == CODEX_BOT_ID
            and user.get("type") == "Bot"
            and review.get("commit_id") == review_tuple.head
            and submitted
            and parse_time(submitted) >= triggered_at
        ):
            return review
    return None


def checkpoint_tuple_for_trigger(
    trigger: dict[str, Any], reviews: Iterable[dict[str, Any]], reviewer_login: str
) -> ReviewTuple | None:
    created = trigger.get("created_at")
    if not created:
        return None
    trigger_time = parse_time(str(created))
    candidates: list[tuple[datetime, ReviewTuple]] = []
    for review in reviews:
        submitted = review.get("submitted_at")
        user = review.get("user") or {}
        match = CHECKPOINT_RE.fullmatch(str(review.get("body") or "").strip())
        if (
            not submitted
            or user.get("login") != reviewer_login
            or review.get("state") != "COMMENTED"
            or not match
        ):
            continue
        submitted_at = parse_time(str(submitted))
        review_tuple = ReviewTuple(*match.groups())
        if submitted_at < trigger_time and review.get("commit_id") == review_tuple.head:
            candidates.append((submitted_at, review_tuple))
    if not candidates:
        return None
    return max(candidates, key=lambda item: item[0])[1]


def state_fingerprint(states: list[dict[str, Any]]) -> str:
    volatile = {"generated_at", "runtime_seconds", "last_event_at", "seconds_running"}

    def stable(value: Any) -> Any:
        if isinstance(value, dict):
            return {
                key: stable(item) for key, item in value.items() if key not in volatile
            }
        if isinstance(value, list):
            return [stable(item) for item in value]
        return value

    return json.dumps(stable(states), sort_keys=True, separators=(",", ":"))


def validate_active_rows(states: Iterable[dict[str, Any]], issue_number: int) -> None:
    for state in states:
        for field in ("running", "blocked", "retrying"):
            for row in state.get(field) or []:
                if not issue_matches(row, issue_number):
                    raise ValueError(
                        f"unexpected active issue in {field}: "
                        f"{row.get('issue_identifier') or row.get('issue_url') or row.get('issue_id')}"
                    )


def workflow_rows(state: dict[str, Any], issue_number: int) -> list[dict[str, Any]]:
    rows: list[dict[str, Any]] = []
    for field in ("completed_session_usage", "running", "blocked"):
        rows.extend(
            row for row in (state.get(field) or []) if issue_matches(row, issue_number)
        )
    return rows


def validate_workflow_rows(
    state: dict[str, Any], issue_number: int, expected_path: str
) -> bool:
    expected = str(Path(expected_path).resolve())
    rows = workflow_rows(state, issue_number)
    for row in rows:
        source = row.get("workflow_source")
        path = row.get("workflow_path")
        if source != "file" or not path or str(Path(path).resolve()) != expected:
            raise ValueError(
                f"workflow binding differs: source={source!r} path={path!r}; "
                f"want file {expected!r}"
            )
    return bool(rows)


def append_event(path: Path, event: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    payload = dict(event)
    payload.setdefault("recorded_at", utc_now())
    with path.open("a", encoding="utf-8") as handle:
        handle.write(json.dumps(payload, sort_keys=True, separators=(",", ":")) + "\n")
        handle.flush()
        os.fsync(handle.fileno())


def parse_proc_stat(stat: str) -> tuple[int, str, int, int]:
    fields = stat[stat.rindex(")") + 1 :].split()
    return (
        int(stat.split(" ", 1)[0]),
        fields[0],
        int(fields[2]),
        int(fields[3]),
    )


def decode_mount_field(field: str) -> str:
    return re.sub(
        r"\\([0-7]{3})",
        lambda match: chr(int(match.group(1), 8)),
        field,
    )


def proc_visibility_complete() -> bool:
    if not sys.platform.startswith("linux") or not PROC_ROOT.is_dir():
        return False
    try:
        self_pid, _, self_group, _ = parse_proc_stat(
            (PROC_ROOT / "self" / "stat").read_text(encoding="utf-8")
        )
        mount_lines = (PROC_ROOT / "mounts").read_text(encoding="utf-8").splitlines()
        proc_root = str(PROC_ROOT.resolve())
    except (IndexError, OSError, ValueError):
        return False
    if self_pid != os.getpid() or self_group != os.getpgrp():
        return False
    for line in mount_lines:
        fields = line.split()
        if (
            len(fields) < 4
            or fields[2] != "proc"
            or decode_mount_field(fields[1]) != proc_root
        ):
            continue
        for option in fields[3].split(","):
            if option == "hidepid" or (
                option.startswith("hidepid=")
                and option.partition("=")[2] not in {"0", "off"}
            ):
                return False
        return True
    return False


def linux_process_group_snapshot(
    pgid: int,
) -> tuple[frozenset[int], bool] | None:
    """Inspect the worker session whose leader and initial group share pgid."""
    try:
        process_roots = tuple(PROC_ROOT.iterdir())
    except OSError:
        return None
    members: set[int] = set()
    has_live_member = False
    for process_root in process_roots:
        if not process_root.name.isdigit():
            continue
        try:
            pid, state, _, session = parse_proc_stat(
                (process_root / "stat").read_text(encoding="utf-8")
            )
        except (IndexError, OSError, ValueError):
            return None
        if session != pgid:
            continue
        members.add(pid)
        if state not in TERMINAL_PROC_STATES:
            has_live_member = True
    if not members:
        return None
    return frozenset(members), has_live_member


def linux_process_group_has_live_member(pgid: int) -> bool | None:
    if not proc_visibility_complete():
        return None
    # A stable terminal-only session is quiescent: terminal processes cannot
    # fork, and a process outside this start_new_session boundary cannot join it.
    previous_terminal_members: frozenset[int] | None = None
    for _ in range(PROC_SCAN_ATTEMPTS):
        snapshot = linux_process_group_snapshot(pgid)
        if snapshot is None:
            previous_terminal_members = None
            continue
        members, has_live_member = snapshot
        if has_live_member:
            return True
        if members == previous_terminal_members:
            return False
        previous_terminal_members = members
    return None


def process_group_exists(pgid: int) -> bool:
    try:
        os.killpg(pgid, 0)
    except ProcessLookupError:
        return False
    except PermissionError:
        return True
    return True


def process_group_alive(pgid: int) -> bool:
    if not process_group_exists(pgid):
        return False
    live_member = linux_process_group_has_live_member(pgid)
    if live_member is not None:
        return live_member
    return process_group_exists(pgid)


@contextlib.contextmanager
def termination_signal_handlers() -> Iterator[None]:
    previous: dict[signal.Signals, Any] = {}

    def interrupt(signum: int, _frame: Any) -> None:
        raise SupervisorSignal(signum)

    try:
        for signum in (signal.SIGTERM, signal.SIGHUP):
            previous[signum] = signal.getsignal(signum)
            signal.signal(signum, interrupt)
        yield
    finally:
        for restore_signal, handler in previous.items():
            signal.signal(restore_signal, handler)


@contextlib.contextmanager
def defer_spawn_signals() -> Iterator[list[int]]:
    previous: dict[signal.Signals, Any] = {}
    pending: list[int] = []

    def defer(signum: int, _frame: Any) -> None:
        pending.append(signum)

    try:
        for signum in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
            previous[signum] = signal.getsignal(signum)
            signal.signal(signum, defer)
        yield pending
    finally:
        for restore_signal, handler in previous.items():
            signal.signal(restore_signal, handler)


@contextlib.contextmanager
def shutdown_signal_guard() -> Iterator[None]:
    previous: dict[signal.Signals, Any] = {}
    try:
        for signum in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
            previous[signum] = signal.getsignal(signum)
            signal.signal(signum, signal.SIG_IGN)
        yield
    finally:
        for restore_signal, handler in previous.items():
            signal.signal(restore_signal, handler)


def terminate_workers(
    processes: Iterable[subprocess.Popen[str] | WorkerProcess],
    event_log: Path,
    first_event: dict[str, Any],
    *,
    grace_seconds: float,
) -> None:
    raw = [
        item.process if isinstance(item, WorkerProcess) else item for item in processes
    ]
    attempt = TerminationAttempt(raw, event_log)

    with shutdown_signal_guard():
        persisted = dict(first_event)
        detected_ns = int(
            persisted.setdefault("detected_monotonic_ns", time.monotonic_ns())
        )
        attempt.record_event(persisted)
        signaled = attempt.signal_groups(signal.SIGTERM)
        signal_ns = time.monotonic_ns()
        attempt.record_event(
            {
                "event": "signal_sent",
                "signal": "SIGTERM",
                "pids": signaled,
                "signal_monotonic_ns": signal_ns,
                "detection_to_signal_ms": (signal_ns - detected_ns) / 1_000_000,
            },
        )
        attempt.wait_for_grace(grace_seconds)
        killed = attempt.kill_survivors()
        attempt.wait_for_exit()
        exit_codes, alive = attempt.final_status()
        attempt.record_event(
            {
                "event": "workers_stopped" if not alive else "workers_stop_incomplete",
                "sigkill_pids": killed,
                "exit_codes": exit_codes,
                "alive_pids": alive,
            },
        )
    if alive:
        survivors_error = WorkersStillRunningError(alive, attempt.operation_errors)
        for event_error in attempt.event_errors:
            survivors_error.add_note(f"termination evidence failed: {event_error}")
        cause = attempt.operation_errors[0] if attempt.operation_errors else None
        if cause is not None:
            raise survivors_error from cause
        raise survivors_error
    if attempt.event_errors:
        evidence_error = TerminationEvidenceError(attempt.event_errors)
        for operation_error in attempt.operation_errors:
            evidence_error.add_note(f"shutdown operation recovered: {operation_error}")
        raise evidence_error from attempt.event_errors[0]
    if attempt.operation_errors:
        raise TerminationOperationError(
            attempt.operation_errors
        ) from attempt.operation_errors[0]


def fetch_text(url: str, *, timeout: float = 0.5) -> str:
    request = urllib.request.Request(url, headers={"Accept": "application/json"})
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            return response.read().decode("utf-8")
    except (urllib.error.URLError, TimeoutError) as exc:
        raise RuntimeError(f"GET {url} failed: {exc}") from exc


def fetch_json(url: str, *, timeout: float = 0.5) -> dict[str, Any]:
    try:
        value = json.loads(fetch_text(url, timeout=timeout))
    except json.JSONDecodeError as exc:
        raise RuntimeError(f"GET {url} returned invalid JSON: {exc}") from exc
    if not isinstance(value, dict):
        raise RuntimeError(f"GET {url} returned non-object JSON")
    return value


class GhClient:
    def __init__(self, config_dir: Path, *, timeout: float = 30):
        self.config_dir = config_dir
        self.timeout = timeout

    def run(self, args: list[str]) -> str:
        env = os.environ.copy()
        env.pop("GH_TOKEN", None)
        env.pop("GITHUB_TOKEN", None)
        env["GH_CONFIG_DIR"] = str(self.config_dir)
        result = subprocess.run(
            ["gh", *args],
            env=env,
            text=True,
            capture_output=True,
            timeout=self.timeout,
            check=False,
        )
        if result.returncode != 0:
            detail = (result.stderr or result.stdout).strip()
            raise RuntimeError(f"gh {' '.join(args[:3])} failed: {detail}")
        return result.stdout

    def json(self, args: list[str]) -> Any:
        output = self.run(args)
        try:
            return json.loads(output)
        except json.JSONDecodeError as exc:
            raise RuntimeError(
                f"gh returned invalid JSON for {args[:3]}: {exc}"
            ) from exc

    def api(self, endpoint: str) -> Any:
        return self.json(["api", endpoint])

    def paginated(self, endpoint: str) -> list[dict[str, Any]]:
        pages = self.json(["api", "--paginate", "--slurp", endpoint])
        if not isinstance(pages, list):
            raise RuntimeError(f"paginated gh response for {endpoint} is not a list")
        flattened: list[dict[str, Any]] = []
        for page in pages:
            if isinstance(page, list):
                flattened.extend(item for item in page if isinstance(item, dict))
            elif isinstance(page, dict):
                flattened.append(page)
        return flattened

    def identity(self) -> str:
        data = self.api("user")
        return str(data.get("login") or "")

    def activate(self, repo: str, issue_number: int) -> None:
        self.run(
            [
                "issue",
                "edit",
                str(issue_number),
                "--repo",
                repo,
                "--add-label",
                "aiops:todo",
            ]
        )


THREAD_QUERY = """
query($owner:String!,$name:String!,$number:Int!,$after:String){
  repository(owner:$owner,name:$name){
    pullRequest(number:$number){
      reviewThreads(first:100,after:$after){
        nodes{
          id isResolved isOutdated
          comments(first:100){nodes{author{login} body createdAt url}}
        }
        pageInfo{hasNextPage endCursor}
      }
    }
  }
}
"""


def fetch_review_threads(
    client: GhClient, repo: str, pr_number: int
) -> list[dict[str, Any]]:
    owner, name = repo.split("/", 1)
    cursor: str | None = None
    seen_cursors: set[str] = set()
    threads: list[dict[str, Any]] = []
    while True:
        args = [
            "api",
            "graphql",
            "-f",
            f"query={THREAD_QUERY}",
            "-F",
            f"owner={owner}",
            "-F",
            f"name={name}",
            "-F",
            f"number={pr_number}",
        ]
        if cursor is not None:
            args.extend(["-F", f"after={cursor}"])
        data = client.json(args)
        connection = (
            ((data.get("data") or {}).get("repository") or {}).get("pullRequest") or {}
        ).get("reviewThreads") or {}
        threads.extend(connection.get("nodes") or [])
        page = connection.get("pageInfo") or {}
        if not page.get("hasNextPage"):
            return threads
        next_cursor = page.get("endCursor")
        if not next_cursor:
            raise RuntimeError("reviewThreads pagination hasNextPage without endCursor")
        if next_cursor == cursor or next_cursor in seen_cursors:
            raise RuntimeError("reviewThreads pagination cursor did not advance")
        seen_cursors.add(next_cursor)
        cursor = next_cursor


def discover_pr_number(repo: str, comments: list[dict[str, Any]]) -> int | None:
    pattern = re.compile(rf"https://github\.com/{re.escape(repo)}/pull/(\d+)")
    for comment in reversed(comments):
        match = pattern.search(str(comment.get("body") or ""))
        if match:
            return int(match.group(1))
    return None


def forge_snapshot(client: GhClient, repo: str, issue_number: int) -> dict[str, Any]:
    issue = client.api(f"repos/{repo}/issues/{issue_number}")
    issue_comments = client.paginated(
        f"repos/{repo}/issues/{issue_number}/comments?per_page=100"
    )
    snapshot: dict[str, Any] = {"issue": issue, "issue_comments": issue_comments}
    pr_number = discover_pr_number(repo, issue_comments)
    if pr_number is None:
        return snapshot
    pr = client.json(
        [
            "pr",
            "view",
            str(pr_number),
            "--repo",
            repo,
            "--json",
            "number,state,author,headRefOid,baseRefOid,baseRefName,mergedAt,"
            "statusCheckRollup,autoMergeRequest,url",
        ]
    )
    snapshot.update(
        {
            "pr": pr,
            "pr_comments": client.paginated(
                f"repos/{repo}/issues/{pr_number}/comments?per_page=100"
            ),
            "reviews": client.paginated(
                f"repos/{repo}/pulls/{pr_number}/reviews?per_page=100"
            ),
            "review_threads": fetch_review_threads(client, repo, pr_number),
        }
    )
    return snapshot


def labels(issue: dict[str, Any]) -> set[str]:
    return {
        str(item.get("name") or "")
        for item in (issue.get("labels") or [])
        if isinstance(item, dict)
    }


def is_closed(snapshot: dict[str, Any]) -> bool:
    return str((snapshot.get("issue") or {}).get("state") or "").lower() == "closed"


def native_close_breach(snapshot: dict[str, Any]) -> LimitBreach | None:
    if not is_closed(snapshot):
        return None
    pr = snapshot.get("pr")
    if not isinstance(pr, dict) or not pr.get("mergedAt"):
        return LimitBreach("issue_closed_without_merged_pr", 1, 0)
    return None


def tuple_from_snapshot(snapshot: dict[str, Any]) -> ReviewTuple | None:
    pr = snapshot.get("pr") or {}
    if not pr:
        return None
    values = (pr.get("headRefOid"), pr.get("baseRefOid"), pr.get("baseRefName"))
    if not all(values):
        raise RuntimeError(f"PR tuple is incomplete: {values!r}")
    return ReviewTuple(*map(str, values))


class Supervisor:
    def __init__(self, args: argparse.Namespace):
        self.args = args
        self.run_root = Path(args.run_root).resolve()
        self.event_log = self.run_root / "evidence" / "events.jsonl"
        self.operator = GhClient(Path(args.operator_gh_config_dir).resolve())
        self.workers: list[WorkerProcess] = []
        self.shutdown_completed = False
        self.trigger_tuples: dict[int, ReviewTuple] = {}
        self.workflow_seen = {"maker": False, "reviewer": False}

    def specs(self) -> list[WorkerSpec]:
        return [
            WorkerSpec(
                "maker",
                self.args.maker_port,
                Path(self.args.maker_workflow).resolve(),
                self.args.maker_workflow_sha256,
                Path(self.args.maker_gh_config_dir).resolve(),
                self.args.maker_login,
                Path(self.args.maker_mirror_root).resolve(),
                "AIOPS_BENCH_MAKER_GITHUB_TOKEN",
            ),
            WorkerSpec(
                "reviewer",
                self.args.reviewer_port,
                Path(self.args.reviewer_workflow).resolve(),
                self.args.reviewer_workflow_sha256,
                Path(self.args.reviewer_gh_config_dir).resolve(),
                self.args.reviewer_login,
                Path(self.args.reviewer_mirror_root).resolve(),
                "AIOPS_BENCH_REVIEWER_GITHUB_TOKEN",
            ),
        ]

    def verify_files(self) -> None:
        worker_bin = Path(self.args.worker_bin).resolve()
        if not worker_bin.is_file() or not os.access(worker_bin, os.X_OK):
            raise RuntimeError(f"worker binary is not executable: {worker_bin}")
        version = subprocess.run(
            [str(worker_bin), "--version"],
            text=True,
            capture_output=True,
            timeout=10,
            check=False,
        )
        if version.returncode != 0 or version.stdout.strip() != "v0.1.16":
            raise RuntimeError(
                f"worker version is {version.stdout.strip()!r}; want 'v0.1.16'"
            )
        for spec in self.specs():
            if not spec.workflow.is_file():
                raise RuntimeError(f"{spec.role} workflow is missing: {spec.workflow}")
            if spec.workflow.stat().st_mode & 0o222:
                raise RuntimeError(f"{spec.role} workflow must be read-only")
            actual = sha256_file(spec.workflow)
            if actual != spec.workflow_sha256:
                raise RuntimeError(
                    f"{spec.role} workflow sha256 {actual} != {spec.workflow_sha256}"
                )
        append_event(
            self.event_log,
            {
                "event": "preflight_files",
                "worker_version": version.stdout.strip(),
                "worker_sha256": sha256_file(worker_bin),
                "workflows": {
                    spec.role: {
                        "path": str(spec.workflow),
                        "sha256": spec.workflow_sha256,
                        "mode": oct(spec.workflow.stat().st_mode & 0o777),
                    }
                    for spec in self.specs()
                },
            },
        )

    def verify_identities_and_initial_state(self) -> None:
        clients = {
            "maker": GhClient(Path(self.args.maker_gh_config_dir).resolve()),
            "reviewer": GhClient(Path(self.args.reviewer_gh_config_dir).resolve()),
            "operator": self.operator,
        }
        expected = {
            "maker": self.args.maker_login,
            "reviewer": self.args.reviewer_login,
            "operator": self.args.operator_login,
        }
        observed = {role: client.identity() for role, client in clients.items()}
        validate_role_identities(observed, expected)
        permissions = {
            role: client.api(f"repos/{self.args.repo}").get("permissions") or {}
            for role, client in clients.items()
        }
        if not permissions["maker"].get("push"):
            raise RuntimeError("maker lacks push permission")
        if not permissions["operator"].get("triage"):
            raise RuntimeError(
                f"operator lacks issue-triage access: {permissions['operator']}"
            )
        issues = [
            self.operator.api(f"repos/{self.args.repo}/issues/{number}")
            for number in self.args.issues
        ]
        if any(
            issue.get("state") != "open" or labels(issue) & ACTIVE_LABELS
            for issue in issues
        ):
            raise RuntimeError("issues must be open with no active lifecycle labels")
        observed_hashes = [canonical_issue_hash(issue) for issue in issues]
        if observed_hashes != self.args.issue_json_sha256:
            raise RuntimeError(
                f"canonical issue hashes differ: {observed_hashes} != {self.args.issue_json_sha256}"
            )
        append_event(
            self.event_log,
            {
                "event": "preflight_forge",
                "identities": observed,
                "permissions": permissions,
                "issues": [
                    {
                        "number": issue.get("number"),
                        "state": issue.get("state"),
                        "labels": sorted(labels(issue)),
                        "canonical_sha256": canonical_issue_hash(issue),
                    }
                    for issue in issues
                ],
            },
        )

    def verify_run_directories(self) -> None:
        directories: dict[str, Path] = {}
        for spec in self.specs():
            directories[f"{spec.role}_workspace"] = workflow_workspace_root(
                spec.workflow
            )
            directories[f"{spec.role}_mirror"] = spec.mirror_root
        manifest = validate_fresh_directories(directories)
        append_event(
            self.event_log,
            {"event": "preflight_directories", "directories": manifest},
        )

    def start_workers(self) -> None:
        logs = self.run_root / "logs"
        logs.mkdir(parents=True, exist_ok=True)
        worker_bin = str(Path(self.args.worker_bin).resolve())
        for spec in self.specs():
            token = os.environ.get(spec.token_env)
            if not token:
                raise RuntimeError(
                    f"required secret environment {spec.token_env} is missing"
                )
            spec.mirror_root.mkdir(parents=True, exist_ok=True)
            env = os.environ.copy()
            env.pop("GH_TOKEN", None)
            env.pop("AIOPS_BENCH_MAKER_GITHUB_TOKEN", None)
            env.pop("AIOPS_BENCH_REVIEWER_GITHUB_TOKEN", None)
            env.update(
                {
                    "GITHUB_TOKEN": token,
                    "GH_CONFIG_DIR": str(spec.gh_config_dir),
                    "AIOPS_MIRROR_ROOT": str(spec.mirror_root),
                    "AIOPS_EXPECTED_GITHUB_LOGIN": spec.expected_login,
                    "AIOPS_GITHUB_REPO_CLONE_URL": self.args.clone_url,
                }
            )
            handle = (logs / f"{spec.role}-worker.log").open("w", encoding="utf-8")
            process: subprocess.Popen[str] | None = None
            try:
                with defer_spawn_signals() as pending_signals:
                    process = subprocess.Popen(
                        [worker_bin, "--port", str(spec.port), str(spec.workflow)],
                        env=env,
                        text=True,
                        stdout=handle,
                        stderr=subprocess.STDOUT,
                        start_new_session=True,
                    )
                    self.workers.append(WorkerProcess(spec, process, handle))
                if pending_signals:
                    if pending_signals[0] == signal.SIGINT:
                        raise KeyboardInterrupt("received SIGINT")
                    raise SupervisorSignal(pending_signals[0])
            except BaseException as exc:
                with shutdown_signal_guard():
                    tracked = process is not None and any(
                        worker.process is process for worker in self.workers
                    )
                    if process is not None and not tracked:
                        cleanup_error: BaseException | None = None
                        try:
                            terminate_workers(
                                [process],
                                self.event_log,
                                {
                                    "event": "breach",
                                    "reason": "worker_start_interrupted",
                                    "role": spec.role,
                                    "error": str(exc),
                                },
                                grace_seconds=self.args.term_grace_seconds,
                            )
                        except BaseException as stop_error:
                            cleanup_error = stop_error
                        if cleanup_error is not None and not isinstance(
                            cleanup_error, WorkersStoppedError
                        ):
                            self.workers = [
                                *self.workers,
                                WorkerProcess(spec, process, handle),
                            ]
                            raise cleanup_error
                        close_error: BaseException | None = None
                        try:
                            handle.close()
                        except BaseException as log_error:
                            close_error = log_error
                        if cleanup_error is not None:
                            if close_error is not None:
                                cleanup_error.add_note(
                                    f"worker log close failed: {close_error}"
                                )
                            raise cleanup_error
                        if close_error is not None:
                            raise WorkerLogCloseError([close_error]) from close_error
                    elif process is None:
                        handle.close()
                raise
        append_event(
            self.event_log,
            {
                "event": "workers_started",
                "workers": [
                    {
                        "role": worker.spec.role,
                        "pid": worker.process.pid,
                        "port": worker.spec.port,
                        "workflow": str(worker.spec.workflow),
                    }
                    for worker in self.workers
                ],
            },
        )

    def states(self) -> list[dict[str, Any]]:
        result = []
        for worker in self.workers:
            if worker.process.poll() is not None:
                raise RuntimeError(
                    f"{worker.spec.role} worker exited unexpectedly with {worker.process.returncode}"
                )
            state = fetch_json(
                f"http://127.0.0.1:{worker.spec.port}/api/v1/state",
                timeout=min(0.1, self.args.state_poll_seconds / 2),
            )
            validate_state_payload(state)
            result.append(state)
        return result

    def wait_ready(self) -> list[dict[str, Any]]:
        deadline = time.monotonic() + self.args.ready_timeout_seconds
        while time.monotonic() < deadline:
            try:
                for worker in self.workers:
                    if worker.process.poll() is not None:
                        raise RuntimeError(
                            f"{worker.spec.role} worker exited before readiness"
                        )
                    fetch_text(f"http://127.0.0.1:{worker.spec.port}/readyz")
                states = self.states()
                for state in states:
                    if state.get("version") != "v0.1.16":
                        raise RuntimeError(f"state version is {state.get('version')!r}")
                    if (
                        state.get("running")
                        or state.get("blocked")
                        or state.get("retrying")
                    ):
                        raise RuntimeError("worker was not quiescent before activation")
                    if token_total(state) != 0:
                        raise RuntimeError(
                            "worker token total was non-zero before activation"
                        )
                append_event(
                    self.event_log, {"event": "workers_ready", "states": states}
                )
                return states
            except RuntimeError:
                time.sleep(0.1)
        raise RuntimeError("workers did not become ready before deadline")

    def ensure_workflows_unchanged(self) -> None:
        for spec in self.specs():
            if sha256_file(spec.workflow) != spec.workflow_sha256:
                raise RuntimeError(f"{spec.role} workflow changed after preflight")

    def record_state_change(
        self, states: list[dict[str, Any]], previous_signature: str | None
    ) -> str:
        signature = state_fingerprint(states)
        if signature != previous_signature:
            append_event(self.event_log, {"event": "worker_state", "states": states})
        return signature

    def observe_workflow_bindings(
        self, states: list[dict[str, Any]], issue_number: int
    ) -> None:
        for worker, state in zip(self.workers, states, strict=True):
            seen = validate_workflow_rows(
                state, issue_number, str(worker.spec.workflow)
            )
            self.workflow_seen[worker.spec.role] |= seen
            if (
                claim_count([state], issue_number)
                and not self.workflow_seen[worker.spec.role]
            ):
                raise RuntimeError(
                    f"{worker.spec.role} claim appeared without workflow path/source evidence"
                )

    def assign_triggers(
        self, snapshot: dict[str, Any], review_tuple: ReviewTuple
    ) -> tuple[dict[str, Any] | None, LimitBreach | None]:
        comments = snapshot.get("pr_comments") or []
        for comment in comments:
            comment_id = int(comment.get("id") or 0)
            user = comment.get("user") or {}
            if (
                comment_id
                and comment_id not in self.trigger_tuples
                and str(comment.get("body") or "").strip() == "@codex review"
                and user.get("login") == self.args.reviewer_login
            ):
                bound = checkpoint_tuple_for_trigger(
                    comment, snapshot.get("reviews") or [], self.args.reviewer_login
                )
                if bound is None:
                    return None, LimitBreach(
                        "external_trigger_without_checkpoint", 1, 0
                    )
                self.trigger_tuples[comment_id] = bound
        current = [
            comment
            for comment in comments
            if self.trigger_tuples.get(int(comment.get("id") or 0)) == review_tuple
        ]
        if len(current) > 1:
            return None, LimitBreach("duplicate_external_trigger", len(current), 1)
        if not current:
            return None, None
        return current[0], None

    def external_status(
        self, snapshot: dict[str, Any]
    ) -> tuple[bool, LimitBreach | None]:
        review_tuple = tuple_from_snapshot(snapshot)
        if review_tuple is None:
            return False, None
        trigger, breach = self.assign_triggers(snapshot, review_tuple)
        if breach or trigger is None:
            return False, breach
        created = trigger.get("created_at")
        if not created:
            return False, LimitBreach("external_trigger_missing_time", 1, 0)
        triggered_at = parse_time(str(created))
        signal_review = reliable_external_review(
            snapshot.get("reviews") or [], review_tuple, triggered_at
        )
        if signal_review is not None:
            return True, None
        waited = (datetime.now(timezone.utc) - triggered_at).total_seconds()
        if waited >= MAX_EXTERNAL_WAIT_SECONDS:
            return False, LimitBreach(
                "external_review_signal_timeout", waited, MAX_EXTERNAL_WAIT_SECONDS
            )
        return False, None

    def activate_issue(
        self, issue_number: int, states: list[dict[str, Any]]
    ) -> tuple[list[int], float]:
        issue = self.operator.api(f"repos/{self.args.repo}/issues/{issue_number}")
        expected_hash = self.args.issue_json_sha256[
            self.args.issues.index(issue_number)
        ]
        if canonical_issue_hash(issue) != expected_hash:
            raise RuntimeError(
                f"issue {issue_number} canonical content changed before activation"
            )
        if issue.get("state") != "open" or labels(issue) & ACTIVE_LABELS:
            raise RuntimeError(
                f"issue {issue_number} is not inactive/open before activation"
            )
        baselines = [token_total(state) for state in states]
        started = time.monotonic()
        append_event(
            self.event_log,
            {
                "event": "activation_requested",
                "issue": issue_number,
                "operator": self.args.operator_login,
                "token_baselines": baselines,
            },
        )
        self.operator.activate(self.args.repo, issue_number)
        append_event(
            self.event_log,
            {"event": "activation_completed", "issue": issue_number},
        )
        return baselines, started

    def run_issue(
        self, issue_number: int, states: list[dict[str, Any]]
    ) -> tuple[bool, list[dict[str, Any]]]:
        baselines, started = self.activate_issue(issue_number, states)
        previous_totals = baselines[:]
        last_below_states = states
        state_signature: str | None = None
        forge: dict[str, Any] = {"issue": {"state": "open"}}
        external_satisfied = False
        forge_poller = ForgePoller(
            self.operator,
            self.args.repo,
            issue_number,
            interval_seconds=self.args.forge_poll_seconds,
            timeout_seconds=self.args.forge_request_timeout_seconds,
        )
        breach: LimitBreach | None = None
        while True:
            breach = None
            cycle_started = time.monotonic()
            try:
                states = self.states()
                self.ensure_workflows_unchanged()
                validate_active_rows(states, issue_number)
                self.observe_workflow_bindings(states, issue_number)
            except Exception as exc:
                breach = LimitBreach("state_observation_failed", 1, 0)
                self.abort(issue_number, breach, states, {"error": str(exc)})
                return False, states

            now = time.monotonic()
            try:
                for index, state in enumerate(states):
                    current = token_total(state)
                    if current < previous_totals[index]:
                        raise CounterRegressionError(
                            f"worker {index} token counter regressed "
                            f"from {previous_totals[index]} to {current}"
                        )
                    previous_totals[index] = current
                observed_tokens = validate_token_accounting(
                    states, baselines, issue_number
                )
                if observed_tokens <= MAX_TOKENS_PER_ISSUE:
                    last_below_states = states
                breach = evaluate_limits(
                    states,
                    baselines,
                    issue_number=issue_number,
                    elapsed_seconds=now - started,
                    issue_closed=is_closed(forge),
                )
            except CounterRegressionError as exc:
                breach = LimitBreach("counter_regression", 1, 0)
                self.abort(
                    issue_number, breach, states, {"error": str(exc), "forge": forge}
                )
                return False, states
            except ValueError as exc:
                breach = LimitBreach("token_accounting_failed", 1, 0)
                self.abort(
                    issue_number, breach, states, {"error": str(exc), "forge": forge}
                )
                return False, states
            if breach is not None:
                self.abort(
                    issue_number,
                    breach,
                    states,
                    {"forge": forge, "last_below_states": last_below_states},
                )
                return False, states

            try:
                state_signature = self.record_state_change(states, state_signature)
            except Exception as exc:
                breach = LimitBreach("state_persistence_failed", 1, 0)
                self.abort(issue_number, breach, states, {"error": str(exc)})
                return False, states

            try:
                forge_poller.maybe_start(now)
                snapshot = forge_poller.take(now)
            except Exception as exc:
                breach = LimitBreach("forge_observation_failed", 1, 0)
                self.abort(issue_number, breach, states, {"error": str(exc)})
                return False, states
            if snapshot is not None:
                forge = snapshot
                append_event(
                    self.event_log,
                    {"event": "forge_state", "issue": issue_number, "snapshot": forge},
                )
                breach = native_close_breach(forge)
                if breach is not None:
                    self.abort(issue_number, breach, states, {"forge": forge})
                    return False, states
                external_satisfied, breach = self.external_status(forge)
                if breach is not None:
                    self.abort(issue_number, breach, states, {"forge": forge})
                    return False, states

            closed = is_closed(forge)
            if closed and not issue_active(states, issue_number):
                if not external_satisfied:
                    breach = LimitBreach("external_review_required", 0, 1)
                    self.abort(issue_number, breach, states, {"forge": forge})
                    return False, states
                append_event(
                    self.event_log,
                    {
                        "event": "issue_completed",
                        "issue": issue_number,
                        "elapsed_seconds": now - started,
                        "claims": claim_count(states, issue_number),
                        "worker_tokens": token_delta(states, baselines),
                        "forge": forge,
                    },
                )
                return True, states
            delay = self.args.state_poll_seconds - (time.monotonic() - cycle_started)
            if delay > 0:
                time.sleep(delay)

    def stop_workers(self, event: dict[str, Any]) -> None:
        termination_error: BaseException | None = None
        close_errors: list[BaseException] = []
        with shutdown_signal_guard():
            try:
                terminate_workers(
                    self.workers,
                    self.event_log,
                    event,
                    grace_seconds=self.args.term_grace_seconds,
                )
            except BaseException as exc:
                termination_error = exc
            for worker in self.workers:
                try:
                    worker.log_handle.close()
                except BaseException as exc:
                    close_errors.append(exc)
            self.shutdown_completed = termination_error is None or isinstance(
                termination_error, WorkersStoppedError
            )
        if termination_error is not None:
            for error in close_errors:
                termination_error.add_note(f"worker log close failed: {error}")
            raise termination_error
        if close_errors:
            raise WorkerLogCloseError(close_errors) from close_errors[0]

    def abort(
        self,
        issue_number: int,
        breach: LimitBreach,
        states: list[dict[str, Any]],
        extra: dict[str, Any],
    ) -> None:
        detected = time.monotonic_ns()
        event = {
            "event": "breach",
            "issue": issue_number,
            "reason": breach.reason,
            "observed": breach.observed,
            "limit": breach.limit,
            "detected_monotonic_ns": detected,
            "states": states,
            **extra,
        }
        self.stop_workers(event)

    def run(self) -> int:
        self.run_root.mkdir(parents=True, exist_ok=True)
        self.verify_files()
        self.verify_run_directories()
        self.verify_identities_and_initial_state()
        try:
            self.start_workers()
            states = self.wait_ready()
            for issue_number in self.args.issues:
                completed, states = self.run_issue(issue_number, states)
                if not completed:
                    return 3
            self.ensure_workflows_unchanged()
            self.stop_workers({"event": "arm_completed", "issues": self.args.issues})
            return 0
        except BaseException as exc:
            cleanup_error: BaseException | None = None
            if self.workers and not self.shutdown_completed:
                try:
                    self.stop_workers(
                        {
                            "event": "breach",
                            "reason": "supervisor_failed",
                            "error": str(exc),
                        }
                    )
                except BaseException as stop_error:
                    cleanup_error = stop_error
            if cleanup_error is not None:
                if isinstance(exc, TerminationEvidenceError) and isinstance(
                    cleanup_error, WorkersStoppedError
                ):
                    exc.add_note(f"additional cleanup error: {cleanup_error}")
                    raise exc from cleanup_error
                raise cleanup_error
            raise


def parse_args(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--run-root", required=True)
    parser.add_argument("--repo", required=True)
    parser.add_argument("--clone-url", required=True)
    parser.add_argument("--worker-bin", required=True)
    parser.add_argument("--issues", type=int, nargs=2, default=[1, 2])
    parser.add_argument("--issue-json-sha256", nargs=2, required=True)
    parser.add_argument("--maker-workflow", required=True)
    parser.add_argument("--reviewer-workflow", required=True)
    parser.add_argument("--maker-workflow-sha256", required=True)
    parser.add_argument("--reviewer-workflow-sha256", required=True)
    parser.add_argument("--maker-gh-config-dir", required=True)
    parser.add_argument("--reviewer-gh-config-dir", required=True)
    parser.add_argument("--operator-gh-config-dir", required=True)
    parser.add_argument("--maker-mirror-root", required=True)
    parser.add_argument("--reviewer-mirror-root", required=True)
    parser.add_argument("--maker-login", default="xrf-9527")
    parser.add_argument("--reviewer-login", default="zjlgdx")
    parser.add_argument("--operator-login", default="zjlgdx")
    parser.add_argument("--maker-port", type=int, default=4928)
    parser.add_argument("--reviewer-port", type=int, default=4929)
    parser.add_argument("--state-poll-seconds", type=float, default=0.25)
    parser.add_argument("--forge-poll-seconds", type=float, default=5.0)
    parser.add_argument("--forge-request-timeout-seconds", type=float, default=5.0)
    parser.add_argument("--ready-timeout-seconds", type=float, default=60.0)
    parser.add_argument("--term-grace-seconds", type=float, default=5.0)
    args = parser.parse_args(argv)
    if args.state_poll_seconds <= 0 or args.state_poll_seconds > 0.25:
        parser.error("--state-poll-seconds must be >0 and <=0.25")
    if args.forge_poll_seconds <= 0 or args.forge_poll_seconds > 5:
        parser.error("--forge-poll-seconds must be >0 and <=5")
    if (
        args.forge_request_timeout_seconds <= 0
        or args.forge_request_timeout_seconds > 5
    ):
        parser.error("--forge-request-timeout-seconds must be >0 and <=5")
    return args


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv or sys.argv[1:])
    try:
        with termination_signal_handlers():
            return Supervisor(args).run()
    except Exception as exc:
        print(f"supervisor failed: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
