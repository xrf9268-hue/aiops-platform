# Batch: GitHub issues 658 655 656 657 659 660 661

## Goal

Process the fixed GitHub item order `#658 -> #655 -> #656 -> #657 -> #659 -> #660 -> #661` until each item has an explicit terminal state: `merge-ready`, `merged`, `skipped`, or `deferred`.

## Startup Evidence

- `gh auth status`: authenticated to github.com as active account `xrf9268-hue`.
- `git checkout main && git pull --ff-only`: fast-forwarded `main` to `4db4d3c8faa34018b03d1767de501a60f8b9dc8e`.
- Root `WORKFLOW.md`: absent on current `main`.
- Workflow fallback read: `examples/github-local-WORKFLOW.md`.
- Multi-agent/subagent discovery: `tool_search` found `multi_agent_v1.spawn_agent` and `multi_agent_v1.wait_agent`.
- No open pull requests currently exist in `xrf9268-hue/aiops-platform`.

## Batch Rules

- Fixed issue order is mandatory.
- One issue maps to one branch/worktree and one draft PR.
- Default concurrency is serial unless issue surfaces are proven independent; `#655` and `#656` are serialized because both touch `.trellis/scripts/common`.
- Each implementation issue needs a short plan before code and a `grill-with-docs` challenge against `CONTEXT.md`, runbooks, and relevant code/docs.
- Pre-push review must include an available independent subagent reviewer plus Codex-family and Claude-family review coverage.
- Every push reopens the PR follow-through loop: local gates, GitHub CI, fresh `@codex review`, GraphQL reviewThreads, warning audit, PR body ledger, and size gate.
- Authorized auto-merge applies only when every documented gate is satisfied and the PR is `within budget`.

## Live Item Ledger

| Item | Title | Dependency | Branch/Worktree | PR | Head | State | Notes |
| --- | --- | --- | --- | --- | --- | --- | --- |
| #658 | docs: require multi-agent reviewer usage in issue and PR workflows | none found | `fix/658-multi-agent-reviewers` / `/Users/yvan/.codex/worktrees/658/aiops-platform` | #662 | `b8e181de0e75ac3df31d6e96752705414e45b7af` | merged | PR #662 was manually merged at `2026-06-05T12:29:26Z`, merge commit `eff46e0faad5a6b6d3c8f49af84814c94b9b8e74`. Before merge, local gate, mutation checks, subagent/Codex/Claude local reviewers, CI run `27014282779`, metadata run `27014788948`, GraphQL reviewThreads, warning audit, and size gate were clean; GitHub Codex connector had failed twice. |
| #655 | Follow up unresolved review thread from PR #650 | soft overlap with #656 | `fix/655-task-create-duplicates` / `/Users/yvan/.codex/worktrees/655/aiops-platform` | #663 | `dd8b066047ed70ef71ce3240ac802f64600dc3f8` | merged | PR #663 merged at `2026-06-05T13:08:13Z`, merge commit `e7df3213e3013276e9062de28615323b915b5993`. Local gate, mutation check, subagent/Codex/Claude local reviewers, CI run `27015678121`, metadata run `27016167602`, GraphQL reviewThreads, warning audit, and size gate were clean before merge. |
| #656 | Follow up unresolved review thread from PR #650 | soft overlap with #655 | `fix/656-lifecycle-hook-timeouts` / `/Users/yvan/.codex/worktrees/656/aiops-platform` | #664 | `c4c47597e4d564017adc1c0efec1eff3851b3297` | merged | PR #664 merged at `2026-06-05T14:07:29Z`, merge commit `2f5e81ed9572b377cecaa41e0596c80567d1a7b9`. Before merge, local gate, mutation checks, subagent/Codex/Claude local reviewers, CI run `27019226108`, metadata run `27019270068`, GraphQL reviewThreads, warning audit, and size gate were clean; GitHub Codex connector had failed twice. |
| #657 | ci: enforce production Go file size budget | already terminal | `fix/647-large-file-size-signal` | #657 | `72bc1080b415875b76e40cb956beb6500a2831e7` | skipped | Live PR #657 is merged at `2026-06-05T10:36:14Z` with merge commit `4db4d3c8faa34018b03d1767de501a60f8b9dc8e`. |
| #659 | docs: align go vet checklist with CI workflow | after #657 | `fix/659-go-vet-ci-checklist` / `/Users/yvan/.codex/worktrees/659/aiops-platform` | #674 | `4dc3a33313cb23dbe372ecdea4a03e427a3f64c5` | merged | PR #674 merged at `2026-06-05T14:33:57Z`, merge commit `123025673c34d9422c96c364e99986e36674abf1`. CI, metadata, GraphQL reviewThreads, warning audit, and size gate were clean before merge. |
| #660 | docs: audit file-size gate against official Go guidance | after #657 | `docs/660-file-size-go-guidance` / `/Users/yvan/.codex/worktrees/660/aiops-platform` | #681 | `4898445de23ce27bac1e6ce90093aa2951be555c` | deferred | Draft PR #681 opened. Local gate, mutation check, subagent/Codex/Claude local reviewers, CI run `27042765243`, metadata run `27042765272`, GraphQL reviewThreads, warning audit, and size gate are clean. Deferred because GitHub Codex review did not return clean: trigger `4635973663` and trigger `4635997269` both failed with connector `An unknown error occurred`. PR body records the #661 policy verdict. |
| #661 | refactor: burn down oversized Go file baselines | hard dependency on #660 policy outcome | none | none | n/a | deferred | Not started. #660 produced a policy verdict in PR #681, but that PR remains deferred/open because GitHub Codex review failed twice. Defer #661 until PR #681 is accepted/merged or the operator manually waives the dependency gate. |

## Acceptance Criteria

- [x] Every listed item has a terminal state with evidence.
- [x] Skipped items cite live state evidence.
- [x] Each implemented issue has a child Trellis task, branch/worktree, draft PR, head SHA, and PR body ledger.
- [x] Each implemented issue passes the local gate or records a specific blocker/deferral.
- [x] Every pushed PR has CI, fresh `@codex review`, GraphQL reviewThreads, warning audit, size gate, and PR metadata state recorded.
- [x] Any unmet acceptance criterion is linked to a follow-up issue at the moment of deferral, or the deferral is only an external merge gate with acceptance criteria satisfied.

## Out of Scope

- Issues outside `#658`, `#655`, `#656`, `#657`, `#659`, `#660`, and `#661`.
- Reopening or reimplementing merged PR #657.
- Starting #661 before #660 has reached a policy verdict.
