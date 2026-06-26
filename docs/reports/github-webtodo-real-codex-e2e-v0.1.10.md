# 最新二进制 + 真实 Codex + GitHub Web Todo E2E 实战报告

## Verdict

**PASS.** 本次使用 GitHub release `v0.1.10` 的 `darwin_arm64` 二进制和真实 `codex app-server` 跑完 GitHub Web Todo 实战：创建 14 个 issues，其中 12 个 feature issues 全部由真实 Codex agent 产出 PR、等待 CI 成功并执行 agent-side squash merge；#13 no-ready 控制场景保持 idle；#14 cancel-running 控制场景通过 tracker state 取消并未合并 PR。最终 fresh clone 验证全部通过。

关键边界：本次“合并机制”是 **agent-side merge**，不是 orchestrator/worker merge。worker 只负责 poll、workspace、runner lifecycle、reconcile；合并动作由 Codex agent 通过运行时 `gh pr merge --squash --delete-branch` 完成。

## Links And Roots

- Release: https://github.com/xrf9268-hue/aiops-platform/releases/tag/v0.1.10
- Repo: https://github.com/zjlgdx/aiops-e2e-vite-react-webtodo-20260626-0938
- Issues: https://github.com/zjlgdx/aiops-e2e-vite-react-webtodo-20260626-0938/issues
- PRs: https://github.com/zjlgdx/aiops-e2e-vite-react-webtodo-20260626-0938/pulls
- Actions: https://github.com/zjlgdx/aiops-e2e-vite-react-webtodo-20260626-0938/actions
- Run root: `/tmp/aiops-github-webtodo-e2e-20260626-093809`
- Workflow: `/tmp/aiops-github-webtodo-e2e-20260626-093809/workflows/WORKFLOW.md`
- Final report: `/tmp/aiops-github-webtodo-e2e-20260626-093809/reports/report.md`

## In-Repo Archive

This committed report keeps the original `/tmp` evidence paths for audit
fidelity. The selected in-repo archive is:

- Main report: `docs/reports/github-webtodo-real-codex-e2e-v0.1.10.md`
- Merge mechanism note: `docs/reports/github-webtodo-real-codex-e2e-v0.1.10-merge-mechanism-retro.md`
- Driver script: `docs/reports/scripts/github-webtodo-real-codex-e2e-v0.1.10/github_webtodo_driver.py`
- Final screenshot script: `docs/reports/scripts/github-webtodo-real-codex-e2e-v0.1.10/capture_final_screens.py`
- Selected screenshots: `docs/reports/assets/github-webtodo-real-codex-e2e-v0.1.10/`

## Binary / Auth / Doctor

| Item | Result |
| --- | --- |
| aiops release asset | `aiops-platform_v0.1.10_darwin_arm64.tar.gz` |
| aiops asset SHA256 | `a1c1ff710256dd900805e1f188a2d75e4417fcbbbbe3a97aef97d4cb5f6eaf34` |
| SBOM SHA256 | `ac885fa93d58556b3f1e5badc4c7f7c494e1be000c751d5bcd66593d9dcc13d2` |
| attestation | `gh attestation verify` exited `0`; command emitted no output, recorded in `artifacts/aiops-release-attestation-status.log` |
| `worker --version` | `v0.1.10` |
| `tui --version` | `v0.1.10` |
| Codex runtime | pinned OpenAI Codex `0.142.0`, `codex-cli 0.142.0` |
| Codex package SHA256 | `70e6d48af6b7f1b69f02667d209637a13db6e061c66a04394914f076a04ea092` |
| Doctor | `worker --doctor --deploy=binary --mode=real` passed; warning only for custom `codex.command` version/auth skip, with app-server probe and ChatGPT/Codex auth both passing |

Doctor evidence: `/tmp/aiops-github-webtodo-e2e-20260626-093809/artifacts/worker-doctor-real.log`

## Workflow Shape

- Tracker: GitHub, `active_states: [aiops:ready]`, `terminal_states: [closed, aiops:canceled]`.
- Runner: `agent.default: codex-app-server`, `codex.command: /tmp/.../tools/codex-0.142.0/bin/codex app-server`.
- Sandbox: `danger-full-access`.
- Concurrency: `max_concurrent_agents: 1`.
- Agent prompt required: one issue per branch/PR, local `npm test`, `npm run build`, `npm run test:e2e`, open PR with `Closes #N`, wait GitHub Actions, fix failures, then agent-side squash merge.
- No manual code fix / PR edit / label tweak occurred after activation. Ready/cancel/refresh/screenshot were performed only by the pre-started driver.

## Timeline

Times below are UTC.

| Time | Event |
| --- | --- |
| 2026-06-26 01:38 | Run root created and release/binary setup began |
| 2026-06-26 01:43-01:45 | Seed Vite + React + TS repo pushed; baseline GitHub Actions succeeded |
| 2026-06-26 01:47 | 14 GitHub issues created without `aiops:ready` |
| 2026-06-26 01:51:52 | Driver activated #1 |
| 2026-06-26 02:03:38 | PR #15 merged, closing #1 |
| 2026-06-26 02:04:03 | Driver activated cancel control #14 |
| 2026-06-26 02:05:00 | Driver observed #14 running, removed `aiops:ready`, added `aiops:canceled` |
| 2026-06-26 02:16:26 | PR #16 merged, closing #2 |
| 2026-06-26 02:29:49 | PR #17 merged, closing #3 |
| 2026-06-26 02:45:36 | PR #18 merged, closing #4 |
| 2026-06-26 02:58:08 | PR #19 merged, closing #5 |
| 2026-06-26 03:19:33 | PR #20 merged, closing #6 |
| 2026-06-26 03:37:06 | PR #21 merged, closing #7 |
| 2026-06-26 03:55:17 | PR #22 merged, closing #8 |
| 2026-06-26 04:20:26 | PR #23 merged, closing #9 |
| 2026-06-26 04:39:01 | PR #24 merged, closing #10 |
| 2026-06-26 05:02:20 | PR #25 merged, closing #11 |
| 2026-06-26 05:17:09 | PR #26 merged, closing #12 |
| 2026-06-26 05:17:47 | Driver emitted PASS and captured final success evidence |
| 2026-06-26 05:18:20 | Final main GitHub Actions run completed successfully |
| 2026-06-26 05:19-05:21 | Fresh clone verification and final app screenshots completed |

## Issue / PR Results

| Issue | Result | PR | Title | Merged At |
| --- | --- | --- | --- | --- |
| #1 | CLOSED | #15 | `feat: scaffold todo app shell` | 2026-06-26T02:03:38Z |
| #2 | CLOSED | #16 | `feat: add localStorage todo repository` | 2026-06-26T02:16:26Z |
| #3 | CLOSED | #17 | `feat: add todo create and list UI` | 2026-06-26T02:29:49Z |
| #4 | CLOSED | #18 | `feat: add todo toggle and delete controls` | 2026-06-26T02:45:36Z |
| #5 | CLOSED | #19 | `feat: add todo filters and active count` | 2026-06-26T02:58:08Z |
| #6 | CLOSED | #20 | `feat: add inline todo title editing` | 2026-06-26T03:19:33Z |
| #7 | CLOSED | #21 | `feat: add todo due dates` | 2026-06-26T03:37:06Z |
| #8 | CLOSED | #22 | `feat: add todo search sort preferences` | 2026-06-26T03:55:17Z |
| #9 | CLOSED | #23 | `feat: add accessible todo announcements` | 2026-06-26T04:20:26Z |
| #10 | CLOSED | #24 | `feat: add responsive dark mode polish` | 2026-06-26T04:39:01Z |
| #11 | CLOSED | #25 | `feat: add todo JSON import export` | 2026-06-26T05:02:20Z |
| #12 | CLOSED | #26 | `docs: add release README and e2e journey` | 2026-06-26T05:17:09Z |
| #13 | OPEN | none | CONTROL no-ready idle | n/a |
| #14 | OPEN + `aiops:canceled` | none merged | CONTROL cancel running | n/a |

Additional checks:

- Closing PR claims are exactly one per feature issue #1-#12.
- `author` and `mergedBy` for PR #15-#26 are `zjlgdx`; the agent used the active GitHub account via runtime tooling.
- Remote branches after closeout: only `main`.
- Final main commit: `1ba4e99fc5bd4b2241a6a5ec0aa759a2d461b5f6`.

Evidence JSON:

- Issues: `/tmp/aiops-github-webtodo-e2e-20260626-093809/github-json/issues-final.json`
- PRs: `/tmp/aiops-github-webtodo-e2e-20260626-093809/github-json/prs-final-rich.json`
- Actions: `/tmp/aiops-github-webtodo-e2e-20260626-093809/github-json/runs-final.json`
- Branches: `/tmp/aiops-github-webtodo-e2e-20260626-093809/github-json/branches-final.json`

## Abnormal / Control Scenarios

| Scenario | Expected | Actual | Verdict |
| --- | --- | --- | --- |
| #13 no-ready idle | Never add `aiops:ready`; no run, branch, or PR | Issue stayed OPEN with only `e2e:control`; worker log has no #13 dispatch; final remote branches only `main`; no PR claim | PASS |
| #14 cancel running | Start a run, then remove ready + add `aiops:canceled`; no merge | Driver activated #14, observed it in running state, then canceled it. Worker logged `CanceledByReconciliation` / `runner stopped: reconcile ineligible`; issue remained OPEN with `aiops:canceled`; no merged PR | PASS |
| Codex app-server startup flake | Retry or fail clearly | 9 `thread/start` 5s read timeouts were logged; worker retried and all feature issues ultimately completed | PASS with note |
| Agent self-recovery | Agent fixes local or CI failures itself | #9 had a local `npm test` failure in worker output; agent continued, fixed, reran, opened/merged green PR #23 | PASS |
| Agent PR iteration | Agent can update existing PR before merge | #11 produced multiple successful Actions runs before final merge, then deleted branch | PASS |

Worker counters at final snapshot: `running=0`, `operator_terminal_stops_total=13`. `runner_start=22`, `runner_stopped=13`, `startup_failed=9`, `runner_timeout=9`.

## Merge Mechanism Retrospective

This run used **agent-side merge**:

- Worker: poll GitHub, prepare deterministic workspace, launch `codex app-server`, reconcile ineligible/terminal tracker states, serve dashboard/API.
- Codex agent: implement, test, push, open PR, wait GitHub Actions, repair failures, and run `gh pr merge --squash --delete-branch`.
- GitHub: closes each feature issue via `Closes #N` on merged PR.

Comparison with repo examples:

| Workflow | Who implements | Who reviews | Who lands / closes | Difference from this run |
| --- | --- | --- | --- | --- |
| `examples/maker-WORKFLOW.md` | maker agent | separate reviewer/human later | maker explicitly must not merge; hands off with `Refs #N` + Human Review | This run has no separate reviewer; coding agent self-merges after CI |
| `examples/reviewer-WORKFLOW.md` | maker already did | reviewer agent only | reviewer comments verdict and flips labels; does not land code | This run is not review-only; same agent owns code and merge |
| `examples/reviewer-automerge-WORKFLOW.md` | maker agent | reviewer bot | reviewer approves, enables CI-gated forge auto-merge, confirms merged, then `aiops/done` | This run does direct `gh pr merge` from coding agent, not reviewer-owned auto-merge |
| Current GitHub E2E workflow | coding agent | none separate | coding agent runs `gh pr merge`; GitHub closes issue by `Closes #N` | Valid for this disposable private repo, but weaker maker/checker separation |

Takeaway: this is SPEC-boundary compliant because the worker did not create, approve, or merge PRs. It is operationally different from the maker/reviewer split and should be treated as a separate harness mode: faster and simpler for disposable GitHub E2E, weaker for production governance.

Detailed note: `/tmp/aiops-github-webtodo-e2e-20260626-093809/reports/notes/merge-mechanism-retro.md`

## Driver Mechanism Retrospective

This run used a **pre-started dependency-aware watcher/capture/control driver**:

- It captured `pre-activation`, then added `aiops:ready` to #1.
- It activated the next feature issue only after the previous feature issue closed.
- It activated #14 only after #1 closed, waited until worker state showed #14 running, then removed `aiops:ready` and added `aiops:canceled`.
- It captured dashboard/evidence pages at activation, every 2 feature completions, cancel before/after, and final success.
- It did not edit code, PRs, branches, PR bodies, or CI.

Comparison:

| Mechanism | Pros | Risks / Fit |
| --- | --- | --- |
| Human batch-ready backlog | Most realistic operator behavior; all intended issues are marked ready once | Requires dependencies to be expressed in issue bodies/tracker semantics or max concurrency/base freshness can create out-of-order work |
| Dependency-aware driver, this run | Deterministic sequencing, repeatable screenshots, clean control scenario timing | More harness-owned than a pure human backlog; must be clearly labeled as automation, not manual intervention |
| Control-only driver | Human/operator can mark backlog ready once; driver only triggers cancellation once running | Good middle ground for realistic backlog plus precise abnormal scenario |
| Capture-only observer | Lowest interference; screenshots and JSON only | Cannot reliably trigger cancel-running at the exact moment needed |

Recommendation for next replay: use batch-ready for #1-#12 if the workflow/repo has reliable dependency gating, plus a small control-only driver for #14 and a capture-only observer for screenshots. Keep this run as the deterministic baseline.

## Screenshots Index

Worker/dashboard and local evidence:

- Pre-activation dashboard: `/tmp/aiops-github-webtodo-e2e-20260626-093809/screenshots/dashboard-pre-activation.png`
- #14 running: `/tmp/aiops-github-webtodo-e2e-20260626-093809/screenshots/dashboard-cancel-control-running.png`
- #14 canceled: `/tmp/aiops-github-webtodo-e2e-20260626-093809/screenshots/dashboard-cancel-control-canceled.png`
- 12 features closed: `/tmp/aiops-github-webtodo-e2e-20260626-093809/screenshots/dashboard-12-features-closed.png`
- Final dashboard: `/tmp/aiops-github-webtodo-e2e-20260626-093809/screenshots/dashboard-final-success.png`
- Final issue/PR evidence page: `/tmp/aiops-github-webtodo-e2e-20260626-093809/screenshots/evidence-final-success.png`
- Cancel-control evidence: `/tmp/aiops-github-webtodo-e2e-20260626-093809/screenshots/evidence-cancel-control-canceled.png`
- Actions summary evidence: `/tmp/aiops-github-webtodo-e2e-20260626-093809/screenshots/actions-summary-final.png`

Final app screenshots:

- Desktop: `/tmp/aiops-github-webtodo-e2e-20260626-093809/final-verify/screenshots/final-app-desktop.png`
- Mobile 390px: `/tmp/aiops-github-webtodo-e2e-20260626-093809/final-verify/screenshots/final-app-mobile.png`
- After import/export: `/tmp/aiops-github-webtodo-e2e-20260626-093809/final-verify/screenshots/final-app-after-import-export.png`

Total PNG screenshots captured: 50.

## Final Fresh-Clone Verification

Fresh clone path: `/tmp/aiops-github-webtodo-e2e-20260626-093809/final-verify/app`

| Command | Result |
| --- | --- |
| `npm ci` | PASS; 123 packages installed, 0 vulnerabilities |
| `npm test` | PASS; 3 test files, 28 tests |
| `npm run build` | PASS; Vite build succeeded |
| `npm run test:e2e` | PASS; 12 Playwright tests |
| Screenshot capture | PASS; 3 app screenshots, browser console JSON has 0 messages |

Logs:

- `/tmp/aiops-github-webtodo-e2e-20260626-093809/final-verify/npm-ci.log`
- `/tmp/aiops-github-webtodo-e2e-20260626-093809/final-verify/npm-test.log`
- `/tmp/aiops-github-webtodo-e2e-20260626-093809/final-verify/npm-build.log`
- `/tmp/aiops-github-webtodo-e2e-20260626-093809/final-verify/npm-test-e2e.log`
- `/tmp/aiops-github-webtodo-e2e-20260626-093809/final-verify/screenshot-capture.log`

## Notes / Residual Risks

- The attestation command succeeded but emitted no detailed text; the exit status and status note were preserved.
- `codex login status` produced an empty artifact in this environment; `worker --doctor` independently verified ChatGPT/Codex auth and app-server probe success.
- Worker output truncates app-server stream after 1 MiB per run (`output_dropped` visible). This is acceptable for pass/fail but is an observability limitation for deep postmortem.
- The dashboard state showed `turn_count: 0` while token counters grew during runs. This did not block execution, but it is worth investigating as a state/API fidelity issue.
- `thread/start` 5s read timeouts occurred 9 times and recovered via retry. A future harness should decide whether to tune startup timeout or classify this as expected app-server cold-start behavior.
- `driver.log` contains duplicate lines because the driver wrote to the log file and stdout was also teed into the same file.
