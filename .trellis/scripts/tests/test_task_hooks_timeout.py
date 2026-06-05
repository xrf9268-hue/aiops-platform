from __future__ import annotations

import contextlib
import io
import os
import shlex
import signal
import sys
import tempfile
import time
import unittest
from pathlib import Path

from common import task_utils


MAX_HOOK_TEST_SECONDS = 2.0


class TaskHookTimeoutTests(unittest.TestCase):
    def test_timed_out_hook_warns_without_raising(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            repo = Path(tmp)
            trellis = repo / ".trellis"
            trellis.mkdir()
            marker = repo / "leaked"
            continued_marker = repo / "continued"
            child_code = (
                "import pathlib, signal, sys, time; "
                "signal.signal(signal.SIGTERM, signal.SIG_IGN); "
                "time.sleep(0.8); "
                "pathlib.Path(sys.argv[1]).write_text('leaked')"
            )
            hook_cmd = (
                f"{shlex.quote(sys.executable)} -c {shlex.quote(child_code)} "
                f"{shlex.quote(str(marker))} & wait"
            )
            continue_code = "import pathlib, sys; pathlib.Path(sys.argv[1]).write_text('continued')"
            continue_cmd = (
                f"{shlex.quote(sys.executable)} -c {shlex.quote(continue_code)} "
                f"{shlex.quote(str(continued_marker))}"
            )
            (trellis / "config.yaml").write_text(
                "hooks:\n"
                "  after_create:\n"
                f"    - {hook_cmd}\n"
                f"    - {continue_cmd}\n",
                encoding="utf-8",
            )
            task_json = trellis / "tasks" / "demo" / "task.json"
            task_json.parent.mkdir(parents=True)
            task_json.write_text("{}", encoding="utf-8")

            previous_timeout = task_utils.HOOK_TIMEOUT_SECONDS
            previous_grace = task_utils.HOOK_TERMINATION_GRACE_SECONDS
            task_utils.HOOK_TIMEOUT_SECONDS = 0.05
            task_utils.HOOK_TERMINATION_GRACE_SECONDS = 0.05
            stderr = io.StringIO()
            start = time.monotonic()
            try:
                with contextlib.redirect_stderr(stderr):
                    task_utils.run_task_hooks("after_create", task_json, repo)
            finally:
                task_utils.HOOK_TIMEOUT_SECONDS = previous_timeout
                task_utils.HOOK_TERMINATION_GRACE_SECONDS = previous_grace

            elapsed = time.monotonic() - start
            output = stderr.getvalue()
            self.assertIn("[WARN] Hook timed out (after_create):", output)
            self.assertIn("after 0.05s", output)
            self.assertLess(elapsed, MAX_HOOK_TEST_SECONDS)
            self.assertTrue(continued_marker.exists())
            time.sleep(0.9)
            self.assertFalse(marker.exists())

    def test_background_hook_child_holding_stdout_is_bounded(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            repo = Path(tmp)
            trellis = repo / ".trellis"
            trellis.mkdir()
            marker = repo / "background-leaked"
            continued_marker = repo / "background-continued"
            child_code = (
                "import pathlib, signal, sys, time; "
                "signal.signal(signal.SIGTERM, signal.SIG_IGN); "
                "sighup = getattr(signal, 'SIGHUP', None); "
                "signal.signal(sighup, signal.SIG_IGN) if sighup is not None else None; "
                "time.sleep(0.8); "
                "pathlib.Path(sys.argv[1]).write_text('leaked')"
            )
            hook_cmd = (
                f"{shlex.quote(sys.executable)} -c {shlex.quote(child_code)} "
                f"{shlex.quote(str(marker))} &"
            )
            continue_code = "import pathlib, sys; pathlib.Path(sys.argv[1]).write_text('continued')"
            continue_cmd = (
                f"{shlex.quote(sys.executable)} -c {shlex.quote(continue_code)} "
                f"{shlex.quote(str(continued_marker))}"
            )
            (trellis / "config.yaml").write_text(
                "hooks:\n"
                "  after_create:\n"
                f"    - {hook_cmd}\n"
                f"    - {continue_cmd}\n",
                encoding="utf-8",
            )
            task_json = trellis / "tasks" / "demo" / "task.json"
            task_json.parent.mkdir(parents=True)
            task_json.write_text("{}", encoding="utf-8")

            previous_timeout = task_utils.HOOK_TIMEOUT_SECONDS
            previous_grace = task_utils.HOOK_TERMINATION_GRACE_SECONDS
            task_utils.HOOK_TIMEOUT_SECONDS = 0.05
            task_utils.HOOK_TERMINATION_GRACE_SECONDS = 0.05
            stderr = io.StringIO()
            start = time.monotonic()
            try:
                with contextlib.redirect_stderr(stderr):
                    task_utils.run_task_hooks("after_create", task_json, repo)
            finally:
                task_utils.HOOK_TIMEOUT_SECONDS = previous_timeout
                task_utils.HOOK_TERMINATION_GRACE_SECONDS = previous_grace

            elapsed = time.monotonic() - start
            self.assertIn("[WARN] Hook timed out (after_create):", stderr.getvalue())
            self.assertLess(elapsed, MAX_HOOK_TEST_SECONDS)
            self.assertTrue(continued_marker.exists())
            time.sleep(0.9)
            self.assertFalse(marker.exists())

    def test_sigterm_responsive_hook_still_exits_without_leaking(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            repo = Path(tmp)
            trellis = repo / ".trellis"
            trellis.mkdir()
            marker = repo / "responsive-leaked"
            child_code = (
                "import pathlib, sys, time; "
                "time.sleep(0.8); "
                "pathlib.Path(sys.argv[1]).write_text('leaked')"
            )
            hook_cmd = (
                f"{shlex.quote(sys.executable)} -c {shlex.quote(child_code)} "
                f"{shlex.quote(str(marker))} & wait"
            )
            (trellis / "config.yaml").write_text(
                "hooks:\n"
                "  after_create:\n"
                f"    - {hook_cmd}\n",
                encoding="utf-8",
            )
            task_json = trellis / "tasks" / "demo" / "task.json"
            task_json.parent.mkdir(parents=True)
            task_json.write_text("{}", encoding="utf-8")

            previous_timeout = task_utils.HOOK_TIMEOUT_SECONDS
            previous_grace = task_utils.HOOK_TERMINATION_GRACE_SECONDS

            task_utils.HOOK_TIMEOUT_SECONDS = 0.05
            task_utils.HOOK_TERMINATION_GRACE_SECONDS = 0.05
            stderr = io.StringIO()
            start = time.monotonic()
            try:
                with contextlib.redirect_stderr(stderr):
                    task_utils.run_task_hooks("after_create", task_json, repo)
            finally:
                task_utils.HOOK_TIMEOUT_SECONDS = previous_timeout
                task_utils.HOOK_TERMINATION_GRACE_SECONDS = previous_grace

            elapsed = time.monotonic() - start
            self.assertIn("[WARN] Hook timed out (after_create):", stderr.getvalue())
            self.assertLess(elapsed, MAX_HOOK_TEST_SECONDS)
            time.sleep(0.9)
            self.assertFalse(marker.exists())

    def test_escaped_child_holding_stdout_does_not_block_later_hooks(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            repo = Path(tmp)
            trellis = repo / ".trellis"
            trellis.mkdir()
            child_pid_file = repo / "child.pid"
            continued_marker = repo / "escaped-continued"
            child_code = (
                "import os, pathlib, sys, time; "
                "os.setsid(); "
                "pathlib.Path(sys.argv[1]).write_text(str(os.getpid())); "
                "time.sleep(3)"
            )
            hook_cmd = (
                f"{shlex.quote(sys.executable)} -c {shlex.quote(child_code)} "
                f"{shlex.quote(str(child_pid_file))} & "
                f"while [ ! -s {shlex.quote(str(child_pid_file))} ]; do sleep 0.01; done; "
                "wait"
            )
            continue_code = "import pathlib, sys; pathlib.Path(sys.argv[1]).write_text('continued')"
            continue_cmd = (
                f"{shlex.quote(sys.executable)} -c {shlex.quote(continue_code)} "
                f"{shlex.quote(str(continued_marker))}"
            )
            (trellis / "config.yaml").write_text(
                "hooks:\n"
                "  after_create:\n"
                f"    - {hook_cmd}\n"
                f"    - {continue_cmd}\n",
                encoding="utf-8",
            )
            task_json = trellis / "tasks" / "demo" / "task.json"
            task_json.parent.mkdir(parents=True)
            task_json.write_text("{}", encoding="utf-8")

            previous_timeout = task_utils.HOOK_TIMEOUT_SECONDS
            previous_grace = task_utils.HOOK_TERMINATION_GRACE_SECONDS
            task_utils.HOOK_TIMEOUT_SECONDS = 0.05
            task_utils.HOOK_TERMINATION_GRACE_SECONDS = 0.05
            stderr = io.StringIO()
            start = time.monotonic()
            try:
                with contextlib.redirect_stderr(stderr):
                    task_utils.run_task_hooks("after_create", task_json, repo)
            finally:
                task_utils.HOOK_TIMEOUT_SECONDS = previous_timeout
                task_utils.HOOK_TERMINATION_GRACE_SECONDS = previous_grace
                if child_pid_file.exists():
                    child_pid = int(child_pid_file.read_text(encoding="utf-8"))
                    try:
                        os.kill(child_pid, signal.SIGKILL)
                    except ProcessLookupError:
                        pass

            elapsed = time.monotonic() - start
            self.assertIn("[WARN] Hook timed out (after_create):", stderr.getvalue())
            self.assertLess(elapsed, MAX_HOOK_TEST_SECONDS)
            self.assertTrue(continued_marker.exists())


if __name__ == "__main__":
    unittest.main()
