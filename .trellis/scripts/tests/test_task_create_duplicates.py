from __future__ import annotations

import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path


class TaskCreateDuplicateTests(unittest.TestCase):
    def setUp(self) -> None:
        self.tempdir = tempfile.TemporaryDirectory()
        self.repo = Path(self.tempdir.name)
        (self.repo / ".trellis").mkdir()
        (self.repo / ".trellis" / ".developer").write_text("name=tester\n", encoding="utf-8")
        subprocess.run(["git", "init", "-q", "-b", "main"], cwd=self.repo, check=True)
        self.task_py = Path(__file__).resolve().parents[1] / "task.py"

    def tearDown(self) -> None:
        self.tempdir.cleanup()

    def test_duplicate_create_fails_without_overwriting_metadata(self) -> None:
        first = self.run_task_create()
        self.assertEqual(first.returncode, 0, first.stderr)

        task_dir = self.created_task_dir(first.stdout)
        task_json = task_dir / "task.json"
        task_data = json.loads(task_json.read_text(encoding="utf-8"))
        task_data.update({
            "status": "in_progress",
            "assignee": "original-assignee",
            "parent": "parent-task",
            "children": ["child-task"],
            "pr_url": "https://example.invalid/pr/1",
            "notes": "must survive duplicate create",
        })
        task_json.write_text(json.dumps(task_data, indent=2, ensure_ascii=False), encoding="utf-8")
        before = task_json.read_text(encoding="utf-8")

        duplicate = self.run_task_create()

        self.assertNotEqual(duplicate.returncode, 0, duplicate.stderr)
        self.assertIn(f"Task directory already exists: {task_dir.name}", duplicate.stderr)
        self.assertIn("Use a new slug", duplicate.stderr)
        self.assertEqual(task_json.read_text(encoding="utf-8"), before)

    def run_task_create(self) -> subprocess.CompletedProcess[str]:
        return subprocess.run(
            [
                sys.executable,
                str(self.task_py),
                "create",
                "Duplicate Example",
                "--slug",
                "duplicate-example",
                "--assignee",
                "tester",
                "--priority",
                "P2",
            ],
            cwd=self.repo,
            capture_output=True,
            text=True,
            encoding="utf-8",
        )

    def created_task_dir(self, stdout: str) -> Path:
        for line in stdout.splitlines():
            if line.startswith(".trellis/tasks/"):
                return self.repo / line
        self.fail(f"task.py create did not print task path; stdout={stdout!r}")


if __name__ == "__main__":
    unittest.main()
