from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path


class TaskPrdReadinessTests(unittest.TestCase):
    def setUp(self) -> None:
        self.tempdir = tempfile.TemporaryDirectory()
        self.repo = Path(self.tempdir.name)
        self.trellis = self.repo / ".trellis"
        self.task_dir = self.trellis / "tasks" / "06-18-demo-task"
        self.task_dir.mkdir(parents=True)
        (self.trellis / ".developer").write_text("name=tester\n", encoding="utf-8")
        subprocess.run(["git", "init", "-q", "-b", "main"], cwd=self.repo, check=True)
        self.task_py = Path(__file__).resolve().parents[1] / "task.py"
        self.write_task_json(status="planning")

    def tearDown(self) -> None:
        self.tempdir.cleanup()

    def test_start_rejects_default_prd_placeholders(self) -> None:
        (self.task_dir / "prd.md").write_text(
            "# Demo Task\n\n"
            "## Goal\n\n"
            "TBD.\n\n"
            "## Requirements\n\n"
            "- TBD\n\n"
            "## Acceptance Criteria\n\n"
            "- [ ] TBD\n",
            encoding="utf-8",
        )

        result = self.run_task_start()

        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("prd.md is incomplete", result.stdout)
        data = json.loads((self.task_dir / "task.json").read_text(encoding="utf-8"))
        self.assertEqual(data["status"], "planning")

    def test_start_rejects_empty_required_prd_sections(self) -> None:
        (self.task_dir / "prd.md").write_text(
            "# Demo Task\n\n"
            "## Goal\n\n"
            "Exercise the start gate.\n\n"
            "## Requirements\n\n"
            "## Acceptance Criteria\n\n",
            encoding="utf-8",
        )

        result = self.run_task_start()

        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("prd.md is incomplete", result.stdout)
        data = json.loads((self.task_dir / "task.json").read_text(encoding="utf-8"))
        self.assertEqual(data["status"], "planning")

    def test_start_accepts_filled_prd(self) -> None:
        (self.task_dir / "prd.md").write_text(
            "# Demo Task\n\n"
            "## Goal\n\n"
            "Exercise the start gate.\n\n"
            "## Requirements\n\n"
            "- Preserve explicit requirements.\n\n"
            "## Acceptance Criteria\n\n"
            "- [ ] task.py start advances the task after requirements are filled.\n",
            encoding="utf-8",
        )

        result = self.run_task_start()

        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        data = json.loads((self.task_dir / "task.json").read_text(encoding="utf-8"))
        self.assertEqual(data["status"], "in_progress")

    def test_start_accepts_goal_placeholder_when_required_sections_are_filled(self) -> None:
        (self.task_dir / "prd.md").write_text(
            "# Demo Task\n\n"
            "## Goal\n\n"
            "TBD.\n\n"
            "## Requirements\n\n"
            "- Preserve explicit requirements.\n\n"
            "## Acceptance Criteria\n\n"
            "- [ ] task.py start advances when required sections are filled.\n",
            encoding="utf-8",
        )

        result = self.run_task_start()

        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        data = json.loads((self.task_dir / "task.json").read_text(encoding="utf-8"))
        self.assertEqual(data["status"], "in_progress")

    def test_start_accepts_legacy_prd_with_acceptance_criteria(self) -> None:
        (self.task_dir / "prd.md").write_text(
            "# Demo Task\n\n"
            "## Verdict (researched per AGENTS.md principle 7)\n\n"
            "The researched verdict supplies the implementation requirements.\n\n"
            "## Acceptance criteria (from the issue)\n\n"
            "- [ ] task.py start accepts detailed pre-skeleton PRDs.\n",
            encoding="utf-8",
        )

        result = self.run_task_start()

        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        data = json.loads((self.task_dir / "task.json").read_text(encoding="utf-8"))
        self.assertEqual(data["status"], "in_progress")

    def test_start_rejects_legacy_prd_without_acceptance_criteria(self) -> None:
        (self.task_dir / "prd.md").write_text(
            "# Demo Task\n\n"
            "## Verdict\n\n"
            "The task has notes but no acceptance criteria.\n",
            encoding="utf-8",
        )

        result = self.run_task_start()

        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("prd.md is incomplete", result.stdout)
        data = json.loads((self.task_dir / "task.json").read_text(encoding="utf-8"))
        self.assertEqual(data["status"], "planning")

    def write_task_json(self, *, status: str) -> None:
        (self.task_dir / "task.json").write_text(
            json.dumps(
                {
                    "id": "demo-task",
                    "name": "demo-task",
                    "title": "Demo Task",
                    "status": status,
                    "assignee": "tester",
                },
                indent=2,
            ),
            encoding="utf-8",
        )

    def run_task_start(self) -> subprocess.CompletedProcess[str]:
        env = os.environ.copy()
        env["TRELLIS_CONTEXT_ID"] = "test-session"
        return subprocess.run(
            [sys.executable, str(self.task_py), "start", ".trellis/tasks/06-18-demo-task"],
            cwd=self.repo,
            env=env,
            capture_output=True,
            text=True,
            encoding="utf-8",
        )


if __name__ == "__main__":
    unittest.main()
