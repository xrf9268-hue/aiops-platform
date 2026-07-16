import importlib.util
import hashlib
import http.server
import json
import os
import signal
import subprocess
import sys
import tempfile
import threading
import time
import types
import unittest
from datetime import datetime, timezone
from pathlib import Path
from unittest import mock


MODULE_PATH = Path(__file__).with_name("supervisor.py")
SPEC = importlib.util.spec_from_file_location("high_assurance_supervisor", MODULE_PATH)
supervisor = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
sys.modules[SPEC.name] = supervisor
SPEC.loader.exec_module(supervisor)


def state(*, total=0, completed=None, running=None, blocked=None, retrying=None):
    return {
        "codex_totals": {"total_tokens": total},
        "completed_session_usage": completed or [],
        "running": running or [],
        "blocked": blocked or [],
        "retrying": retrying or [],
    }


def row(number, **extra):
    return {"issue_identifier": f"#{number}", **extra}


class AccountingTests(unittest.TestCase):
    def test_claim_count_counts_rows_not_session_id_values(self):
        maker = state(
            completed=[row(1, session_id="same")],
            running=[row(1, session_id="same")],
            blocked=[row(2, session_id="same")],
        )
        reviewer = state(
            completed=[row(1, session_id="same")],
            blocked=[row(1, session_id="")],
        )

        self.assertEqual(supervisor.claim_count([maker, reviewer], 1), 4)
        self.assertEqual(supervisor.claim_count([maker, reviewer], 2), 1)

    def test_token_delta_sums_process_deltas(self):
        baselines = [100, 200]
        states = [state(total=1_000_100), state(total=2_500_101)]

        self.assertEqual(supervisor.token_delta(states, baselines), 3_499_901)

    def test_token_delta_fails_closed_on_counter_regression(self):
        with self.assertRaisesRegex(supervisor.CounterRegressionError, "regressed"):
            supervisor.token_delta([state(total=99), state(total=200)], [100, 200])

    def test_process_token_delta_must_match_issue_attributed_usage(self):
        states = [
            state(total=100, completed=[row(1, tokens={"total_tokens": 99})]),
            state(total=200, running=[row(1, tokens={"total_tokens": 200})]),
        ]

        with self.assertRaisesRegex(ValueError, "token accounting mismatch"):
            supervisor.validate_token_accounting(states, [0, 0], 1)

    def test_issue_attributed_usage_matches_process_delta(self):
        states = [
            state(total=100, completed=[row(1, tokens={"total_tokens": 100})]),
            state(total=200, blocked=[row(1, tokens={"total_tokens": 200})]),
        ]

        self.assertEqual(supervisor.validate_token_accounting(states, [0, 0], 1), 300)

    def test_limit_evaluation_stops_before_fifth_continuation(self):
        maker = state(completed=[row(1), row(1)])
        reviewer = state(
            completed=[row(1), row(1)],
            retrying=[row(1, kind="continuation")],
        )

        breach = supervisor.evaluate_limits(
            [maker, reviewer],
            [0, 0],
            issue_number=1,
            elapsed_seconds=10,
            issue_closed=False,
        )

        self.assertEqual(breach.reason, "worker_sessions_exhausted")
        self.assertEqual(breach.observed, 4)

    def test_limit_evaluation_stops_on_first_observed_token_crossing(self):
        breach = supervisor.evaluate_limits(
            [state(total=1_000_000), state(total=2_500_001)],
            [0, 0],
            issue_number=1,
            elapsed_seconds=10,
            issue_closed=False,
        )

        self.assertEqual(breach.reason, "worker_tokens_exceeded")
        self.assertEqual(breach.observed, 3_500_001)

    def test_limit_evaluation_stops_at_wall_limit(self):
        breach = supervisor.evaluate_limits(
            [state(), state()],
            [0, 0],
            issue_number=1,
            elapsed_seconds=1_800,
            issue_closed=False,
        )

        self.assertEqual(breach.reason, "issue_wall_exceeded")

    def test_closed_but_not_quiescent_still_obeys_end_to_end_wall_limit(self):
        breach = supervisor.evaluate_limits(
            [state(running=[row(1)]), state()],
            [0, 0],
            issue_number=1,
            elapsed_seconds=1_800,
            issue_closed=True,
        )

        self.assertEqual(breach.reason, "issue_wall_exceeded")

    def test_closed_issue_is_not_quiescent_until_live_rows_clear(self):
        self.assertTrue(supervisor.issue_active([state(running=[row(1)])], 1))
        self.assertFalse(supervisor.issue_active([state(completed=[row(1)])], 1))

    def test_canonical_issue_hash_ignores_mutable_forge_fields(self):
        issue = {"number": 1, "title": "title", "body": "body", "state": "open"}
        expected = hashlib.sha256(
            b'{"body":"body","number":1,"title":"title"}\n'
        ).hexdigest()

        self.assertEqual(supervisor.canonical_issue_hash(issue), expected)

    def test_state_payload_rejects_missing_token_counter_or_claim_arrays(self):
        valid = state()
        valid["version"] = "v0.1.16"
        supervisor.validate_state_payload(valid)

        missing_counter = dict(valid)
        missing_counter["codex_totals"] = {}
        with self.assertRaisesRegex(ValueError, "total_tokens"):
            supervisor.validate_state_payload(missing_counter)

        missing_running = dict(valid)
        del missing_running["running"]
        with self.assertRaisesRegex(ValueError, "running"):
            supervisor.validate_state_payload(missing_running)

    def test_personal_repo_role_contract_reuses_reviewer_as_operator(self):
        observed = {
            "maker": "xrf-9527",
            "reviewer": "zjlgdx",
            "operator": "zjlgdx",
        }

        supervisor.validate_role_identities(observed, observed)

    def test_role_contract_still_requires_distinct_maker(self):
        observed = {
            "maker": "zjlgdx",
            "reviewer": "zjlgdx",
            "operator": "zjlgdx",
        }

        with self.assertRaisesRegex(RuntimeError, "maker and reviewer"):
            supervisor.validate_role_identities(observed, observed)


class RunDirectoryTests(unittest.TestCase):
    def write_workflow(self, path, workspace_root):
        path.write_text(
            f"---\nworkspace:\n  root: {workspace_root}\nagent:\n  max_concurrent_agents: 1\n---\n",
            encoding="utf-8",
        )

    def test_workspace_root_is_read_from_front_matter(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            workflow = root / "WORKFLOW.md"
            expected = root / "workspace"
            self.write_workflow(workflow, expected)

            self.assertEqual(
                supervisor.workflow_workspace_root(workflow), expected.resolve()
            )

    def test_fresh_directories_reject_duplicate_resolved_paths(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            shared = root / "shared"

            with self.assertRaisesRegex(ValueError, "must be pairwise distinct"):
                supervisor.validate_fresh_directories(
                    {
                        "maker_workspace": shared,
                        "reviewer_workspace": root / "reviewer-workspace",
                        "maker_mirror": root / "maker-mirror",
                        "reviewer_mirror": shared,
                    }
                )

    def test_fresh_directories_reject_nested_paths(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            shared = root / "shared"

            with self.assertRaisesRegex(ValueError, "must not overlap"):
                supervisor.validate_fresh_directories(
                    {
                        "maker_workspace": shared,
                        "reviewer_workspace": shared / "reviewer",
                        "maker_mirror": root / "maker-mirror",
                        "reviewer_mirror": root / "reviewer-mirror",
                    }
                )

    def test_fresh_directories_reject_nonempty_path(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            nonempty = root / "maker-workspace"
            nonempty.mkdir()
            (nonempty / "stale").write_text("stale", encoding="utf-8")

            with self.assertRaisesRegex(ValueError, "must be empty"):
                supervisor.validate_fresh_directories(
                    {
                        "maker_workspace": nonempty,
                        "reviewer_workspace": root / "reviewer-workspace",
                        "maker_mirror": root / "maker-mirror",
                        "reviewer_mirror": root / "reviewer-mirror",
                    }
                )

    def test_supervisor_persists_distinct_empty_directory_evidence(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            maker_workspace = root / "maker-workspace"
            reviewer_workspace = root / "reviewer-workspace"
            maker_mirror = root / "maker-mirror"
            reviewer_mirror = root / "reviewer-mirror"
            maker_workspace.mkdir()
            maker_mirror.mkdir()
            maker_workflow = root / "maker-WORKFLOW.md"
            reviewer_workflow = root / "reviewer-WORKFLOW.md"
            self.write_workflow(maker_workflow, maker_workspace)
            self.write_workflow(reviewer_workflow, reviewer_workspace)
            args = types.SimpleNamespace(
                run_root=str(root), operator_gh_config_dir=str(root / "operator-auth")
            )

            class DirectorySupervisor(supervisor.Supervisor):
                def specs(self):
                    return [
                        supervisor.WorkerSpec(
                            "maker",
                            4928,
                            maker_workflow,
                            "unused",
                            root / "maker-auth",
                            "maker",
                            maker_mirror,
                            "TEST_MAKER_TOKEN",
                        ),
                        supervisor.WorkerSpec(
                            "reviewer",
                            4929,
                            reviewer_workflow,
                            "unused",
                            root / "reviewer-auth",
                            "reviewer",
                            reviewer_mirror,
                            "TEST_REVIEWER_TOKEN",
                        ),
                    ]

            loop = DirectorySupervisor(args)
            loop.verify_run_directories()

            event = json.loads(loop.event_log.read_text(encoding="utf-8"))
            self.assertEqual(event["event"], "preflight_directories")
            self.assertEqual(
                set(event["directories"]),
                {
                    "maker_workspace",
                    "reviewer_workspace",
                    "maker_mirror",
                    "reviewer_mirror",
                },
            )
            self.assertTrue(event["directories"]["maker_workspace"]["existed"])
            self.assertFalse(event["directories"]["reviewer_workspace"]["existed"])
            self.assertTrue(
                all(item["empty"] for item in event["directories"].values())
            )

    def test_run_wires_directory_gate_before_forge_or_workers(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            args = types.SimpleNamespace(
                run_root=str(root), operator_gh_config_dir=str(root / "operator-auth")
            )
            calls = []

            class GateSupervisor(supervisor.Supervisor):
                def verify_files(self):
                    calls.append("files")

                def verify_run_directories(self):
                    calls.append("directories")
                    raise RuntimeError("directory gate stopped activation")

                def verify_identities_and_initial_state(self):
                    calls.append("forge")

                def start_workers(self):
                    calls.append("workers")

            loop = GateSupervisor(args)
            with self.assertRaisesRegex(RuntimeError, "directory gate"):
                loop.run()

            self.assertEqual(calls, ["files", "directories"])


class ExternalReviewTests(unittest.TestCase):
    def setUp(self):
        self.tuple = supervisor.ReviewTuple("head123", "base456", "main")
        self.triggered_at = datetime(2026, 7, 15, 12, 0, tzinfo=timezone.utc)

    def test_accepts_only_exact_head_bot_review_after_trigger(self):
        reviews = [
            {
                "user": {"id": supervisor.CODEX_BOT_ID, "type": "Bot"},
                "commit_id": "head123",
                "submitted_at": "2026-07-15T12:00:01Z",
            }
        ]

        signal = supervisor.reliable_external_review(
            reviews, self.tuple, self.triggered_at
        )

        self.assertIsNotNone(signal)

    def test_rejects_stale_spoofed_and_pretrigger_reviews(self):
        reviews = [
            {
                "user": {"id": supervisor.CODEX_BOT_ID, "type": "Bot"},
                "commit_id": "old-head",
                "submitted_at": "2026-07-15T12:00:01Z",
            },
            {
                "user": {
                    "id": 1,
                    "login": "chatgpt-codex-connector[bot]",
                    "type": "Bot",
                },
                "commit_id": "head123",
                "submitted_at": "2026-07-15T12:00:01Z",
            },
            {
                "user": {"id": supervisor.CODEX_BOT_ID, "type": "Bot"},
                "commit_id": "head123",
                "submitted_at": "2026-07-15T11:59:59Z",
            },
        ]

        self.assertIsNone(
            supervisor.reliable_external_review(reviews, self.tuple, self.triggered_at)
        )

    def test_comment_or_reaction_is_not_a_reliable_signal(self):
        self.assertFalse(hasattr(supervisor, "reliable_external_comment"))
        self.assertFalse(hasattr(supervisor, "reliable_external_reaction"))

    def test_trigger_binds_to_preceding_reviewer_checkpoint_not_live_tuple(self):
        trigger = {"created_at": "2026-07-15T12:00:02Z"}
        reviews = [
            {
                "user": {"login": "zjlgdx"},
                "state": "COMMENTED",
                "commit_id": "head123",
                "submitted_at": "2026-07-15T12:00:01Z",
                "body": (
                    "Reviewer checkpoint: headRefOid=head123 baseRefOid=base456 "
                    "baseRefName=main local-rubric=PASS"
                ),
            }
        ]

        bound = supervisor.checkpoint_tuple_for_trigger(trigger, reviews, "zjlgdx")

        self.assertEqual(bound, self.tuple)

    def test_same_second_checkpoint_cannot_prove_pretrigger_order(self):
        trigger = {"created_at": "2026-07-15T12:00:01Z"}
        reviews = [
            {
                "user": {"login": "zjlgdx"},
                "state": "COMMENTED",
                "commit_id": "head123",
                "submitted_at": "2026-07-15T12:00:01Z",
                "body": (
                    "Reviewer checkpoint: headRefOid=head123 baseRefOid=base456 "
                    "baseRefName=main local-rubric=PASS"
                ),
            }
        ]

        self.assertIsNone(
            supervisor.checkpoint_tuple_for_trigger(trigger, reviews, "zjlgdx")
        )


class WorkflowAndAbortTests(unittest.TestCase):
    def test_idle_state_does_not_claim_a_workflow_binding(self):
        supervisor.validate_workflow_rows(state(), 1, "/tmp/maker-WORKFLOW.md")

    def test_claim_row_must_bind_exact_file_source_and_path(self):
        with self.assertRaisesRegex(ValueError, "workflow"):
            supervisor.validate_workflow_rows(
                state(running=[row(1, workflow_source="default", workflow_path="")]),
                1,
                "/tmp/maker-WORKFLOW.md",
            )

    def test_state_fingerprint_ignores_sampling_timestamp_but_keeps_tokens(self):
        first = state(total=10)
        first["generated_at"] = "one"
        second = state(total=10)
        second["generated_at"] = "two"
        self.assertEqual(
            supervisor.state_fingerprint([first]),
            supervisor.state_fingerprint([second]),
        )
        second["codex_totals"]["total_tokens"] = 11
        self.assertNotEqual(
            supervisor.state_fingerprint([first]),
            supervisor.state_fingerprint([second]),
        )

    def test_unexpected_live_issue_fails_closed(self):
        with self.assertRaisesRegex(ValueError, "unexpected active issue"):
            supervisor.validate_active_rows(
                [state(running=[row(2, state="aiops:todo")])], issue_number=1
            )

    def test_breach_is_persisted_before_both_process_groups_are_signaled(self):
        with tempfile.TemporaryDirectory() as temp:
            event_log = Path(temp) / "events.jsonl"
            processes = [
                subprocess.Popen(
                    [sys.executable, "-c", "import time; time.sleep(60)"],
                    start_new_session=True,
                )
                for _ in range(2)
            ]
            started = time.monotonic()
            supervisor.terminate_workers(
                processes,
                event_log,
                {"event": "breach", "reason": "worker_tokens_exceeded"},
                grace_seconds=0.2,
            )
            elapsed = time.monotonic() - started

            self.assertLess(elapsed, 1.0)
            self.assertTrue(all(process.poll() is not None for process in processes))
            events = [json.loads(line) for line in event_log.read_text().splitlines()]
            self.assertEqual(events[0]["event"], "breach")
            self.assertEqual(events[1]["event"], "signal_sent")
            self.assertIn("detection_to_signal_ms", events[1])

    def test_termination_signals_group_after_worker_leader_exits(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            ready = root / "child-ready"
            terminated = root / "child-terminated"
            child_code = (
                "import pathlib, signal, time\n"
                f"ready = pathlib.Path({str(ready)!r})\n"
                f"done = pathlib.Path({str(terminated)!r})\n"
                "def stop(*_args):\n"
                "    done.write_text('term')\n"
                "    raise SystemExit(0)\n"
                "signal.signal(signal.SIGTERM, stop)\n"
                "ready.write_text('ready')\n"
                "time.sleep(60)\n"
            )
            parent_code = (
                "import pathlib, subprocess, time\n"
                f"ready = pathlib.Path({str(ready)!r})\n"
                f"child = subprocess.Popen([{sys.executable!r}, '-c', {child_code!r}])\n"
                "for _ in range(200):\n"
                "    if ready.exists():\n"
                "        break\n"
                "    time.sleep(.01)\n"
                "else:\n"
                "    raise RuntimeError('child did not become ready')\n"
                "print(child.pid, flush=True)\n"
            )
            parent = subprocess.Popen(
                [sys.executable, "-c", parent_code],
                start_new_session=True,
                text=True,
                stdout=subprocess.PIPE,
            )
            child_pid = None
            try:
                assert parent.stdout is not None
                child_pid = int(parent.stdout.readline().strip())
                parent.wait(timeout=2)
                parent.stdout.close()
                self.assertIsNotNone(parent.poll())
                supervisor.terminate_workers(
                    [parent],
                    root / "events.jsonl",
                    {"event": "breach", "reason": "test"},
                    grace_seconds=0.3,
                )
                self.assertTrue(terminated.exists())
            finally:
                if child_pid is not None:
                    try:
                        os.kill(child_pid, signal.SIGKILL)
                    except ProcessLookupError:
                        pass

    def test_review_thread_pagination_rejects_nonadvancing_cursor(self):
        class RepeatedCursorClient:
            def __init__(self):
                self.calls = 0

            def json(self, _args):
                self.calls += 1
                if self.calls > 2:
                    raise RuntimeError("test guard: pagination did not stop")
                return {
                    "data": {
                        "repository": {
                            "pullRequest": {
                                "reviewThreads": {
                                    "nodes": [],
                                    "pageInfo": {
                                        "hasNextPage": True,
                                        "endCursor": "same-cursor",
                                    },
                                }
                            }
                        }
                    }
                }

        with self.assertRaisesRegex(RuntimeError, "cursor did not advance"):
            supervisor.fetch_review_threads(RepeatedCursorClient(), "owner/repo", 1)

    def test_fake_state_servers_drive_token_crossing_and_two_process_abort(self):
        class Handler(http.server.BaseHTTPRequestHandler):
            payload = state()

            def do_GET(self):
                encoded = json.dumps(self.payload).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(encoded)))
                self.end_headers()
                self.wfile.write(encoded)

            def log_message(self, *_args):
                return

        servers = []
        threads = []
        processes = []
        with tempfile.TemporaryDirectory() as temp:
            try:
                for total in (1_000_000, 2_500_001):
                    role_handler = type(
                        f"Handler{total}",
                        (Handler,),
                        {"payload": state(total=total)},
                    )
                    server = http.server.ThreadingHTTPServer(
                        ("127.0.0.1", 0), role_handler
                    )
                    thread = threading.Thread(target=server.serve_forever, daemon=True)
                    thread.start()
                    servers.append(server)
                    threads.append(thread)
                observed = [
                    supervisor.fetch_json(
                        f"http://127.0.0.1:{server.server_port}/api/v1/state"
                    )
                    for server in servers
                ]
                breach = supervisor.evaluate_limits(
                    observed,
                    [0, 0],
                    issue_number=1,
                    elapsed_seconds=1,
                    issue_closed=False,
                )
                self.assertEqual(breach.reason, "worker_tokens_exceeded")
                processes = [
                    subprocess.Popen(
                        [sys.executable, "-c", "import time; time.sleep(60)"],
                        start_new_session=True,
                    )
                    for _ in range(2)
                ]
                started = time.monotonic()
                supervisor.terminate_workers(
                    processes,
                    Path(temp) / "events.jsonl",
                    {"event": "breach", "reason": breach.reason},
                    grace_seconds=0.2,
                )
                self.assertLess(time.monotonic() - started, 1.0)
                self.assertTrue(
                    all(process.poll() is not None for process in processes)
                )
            finally:
                for server in servers:
                    server.shutdown()
                    server.server_close()
                for thread in threads:
                    thread.join(timeout=1)
                for process in processes:
                    if process.poll() is None:
                        process.kill()

    def test_run_issue_aborts_crossing_without_waiting_for_slow_forge_io(self):
        below = [
            state(
                total=1_000_000, running=[row(1, tokens={"total_tokens": 1_000_000})]
            ),
            state(
                total=2_499_999, running=[row(1, tokens={"total_tokens": 2_499_999})]
            ),
        ]
        above = [
            state(
                total=1_000_000, running=[row(1, tokens={"total_tokens": 1_000_000})]
            ),
            state(
                total=2_500_001, running=[row(1, tokens={"total_tokens": 2_500_001})]
            ),
        ]

        class LoopSupervisor(supervisor.Supervisor):
            def __init__(self, root, processes):
                self.args = types.SimpleNamespace(
                    repo="example/repo",
                    state_poll_seconds=0.01,
                    forge_poll_seconds=0.01,
                    forge_request_timeout_seconds=0.25,
                )
                self.operator = object()
                self.event_log = Path(root) / "events.jsonl"
                self.processes = processes
                self.observations = iter((below, above))
                self.breach = None

            def activate_issue(self, _issue_number, _states):
                return [0, 0], time.monotonic()

            def states(self):
                return next(self.observations, above)

            def ensure_workflows_unchanged(self):
                return None

            def observe_workflow_bindings(self, _states, _issue_number):
                return None

            def record_state_change(self, _states, _signature):
                return "stable"

            def abort(self, _issue_number, breach, _states, _extra):
                self.breach = breach
                supervisor.terminate_workers(
                    self.processes,
                    self.event_log,
                    {"event": "breach", "reason": breach.reason},
                    grace_seconds=0.1,
                )

        with tempfile.TemporaryDirectory() as temp:
            processes = [
                subprocess.Popen(
                    [sys.executable, "-c", "import time; time.sleep(60)"],
                    start_new_session=True,
                )
                for _ in range(2)
            ]
            loop = LoopSupervisor(temp, processes)

            def slow_forge(*_args):
                time.sleep(1)
                return {"issue": {"state": "open"}}

            started = time.monotonic()
            try:
                with mock.patch.object(
                    supervisor, "forge_snapshot", side_effect=slow_forge
                ):
                    completed, _states = loop.run_issue(1, [state(), state()])
                elapsed = time.monotonic() - started
                self.assertFalse(completed)
                self.assertEqual(loop.breach.reason, "worker_tokens_exceeded")
                self.assertLess(elapsed, 0.5)
                self.assertTrue(
                    all(process.poll() is not None for process in processes)
                )
            finally:
                for process in processes:
                    if process.poll() is None:
                        process.kill()

    def test_run_issue_checks_limit_before_persisting_nonbreach_state(self):
        above = [
            state(
                total=1_000_000, running=[row(1, tokens={"total_tokens": 1_000_000})]
            ),
            state(
                total=2_500_001, running=[row(1, tokens={"total_tokens": 2_500_001})]
            ),
        ]

        class LoopSupervisor(supervisor.Supervisor):
            def __init__(self):
                self.args = types.SimpleNamespace(
                    repo="example/repo",
                    state_poll_seconds=0.01,
                    forge_poll_seconds=0.01,
                    forge_request_timeout_seconds=0.25,
                )
                self.operator = object()
                self.breach = None

            def activate_issue(self, _issue_number, _states):
                return [0, 0], time.monotonic()

            def states(self):
                return above

            def ensure_workflows_unchanged(self):
                return None

            def observe_workflow_bindings(self, _states, _issue_number):
                return None

            def record_state_change(self, _states, _signature):
                time.sleep(1)
                return "too-late"

            def abort(self, _issue_number, breach, _states, _extra):
                self.breach = breach

        loop = LoopSupervisor()
        started = time.monotonic()
        completed, _states = loop.run_issue(1, [state(), state()])

        self.assertFalse(completed)
        self.assertEqual(loop.breach.reason, "worker_tokens_exceeded")
        self.assertLess(time.monotonic() - started, 0.5)

    def test_run_issue_wires_issue_token_reconciliation(self):
        mismatch = [
            state(total=100, running=[row(1, tokens={"total_tokens": 99})]),
            state(),
        ]

        class LoopSupervisor(supervisor.Supervisor):
            def __init__(self):
                self.args = types.SimpleNamespace(
                    repo="example/repo",
                    state_poll_seconds=0.01,
                    forge_poll_seconds=0.01,
                    forge_request_timeout_seconds=0.25,
                )
                self.operator = object()
                self.breach = None

            def activate_issue(self, _issue_number, _states):
                return [0, 0], time.monotonic()

            def states(self):
                return mismatch

            def ensure_workflows_unchanged(self):
                return None

            def observe_workflow_bindings(self, _states, _issue_number):
                return None

            def record_state_change(self, _states, _signature):
                return "stable"

            def abort(self, _issue_number, breach, _states, _extra):
                self.breach = breach

        loop = LoopSupervisor()
        completed, _states = loop.run_issue(1, [state(), state()])

        self.assertFalse(completed)
        self.assertEqual(loop.breach.reason, "token_accounting_failed")

    def test_closed_issue_without_exact_tuple_external_review_aborts(self):
        class LoopSupervisor(supervisor.Supervisor):
            def __init__(self, root):
                self.args = types.SimpleNamespace(
                    repo="example/repo",
                    state_poll_seconds=0.01,
                    forge_poll_seconds=0.01,
                    forge_request_timeout_seconds=0.25,
                )
                self.operator = object()
                self.event_log = Path(root) / "events.jsonl"
                self.breach = None

            def activate_issue(self, _issue_number, _states):
                return [0, 0], time.monotonic()

            def states(self):
                return [state(), state()]

            def ensure_workflows_unchanged(self):
                return None

            def observe_workflow_bindings(self, _states, _issue_number):
                return None

            def record_state_change(self, _states, _signature):
                return "stable"

            def abort(self, _issue_number, breach, _states, _extra):
                self.breach = breach

        with tempfile.TemporaryDirectory() as temp:
            loop = LoopSupervisor(temp)
            closed_without_review = {
                "issue": {"state": "closed"},
                "pr": {
                    "headRefOid": "head",
                    "baseRefOid": "base",
                    "baseRefName": "main",
                    "mergedAt": "2026-07-16T00:00:00Z",
                },
                "pr_comments": [],
                "reviews": [],
            }
            with mock.patch.object(
                supervisor, "forge_snapshot", return_value=closed_without_review
            ):
                completed, _states = loop.run_issue(1, [state(), state()])

        self.assertFalse(completed)
        self.assertEqual(loop.breach.reason, "external_review_required")

    def test_closed_issue_without_merged_pr_is_a_native_close_breach(self):
        snapshot = {
            "issue": {"state": "closed"},
            "pr": {"state": "OPEN", "mergedAt": None},
        }

        breach = supervisor.native_close_breach(snapshot)

        self.assertEqual(breach.reason, "issue_closed_without_merged_pr")

    def test_partial_worker_start_is_cleaned_up(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            worker = root / "fake-worker"
            worker.write_text(
                f"#!{sys.executable}\nimport time\ntime.sleep(60)\n",
                encoding="utf-8",
            )
            worker.chmod(0o755)
            workflow = root / "WORKFLOW.md"
            workflow.write_text("---\n---\n", encoding="utf-8")
            args = types.SimpleNamespace(
                run_root=str(root),
                operator_gh_config_dir=str(root / "operator-auth"),
                worker_bin=str(worker),
                clone_url="https://github.com/example/repo.git",
                term_grace_seconds=0.1,
            )

            class StartFailureSupervisor(supervisor.Supervisor):
                def verify_files(self):
                    return None

                def verify_run_directories(self):
                    return None

                def verify_identities_and_initial_state(self):
                    return None

                def specs(self):
                    return [
                        supervisor.WorkerSpec(
                            "maker",
                            4928,
                            workflow,
                            "unused",
                            root / "maker-auth",
                            "maker",
                            root / "maker-mirror",
                            "TEST_MAKER_TOKEN",
                        ),
                        supervisor.WorkerSpec(
                            "reviewer",
                            4929,
                            workflow,
                            "unused",
                            root / "reviewer-auth",
                            "reviewer",
                            root / "reviewer-mirror",
                            "TEST_REVIEWER_TOKEN",
                        ),
                    ]

            loop = StartFailureSupervisor(args)
            old_maker = os.environ.get("TEST_MAKER_TOKEN")
            old_reviewer = os.environ.pop("TEST_REVIEWER_TOKEN", None)
            os.environ["TEST_MAKER_TOKEN"] = "not-a-real-token"
            try:
                with self.assertRaisesRegex(RuntimeError, "TEST_REVIEWER_TOKEN"):
                    loop.run()
                self.assertEqual(len(loop.workers), 1)
                self.assertIsNotNone(loop.workers[0].process.poll())
                self.assertTrue(loop.workers[0].log_handle.closed)
            finally:
                if loop.workers and loop.workers[0].process.poll() is None:
                    loop.workers[0].process.kill()
                if old_maker is None:
                    os.environ.pop("TEST_MAKER_TOKEN", None)
                else:
                    os.environ["TEST_MAKER_TOKEN"] = old_maker
                if old_reviewer is not None:
                    os.environ["TEST_REVIEWER_TOKEN"] = old_reviewer


if __name__ == "__main__":
    unittest.main()
