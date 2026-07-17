import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest import mock

import controller


CID_MAKER = "a" * 64
CID_REVIEWER = "b" * 64


def completed(args, stdout="", stderr="", returncode=0):
    return subprocess.CompletedProcess(args, returncode, stdout, stderr)


class BindSourceTests(unittest.TestCase):
    def valid_mounts(self, root: Path):
        paths = [
            root / name
            for name in (
                "maker-workspace",
                "maker-mirror",
                "maker-codex-home",
                "reviewer-workspace",
                "reviewer-mirror",
                "reviewer-codex-home",
            )
        ]
        for path in paths:
            path.mkdir(parents=True)
        return (
            controller.BindMount("workspace", paths[0], "/workspaces", True),
            controller.BindMount("mirror", paths[1], "/mirrors", True),
            controller.BindMount("codex_home", paths[2], "/home/aiops/.codex", False),
            controller.BindMount("workspace", paths[3], "/workspaces", True),
            controller.BindMount("mirror", paths[4], "/mirrors", True),
            controller.BindMount("codex_home", paths[5], "/home/aiops/.codex", False),
        )

    def test_bind_sources_accept_distinct_existing_roots(self):
        with tempfile.TemporaryDirectory() as tmp:
            mounts = self.valid_mounts(Path(tmp))
            got = controller.validate_bind_sources(mounts)
            self.assertEqual(got, mounts)

    def test_bind_sources_reject_missing_equal_nested_and_nonempty(self):
        for name in ("missing", "equal", "nested", "nonempty"):
            with self.subTest(name=name), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                base = self.valid_mounts(root)
                if name == "missing":
                    mounts = (
                        controller.BindMount(
                            "workspace", root / "absent", "/workspaces", True
                        ),
                        *base[1:],
                    )
                elif name == "equal":
                    mounts = (
                        base[0],
                        controller.BindMount(
                            "mirror", base[0].source, "/mirrors", True
                        ),
                        *base[2:],
                    )
                elif name == "nested":
                    nested = base[2].source / "nested"
                    nested.mkdir()
                    mounts = (
                        base[0],
                        base[1],
                        base[2],
                        controller.BindMount("workspace", nested, "/workspaces", True),
                        *base[4:],
                    )
                else:
                    (base[3].source / "unexpected").write_text("x", encoding="utf-8")
                    mounts = base
                with self.assertRaisesRegex(ValueError, name):
                    controller.validate_bind_sources(mounts)


class DockerBoundaryTests(unittest.TestCase):
    def setUp(self):
        self.container = controller.ContainerRef("maker", CID_MAKER)
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        root = Path(self.tmp.name)
        self.mounts = tuple(
            controller.BindMount(name, root / name, destination, name != "codex_home")
            for name, destination in (
                ("workspace", "/workspaces"),
                ("mirror", "/mirrors"),
                ("codex_home", "/home/aiops/.codex"),
            )
        )
        for mount in self.mounts:
            mount.source.mkdir()

        secret_root = root / "secrets"
        secret_root.mkdir()
        self.secret_mounts = tuple(
            controller.BindMount(
                name, secret_root / name, destination, False, writable=False
            )
            for name, destination in (
                ("github_token", "/run/secrets/github_token"),
                ("state_token", controller.STATE_SECRET),
                ("state_wgetrc", controller.STATE_REQUEST_CONFIG),
            )
        )
        for mount in self.secret_mounts:
            mount.source.write_text("test-secret\n", encoding="utf-8")

    def valid_state(self):
        return {
            "codex_totals": {"total_tokens": 0},
            "completed_session_usage": [],
            "running": [],
            "blocked": [],
            "retrying": [],
        }

    def inspect_payload(self):
        return {
            "Id": CID_MAKER,
            "Config": {"Env": ["CODEX_HOME=/home/aiops/.codex"]},
            "HostConfig": {"PortBindings": {}},
            "Mounts": [
                {
                    "Type": "bind",
                    "Source": str(mount.source.resolve()),
                    "Destination": mount.destination,
                    "RW": True,
                }
                for mount in self.mounts
            ]
            + [
                {
                    "Type": "bind",
                    "Source": str(mount.source.resolve()),
                    "Destination": mount.destination,
                    "RW": False,
                }
                for mount in self.secret_mounts
            ],
        }

    def test_mount_inspect_and_probe_uses_default_aiops_user(self):
        calls = []

        def fake_run(args, **kwargs):
            calls.append((args, kwargs))
            if args[:2] == ["docker", "inspect"]:
                return completed(args, json.dumps([self.inspect_payload()]))
            self.assertEqual(args[:3], ["docker", "exec", CID_MAKER])
            self.assertNotIn("--user", args)
            self.assertIn("conv=fsync", args[5])
            destination = args[-1]
            return completed(args, f"aiops\t1000\t1000\t{destination}\n")

        with mock.patch.object(controller.subprocess, "run", side_effect=fake_run):
            proof = controller.inspect_and_probe_mounts(
                self.container, self.mounts, timeout_seconds=2
            )

        self.assertEqual(len(proof["mounts"]), 3)
        self.assertTrue(all(item["rw"] for item in proof["mounts"]))
        self.assertEqual({item["user"] for item in proof["probes"]}, {"aiops"})
        self.assertEqual(len(calls), 4)

    def test_mount_inspect_rejects_wrong_id_source_destination_type_or_rw(self):
        mutations = {
            "id": lambda p: p.update(Id=CID_REVIEWER),
            "source": lambda p: p["Mounts"][0].update(Source="/wrong"),
            "destination": lambda p: p["Mounts"][0].update(Destination="/wrong"),
            "type": lambda p: p["Mounts"][0].update(Type="volume"),
            "rw": lambda p: p["Mounts"][0].update(RW=False),
        }
        for name, mutate in mutations.items():
            with self.subTest(name=name):
                payload = self.inspect_payload()
                mutate(payload)
                fake = mock.Mock(return_value=completed([], json.dumps([payload])))
                with mock.patch.object(controller.subprocess, "run", fake):
                    with self.assertRaises(ValueError):
                        controller.inspect_and_probe_mounts(
                            self.container, self.mounts, timeout_seconds=2
                        )
                self.assertEqual(fake.call_count, 1, "probe ran after inspect failure")

    def test_mount_inspect_rejects_published_state_port(self):
        payload = self.inspect_payload()
        payload["HostConfig"]["PortBindings"] = {
            "4000/tcp": [{"HostIp": "127.0.0.1", "HostPort": "49152"}]
        }
        with mock.patch.object(
            controller.subprocess,
            "run",
            return_value=completed([], json.dumps([payload])),
        ):
            with self.assertRaisesRegex(ValueError, "published"):
                controller.inspect_and_probe_mounts(
                    self.container, self.mounts, timeout_seconds=2
                )

    def test_mount_inspect_rejects_tracker_or_token_config_env(self):
        for name in (
            "GITHUB_TOKEN",
            "GH_TOKEN",
            "AIOPS_TRACKER_SECRET",
            "AIOPS_STATE_API_TOKEN",
        ):
            with self.subTest(name=name):
                payload = self.inspect_payload()
                payload["Config"]["Env"].append(f"{name}=not-a-real-token")
                fake = mock.Mock(return_value=completed([], json.dumps([payload])))
                with mock.patch.object(controller.subprocess, "run", fake):
                    with self.assertRaisesRegex(ValueError, "Config.Env"):
                        controller.inspect_and_probe_mounts(
                            self.container, self.mounts, timeout_seconds=2
                        )
                self.assertEqual(fake.call_count, 1)

    def test_mount_inspect_proves_read_only_secret_sources_and_destinations(self):
        calls = []

        def fake_run(args, **kwargs):
            calls.append(args)
            if args[:2] == ["docker", "inspect"]:
                return completed(args, json.dumps([self.inspect_payload()]))
            return completed(args, f"aiops\t1000\t1000\t{args[-1]}\n")

        with mock.patch.object(controller.subprocess, "run", side_effect=fake_run):
            proof = controller.inspect_and_probe_mounts(
                self.container,
                self.mounts + self.secret_mounts,
                timeout_seconds=2,
            )

        secrets = [item for item in proof["mounts"] if not item["rw"]]
        self.assertEqual(
            {item["destination"] for item in secrets},
            {
                "/run/secrets/github_token",
                controller.STATE_SECRET,
                controller.STATE_REQUEST_CONFIG,
            },
        )
        self.assertEqual(
            len(calls), 4, "read-only secrets must not receive write probes"
        )

    def test_state_boundary_requires_unauthenticated_401_then_token_success(self):
        calls = []

        def fake_run(args, **kwargs):
            calls.append(args)
            if len(calls) == 1:
                self.assertIn(
                    'expected="header = Authorization: Bearer $token"', args[-1]
                )
            if len(calls) == 2:
                return completed(args, "HTTP/1.1 401 Unauthorized\n")
            if len(calls) == 3:
                return completed(args, json.dumps(self.valid_state()))
            return completed(args)

        with mock.patch.object(controller.subprocess, "run", side_effect=fake_run):
            proof = controller.prove_state_exec_boundary(
                self.container, timeout_seconds=2
            )

        self.assertEqual(proof["unauthenticated_status"], 401)
        self.assertEqual(proof["state"]["running"], [])
        flattened = " ".join(" ".join(call) for call in calls)
        self.assertIn("127.0.0.1:4000/api/v1/state", flattened)
        self.assertIn("/run/secrets/state_wgetrc", flattened)
        self.assertIn("/run/secrets/aiops_state_api_token", flattened)
        self.assertNotIn("state-token-value", flattened)
        self.assertNotIn("-p 4000", flattened)

    def test_state_boundary_rejects_secret_and_request_config_mismatch(self):
        fake = mock.Mock(return_value=completed([], stderr="mismatch", returncode=1))
        with mock.patch.object(controller.subprocess, "run", fake):
            with self.assertRaisesRegex(RuntimeError, "secret request config"):
                controller.prove_state_exec_boundary(self.container, timeout_seconds=2)
        self.assertEqual(fake.call_count, 1)

    def test_state_read_rejects_missing_or_malformed_accounting_schema(self):
        cases = {
            "empty": {},
            "missing-list": {
                "codex_totals": {"total_tokens": 0},
                "completed_session_usage": [],
                "running": [],
                "blocked": [],
            },
            "missing-row-tokens": {
                **self.valid_state(),
                "running": [{"issue_identifier": "#1"}],
            },
            "wrong-total-type": {
                **self.valid_state(),
                "codex_totals": {"total_tokens": "0"},
            },
        }
        for name, state in cases.items():
            with (
                self.subTest(name=name),
                mock.patch.object(
                    controller.subprocess,
                    "run",
                    return_value=completed([], json.dumps(state)),
                ),
            ):
                with self.assertRaisesRegex(ValueError, "state schema"):
                    controller.read_state_via_exec(self.container, timeout_seconds=2)

    def test_state_boundary_rejects_unauthenticated_200(self):
        fake = mock.Mock(return_value=completed([], "HTTP/1.1 200 OK\n"))
        with mock.patch.object(controller.subprocess, "run", fake):
            with self.assertRaisesRegex(ValueError, "401"):
                controller.prove_state_exec_boundary(self.container, timeout_seconds=2)
        self.assertEqual(fake.call_count, 2)


class ForgeObservationTests(unittest.TestCase):
    def write_fake_gh(self, root: Path) -> Path:
        gh = root / "gh"
        gh.write_text(
            """#!/bin/sh
set -eu
printf '%s\\n' "$*" >>"$FAKE_GH_LOG"
case "$*" in
  *"/issues/1/comments"*)
    if [ "${FAKE_GH_MODE:-no_pr}" = no_pr ]; then
      printf '%s\\n' '[]'
    else
      printf '%s\\n' '[{"user":{"login":"xrf-9527"},"body":"https://github.com/acme/fresh/pull/7","created_at":"2026-07-17T00:00:00Z"}]'
    fi
    ;;
  *"/issues/1"*) printf '%s\\n' '{"number":1,"state":"open"}' ;;
  *"pr view 7"*)
    if [ "${FAKE_GH_MODE:-}" = timeout ]; then
      printf '%s' "$$" >"$FAKE_GH_PID"
      trap '' TERM HUP
      while :; do sleep 30; done
    fi
    if [ "${FAKE_GH_MODE:-}" = attacker_pr ]; then
      printf '%s\\n' '{"number":7,"author":{"login":"attacker"},"headRefOid":"head","baseRefOid":"base"}'
    else
      printf '%s\\n' '{"number":7,"author":{"login":"xrf-9527"},"headRefOid":"head","baseRefOid":"base"}'
    fi
    ;;
  *"/issues/7/comments"*|*"/pulls/7/reviews"*) printf '%s\\n' '[]' ;;
  *"graphql"*)
    if [ "${FAKE_GH_MODE:-}" = nested_more ]; then
      printf '%s\\n' '{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[{"comments":{"nodes":[],"pageInfo":{"hasNextPage":true,"endCursor":"more"}}}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}}'
    else
      printf '%s\\n' '{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}}'
    fi
    ;;
  *) printf '%s\\n' '[]' ;;
esac
""",
            encoding="utf-8",
        )
        gh.chmod(0o755)
        return gh

    def test_no_pr_poll_uses_only_two_deterministic_stages(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            fake_gh = self.write_fake_gh(root)
            event_log = root / "events.jsonl"
            call_log = root / "calls.jsonl"
            env = {
                **os.environ,
                "FAKE_GH_LOG": str(call_log),
                "FAKE_GH_MODE": "no_pr",
            }

            snapshot = controller.observe_forge_stages(
                repo="acme/fresh",
                issue_number=1,
                poll_id="poll-0001",
                gh_config_dir=root,
                event_log=event_log,
                timeout_seconds=5,
                gh_binary=str(fake_gh),
                env=env,
            )

            self.assertEqual(snapshot["issue"]["number"], 1)
            self.assertEqual(snapshot["issue_comments"], [])
            self.assertNotIn("pr", snapshot)
            calls = call_log.read_text().splitlines()
            self.assertEqual(len(calls), 2)
            events = [json.loads(line) for line in event_log.read_text().splitlines()]
            completed = [
                event["stage_id"]
                for event in events
                if event["event"] == "forge_stage_completed"
            ]
            self.assertEqual(
                completed, ["poll-0001/01-issue", "poll-0001/02-issue-comments"]
            )
            self.assertEqual(events[-1]["event"], "forge_poll_completed")

    def test_timeout_persists_stage_and_partial_results_and_reaps_process(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            fake_gh = self.write_fake_gh(root)
            event_log = root / "events.jsonl"
            call_log = root / "calls.jsonl"
            pid_file = root / "pid"
            env = {
                **os.environ,
                "FAKE_GH_LOG": str(call_log),
                "FAKE_GH_MODE": "timeout",
                "FAKE_GH_PID": str(pid_file),
            }

            with self.assertRaises(controller.ForgeObservationError) as caught:
                controller.observe_forge_stages(
                    repo="acme/fresh",
                    issue_number=1,
                    poll_id="poll-0001",
                    gh_config_dir=root,
                    event_log=event_log,
                    timeout_seconds=2,
                    gh_binary=str(fake_gh),
                    env=env,
                )

            self.assertEqual(caught.exception.stage_id, "poll-0001/03-pr")
            self.assertEqual(caught.exception.partial_results["issue"]["number"], 1)
            self.assertEqual(len(caught.exception.partial_results["issue_comments"]), 1)
            events = [json.loads(line) for line in event_log.read_text().splitlines()]
            self.assertEqual(
                [event["event"] for event in events],
                [
                    "forge_stage_started",
                    "forge_stage_completed",
                    "forge_stage_started",
                    "forge_stage_completed",
                    "forge_stage_started",
                    "forge_stage_failed",
                ],
            )
            self.assertEqual(events[-1]["stage_id"], "poll-0001/03-pr")
            self.assertEqual(events[-1]["partial_results"]["issue"]["number"], 1)
            self.assertTrue(events[-1]["process_reaped"])
            pid = int(pid_file.read_text())
            with self.assertRaises(ProcessLookupError):
                os.kill(pid, 0)

    def test_pr_discovery_and_pr_author_are_bound_to_maker(self):
        comments = [
            {
                "user": {"login": "xrf-9527"},
                "body": "https://github.com/acme/fresh/pull/7",
                "created_at": "2026-07-17T00:00:00Z",
            },
            {
                "user": {"login": "attacker"},
                "body": "https://github.com/acme/fresh/pull/8",
                "created_at": "2026-07-17T00:01:00Z",
            },
        ]
        self.assertEqual(controller._newest_pr_number(comments, "acme/fresh"), 7)

        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            env = {
                **os.environ,
                "FAKE_GH_LOG": str(root / "calls.jsonl"),
                "FAKE_GH_MODE": "attacker_pr",
            }
            with self.assertRaises(controller.ForgeObservationError) as caught:
                controller.observe_forge_stages(
                    repo="acme/fresh",
                    issue_number=1,
                    poll_id="attacker-pr",
                    gh_config_dir=root,
                    event_log=root / "events.jsonl",
                    timeout_seconds=5,
                    gh_binary=str(self.write_fake_gh(root)),
                    env=env,
                )
        self.assertEqual(caught.exception.stage_id, "attacker-pr/03-pr")

    def test_nested_review_comment_overflow_fails_closed(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            env = {
                **os.environ,
                "FAKE_GH_LOG": str(root / "calls.jsonl"),
                "FAKE_GH_MODE": "nested_more",
            }
            with self.assertRaises(controller.ForgeObservationError) as caught:
                controller.observe_forge_stages(
                    repo="acme/fresh",
                    issue_number=1,
                    poll_id="nested-overflow",
                    gh_config_dir=root,
                    event_log=root / "events.jsonl",
                    timeout_seconds=5,
                    gh_binary=str(self.write_fake_gh(root)),
                    env=env,
                )
        self.assertEqual(caught.exception.stage_id, "nested-overflow/06-review-threads")
        self.assertEqual(caught.exception.reason, "incomplete")

    def test_live_timeout_injection_reaches_pr_stage(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            proof = controller._forge_timeout_injection(root, root / "events.jsonl")
        self.assertEqual(proof["stage_id"], "preactivation-timeout-injection/03-pr")
        self.assertEqual(proof["reason"], "timeout")
        self.assertEqual(proof["partial_keys"], ["issue", "issue_comments"])

    def test_append_event_fsyncs_before_return(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "events.jsonl"
            with mock.patch.object(controller.os, "fsync", wraps=os.fsync) as fsync:
                controller.append_event(path, {"event": "proof"})
            self.assertEqual(fsync.call_count, 1)
            event = json.loads(path.read_text())
            self.assertEqual(event["event"], "proof")
            self.assertIn("timestamp", event)


class RuntimeLimitTests(unittest.TestCase):
    def state(self, total, *, completed=(), running=(), blocked=(), retrying=()):
        return {
            "codex_totals": {"total_tokens": total},
            "completed_session_usage": list(completed),
            "running": list(running),
            "blocked": list(blocked),
            "retrying": list(retrying),
        }

    def row(self, issue, tokens):
        return {
            "issue_identifier": f"#{issue}",
            "tokens": {"total_tokens": tokens},
        }

    def test_usage_counts_each_claim_and_matches_process_delta(self):
        states = {
            "maker": self.state(
                150,
                completed=(self.row(1, 100),),
                running=(self.row(1, 50),),
            ),
            "reviewer": self.state(40, running=(self.row(1, 40),)),
        }
        usage = controller.summarize_issue_usage(
            states, issue_number=1, baseline_totals={"maker": 0, "reviewer": 0}
        )
        self.assertEqual(usage["claim_count"], 3)
        self.assertEqual(usage["worker_total_delta"], 190)
        self.assertEqual(usage["issue_attributed_tokens"], 190)
        self.assertTrue(usage["accounting_matches"])

    def test_fourth_claim_with_pending_retry_breaches_before_claim_five(self):
        claims = tuple(self.row(1, 10) for _ in range(4))
        states = {
            "maker": self.state(40, completed=claims, retrying=(self.row(1, 0),)),
            "reviewer": self.state(0),
        }
        usage = controller.summarize_issue_usage(
            states, issue_number=1, baseline_totals={"maker": 0, "reviewer": 0}
        )
        breach = controller.detect_limit_breach(
            usage, wall_seconds=10, external_evidence={}
        )
        self.assertEqual(breach["reason"], "claim_five_pending")

    def test_token_accounting_mismatch_fails_closed(self):
        states = {
            "maker": self.state(101, running=(self.row(1, 100),)),
            "reviewer": self.state(0),
        }
        usage = controller.summarize_issue_usage(
            states, issue_number=1, baseline_totals={"maker": 0, "reviewer": 0}
        )
        breach = controller.detect_limit_breach(
            usage, wall_seconds=10, external_evidence={}
        )
        self.assertEqual(breach["reason"], "token_accounting_mismatch")

    def test_completion_requires_reliable_external_signal(self):
        snapshot = {
            "issue": {"number": 1, "state": "closed"},
            "pr": {"number": 7, "mergedAt": "2026-07-17T00:00:00Z"},
        }
        states = {"maker": self.state(0), "reviewer": self.state(0)}
        incomplete, breach = controller._completion_state(
            snapshot, states, 1, {"reliable_signal": False}
        )
        complete, _ = controller._completion_state(
            snapshot, states, 1, {"reliable_signal": True}
        )
        self.assertFalse(incomplete)
        self.assertIsNone(breach)
        self.assertTrue(complete)

    def test_secret_scan_normalizes_trailing_newline(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "events.jsonl"
            path.write_text("complete-token-value-without-newline", encoding="utf-8")
            with self.assertRaisesRegex(ValueError, "credential"):
                controller._secret_scan(
                    ["complete-token-value-without-newline\n"], [path], {}
                )

    def checkpoint(self, *, head="head", base="base", submitted="2026-07-17T00:00:00Z"):
        return {
            "user": {"login": "zjlgdx", "id": 1},
            "state": "COMMENTED",
            "body": (
                f"Reviewer checkpoint: headRefOid={head} baseRefOid={base} "
                "baseRefName=main local-rubric=PASS"
            ),
            "commit_id": head,
            "submitted_at": submitted,
        }

    def forge_snapshot(self, *, comments, reviews):
        return {
            "pr": {
                "headRefOid": "head",
                "baseRefOid": "base",
                "baseRefName": "main",
            },
            "pr_comments": comments,
            "reviews": reviews,
        }

    def trigger(self, trigger_id, created="2026-07-17T00:01:00Z"):
        return {
            "id": trigger_id,
            "user": {"login": "zjlgdx"},
            "body": "@codex review",
            "created_at": created,
        }

    def test_external_signal_is_exact_head_bot_review_after_trigger(self):
        bot_review = {
            "user": {"login": "chatgpt-codex-connector[bot]", "id": 199175422},
            "commit_id": "head",
            "submitted_at": "2026-07-17T00:02:00Z",
            "state": "COMMENTED",
        }
        seen = {}
        evidence = controller.evaluate_external_review(
            self.forge_snapshot(
                comments=[self.trigger(10)],
                reviews=[self.checkpoint(), bot_review],
            ),
            seen,
            now="2026-07-17T00:03:00Z",
        )
        self.assertTrue(evidence["reliable_signal"])
        self.assertIsNone(evidence["breach"])

    def test_external_trigger_without_checkpoint_and_duplicate_fail_closed(self):
        missing = controller.evaluate_external_review(
            self.forge_snapshot(comments=[self.trigger(10)], reviews=[]),
            {},
            now="2026-07-17T00:02:00Z",
        )
        self.assertEqual(missing["breach"]["reason"], "trigger_without_checkpoint")

        seen = {}
        duplicate = controller.evaluate_external_review(
            self.forge_snapshot(
                comments=[self.trigger(10), self.trigger(11, "2026-07-17T00:01:01Z")],
                reviews=[self.checkpoint()],
            ),
            seen,
            now="2026-07-17T00:02:00Z",
        )
        self.assertEqual(duplicate["breach"]["reason"], "duplicate_exact_tuple_trigger")

    def test_external_signal_timeout_is_measured_from_trigger_timestamp(self):
        evidence = controller.evaluate_external_review(
            self.forge_snapshot(
                comments=[self.trigger(10)],
                reviews=[self.checkpoint()],
            ),
            {},
            now="2026-07-17T00:11:01Z",
        )
        self.assertEqual(evidence["breach"]["reason"], "external_review_timeout")


class FakeStopProcess:
    launched = []
    communicated = []

    def __init__(self, args, **kwargs):
        self.args = args
        self.returncode = 0
        self.stdout = ""
        self.stderr = ""
        self.__class__.launched.append(self)

    def communicate(self, timeout=None):
        self.__class__.communicated.append(self)
        if len(self.__class__.communicated) == 1:
            if len(self.__class__.launched) != 2:
                raise AssertionError("waited before both stop commands launched")
        return self.stdout, self.stderr

    def poll(self):
        return self.returncode

    def kill(self):
        self.returncode = -9


class StopBoundaryTests(unittest.TestCase):
    def setUp(self):
        FakeStopProcess.launched = []
        FakeStopProcess.communicated = []

    def test_stop_persists_both_requests_before_wait_and_exact_terminal_state(self):
        maker = controller.ContainerRef("maker", CID_MAKER)
        reviewer = controller.ContainerRef("reviewer", CID_REVIEWER)

        def fake_run(args, **kwargs):
            if args[:2] == ["docker", "wait"]:
                return completed(args, "137\n")
            if args[:2] == ["docker", "inspect"]:
                cid = args[-1]
                payload = [
                    {
                        "Id": cid,
                        "State": {"Running": False, "Pid": 0, "ExitCode": 137},
                    }
                ]
                return completed(args, json.dumps(payload))
            raise AssertionError(args)

        with tempfile.TemporaryDirectory() as tmp:
            event_log = Path(tmp) / "events.jsonl"
            with (
                mock.patch.object(controller.subprocess, "Popen", FakeStopProcess),
                mock.patch.object(controller.subprocess, "run", side_effect=fake_run),
            ):
                proof = controller.stop_both_and_prove(
                    maker,
                    reviewer,
                    event_log,
                    {"event": "limit_breached", "reason": "injection"},
                    grace_seconds=1,
                    timeout_seconds=2,
                )

            events = [json.loads(line) for line in event_log.read_text().splitlines()]
            self.assertEqual(events[0]["event"], "limit_breached")
            self.assertEqual(events[1]["event"], "stop_requests")
            self.assertEqual(
                {item["role"] for item in events[1]["requests"]}, {"maker", "reviewer"}
            )
            self.assertEqual(len(FakeStopProcess.launched), 2)
            self.assertEqual(len(proof), 2)
            self.assertTrue(
                all(not item["running"] and item["pid"] == 0 for item in proof)
            )

    def test_terminal_proof_rejects_running_pid_or_exit_mismatch(self):
        maker = controller.ContainerRef("maker", CID_MAKER)
        reviewer = controller.ContainerRef("reviewer", CID_REVIEWER)
        cases = (
            ("running", {"Running": True, "Pid": 0, "ExitCode": 137}),
            ("pid", {"Running": False, "Pid": 42, "ExitCode": 137}),
            ("exit", {"Running": False, "Pid": 0, "ExitCode": 1}),
        )
        for name, state in cases:
            with self.subTest(name=name), tempfile.TemporaryDirectory() as tmp:
                FakeStopProcess.launched = []
                FakeStopProcess.communicated = []

                def fake_run(args, **kwargs):
                    if args[:2] == ["docker", "wait"]:
                        return completed(args, "137\n")
                    cid = args[-1]
                    return completed(args, json.dumps([{"Id": cid, "State": state}]))

                with (
                    mock.patch.object(controller.subprocess, "Popen", FakeStopProcess),
                    mock.patch.object(
                        controller.subprocess, "run", side_effect=fake_run
                    ),
                ):
                    with self.assertRaises(ValueError):
                        controller.stop_both_and_prove(
                            maker,
                            reviewer,
                            Path(tmp) / "events.jsonl",
                            {"event": "limit_breached", "reason": "injection"},
                            grace_seconds=1,
                            timeout_seconds=2,
                        )

    def test_initial_evidence_fsync_failure_still_kills_both_containers(self):
        maker = controller.ContainerRef("maker", CID_MAKER)
        reviewer = controller.ContainerRef("reviewer", CID_REVIEWER)
        run = mock.Mock(return_value=completed([]))
        popen = mock.Mock()
        with (
            mock.patch.object(
                controller, "append_event", side_effect=OSError("disk full")
            ),
            mock.patch.object(controller.subprocess, "run", run),
            mock.patch.object(controller.subprocess, "Popen", popen),
        ):
            with self.assertRaisesRegex(OSError, "disk full"):
                controller.stop_both_and_prove(
                    maker,
                    reviewer,
                    Path("ignored"),
                    {"event": "limit_breached"},
                    grace_seconds=1,
                    timeout_seconds=2,
                )
        popen.assert_not_called()
        killed = [call.args[0][-1] for call in run.call_args_list]
        self.assertEqual(killed, [CID_MAKER, CID_REVIEWER])


class ControllerRunTests(unittest.TestCase):
    def test_activation_confirmation_failure_uses_abort_stop_path(self):
        runtime = {"roles": {"reviewer": {"gh_config": "/tmp/reviewer-gh"}}}
        containers = {
            "maker": controller.ContainerRef("maker", CID_MAKER),
            "reviewer": controller.ContainerRef("reviewer", CID_REVIEWER),
        }
        states = {
            "maker": {"codex_totals": {"total_tokens": 0}},
            "reviewer": {"codex_totals": {"total_tokens": 0}},
        }
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "preflight.json").write_text("{}", encoding="utf-8")
            abort = mock.Mock(side_effect=RuntimeError("aborted safely"))
            with (
                mock.patch.object(controller, "ASSET_DIR", root),
                mock.patch.object(controller, "_load_runtime", return_value=runtime),
                mock.patch.object(
                    controller, "_container_refs", return_value=containers
                ),
                mock.patch.object(
                    controller, "_verify_activation_gate", return_value=states
                ),
                mock.patch.object(
                    controller,
                    "_activate_issue",
                    side_effect=ValueError("confirmation failed"),
                ),
                mock.patch.object(controller, "_abort_run", abort),
            ):
                with self.assertRaisesRegex(RuntimeError, "aborted safely"):
                    controller.execute_run(root)
        abort.assert_called_once()
        self.assertEqual(abort.call_args.kwargs["containers"], containers)

    def test_activation_gate_wires_all_read_only_secret_mounts(self):
        containers = {
            "maker": controller.ContainerRef("maker", CID_MAKER),
            "reviewer": controller.ContainerRef("reviewer", CID_REVIEWER),
        }
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            roles = {}
            for role in ("maker", "reviewer"):
                role_root = root / role
                role_root.mkdir()
                paths = {
                    name: role_root / name
                    for name in (
                        "workspace",
                        "mirror",
                        "codex_home",
                        "github_token",
                        "state_token",
                        "state_wgetrc",
                    )
                }
                for name in ("github_token", "state_token", "state_wgetrc"):
                    paths[name].write_text("secret\n", encoding="utf-8")
                    paths[name].chmod(0o600)
                roles[role] = {name: str(path) for name, path in paths.items()}
                gh_config = role_root / "gh-config"
                gh_config.mkdir()
                roles[role]["gh_config"] = str(gh_config)
            runtime = {"image": "frozen-image", "roles": roles}
            preflight = {
                "decision": "GO",
                "frozen_files": {},
                "image": {"tag": "frozen-image"},
                "containers": {
                    role: {"container_id": container.container_id}
                    for role, container in containers.items()
                },
            }
            inspect = mock.Mock(
                side_effect=lambda container, mounts, **kwargs: {
                    "container_id": container.container_id,
                    "mounts": mounts,
                }
            )
            with (
                mock.patch.object(controller, "inspect_and_probe_mounts", inspect),
                mock.patch.object(
                    controller,
                    "_wait_for_state",
                    side_effect=lambda container, **kwargs: {"state": {}},
                ),
                mock.patch.object(controller, "_require_quiescent"),
                mock.patch.object(
                    controller, "_gh_json", return_value={"state": "open", "labels": []}
                ),
                mock.patch.object(controller, "append_event"),
            ):
                controller._verify_activation_gate(
                    runtime, preflight, containers, root / "events.jsonl"
                )

        self.assertEqual(inspect.call_count, 2)
        for call in inspect.call_args_list:
            mounts = call.args[1]
            self.assertIn(
                "/home/aiops/.config",
                {mount.destination for mount in mounts if mount.writable},
            )
            secrets = {mount.destination for mount in mounts if not mount.writable}
            self.assertEqual(
                secrets,
                {
                    "/run/secrets/github_token",
                    controller.STATE_SECRET,
                    controller.STATE_REQUEST_CONFIG,
                },
            )


if __name__ == "__main__":
    unittest.main()
