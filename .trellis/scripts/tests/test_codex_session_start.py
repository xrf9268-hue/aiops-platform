from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path


class CodexSessionStartTests(unittest.TestCase):
    def test_session_start_context_begins_with_sub_agent_notice(self) -> None:
        context = self.run_hook_context()
        self.assertTrue(
            context.startswith("<sub-agent-notice>\n"),
            f"additionalContext starts with {context[:80]!r}; want sub-agent notice first",
        )
        self.assertLess(
            context.index("Ignore all Trellis workflow guidance below this notice."),
            context.index("<session-context>"),
        )

    def test_session_start_preserves_platform_routing_labels(self) -> None:
        context = self.run_hook_context()

        self.assertIn("[Claude Code, Cursor, OpenCode, codex-sub-agent", context)
        self.assertIn("[codex-inline, Kilo, Antigravity, Windsurf]", context)
        self.assertLess(
            context.index("[codex-inline, Kilo, Antigravity, Windsurf]"),
            context.index("Before editing -> `trellis-before-dev`"),
        )

    def test_session_start_preserves_in_progress_status_when_prd_is_incomplete(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            repo = Path(tmp)
            trellis = repo / ".trellis"
            task_dir = trellis / "tasks" / "06-18-demo-task"
            task_dir.mkdir(parents=True)
            shutil.copytree(
                Path(__file__).resolve().parents[1] / "common",
                trellis / "scripts" / "common",
            )
            (trellis / "workflow.md").write_text(
                "## Phase Index\n\n- Test phase.\n\n## Phase 1: Plan\n",
                encoding="utf-8",
            )
            (task_dir / "task.json").write_text(
                json.dumps(
                    {
                        "id": "demo-task",
                        "name": "demo-task",
                        "title": "Demo Task",
                        "status": "in_progress",
                    },
                    indent=2,
                ),
                encoding="utf-8",
            )
            (task_dir / "prd.md").write_text(
                "# Demo Task\n\n"
                "## Goal\n\n"
                "TBD.\n\n"
                "## Requirements\n\n"
                "- TBD\n\n"
                "## Acceptance Criteria\n\n"
                "- [ ] TBD\n",
                encoding="utf-8",
            )
            sessions = trellis / ".runtime" / "sessions"
            sessions.mkdir(parents=True)
            (sessions / "demo.json").write_text(
                json.dumps({"current_task": ".trellis/tasks/06-18-demo-task"}),
                encoding="utf-8",
            )

            context = self.run_hook_context(repo)

        self.assertIn("Status: IN_PROGRESS\nTask: Demo Task", context)
        self.assertIn("Follow the matching per-turn workflow-state", context)
        self.assertNotIn("Load trellis-brainstorm", context)

    def run_hook_context(self, cwd: Path | None = None) -> str:
        repo = Path(__file__).resolve().parents[3]
        hook = repo / ".codex" / "hooks" / "session-start.py"
        env = os.environ.copy()
        for key in (
            "TRELLIS_HOOKS",
            "TRELLIS_DISABLE_HOOKS",
            "CODEX_NON_INTERACTIVE",
            "CODEX_SESSION_ID",
            "CODEX_THREAD_ID",
        ):
            env.pop(key, None)

        hook_cwd = cwd or repo
        result = subprocess.run(
            [sys.executable, str(hook)],
            cwd=hook_cwd,
            input=json.dumps({"cwd": str(hook_cwd)}),
            capture_output=True,
            text=True,
            encoding="utf-8",
            env=env,
        )

        self.assertEqual(result.returncode, 0, result.stderr)
        payload = json.loads(result.stdout)
        return payload["hookSpecificOutput"]["additionalContext"]


if __name__ == "__main__":
    unittest.main()
