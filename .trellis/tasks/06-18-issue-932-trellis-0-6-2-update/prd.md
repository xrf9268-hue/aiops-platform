# Issue #932: Update Trellis project files to 0.6.2

## Goal

Update this repository's Trellis-managed project files from Trellis 0.5.19 to 0.6.2 in a dedicated branch while preserving or intentionally merging local edits to the files that `trellis update --dry-run` reports as modified locally.

## What I Already Know

- GitHub issue: https://github.com/xrf9268-hue/aiops-platform/issues/932
- Labels: `area:foundation`, `priority:p2`, `type:chore`.
- Current project and local CLI were reported at 0.5.19 in the issue; `npm view @mindfoldhq/trellis version` returns 0.6.2 in this session.
- The issue says `trellis update --dry-run` previously reported local decisions needed for:
  - `.trellis/scripts/common/task_utils.py`
  - `.trellis/scripts/common/task_store.py`
  - `.claude/settings.json`
- Issue-phase owner probe:
  - `git ls-remote origin 'refs/heads/fix/932-*'` returned no branch.
  - REST PR search for issue `932` returned no open PR; broad search only surfaced unrelated closed PRs.

## Requirements

- Review Trellis 0.5.19 to 0.6.2 changes before applying the update.
- Run `trellis update --dry-run` first and record the decision before applying.
- Preserve or consciously merge local changes in `.trellis/scripts/common/task_utils.py`, `.trellis/scripts/common/task_store.py`, and `.claude/settings.json`.
- Apply the update on branch `fix/932-trellis-0-6-2-update`.
- Verify Trellis bootstrap, `get_context.py`, task archive, and `add_session.py` still work after the update.
- Keep the PR scoped to Trellis project files and directly related validation artifacts.

## Acceptance Criteria

- [x] Trellis 0.5.19 to 0.6.2 changes are reviewed and summarized.
- [x] `trellis update --dry-run` output is captured before applying.
- [x] Local changes in the three reported files are preserved or consciously merged.
- [x] Update is applied on a dedicated branch/PR.
- [x] Trellis bootstrap works after the update.
- [x] `python3 ./.trellis/scripts/get_context.py` works after the update.
- [x] Task archive flow works after the update.
- [x] `python3 ./.trellis/scripts/add_session.py` works after the update.

## Definition of Done

- Local validation covers the commands required by the issue.
- Existing repo gates affected by Trellis scripts pass where practical.
- PR body includes `Closes #932`, dry-run decision, acceptance criteria, verification commands, and size-gate classification.
- Do not merge without explicit user permission.

## Out of Scope

- Product docs changes unrelated to Trellis.
- Worker behavior changes.
- `trellis update --force` without reviewing the local modifications.

## Technical Notes

- Source-of-truth workflow: `.claude/skills/handle-issue/SKILL.md`.
- Shared PR/review protocol: `docs/runbooks/pr-review-merge-protocol.md`.
- Trellis guidance: `.trellis/spec/backend/agent-workflow-guidelines.md` and `.trellis/spec/backend/quality-guidelines.md`.
- Dry run was run with both installed Trellis 0.5.19 and `npx -y @mindfoldhq/trellis@0.6.2`; the 0.6.2 dry run recommended `--migrate` and listed the same three local-decision files.
- Applied with `npx -y @mindfoldhq/trellis@0.6.2 update --migrate --skip-all`, then manually merged `.trellis/scripts/common/task_store.py` so upstream 0.6.2 PRD/archive behavior and the repo's duplicate-task rejection both survive.
- `.trellis/scripts/common/task_utils.py` and `.claude/settings.json` were intentionally preserved because their local changes are not changed by the 0.6.2 templates.
- Verification evidence: `init_developer.py yvan`, `get_context.py`, `get_context.py --mode packages`, smoke `task.py create` + `task.py archive --no-commit` cleanup, real `add_session.py --no-commit` cleanup, Trellis Python tests, Go CI gates, JSON/TOML parse, and Python compile checks.
- Pre-push review resolution: Codex CLI flagged stale `--tag` examples in the new generated `trellis-channel` docs; verified against `npx -y @mindfoldhq/trellis@0.6.2 channel {send,wait,messages} --help` and patched both `.agents` and `.claude` copies to use supported `--kind` / plain targeted-message examples. The final 0.6.2 dry run therefore reports those channel docs as local customizations in addition to the original three files. Codex also questioned the untracked-task archive auto-commit branch in `task_store.py`; that branch matches the published 0.6.2 template exactly and is intentionally left unchanged.
- Second review resolution: Codex CLI found additional stale channel examples (`--kind interrupt`, `forum list`, `thread show`, and `context <channel> --as <worker>`). Verified against `npx -y @mindfoldhq/trellis@0.6.2 channel --help` plus subcommand help, then patched both generated skill trees to use `interrupt_requested,interrupted`, `forum <channel>`, `thread <channel> <thread>`, and `context list <channel>`. Also aligned `.codex/hooks/session-start.py` with the safer `.claude` status formatting (`str(task_status).upper()`).
