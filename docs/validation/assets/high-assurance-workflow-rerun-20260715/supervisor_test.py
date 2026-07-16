import importlib.util
import hashlib
import http.server
import io
import json
import os
import shlex
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


def proc_stat_text(
    pid, state, process_group, session, *, num_threads=1, start_time=None
):
    if start_time is None:
        start_time = pid * 1_000
    fields = [
        state,
        "1",
        str(process_group),
        str(session),
        *(["0"] * 13),
        str(num_threads),
        "0",
        str(start_time),
    ]
    return f"{pid} (worker) child) {' '.join(fields)}\n"


def write_proc_stat(
    proc_root,
    pid,
    state,
    process_group,
    *,
    session=None,
    num_threads=1,
    start_time=None,
):
    if session is None:
        session = process_group
    process_root = proc_root / str(pid)
    process_root.mkdir(exist_ok=True)
    (process_root / "stat").write_text(
        proc_stat_text(
            pid,
            state,
            process_group,
            session,
            num_threads=num_threads,
            start_time=start_time,
        ),
        encoding="utf-8",
    )


def write_proc_visibility_proof(proc_root, *, mount_options="rw,nosuid,nodev"):
    self_root = proc_root / "self"
    self_root.mkdir(exist_ok=True)
    (self_root / "stat").write_text(
        proc_stat_text(os.getpid(), "S", os.getpgrp(), os.getsid(0)),
        encoding="utf-8",
    )
    (proc_root / "mounts").write_text(
        f"proc {proc_root.resolve()} proc {mount_options} 0 0\n",
        encoding="utf-8",
    )


class ArtifactReconstructionTests(unittest.TestCase):
    def test_summary_pins_reconstruction_manifest(self):
        asset_root = Path(__file__).parent
        manifest_path = asset_root / "at-activation-artifacts.json"
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        summary = json.loads((asset_root / "summary.json").read_text(encoding="utf-8"))
        pins = summary["pins"]

        self.assertEqual(
            hashlib.sha256(manifest_path.read_bytes()).hexdigest(),
            pins["at_activation_artifact_manifest_sha256"],
        )
        for artifact in manifest["artifacts"]:
            with self.subTest(file=artifact["published_file"]):
                prefix = Path(artifact["published_file"]).stem
                self.assertEqual(
                    artifact["published_sha256"], pins[f"published_{prefix}_sha256"]
                )
                self.assertEqual(
                    artifact["at_activation_sha256"],
                    pins[f"{prefix}_at_activation_sha256"],
                )
                self.assertEqual(
                    artifact["reconstruction_patch_sha256"],
                    pins[f"{prefix}_at_activation_patch_sha256"],
                )

    def test_patches_apply_to_committed_files_and_recreate_activation_bytes(self):
        asset_root = Path(__file__).parent
        manifest = json.loads(
            (asset_root / "at-activation-artifacts.json").read_text(encoding="utf-8")
        )

        for artifact in manifest["artifacts"]:
            with self.subTest(file=artifact["published_file"]):
                published = asset_root / artifact["published_file"]
                patch = asset_root / artifact["reconstruction_patch"]
                self.assertEqual(
                    hashlib.sha256(published.read_bytes()).hexdigest(),
                    artifact["published_sha256"],
                )
                self.assertEqual(
                    hashlib.sha256(patch.read_bytes()).hexdigest(),
                    artifact["reconstruction_patch_sha256"],
                )
                command, separator, patch_name = artifact[
                    "reconstruction_command"
                ].partition(" < ")
                self.assertEqual(separator, " < ")
                self.assertEqual(patch_name, artifact["reconstruction_patch"])
                reconstruction = subprocess.run(
                    shlex.split(command),
                    cwd=asset_root,
                    input=patch.read_bytes(),
                    capture_output=True,
                    check=False,
                )
                self.assertEqual(
                    reconstruction.returncode,
                    0,
                    reconstruction.stdout.decode() + reconstruction.stderr.decode(),
                )
                self.assertEqual(
                    hashlib.sha256(reconstruction.stdout).hexdigest(),
                    artifact["at_activation_sha256"],
                )
                headers = patch.read_text(encoding="utf-8").splitlines()[:2]
                self.assertEqual(
                    headers,
                    [
                        f"--- {artifact['published_file']}",
                        f"+++ {artifact['published_file']}",
                    ],
                )
                with tempfile.TemporaryDirectory() as temp:
                    candidate = Path(temp) / artifact["published_file"]
                    candidate.write_bytes(published.read_bytes())
                    result = subprocess.run(
                        ["patch", "-f", "-p0", "-i", str(patch.resolve())],
                        cwd=temp,
                        capture_output=True,
                        check=False,
                    )
                    self.assertEqual(
                        result.returncode,
                        0,
                        result.stdout.decode() + result.stderr.decode(),
                    )
                    self.assertEqual(
                        hashlib.sha256(candidate.read_bytes()).hexdigest(),
                        artifact["at_activation_sha256"],
                    )


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
            original_fsync = os.fsync
            original_killpg = os.killpg
            fsynced: list[int] = []

            def observe_fsync(fd):
                original_fsync(fd)
                fsynced.append(fd)

            def observe_signal(pgid, sent_signal):
                if sent_signal == signal.SIGTERM:
                    self.assertTrue(fsynced)
                return original_killpg(pgid, sent_signal)

            started = time.monotonic()
            with (
                mock.patch.object(supervisor.os, "fsync", side_effect=observe_fsync),
                mock.patch.object(supervisor.os, "killpg", side_effect=observe_signal),
            ):
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

    def test_event_log_failure_cannot_block_worker_shutdown(self):
        processes = [
            subprocess.Popen(
                [sys.executable, "-c", "import time; time.sleep(60)"],
                start_new_session=True,
            )
            for _ in range(2)
        ]
        try:
            with mock.patch.object(
                supervisor, "append_event", side_effect=OSError("disk full")
            ):
                with self.assertRaisesRegex(
                    supervisor.TerminationEvidenceError, "3 writes.*disk full"
                ) as caught:
                    supervisor.terminate_workers(
                        processes,
                        Path("unused-events.jsonl"),
                        {"event": "breach", "reason": "worker_tokens_exceeded"},
                        grace_seconds=0.2,
                    )

            self.assertTrue(all(process.poll() is not None for process in processes))
            self.assertIsInstance(caught.exception.__cause__, OSError)
            self.assertEqual(len(caught.exception.failures), 3)
        finally:
            for process in processes:
                if process.poll() is None:
                    os.killpg(process.pid, signal.SIGKILL)
                    process.wait(timeout=1)

    def test_signal_event_failure_still_reaches_sigkill_and_wait(self):
        process = subprocess.Popen(
            [
                sys.executable,
                "-c",
                "import signal,time; "
                "signal.signal(signal.SIGTERM, signal.SIG_IGN); "
                "print('ready', flush=True); time.sleep(60)",
            ],
            start_new_session=True,
            text=True,
            stdout=subprocess.PIPE,
        )
        try:
            assert process.stdout is not None
            self.assertEqual(process.stdout.readline().strip(), "ready")
            with mock.patch.object(
                supervisor,
                "append_event",
                side_effect=[None, OSError("signal log full"), None],
            ):
                with self.assertRaisesRegex(
                    supervisor.TerminationEvidenceError, "signal log full"
                ):
                    supervisor.terminate_workers(
                        [process],
                        Path("unused-events.jsonl"),
                        {"event": "breach", "reason": "test"},
                        grace_seconds=0.05,
                    )

            self.assertEqual(process.returncode, -signal.SIGKILL)
        finally:
            if process.stdout is not None:
                process.stdout.close()
            if process.poll() is None:
                os.killpg(process.pid, signal.SIGKILL)
                process.wait(timeout=1)

    def test_final_event_failure_is_reported_after_worker_stops(self):
        process = subprocess.Popen(
            [sys.executable, "-c", "import time; time.sleep(60)"],
            start_new_session=True,
        )
        try:
            with mock.patch.object(
                supervisor,
                "append_event",
                side_effect=[None, None, OSError("stopped log full")],
            ):
                with self.assertRaisesRegex(
                    supervisor.TerminationEvidenceError, "stopped log full"
                ):
                    supervisor.terminate_workers(
                        [process],
                        Path("unused-events.jsonl"),
                        {"event": "breach", "reason": "test"},
                        grace_seconds=0.2,
                    )

            self.assertIsNotNone(process.poll())
        finally:
            if process.poll() is None:
                os.killpg(process.pid, signal.SIGKILL)
                process.wait(timeout=1)

    def test_poll_and_wait_failures_are_reported_after_worker_stops(self):
        with tempfile.TemporaryDirectory() as temp:
            event_log = Path(temp) / "events.jsonl"
            process = subprocess.Popen(
                [sys.executable, "-c", "import time; time.sleep(60)"],
                start_new_session=True,
            )
            real_poll = process.poll
            real_wait = process.wait
            poll_failed = False
            wait_failed = False

            def fail_first_poll():
                nonlocal poll_failed
                if not poll_failed:
                    poll_failed = True
                    raise OSError("injected poll failure")
                return real_poll()

            def fail_first_wait(*, timeout=None):
                nonlocal wait_failed
                if not wait_failed:
                    wait_failed = True
                    raise OSError("injected wait failure")
                return real_wait(timeout=timeout)

            try:
                with (
                    mock.patch.object(process, "poll", side_effect=fail_first_poll),
                    mock.patch.object(process, "wait", side_effect=fail_first_wait),
                ):
                    with self.assertRaises(
                        supervisor.TerminationOperationError
                    ) as caught:
                        supervisor.terminate_workers(
                            [process],
                            event_log,
                            {"event": "breach", "reason": "test"},
                            grace_seconds=0.1,
                        )

                self.assertTrue(poll_failed)
                self.assertTrue(wait_failed)
                self.assertIsNotNone(real_poll())
                self.assertEqual(len(caught.exception.failures), 2)
                self.assertEqual(
                    {str(failure) for failure in caught.exception.failures},
                    {"injected poll failure", "injected wait failure"},
                )
                events = [
                    json.loads(line)
                    for line in event_log.read_text(encoding="utf-8").splitlines()
                ]
                self.assertEqual(events[-1]["event"], "workers_stopped")
                self.assertEqual(events[-1]["alive_pids"], [])
            finally:
                if real_poll() is None:
                    os.killpg(process.pid, signal.SIGKILL)
                    real_wait(timeout=1)

    def test_linux_group_scan_ignores_zombies_but_keeps_live_members(self):
        with tempfile.TemporaryDirectory() as temp:
            proc_root = Path(temp)
            write_proc_visibility_proof(proc_root)
            write_proc_stat(proc_root, 42, "Z", 42)
            write_proc_stat(proc_root, 101, "Z", 42)
            write_proc_stat(proc_root, 102, "X", 42)
            write_proc_stat(proc_root, 103, "x", 42)
            write_proc_stat(proc_root, 104, "S", 99)
            with (
                mock.patch.object(supervisor, "PROC_ROOT", proc_root, create=True),
                mock.patch.object(supervisor.sys, "platform", "linux"),
                mock.patch.object(supervisor.os, "killpg"),
            ):
                self.assertFalse(supervisor.process_group_alive(42))
                write_proc_stat(proc_root, 105, "S", 43, session=42)
                self.assertTrue(supervisor.process_group_alive(42))

    def test_linux_group_scan_treats_terminal_leader_with_threads_as_live(self):
        with tempfile.TemporaryDirectory() as temp:
            proc_root = Path(temp)
            write_proc_visibility_proof(proc_root)
            write_proc_stat(proc_root, 42, "Z", 42, num_threads=2)
            with (
                mock.patch.object(supervisor, "PROC_ROOT", proc_root, create=True),
                mock.patch.object(supervisor.sys, "platform", "linux"),
                mock.patch.object(supervisor.os, "killpg"),
            ):
                self.assertTrue(supervisor.process_group_alive(42))

    def test_linux_group_scan_fails_closed_when_proc_state_is_unknown(self):
        with tempfile.TemporaryDirectory() as temp:
            proc_root = Path(temp)
            write_proc_visibility_proof(proc_root)
            process_root = proc_root / "101"
            process_root.mkdir()
            (process_root / "stat").write_text("malformed\n", encoding="utf-8")
            with (
                mock.patch.object(supervisor, "PROC_ROOT", proc_root, create=True),
                mock.patch.object(supervisor.sys, "platform", "linux"),
                mock.patch.object(supervisor.os, "killpg"),
            ):
                self.assertTrue(supervisor.process_group_alive(42))

    def test_linux_group_scan_handles_disappeared_or_unreadable_stat(self):
        for failure in ("disappeared", "permission", "invalid_utf8"):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory() as temp:
                proc_root = Path(temp)
                write_proc_visibility_proof(proc_root)
                write_proc_stat(proc_root, 42, "Z", 42)
                write_proc_stat(proc_root, 101, "S", 42)
                write_proc_stat(proc_root, 102, "Z", 42)
                stat_path = proc_root / "101" / "stat"
                real_read_text = Path.read_text

                def read_or_fail(path, *args, **kwargs):
                    if path == stat_path and failure == "disappeared":
                        raise FileNotFoundError("injected vanished stat")
                    if path == stat_path and failure == "permission":
                        raise PermissionError("injected unreadable stat")
                    return real_read_text(path, *args, **kwargs)

                if failure == "invalid_utf8":
                    stat_path.write_bytes(b"\xff")
                with (
                    mock.patch.object(supervisor, "PROC_ROOT", proc_root, create=True),
                    mock.patch.object(supervisor.sys, "platform", "linux"),
                    mock.patch.object(Path, "read_text", new=read_or_fail),
                    mock.patch.object(supervisor.os, "killpg"),
                ):
                    self.assertEqual(
                        supervisor.process_group_alive(42),
                        failure != "disappeared",
                    )

    def test_linux_group_scan_requires_visible_stable_membership(self):
        for new_state in ("S", "Z"):
            with (
                self.subTest(new_state=new_state),
                tempfile.TemporaryDirectory() as temp,
            ):
                proc_root = Path(temp)
                write_proc_visibility_proof(proc_root)
                write_proc_stat(proc_root, 42, "Z", 42)
                write_proc_stat(proc_root, 101, "Z", 42)
                real_iterdir = Path.iterdir
                scan_count = 0

                def add_member_before_second_scan(path):
                    nonlocal scan_count
                    if path == proc_root:
                        scan_count += 1
                        if scan_count == 2:
                            write_proc_stat(proc_root, 102, new_state, 42)
                    return real_iterdir(path)

                with (
                    mock.patch.object(supervisor, "PROC_ROOT", proc_root, create=True),
                    mock.patch.object(supervisor.sys, "platform", "linux"),
                    mock.patch.object(
                        Path, "iterdir", new=add_member_before_second_scan
                    ),
                    mock.patch.object(supervisor.os, "killpg"),
                ):
                    self.assertEqual(
                        supervisor.process_group_alive(42), new_state == "S"
                    )
                self.assertEqual(scan_count, 2 if new_state == "S" else 3)

    def test_linux_group_scan_fails_closed_without_session_leader(self):
        with tempfile.TemporaryDirectory() as temp:
            proc_root = Path(temp)
            write_proc_visibility_proof(proc_root)
            write_proc_stat(proc_root, 101, "Z", 42)
            with (
                mock.patch.object(supervisor, "PROC_ROOT", proc_root, create=True),
                mock.patch.object(supervisor.sys, "platform", "linux"),
            ):
                self.assertTrue(supervisor.process_group_alive(42))

    def test_linux_group_scan_rejects_hidden_proc_and_non_linux_proc(self):
        with tempfile.TemporaryDirectory() as temp:
            proc_root = Path(temp)
            write_proc_visibility_proof(proc_root, mount_options="rw,hidepid=2")
            write_proc_stat(proc_root, 101, "Z", 42)
            with (
                mock.patch.object(supervisor, "PROC_ROOT", proc_root, create=True),
                mock.patch.object(supervisor.sys, "platform", "linux"),
                mock.patch.object(supervisor.os, "killpg"),
            ):
                self.assertTrue(supervisor.process_group_alive(42))
            write_proc_visibility_proof(proc_root)
            (proc_root / "self" / "stat").write_text(
                proc_stat_text(
                    os.getpid() + 1,
                    "S",
                    os.getpgrp(),
                    os.getpgrp(),
                ),
                encoding="utf-8",
            )
            with (
                mock.patch.object(supervisor, "PROC_ROOT", proc_root, create=True),
                mock.patch.object(supervisor.sys, "platform", "linux"),
                mock.patch.object(supervisor.os, "killpg"),
            ):
                self.assertTrue(supervisor.process_group_alive(42))
            write_proc_visibility_proof(proc_root)
            with (
                mock.patch.object(supervisor, "PROC_ROOT", proc_root, create=True),
                mock.patch.object(supervisor.sys, "platform", "sunos"),
                mock.patch.object(supervisor.os, "killpg"),
            ):
                self.assertTrue(supervisor.process_group_alive(42))

    def test_proc_visibility_rejects_stacked_hidden_mount(self):
        for hidden_first in (False, True):
            with (
                self.subTest(hidden_first=hidden_first),
                tempfile.TemporaryDirectory() as temp,
            ):
                proc_root = Path(temp)
                write_proc_visibility_proof(proc_root)
                safe = f"proc {proc_root.resolve()} proc rw,nosuid,nodev 0 0\n"
                hidden = f"proc {proc_root.resolve()} proc rw,hidepid=invisible 0 0\n"
                (proc_root / "mounts").write_text(
                    hidden + safe if hidden_first else safe + hidden,
                    encoding="utf-8",
                )
                with (
                    mock.patch.object(supervisor, "PROC_ROOT", proc_root, create=True),
                    mock.patch.object(supervisor.sys, "platform", "linux"),
                ):
                    self.assertFalse(supervisor.proc_visibility_complete())

    def test_signal_groups_never_reaps_worker_before_signaling(self):
        process = mock.Mock(pid=42)
        process.returncode = None
        attempt = supervisor.TerminationAttempt([process], Path("unused"))
        with (
            mock.patch.object(supervisor.os, "killpg"),
            mock.patch.object(supervisor, "linux_session_pidfds", return_value=[]),
        ):
            attempt.signal_groups(signal.SIGTERM)
        process.poll.assert_not_called()

    def test_grace_and_kill_never_reap_before_last_signal(self):
        process = mock.Mock(pid=42)
        process.returncode = None
        attempt = supervisor.TerminationAttempt([process], Path("unused"))
        with (
            mock.patch.object(attempt, "group_alive", return_value=False),
            mock.patch.object(supervisor.os, "killpg"),
            mock.patch.object(supervisor, "linux_session_pidfds", return_value=[]),
        ):
            attempt.wait_for_grace(0.01)
            attempt.kill_survivors()
        process.poll.assert_not_called()
        process.wait.assert_not_called()

    def test_incomplete_session_is_not_reaped_before_outer_retry(self):
        process = mock.Mock(pid=42)
        process.returncode = None
        with (
            mock.patch.object(supervisor, "append_event"),
            mock.patch.object(supervisor, "process_group_exists", return_value=True),
            mock.patch.object(supervisor, "process_group_alive", return_value=True),
            mock.patch.object(supervisor, "linux_session_pidfds", return_value=[]),
            mock.patch.object(supervisor.os, "killpg"),
        ):
            with self.assertRaises(supervisor.WorkersStillRunningError):
                supervisor.terminate_workers(
                    [process],
                    Path("unused"),
                    {"event": "breach", "reason": "test"},
                    grace_seconds=0,
                )
        process.poll.assert_not_called()
        process.wait.assert_not_called()

    def test_reaped_leader_never_authorizes_numeric_session_signaling(self):
        process = mock.Mock(pid=42)
        process.returncode = 0
        attempt = supervisor.TerminationAttempt([process], Path("unused"))
        with (
            mock.patch.object(supervisor.os, "killpg") as killpg,
            mock.patch.object(supervisor, "linux_session_pidfds") as pidfds,
        ):
            self.assertEqual(attempt.signal_groups(signal.SIGKILL), [])
            self.assertTrue(attempt.group_alive(process))
        killpg.assert_not_called()
        pidfds.assert_not_called()

    def test_no_live_proof_precedes_reap_and_no_scan_follows(self):
        process = mock.Mock(pid=42)
        process.returncode = None
        order = []
        process.wait.side_effect = lambda timeout: order.append("wait")
        process.poll.side_effect = lambda: order.append("poll") or 0

        def prove_stopped(_session_id):
            order.append("proof")
            return False

        with (
            mock.patch.object(supervisor, "append_event"),
            mock.patch.object(supervisor, "process_group_exists", return_value=False),
            mock.patch.object(supervisor, "process_group_alive", new=prove_stopped),
            mock.patch.object(supervisor, "linux_session_pidfds", return_value=[]),
        ):
            supervisor.terminate_workers(
                [process],
                Path("unused"),
                {"event": "breach", "reason": "test"},
                grace_seconds=0,
            )
        self.assertLess(
            max(i for i, item in enumerate(order) if item == "proof"),
            order.index("wait"),
        )
        self.assertEqual(order[-1], "poll")

    def test_session_member_signal_uses_pidfd_and_closes_it(self):
        process = mock.Mock(pid=42)
        process.returncode = None
        attempt = supervisor.TerminationAttempt([process], Path("unused"))
        with (
            mock.patch.object(
                supervisor, "linux_session_pidfds", return_value=[(101, 900)]
            ),
            mock.patch.object(
                supervisor.signal, "pidfd_send_signal", create=True
            ) as send,
            mock.patch.object(supervisor.os, "close") as close,
        ):
            self.assertEqual(attempt.signal_session_members(42, signal.SIGTERM), [101])
        send.assert_called_once_with(900, signal.SIGTERM, None, 0)
        close.assert_called_once_with(900)

    def test_pidfd_open_recheck_rejects_reused_process_identity(self):
        with tempfile.TemporaryDirectory() as temp:
            proc_root = Path(temp)
            write_proc_visibility_proof(proc_root)
            write_proc_stat(proc_root, 42, "Z", 42)
            write_proc_stat(proc_root, 101, "S", 43, session=42)

            def reuse_pid(_pid, _flags):
                write_proc_stat(
                    proc_root,
                    101,
                    "S",
                    43,
                    session=42,
                    start_time=999_999,
                )
                return 900

            with (
                mock.patch.object(supervisor, "PROC_ROOT", proc_root, create=True),
                mock.patch.object(supervisor.sys, "platform", "linux"),
                mock.patch.object(
                    supervisor.os, "pidfd_open", new=reuse_pid, create=True
                ),
                mock.patch.object(supervisor.os, "close") as close,
                mock.patch.object(supervisor.signal, "pidfd_send_signal", create=True),
            ):
                self.assertEqual(supervisor.linux_session_pidfds(42), [])
            close.assert_called_once_with(900)

    def test_state_checks_do_not_reap_worker_before_cleanup(self):
        process = mock.Mock(pid=42)
        runtime = object.__new__(supervisor.Supervisor)
        runtime.args = types.SimpleNamespace(
            state_poll_seconds=0.2,
            ready_timeout_seconds=1,
        )
        runtime.workers = [
            supervisor.WorkerProcess(
                types.SimpleNamespace(role="maker", port=9001),
                process,
                mock.Mock(),
            )
        ]
        with mock.patch.object(
            supervisor, "fetch_json", side_effect=RuntimeError("worker unavailable")
        ):
            with self.assertRaisesRegex(RuntimeError, "worker unavailable"):
                runtime.states()
        with (
            mock.patch.object(
                supervisor, "fetch_text", side_effect=RuntimeError("not ready")
            ),
            mock.patch.object(
                supervisor.time, "monotonic", side_effect=[0.0, 0.0, 2.0]
            ),
            mock.patch.object(supervisor.time, "sleep"),
        ):
            with self.assertRaisesRegex(RuntimeError, "did not become ready"):
                runtime.wait_ready()
        process.poll.assert_not_called()

    def test_kill_survivors_skips_terminal_only_session(self):
        process = mock.Mock(pid=42)
        process.returncode = None
        process.poll.return_value = None
        attempt = supervisor.TerminationAttempt([process], Path("unused"))
        with (
            mock.patch.object(
                supervisor, "process_group_exists", return_value=True, create=True
            ),
            mock.patch.object(supervisor, "process_group_alive", return_value=False),
            mock.patch.object(supervisor.os, "killpg") as killpg,
        ):
            self.assertEqual(attempt.kill_survivors(), [])
        killpg.assert_not_called()

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
                parent.stdout.close()
                time.sleep(0.1)
                self.assertIsNone(parent.returncode)
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

    @unittest.skipUnless(
        sys.platform.startswith("linux"), "requires Linux procfs session evidence"
    )
    def test_termination_stops_sibling_group_after_worker_leader_exits(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            ready = root / "sibling-ready"
            child_code = (
                "import os, pathlib, time\n"
                "os.setpgid(0, 0)\n"
                f"pathlib.Path({str(ready)!r}).write_text('ready')\n"
                "time.sleep(60)\n"
            )
            parent_code = (
                "import os, pathlib, subprocess, time\n"
                f"ready = pathlib.Path({str(ready)!r})\n"
                f"child = subprocess.Popen([{sys.executable!r}, '-c', {child_code!r}])\n"
                "while not ready.exists():\n"
                "    time.sleep(.01)\n"
                "print(child.pid, os.getpgid(child.pid), os.getsid(child.pid), flush=True)\n"
            )
            parent = subprocess.Popen(
                [sys.executable, "-c", parent_code],
                start_new_session=True,
                text=True,
                stdout=subprocess.PIPE,
            )
            child_pid = None
            child_group = None
            try:
                assert parent.stdout is not None
                child_pid, child_group, child_session = map(
                    int, parent.stdout.readline().split()
                )
                parent.stdout.close()
                parent_stat = Path("/proc") / str(parent.pid) / "stat"
                deadline = time.monotonic() + 2
                while time.monotonic() < deadline:
                    _, parent_state, _, _, _, _ = supervisor.parse_proc_stat(
                        parent_stat.read_text(encoding="utf-8")
                    )
                    if parent_state in supervisor.TERMINAL_PROC_STATES:
                        break
                    time.sleep(0.01)
                else:
                    self.fail("worker leader did not exit before shutdown")
                self.assertIsNone(parent.returncode)
                self.assertEqual(child_group, child_pid)
                self.assertEqual(child_session, parent.pid)
                self.assertNotEqual(child_group, parent.pid)

                event_log = root / "events.jsonl"
                supervisor.terminate_workers(
                    [parent],
                    event_log,
                    {"event": "breach", "reason": "test"},
                    grace_seconds=0.05,
                )

                child_stat = Path("/proc") / str(child_pid) / "stat"
                if child_stat.exists():
                    _, state, _, _, _, _ = supervisor.parse_proc_stat(
                        child_stat.read_text(encoding="utf-8")
                    )
                    self.assertIn(state, supervisor.TERMINAL_PROC_STATES)
                events = [
                    json.loads(line)
                    for line in event_log.read_text(encoding="utf-8").splitlines()
                ]
                self.assertEqual(events[-1]["event"], "workers_stopped")
                self.assertIsNotNone(parent.returncode)
            finally:
                if child_group is not None:
                    try:
                        os.killpg(child_group, signal.SIGKILL)
                    except ProcessLookupError:
                        pass

    def test_group_signal_failures_are_retried_before_reap(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            child_code = "import time; time.sleep(60)"
            parent_code = (
                "import subprocess, time\n"
                f"child = subprocess.Popen([{sys.executable!r}, '-c', {child_code!r}])\n"
                "print(child.pid, flush=True)\n"
                "time.sleep(60)\n"
            )
            parent = subprocess.Popen(
                [sys.executable, "-c", parent_code],
                start_new_session=True,
                text=True,
                stdout=subprocess.PIPE,
            )
            child_pid = None
            real_killpg = os.killpg
            failed_signals = set()

            def fail_first_group_signal(pgid, signum):
                if (
                    signum in (signal.SIGTERM, signal.SIGKILL)
                    and signum not in failed_signals
                ):
                    failed_signals.add(signum)
                    raise PermissionError(
                        f"injected first {signal.Signals(signum).name}"
                    )
                return real_killpg(pgid, signum)

            try:
                assert parent.stdout is not None
                child_pid = int(parent.stdout.readline().strip())
                parent.stdout.close()
                first_log = root / "first-events.jsonl"
                with (
                    mock.patch.object(
                        supervisor.os, "killpg", new=fail_first_group_signal
                    ),
                    mock.patch.object(
                        supervisor, "linux_session_pidfds", return_value=[]
                    ),
                ):
                    with self.assertRaises(
                        supervisor.TerminationOperationError
                    ) as caught:
                        supervisor.terminate_workers(
                            [parent],
                            first_log,
                            {"event": "breach", "reason": "test"},
                            grace_seconds=0.01,
                        )

                self.assertEqual(failed_signals, {signal.SIGTERM, signal.SIGKILL})
                self.assertEqual(parent.poll(), -signal.SIGKILL)
                self.assertEqual(len(caught.exception.failures), 2)
                events = [
                    json.loads(line)
                    for line in first_log.read_text(encoding="utf-8").splitlines()
                ]
                self.assertEqual(events[-1]["event"], "workers_stopped")
                self.assertEqual(events[-1]["alive_pids"], [])
            finally:
                if parent.poll() is None:
                    real_killpg(parent.pid, signal.SIGKILL)
                    parent.wait(timeout=1)
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

    def test_abort_closes_worker_log_when_termination_evidence_fails(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            process = subprocess.Popen(
                [sys.executable, "-c", "import time; time.sleep(60)"],
                start_new_session=True,
                text=True,
            )
            handle = (root / "worker.log").open("w", encoding="utf-8")
            args = types.SimpleNamespace(
                run_root=str(root),
                operator_gh_config_dir=str(root / "operator-auth"),
                term_grace_seconds=0.1,
                issues=[1],
            )

            class AbortingSupervisor(supervisor.Supervisor):
                def verify_files(self):
                    return None

                def verify_run_directories(self):
                    return None

                def verify_identities_and_initial_state(self):
                    return None

                def start_workers(self):
                    self.workers = [
                        supervisor.WorkerProcess(mock.sentinel.spec, process, handle)
                    ]

                def wait_ready(self):
                    return [state(), state()]

                def run_issue(self, issue_number, states):
                    self.abort(
                        issue_number,
                        supervisor.LimitBreach("worker_tokens_exceeded", 11, 10),
                        states,
                        {},
                    )
                    raise AssertionError("abort returned after evidence failure")

            loop = AbortingSupervisor(args)
            try:
                with mock.patch.object(
                    supervisor, "append_event", side_effect=OSError("disk full")
                ) as append:
                    with self.assertRaises(supervisor.TerminationEvidenceError):
                        loop.run()

                self.assertEqual(append.call_count, 3)
                self.assertIsNotNone(process.poll())
                self.assertTrue(handle.closed)
            finally:
                if process.poll() is None:
                    os.killpg(process.pid, signal.SIGKILL)
                    process.wait(timeout=1)
                if not handle.closed:
                    handle.close()

    def test_completion_log_failure_cannot_return_success_or_retry_shutdown(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            process = subprocess.Popen(
                [sys.executable, "-c", "import time; time.sleep(60)"],
                start_new_session=True,
                text=True,
            )
            handle = (root / "worker.log").open("w", encoding="utf-8")
            args = types.SimpleNamespace(
                run_root=str(root),
                operator_gh_config_dir=str(root / "operator-auth"),
                term_grace_seconds=0.1,
                issues=[],
            )

            class CompletionSupervisor(supervisor.Supervisor):
                def verify_files(self):
                    return None

                def verify_run_directories(self):
                    return None

                def verify_identities_and_initial_state(self):
                    return None

                def start_workers(self):
                    self.workers = [
                        supervisor.WorkerProcess(mock.sentinel.spec, process, handle)
                    ]

                def wait_ready(self):
                    return [state(), state()]

                def ensure_workflows_unchanged(self):
                    return None

            loop = CompletionSupervisor(args)
            try:
                with mock.patch.object(
                    supervisor, "append_event", side_effect=OSError("disk full")
                ) as append:
                    with self.assertRaises(supervisor.TerminationEvidenceError):
                        loop.run()

                self.assertEqual(append.call_count, 3)
                self.assertIsNotNone(process.poll())
                self.assertTrue(handle.closed)
            finally:
                if process.poll() is None:
                    os.killpg(process.pid, signal.SIGKILL)
                    process.wait(timeout=1)
                if not handle.closed:
                    handle.close()

    def test_operator_interrupt_still_stops_workers_and_closes_logs(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            process = subprocess.Popen(
                [sys.executable, "-c", "import time; time.sleep(60)"],
                start_new_session=True,
                text=True,
            )
            handle = (root / "worker.log").open("w", encoding="utf-8")
            args = types.SimpleNamespace(
                run_root=str(root),
                operator_gh_config_dir=str(root / "operator-auth"),
                term_grace_seconds=0.1,
                issues=[1],
            )

            class InterruptingSupervisor(supervisor.Supervisor):
                def verify_files(self):
                    return None

                def verify_run_directories(self):
                    return None

                def verify_identities_and_initial_state(self):
                    return None

                def start_workers(self):
                    self.workers = [
                        supervisor.WorkerProcess(mock.sentinel.spec, process, handle)
                    ]

                def wait_ready(self):
                    return [state(), state()]

                def run_issue(self, _issue_number, _states):
                    raise KeyboardInterrupt("operator interrupted")

            loop = InterruptingSupervisor(args)
            try:
                with self.assertRaisesRegex(KeyboardInterrupt, "operator interrupted"):
                    loop.run()

                self.assertIsNotNone(process.poll())
                self.assertTrue(handle.closed)
            finally:
                if process.poll() is None:
                    os.killpg(process.pid, signal.SIGKILL)
                    process.wait(timeout=1)
                if not handle.closed:
                    handle.close()

    def test_sigterm_routes_through_worker_cleanup(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            process = subprocess.Popen(
                [sys.executable, "-c", "import time; time.sleep(60)"],
                start_new_session=True,
                text=True,
            )
            handle = (root / "worker.log").open("w", encoding="utf-8")
            args = types.SimpleNamespace(
                run_root=str(root),
                operator_gh_config_dir=str(root / "operator-auth"),
                term_grace_seconds=0.1,
                issues=[1],
            )

            class SignaledSupervisor(supervisor.Supervisor):
                def verify_files(self):
                    return None

                def verify_run_directories(self):
                    return None

                def verify_identities_and_initial_state(self):
                    return None

                def start_workers(self):
                    self.workers = [
                        supervisor.WorkerProcess(mock.sentinel.spec, process, handle)
                    ]

                def wait_ready(self):
                    return [state(), state()]

                def run_issue(self, _issue_number, _states):
                    os.kill(os.getpid(), signal.SIGTERM)
                    raise AssertionError("SIGTERM did not interrupt the supervisor")

            loop = SignaledSupervisor(args)
            previous = signal.getsignal(signal.SIGTERM)
            try:
                with supervisor.termination_signal_handlers():
                    with self.assertRaisesRegex(supervisor.SupervisorSignal, "SIGTERM"):
                        loop.run()

                self.assertIs(signal.getsignal(signal.SIGTERM), previous)
                self.assertIsNotNone(process.poll())
                self.assertTrue(handle.closed)
            finally:
                if process.poll() is None:
                    os.killpg(process.pid, signal.SIGKILL)
                    process.wait(timeout=1)
                if not handle.closed:
                    handle.close()

    def test_main_routes_termination_signals_through_worker_cleanup(self):
        for signum in (signal.SIGTERM, signal.SIGHUP):
            with self.subTest(signal=signal.Signals(signum).name):
                with tempfile.TemporaryDirectory() as temp:
                    root = Path(temp)
                    process = subprocess.Popen(
                        [sys.executable, "-c", "import time; time.sleep(60)"],
                        start_new_session=True,
                        text=True,
                    )
                    handle = (root / "worker.log").open("w", encoding="utf-8")
                    args = types.SimpleNamespace(
                        run_root=str(root),
                        operator_gh_config_dir=str(root / "operator-auth"),
                        term_grace_seconds=0.1,
                        issues=[1],
                    )

                    class SignaledSupervisor(supervisor.Supervisor):
                        def verify_files(self):
                            return None

                        def verify_run_directories(self):
                            return None

                        def verify_identities_and_initial_state(self):
                            return None

                        def start_workers(self):
                            self.workers = [
                                supervisor.WorkerProcess(
                                    mock.sentinel.spec, process, handle
                                )
                            ]

                        def wait_ready(self):
                            return [state(), state()]

                        def run_issue(self, _issue_number, _states):
                            os.kill(os.getpid(), signum)
                            raise AssertionError(
                                f"{signal.Signals(signum).name} did not interrupt"
                            )

                    previous = {
                        watched: signal.getsignal(watched)
                        for watched in (signal.SIGTERM, signal.SIGHUP)
                    }
                    for watched in previous:
                        signal.signal(watched, signal.SIG_IGN)
                    stderr = io.StringIO()
                    try:
                        with (
                            mock.patch.object(
                                supervisor, "parse_args", return_value=args
                            ),
                            mock.patch.object(
                                supervisor, "Supervisor", SignaledSupervisor
                            ),
                            mock.patch.object(supervisor.sys, "stderr", stderr),
                        ):
                            self.assertEqual(supervisor.main([]), 2)

                        self.assertIn(
                            f"received {signal.Signals(signum).name}", stderr.getvalue()
                        )
                        for watched in previous:
                            self.assertIs(signal.getsignal(watched), signal.SIG_IGN)
                        self.assertIsNotNone(process.poll())
                        self.assertTrue(handle.closed)
                    finally:
                        for watched, handler in previous.items():
                            signal.signal(watched, handler)
                        if process.poll() is None:
                            os.killpg(process.pid, signal.SIGKILL)
                            process.wait(timeout=1)
                        if not handle.closed:
                            handle.close()

    def test_signal_after_fork_before_popen_return_stops_worker(self):
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
            spec = supervisor.WorkerSpec(
                "maker",
                4928,
                workflow,
                "unused",
                root / "maker-auth",
                "maker",
                root / "maker-mirror",
                "TEST_START_TOKEN",
            )
            args = types.SimpleNamespace(
                run_root=str(root),
                operator_gh_config_dir=str(root / "operator-auth"),
                worker_bin=str(worker),
                clone_url="https://github.com/example/repo.git",
                term_grace_seconds=0.1,
            )

            class StartSupervisor(supervisor.Supervisor):
                def verify_files(self):
                    return None

                def verify_run_directories(self):
                    return None

                def verify_identities_and_initial_state(self):
                    return None

                def specs(self):
                    return [spec]

            created = []
            real_fork_exec = supervisor.subprocess._fork_exec

            def interrupt_after_fork(*fork_args, **fork_kwargs):
                pid = real_fork_exec(*fork_args, **fork_kwargs)
                created.append(pid)
                os.kill(os.getpid(), signal.SIGTERM)
                return pid

            loop = StartSupervisor(args)
            old_token = os.environ.get("TEST_START_TOKEN")
            os.environ["TEST_START_TOKEN"] = "not-a-real-token"
            try:
                with (
                    supervisor.termination_signal_handlers(),
                    mock.patch.object(
                        supervisor.subprocess,
                        "_fork_exec",
                        new=interrupt_after_fork,
                    ),
                ):
                    with self.assertRaisesRegex(supervisor.SupervisorSignal, "SIGTERM"):
                        loop.run()

                self.assertEqual(len(created), 1)
                self.assertEqual(len(loop.workers), 1)
                self.assertIsNotNone(loop.workers[0].process.poll())
                self.assertTrue(loop.workers[0].log_handle.closed)
                self.assertTrue(loop.shutdown_completed)
            finally:
                for pid in created:
                    try:
                        os.killpg(pid, signal.SIGKILL)
                    except ProcessLookupError:
                        try:
                            os.kill(pid, signal.SIGKILL)
                        except ProcessLookupError:
                            pass
                    try:
                        os.waitpid(pid, 0)
                    except ChildProcessError:
                        pass
                if old_token is None:
                    os.environ.pop("TEST_START_TOKEN", None)
                else:
                    os.environ["TEST_START_TOKEN"] = old_token

    def test_second_termination_signal_does_not_interrupt_worker_cleanup(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            process = subprocess.Popen(
                [sys.executable, "-c", "import time; time.sleep(60)"],
                start_new_session=True,
                text=True,
            )
            handle = (root / "worker.log").open("w", encoding="utf-8")
            args = types.SimpleNamespace(
                run_root=str(root),
                operator_gh_config_dir=str(root / "operator-auth"),
                term_grace_seconds=0.1,
                issues=[1],
            )

            class SignaledSupervisor(supervisor.Supervisor):
                def verify_files(self):
                    return None

                def verify_run_directories(self):
                    return None

                def verify_identities_and_initial_state(self):
                    return None

                def start_workers(self):
                    self.workers = [
                        supervisor.WorkerProcess(mock.sentinel.spec, process, handle)
                    ]

                def wait_ready(self):
                    return [state(), state()]

                def run_issue(self, _issue_number, _states):
                    os.kill(os.getpid(), signal.SIGTERM)
                    raise AssertionError("first SIGTERM did not interrupt")

            real_killpg = os.killpg
            second_signal_sent = False

            def send_second_signal_then_kill(pgid, signum):
                nonlocal second_signal_sent
                if signum == signal.SIGTERM and not second_signal_sent:
                    second_signal_sent = True
                    os.kill(os.getpid(), signal.SIGTERM)
                return real_killpg(pgid, signum)

            loop = SignaledSupervisor(args)
            try:
                with (
                    supervisor.termination_signal_handlers(),
                    mock.patch.object(
                        supervisor.os, "killpg", new=send_second_signal_then_kill
                    ),
                ):
                    with self.assertRaisesRegex(supervisor.SupervisorSignal, "SIGTERM"):
                        loop.run()

                self.assertTrue(second_signal_sent)
                self.assertTrue(loop.shutdown_completed)
                self.assertIsNotNone(process.poll())
                self.assertTrue(handle.closed)
            finally:
                if process.poll() is None:
                    real_killpg(process.pid, signal.SIGKILL)
                    process.wait(timeout=1)
                if not handle.closed:
                    handle.close()

    def test_unstoppable_worker_does_not_mark_shutdown_completed(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            process = subprocess.Popen(
                [sys.executable, "-c", "import time; time.sleep(60)"],
                start_new_session=True,
                text=True,
            )
            handle = (root / "worker.log").open("w", encoding="utf-8")
            args = types.SimpleNamespace(
                run_root=str(root),
                operator_gh_config_dir=str(root / "operator-auth"),
                term_grace_seconds=0.01,
            )
            loop = supervisor.Supervisor(args)
            loop.workers = [
                supervisor.WorkerProcess(mock.sentinel.spec, process, handle)
            ]
            real_killpg = os.killpg

            def deny_group_signal(_pgid, signum):
                if signum in (signal.SIGTERM, signal.SIGKILL):
                    raise PermissionError("injected group signal failure")
                return real_killpg(_pgid, signum)

            try:
                with (
                    mock.patch.object(supervisor.os, "killpg", new=deny_group_signal),
                    mock.patch.object(
                        supervisor, "linux_session_pidfds", return_value=[]
                    ),
                    mock.patch.object(
                        process,
                        "kill",
                        side_effect=PermissionError("injected fallback failure"),
                    ),
                ):
                    with self.assertRaises(supervisor.WorkersStillRunningError):
                        loop.stop_workers({"event": "breach"})

                self.assertFalse(loop.shutdown_completed)
                self.assertIsNone(process.poll())
                self.assertTrue(handle.closed)
                events = [
                    json.loads(line)
                    for line in loop.event_log.read_text(encoding="utf-8").splitlines()
                ]
                self.assertEqual(events[-1]["event"], "workers_stop_incomplete")
                self.assertEqual(events[-1]["alive_pids"], [process.pid])
            finally:
                if process.poll() is None:
                    real_killpg(process.pid, signal.SIGKILL)
                    process.wait(timeout=1)
                if not handle.closed:
                    handle.close()

    def test_worker_start_interruption_stops_process_before_tracking(self):
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
            spec = supervisor.WorkerSpec(
                "maker",
                4928,
                workflow,
                "unused",
                root / "maker-auth",
                "maker",
                root / "maker-mirror",
                "TEST_START_TOKEN",
            )
            args = types.SimpleNamespace(
                run_root=str(root),
                operator_gh_config_dir=str(root / "operator-auth"),
                worker_bin=str(worker),
                clone_url="https://github.com/example/repo.git",
                term_grace_seconds=0.1,
            )

            class StartSupervisor(supervisor.Supervisor):
                def specs(self):
                    return [spec]

            class InterruptingWorkers(list):
                def append(self, _worker):
                    raise supervisor.SupervisorSignal(signal.SIGTERM)

            loop = StartSupervisor(args)
            interrupted_workers = InterruptingWorkers()
            loop.workers = interrupted_workers
            started = []
            real_popen = subprocess.Popen

            def record_popen(*args, **kwargs):
                process = real_popen(*args, **kwargs)
                started.append(process)
                return process

            old_token = os.environ.get("TEST_START_TOKEN")
            os.environ["TEST_START_TOKEN"] = "not-a-real-token"
            try:
                with mock.patch.object(
                    supervisor.subprocess, "Popen", side_effect=record_popen
                ):
                    with self.assertRaises(supervisor.SupervisorSignal):
                        loop.start_workers()

                self.assertEqual(len(started), 1)
                self.assertIsNotNone(started[0].poll())
                self.assertEqual(loop.workers, [])
            finally:
                for process in started:
                    if process.poll() is None:
                        os.killpg(process.pid, signal.SIGKILL)
                        process.wait(timeout=1)
                if old_token is None:
                    os.environ.pop("TEST_START_TOKEN", None)
                else:
                    os.environ["TEST_START_TOKEN"] = old_token

    def test_worker_start_kill_error_does_not_abandon_untracked_process(self):
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
            spec = supervisor.WorkerSpec(
                "maker",
                4928,
                workflow,
                "unused",
                root / "maker-auth",
                "maker",
                root / "maker-mirror",
                "TEST_START_TOKEN",
            )
            args = types.SimpleNamespace(
                run_root=str(root),
                operator_gh_config_dir=str(root / "operator-auth"),
                worker_bin=str(worker),
                clone_url="https://github.com/example/repo.git",
                term_grace_seconds=0.1,
            )

            class StartSupervisor(supervisor.Supervisor):
                def verify_files(self):
                    return None

                def verify_run_directories(self):
                    return None

                def verify_identities_and_initial_state(self):
                    return None

                def specs(self):
                    return [spec]

            class InterruptingWorkers(list):
                rejected = None

                def append(self, worker_process):
                    self.rejected = worker_process
                    raise supervisor.SupervisorSignal(signal.SIGTERM)

            real_killpg = os.killpg
            failed_once = False

            def fail_first_term(pgid, signum):
                nonlocal failed_once
                if signum == signal.SIGTERM and not failed_once:
                    failed_once = True
                    raise PermissionError("injected killpg failure")
                return real_killpg(pgid, signum)

            loop = StartSupervisor(args)
            loop.workers = InterruptingWorkers()
            old_token = os.environ.get("TEST_START_TOKEN")
            os.environ["TEST_START_TOKEN"] = "not-a-real-token"
            try:
                with mock.patch.object(supervisor.os, "killpg", new=fail_first_term):
                    try:
                        loop.run()
                    except BaseException:
                        pass

                self.assertTrue(failed_once)
                self.assertIsNotNone(loop.workers.rejected)
                self.assertIsNotNone(loop.workers.rejected.process.poll())
                self.assertTrue(loop.workers.rejected.log_handle.closed)
            finally:
                rejected = loop.workers.rejected
                if rejected is not None and rejected.process.poll() is None:
                    real_killpg(rejected.process.pid, signal.SIGKILL)
                    rejected.process.wait(timeout=1)
                if old_token is None:
                    os.environ.pop("TEST_START_TOKEN", None)
                else:
                    os.environ["TEST_START_TOKEN"] = old_token

    def test_untracked_survivor_is_retained_for_outer_shutdown_retry(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            worker = root / "fake-worker"
            worker.write_text(
                f"#!{sys.executable}\n"
                "import pathlib, subprocess, sys, time\n"
                "port = sys.argv[sys.argv.index('--port') + 1]\n"
                "subprocess.Popen([sys.executable, '-c', "
                "'import time; time.sleep(60)'])\n"
                f"pathlib.Path({str(root)!r}, 'child-ready-' + port).write_text('ready')\n"
                "time.sleep(60)\n",
                encoding="utf-8",
            )
            worker.chmod(0o755)
            workflow = root / "WORKFLOW.md"
            workflow.write_text("---\n---\n", encoding="utf-8")
            specs = [
                supervisor.WorkerSpec(
                    role,
                    port,
                    workflow,
                    "unused",
                    root / f"{role}-auth",
                    role,
                    root / f"{role}-mirror",
                    "TEST_START_TOKEN",
                )
                for role, port in (("maker", 4928), ("reviewer", 4929))
            ]
            args = types.SimpleNamespace(
                run_root=str(root),
                operator_gh_config_dir=str(root / "operator-auth"),
                worker_bin=str(worker),
                clone_url="https://github.com/example/repo.git",
                term_grace_seconds=0.01,
            )

            class StartSupervisor(supervisor.Supervisor):
                signal_on_worker_transfer = False
                transfer_signal_sent = False

                def __setattr__(self, name, value):
                    if name == "workers" and getattr(
                        self, "signal_on_worker_transfer", False
                    ):
                        object.__setattr__(self, "signal_on_worker_transfer", False)
                        object.__setattr__(self, "transfer_signal_sent", True)
                        os.kill(os.getpid(), signal.SIGTERM)
                    super().__setattr__(name, value)

                def verify_files(self):
                    return None

                def verify_run_directories(self):
                    return None

                def verify_identities_and_initial_state(self):
                    return None

                def specs(self):
                    return specs

            class InterruptingWorkers(list):
                rejected = None

                def append(self, worker_process):
                    if not self:
                        super().append(worker_process)
                        return
                    self.rejected = worker_process
                    child_ready = root / f"child-ready-{worker_process.spec.port}"
                    deadline = time.monotonic() + 1
                    while not child_ready.exists() and time.monotonic() < deadline:
                        time.sleep(0.01)
                    if not child_ready.exists():
                        raise AssertionError("worker child did not start")
                    raise supervisor.SupervisorSignal(signal.SIGTERM)

            loop = StartSupervisor(args)
            interrupted_workers = InterruptingWorkers()
            loop.workers = interrupted_workers
            loop.signal_on_worker_transfer = True
            real_terminate_workers = supervisor.terminate_workers
            retained_pid = None

            def retain_first_untracked(processes, *call_args, **call_kwargs):
                nonlocal retained_pid
                process_list = list(processes)
                rejected = interrupted_workers.rejected
                if (
                    retained_pid is None
                    and rejected is not None
                    and len(process_list) == 1
                    and process_list[0] is rejected.process
                ):
                    retained_pid = rejected.process.pid
                    raise supervisor.WorkersStillRunningError([retained_pid], [])
                return real_terminate_workers(process_list, *call_args, **call_kwargs)

            old_token = os.environ.get("TEST_START_TOKEN")
            os.environ["TEST_START_TOKEN"] = "not-a-real-token"
            observed = None
            try:
                with (
                    supervisor.termination_signal_handlers(),
                    mock.patch.object(
                        supervisor,
                        "terminate_workers",
                        side_effect=retain_first_untracked,
                    ),
                ):
                    try:
                        loop.run()
                    except BaseException as exc:
                        observed = exc

                self.assertIsInstance(observed, supervisor.WorkersStillRunningError)
                self.assertTrue(loop.transfer_signal_sent)
                self.assertEqual(retained_pid, interrupted_workers.rejected.process.pid)
                self.assertIsNotNone(interrupted_workers.rejected.process.poll())
                self.assertEqual(len(loop.workers), 2)
                for worker_process in loop.workers:
                    self.assertIsNotNone(worker_process.process.poll())
                    self.assertTrue(worker_process.log_handle.closed)
                self.assertTrue(loop.shutdown_completed)
            finally:
                real_killpg = os.killpg
                workers = [*loop.workers]
                rejected = interrupted_workers.rejected
                if rejected is not None and all(
                    item.process is not rejected.process for item in workers
                ):
                    workers.append(rejected)
                for worker_process in workers:
                    if worker_process.process.poll() is None:
                        real_killpg(worker_process.process.pid, signal.SIGKILL)
                        worker_process.process.wait(timeout=1)
                if old_token is None:
                    os.environ.pop("TEST_START_TOKEN", None)
                else:
                    os.environ["TEST_START_TOKEN"] = old_token

    def test_worker_start_cleanup_failure_still_stops_tracked_workers(self):
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
            specs = [
                supervisor.WorkerSpec(
                    role,
                    port,
                    workflow,
                    "unused",
                    root / f"{role}-auth",
                    role,
                    root / f"{role}-mirror",
                    "TEST_START_TOKEN",
                )
                for role, port in (("maker", 4928), ("reviewer", 4929))
            ]
            args = types.SimpleNamespace(
                run_root=str(root),
                operator_gh_config_dir=str(root / "operator-auth"),
                worker_bin=str(worker),
                clone_url="https://github.com/example/repo.git",
                term_grace_seconds=0.1,
            )

            class StartSupervisor(supervisor.Supervisor):
                def verify_files(self):
                    return None

                def verify_run_directories(self):
                    return None

                def verify_identities_and_initial_state(self):
                    return None

                def specs(self):
                    return specs

            class InterruptingWorkers(list):
                rejected = None

                def append(self, worker_process):
                    if self:
                        self.rejected = worker_process
                        raise supervisor.SupervisorSignal(signal.SIGTERM)
                    super().append(worker_process)

            loop = StartSupervisor(args)
            loop.workers = InterruptingWorkers()
            loop.event_log.parent.mkdir(parents=True)
            loop.event_log.mkdir()
            old_token = os.environ.get("TEST_START_TOKEN")
            os.environ["TEST_START_TOKEN"] = "not-a-real-token"
            try:
                with self.assertRaises(supervisor.TerminationEvidenceError):
                    loop.run()

                workers = [*loop.workers, loop.workers.rejected]
                self.assertEqual(len(workers), 2)
                for worker_process in workers:
                    self.assertIsNotNone(worker_process.process.poll())
                    self.assertTrue(worker_process.log_handle.closed)
            finally:
                workers = [*loop.workers]
                if loop.workers.rejected is not None:
                    workers.append(loop.workers.rejected)
                for worker_process in workers:
                    if worker_process.process.poll() is None:
                        os.killpg(worker_process.process.pid, signal.SIGKILL)
                        worker_process.process.wait(timeout=1)
                    if not worker_process.log_handle.closed:
                        worker_process.log_handle.close()
                if old_token is None:
                    os.environ.pop("TEST_START_TOKEN", None)
                else:
                    os.environ["TEST_START_TOKEN"] = old_token

    def test_worker_start_close_failure_does_not_mask_evidence_error(self):
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
            spec = supervisor.WorkerSpec(
                "maker",
                4928,
                workflow,
                "unused",
                root / "maker-auth",
                "maker",
                root / "maker-mirror",
                "TEST_START_TOKEN",
            )
            args = types.SimpleNamespace(
                run_root=str(root),
                operator_gh_config_dir=str(root / "operator-auth"),
                worker_bin=str(worker),
                clone_url="https://github.com/example/repo.git",
                term_grace_seconds=0.1,
            )

            class StartSupervisor(supervisor.Supervisor):
                def verify_files(self):
                    return None

                def verify_run_directories(self):
                    return None

                def verify_identities_and_initial_state(self):
                    return None

                def specs(self):
                    return [spec]

            class InterruptingWorkers(list):
                rejected = None

                def append(self, worker_process):
                    self.rejected = worker_process
                    raise supervisor.SupervisorSignal(signal.SIGTERM)

            class CloseFailingHandle:
                def __init__(self, handle):
                    self.handle = handle

                @property
                def closed(self):
                    return self.handle.closed

                def fileno(self):
                    return self.handle.fileno()

                def close(self):
                    self.handle.close()
                    raise OSError("worker log close failed")

            loop = StartSupervisor(args)
            loop.workers = InterruptingWorkers()
            loop.event_log.parent.mkdir(parents=True)
            loop.event_log.mkdir()
            real_open = Path.open

            def open_with_close_failure(path, *open_args, **open_kwargs):
                handle = real_open(path, *open_args, **open_kwargs)
                if path.name.endswith("-worker.log"):
                    return CloseFailingHandle(handle)
                return handle

            old_token = os.environ.get("TEST_START_TOKEN")
            os.environ["TEST_START_TOKEN"] = "not-a-real-token"
            observed = None
            try:
                with mock.patch.object(Path, "open", new=open_with_close_failure):
                    try:
                        loop.run()
                    except BaseException as exc:
                        observed = exc

                self.assertIsInstance(observed, supervisor.TerminationEvidenceError)
                self.assertTrue(
                    any(
                        "worker log close failed" in note for note in observed.__notes__
                    )
                )
                self.assertIsNotNone(loop.workers.rejected.process.poll())
                self.assertTrue(loop.workers.rejected.log_handle.closed)
            finally:
                rejected = loop.workers.rejected
                if rejected is not None and rejected.process.poll() is None:
                    os.killpg(rejected.process.pid, signal.SIGKILL)
                    rejected.process.wait(timeout=1)
                if old_token is None:
                    os.environ.pop("TEST_START_TOKEN", None)
                else:
                    os.environ["TEST_START_TOKEN"] = old_token

    def test_start_evidence_error_survives_tracked_log_close_failure(self):
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
            specs = [
                supervisor.WorkerSpec(
                    role,
                    port,
                    workflow,
                    "unused",
                    root / f"{role}-auth",
                    role,
                    root / f"{role}-mirror",
                    "TEST_START_TOKEN",
                )
                for role, port in (("maker", 4928), ("reviewer", 4929))
            ]
            args = types.SimpleNamespace(
                run_root=str(root),
                operator_gh_config_dir=str(root / "operator-auth"),
                worker_bin=str(worker),
                clone_url="https://github.com/example/repo.git",
                term_grace_seconds=0.1,
            )

            class StartSupervisor(supervisor.Supervisor):
                def verify_files(self):
                    return None

                def verify_run_directories(self):
                    return None

                def verify_identities_and_initial_state(self):
                    return None

                def specs(self):
                    return specs

            class InterruptingWorkers(list):
                rejected = None

                def append(self, worker_process):
                    if self:
                        self.rejected = worker_process
                        raise supervisor.SupervisorSignal(signal.SIGTERM)
                    super().append(worker_process)

            class CloseFailingHandle:
                def __init__(self, handle):
                    self.handle = handle

                @property
                def closed(self):
                    return self.handle.closed

                def fileno(self):
                    return self.handle.fileno()

                def close(self):
                    self.handle.close()
                    raise OSError("tracked worker log close failed")

            real_open = Path.open

            def open_with_close_failure(path, *open_args, **open_kwargs):
                handle = real_open(path, *open_args, **open_kwargs)
                if path.name == "maker-worker.log":
                    return CloseFailingHandle(handle)
                return handle

            real_append_event = supervisor.append_event
            event_calls = 0

            def fail_local_termination_evidence(path, event):
                nonlocal event_calls
                event_calls += 1
                if event_calls <= 3:
                    raise OSError("local termination evidence failed")
                return real_append_event(path, event)

            loop = StartSupervisor(args)
            loop.workers = InterruptingWorkers()
            old_token = os.environ.get("TEST_START_TOKEN")
            os.environ["TEST_START_TOKEN"] = "not-a-real-token"
            observed = None
            try:
                with (
                    mock.patch.object(Path, "open", new=open_with_close_failure),
                    mock.patch.object(
                        supervisor,
                        "append_event",
                        new=fail_local_termination_evidence,
                    ),
                ):
                    try:
                        loop.run()
                    except BaseException as exc:
                        observed = exc

                self.assertIsInstance(observed, supervisor.TerminationEvidenceError)
                self.assertTrue(
                    any(
                        "tracked worker log close failed" in note
                        for note in getattr(observed, "__notes__", [])
                    )
                )
                workers = [*loop.workers, loop.workers.rejected]
                self.assertEqual(len(workers), 2)
                for worker_process in workers:
                    self.assertIsNotNone(worker_process.process.poll())
                    self.assertTrue(worker_process.log_handle.closed)
            finally:
                workers = [*loop.workers]
                if loop.workers.rejected is not None:
                    workers.append(loop.workers.rejected)
                for worker_process in workers:
                    if worker_process.process.poll() is None:
                        os.killpg(worker_process.process.pid, signal.SIGKILL)
                        worker_process.process.wait(timeout=1)
                if old_token is None:
                    os.environ.pop("TEST_START_TOKEN", None)
                else:
                    os.environ["TEST_START_TOKEN"] = old_token

    def test_log_close_failure_closes_every_handle_without_retrying_shutdown(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            first_handle = mock.Mock()
            first_handle.close.side_effect = OSError("first close failed")
            second_handle = mock.Mock()
            second_handle.close.side_effect = OSError("second close failed")
            args = types.SimpleNamespace(
                run_root=str(root),
                operator_gh_config_dir=str(root / "operator-auth"),
                term_grace_seconds=0.1,
                issues=[],
            )

            class CompletionSupervisor(supervisor.Supervisor):
                def verify_files(self):
                    return None

                def verify_run_directories(self):
                    return None

                def verify_identities_and_initial_state(self):
                    return None

                def start_workers(self):
                    self.workers = [
                        supervisor.WorkerProcess(
                            mock.sentinel.first_spec,
                            mock.sentinel.first_process,
                            first_handle,
                        ),
                        supervisor.WorkerProcess(
                            mock.sentinel.second_spec,
                            mock.sentinel.second_process,
                            second_handle,
                        ),
                    ]

                def wait_ready(self):
                    return [state(), state()]

                def ensure_workflows_unchanged(self):
                    return None

            loop = CompletionSupervisor(args)
            with mock.patch.object(supervisor, "terminate_workers") as terminate:
                with self.assertRaisesRegex(
                    supervisor.WorkerLogCloseError, "2 handles.*first close failed"
                ) as caught:
                    loop.run()

            self.assertEqual(terminate.call_count, 1)
            first_handle.close.assert_called_once_with()
            second_handle.close.assert_called_once_with()
            self.assertEqual(
                [str(error) for error in caught.exception.failures],
                ["first close failed", "second close failed"],
            )

    def test_log_close_interrupt_is_classified_without_retrying_shutdown(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            first_handle = mock.Mock()
            first_handle.close.side_effect = KeyboardInterrupt("close interrupted")
            second_handle = mock.Mock()
            args = types.SimpleNamespace(
                run_root=str(root),
                operator_gh_config_dir=str(root / "operator-auth"),
                term_grace_seconds=0.1,
                issues=[],
            )

            class CompletionSupervisor(supervisor.Supervisor):
                def verify_files(self):
                    return None

                def verify_run_directories(self):
                    return None

                def verify_identities_and_initial_state(self):
                    return None

                def start_workers(self):
                    self.workers = [
                        supervisor.WorkerProcess(
                            mock.sentinel.first_spec,
                            mock.sentinel.first_process,
                            first_handle,
                        ),
                        supervisor.WorkerProcess(
                            mock.sentinel.second_spec,
                            mock.sentinel.second_process,
                            second_handle,
                        ),
                    ]

                def wait_ready(self):
                    return [state(), state()]

                def ensure_workflows_unchanged(self):
                    return None

            loop = CompletionSupervisor(args)
            observed = None
            with mock.patch.object(supervisor, "terminate_workers") as terminate:
                try:
                    loop.run()
                except BaseException as exc:
                    observed = exc

            self.assertIsInstance(observed, supervisor.WorkerLogCloseError)
            self.assertEqual(terminate.call_count, 1)
            first_handle.close.assert_called_once_with()
            second_handle.close.assert_called_once_with()

    def test_log_close_failure_does_not_mask_termination_evidence_error(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            first_handle = mock.Mock()
            first_handle.close.side_effect = OSError("first close failed")
            second_handle = mock.Mock()
            args = types.SimpleNamespace(
                run_root=str(root),
                operator_gh_config_dir=str(root / "operator-auth"),
                term_grace_seconds=0.1,
            )
            loop = supervisor.Supervisor(args)
            loop.workers = [
                supervisor.WorkerProcess(
                    mock.sentinel.first_spec,
                    mock.sentinel.first_process,
                    first_handle,
                ),
                supervisor.WorkerProcess(
                    mock.sentinel.second_spec,
                    mock.sentinel.second_process,
                    second_handle,
                ),
            ]
            evidence_error = supervisor.TerminationEvidenceError(
                [OSError("event log full")]
            )

            with mock.patch.object(
                supervisor, "terminate_workers", side_effect=evidence_error
            ) as terminate:
                with self.assertRaises(supervisor.TerminationEvidenceError) as caught:
                    loop.stop_workers({"event": "breach"})

            self.assertIs(caught.exception, evidence_error)
            self.assertEqual(terminate.call_count, 1)
            first_handle.close.assert_called_once_with()
            second_handle.close.assert_called_once_with()
            self.assertIn("first close failed", "\n".join(evidence_error.__notes__))

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
