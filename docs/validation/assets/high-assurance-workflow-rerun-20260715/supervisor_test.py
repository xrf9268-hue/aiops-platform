import importlib.util
import hashlib
import http.server
import json
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from datetime import datetime, timezone
from pathlib import Path


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
        with self.assertRaisesRegex(ValueError, "regressed"):
            supervisor.token_delta([state(total=99), state(total=200)], [100, 200])

    def test_limit_evaluation_stops_before_fifth_continuation(self):
        maker = state(completed=[row(1), row(1)])
        reviewer = state(
            completed=[row(1), row(1)],
            retrying=[row(1, kind="continuation")],
        )

        breach = supervisor.evaluate_limits(
            [maker, reviewer], [0, 0], issue_number=1, elapsed_seconds=10, issue_closed=False
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
            [state(), state()], [0, 0], issue_number=1, elapsed_seconds=1_800, issue_closed=False
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

        signal = supervisor.reliable_external_review(reviews, self.tuple, self.triggered_at)

        self.assertIsNotNone(signal)

    def test_rejects_stale_spoofed_and_pretrigger_reviews(self):
        reviews = [
            {
                "user": {"id": supervisor.CODEX_BOT_ID, "type": "Bot"},
                "commit_id": "old-head",
                "submitted_at": "2026-07-15T12:00:01Z",
            },
            {
                "user": {"id": 1, "login": "chatgpt-codex-connector[bot]", "type": "Bot"},
                "commit_id": "head123",
                "submitted_at": "2026-07-15T12:00:01Z",
            },
            {
                "user": {"id": supervisor.CODEX_BOT_ID, "type": "Bot"},
                "commit_id": "head123",
                "submitted_at": "2026-07-15T11:59:59Z",
            },
        ]

        self.assertIsNone(supervisor.reliable_external_review(reviews, self.tuple, self.triggered_at))

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
        self.assertEqual(supervisor.state_fingerprint([first]), supervisor.state_fingerprint([second]))
        second["codex_totals"]["total_tokens"] = 11
        self.assertNotEqual(
            supervisor.state_fingerprint([first]), supervisor.state_fingerprint([second])
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
                    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), role_handler)
                    thread = threading.Thread(target=server.serve_forever, daemon=True)
                    thread.start()
                    servers.append(server)
                    threads.append(thread)
                observed = [
                    supervisor.fetch_json(f"http://127.0.0.1:{server.server_port}/api/v1/state")
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
                self.assertTrue(all(process.poll() is not None for process in processes))
            finally:
                for server in servers:
                    server.shutdown()
                    server.server_close()
                for thread in threads:
                    thread.join(timeout=1)
                for process in processes:
                    if process.poll() is None:
                        process.kill()


if __name__ == "__main__":
    unittest.main()
