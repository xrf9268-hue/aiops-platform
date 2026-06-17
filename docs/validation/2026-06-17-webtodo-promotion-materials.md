# Promotion Material Notes

Run root: `/tmp/aiops-webtodo-e2e-20260617-134243` (source capture directory; curated reusable assets are committed under `docs/validation/assets/`.)
Captured at: 2026-06-17 Asia/Shanghai

## Headline Story

- Latest binary under test: aiops-platform `v0.1.6`, downloaded from GitHub release and run locally.
- Scenario: local Gitea + maker-WORKFLOW + reviewer-automerge-WORKFLOW + Codex app-server.
- Repository: `aiops-bot/web-todo` on local Gitea.
- Main lifecycle evidence: issues #1-#10 completed end-to-end, each via maker PR handoff, reviewer verification, automerge, and `aiops/done`.
- Final validation: fresh clone passed Go/Make verification plus browser smoke with screenshots and video.

## Strongest Demo Moment

Issue #4 is the best promotion story so far:

1. Maker implemented PATCH/DELETE API and opened PR #17.
2. Reviewer caught a real edge-case bug: `PATCH /api/todos/{id}` accepted `{"completed": null}` and silently treated it as `false`.
3. Reviewer refused to merge, posted the finding, and moved the issue to `aiops/rework`.
4. Maker picked up the rework, pushed a fix back to the same PR.
5. Reviewer rechecked, merged PR #17, and moved issue #4 to `aiops/done`.

This is a clean "AI reviewer prevents a subtle backend bug before merge" story.

Issue #6 adds a controlled reviewer loop:

1. Maker opened PR #19 for toggle/delete UI controls.
2. Reviewer intentionally requested one regression-strengthening pass because the issue title carried `[EXPECT-REWORK]`.
3. The requested coverage is user-visible: deleting the final remaining todo must return the UI to the empty state, preventing stale DOM/state.
4. Maker picked the issue back up from `aiops/rework`.

Issue #7 adds a testing-quality review story:

1. Maker opened PR #20 for client-side filters and active counter.
2. Reviewer ran the Go verification suite successfully, then still requested rework.
3. The finding was not "tests fail"; it was "the regression test is too weak" because a string check would not catch filter hash changes that refetch via a different callback shape.
4. Reviewer asked for behavior-level coverage with mocked `fetch` and hashchange dispatches.

## Screenshot Inventory

- Gitea repo overview: `assets/2026-06-17-webtodo-gitea-repo.png`
- Issue board during run: `assets/2026-06-17-webtodo-gitea-issues.png`
- Issue #4 with rework discussion visible: `assets/2026-06-17-webtodo-gitea-issue-04-rework.png`
- PR #17 during rework: `assets/2026-06-17-webtodo-gitea-pr-17-rework.png`
- Issue #4 after done: `assets/2026-06-17-webtodo-gitea-issue-04-done.png`
- PR #17 after merge: `assets/2026-06-17-webtodo-gitea-pr-17-merged.png`
- Issue list after #4 done: `assets/2026-06-17-webtodo-gitea-issues-after-04.png`
- Maker dashboard initial capture: `assets/2026-06-17-webtodo-maker-dashboard.png`
- Reviewer dashboard initial capture: `assets/2026-06-17-webtodo-reviewer-dashboard.png`
- Maker dashboard after #4 done and #5 dispatch: `assets/2026-06-17-webtodo-maker-dashboard-issue-05.png`
- Reviewer dashboard after #4 done: `assets/2026-06-17-webtodo-reviewer-dashboard-idle-after-04.png`
- Issue #5 in review: `assets/2026-06-17-webtodo-gitea-issue-05-human-review.png`
- PR #18 in review: `assets/2026-06-17-webtodo-gitea-pr-18-open.png`
- Issue #6 controlled rework: `assets/2026-06-17-webtodo-gitea-issue-06-rework.png`
- PR #19 controlled rework: `assets/2026-06-17-webtodo-gitea-pr-19-rework.png`
- Maker dashboard during #6 rework: `assets/2026-06-17-webtodo-maker-dashboard-issue-06-rework.png`
- Issue #6 after done: `assets/2026-06-17-webtodo-gitea-issue-06-done.png`
- PR #19 after merge: `assets/2026-06-17-webtodo-gitea-pr-19-merged.png`
- Issue #7 in review: `assets/2026-06-17-webtodo-gitea-issue-07-human-review.png`
- PR #20 in review: `assets/2026-06-17-webtodo-gitea-pr-20-open.png`
- Issue #7 rework: `assets/2026-06-17-webtodo-gitea-issue-07-rework.png`
- PR #20 rework: `assets/2026-06-17-webtodo-gitea-pr-20-rework.png`
- Maker dashboard during #7 rework: `assets/2026-06-17-webtodo-maker-dashboard-issue-07-rework.png`
- Issue list after all primary work done: `assets/2026-06-17-webtodo-gitea-issues-all-done.png`
- Pull request list after all primary merges: `assets/2026-06-17-webtodo-gitea-pulls-all-merged.png`
- Final maker dashboard: `assets/2026-06-17-webtodo-maker-dashboard-final.png`
- Final reviewer dashboard: `assets/2026-06-17-webtodo-reviewer-dashboard-final.png`
- Final web UI with overdue todo: `assets/2026-06-17-webtodo-webui-final-created-overdue.png`
- Final web UI filtered completed: `assets/2026-06-17-webtodo-webui-final-filtered-completed.png`
- Final web UI inline edit: `assets/2026-06-17-webtodo-webui-final-edited.png`
- Final web UI empty after delete: `assets/2026-06-17-webtodo-webui-final-empty-after-delete.png`
- Final browser smoke video: `assets/2026-06-17-webtodo-browser-smoke.webm`
- Web UI probe on #5 PR branch, blank result retained for investigation: `assets/2026-06-17-webtodo-webui-issue-05-empty.png`
- TUI maker raw text: `assets/2026-06-17-webtodo-tui-maker-raw.txt`
- TUI reviewer raw text: `assets/2026-06-17-webtodo-tui-reviewer-raw.txt`

## Notes For Later Copy

- "The worker does not merge directly. The reviewer agent owns review and merge through the workflow surface."
- "The reviewer did not just rubber-stamp; it found a JSON null edge case even though the normal test suite passed."
- "The system recovered naturally: `human-review` -> `rework` -> maker fix -> `human-review` -> merge -> `done` -> next issue dispatch."
- "The workflow can force an explicit regression-strengthening loop for risky UI behavior."
- "Reviewer-automerge can reject weak tests even when `go test` passes."
- "Dashboard and TUI both expose operator-friendly lifecycle state, token usage, rate-limit snapshots, and handoff counters."

## Caveats To Mention Honestly

- Local Codex app-server inherited user-level skills/plugin startup behavior even with command-line attempts to minimize tool surface.
- Token usage is high in this desktop environment; this is useful operational evidence, not a benchmark.
- One screenshot set intentionally captures the in-flight rework state; the follow-up set captures the completed state.


## Committed Asset Notes

The screenshot inventory above has been rewritten to repository-relative links. The full screenshot set, final smoke video, TUI raw captures, and verification logs use the `2026-06-17-webtodo-` prefix under `docs/validation/assets/`. Raw Codex home, worker configs, downloaded binaries, auth files, and cache directories are intentionally excluded.
