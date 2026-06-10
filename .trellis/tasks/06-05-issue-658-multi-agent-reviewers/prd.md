# Issue #658: require multi-agent reviewer usage

## Goal

Close GitHub issue #658 by making the pre-push reviewer workflow explicit about using available multi-agent/subagent tooling before falling back to CLI-only review paths.

## Live Issue Facts

- Issue: #658, `docs: require multi-agent reviewer usage in issue and PR workflows`
- Labels: `area:docs`, `area:workflow`, `type:docs`, `priority:p2`
- State: open
- Existing open PR: none
- Main head: `4db4d3c8faa34018b03d1767de501a60f8b9dc8e`
- Upstream Symphony mirror: `/tmp/symphony-upstream` at `54b456b`
- Root `WORKFLOW.md`: absent; `examples/github-local-WORKFLOW.md` was read as the current dogfood workflow example.

## Plan

1. Update `docs/runbooks/pr-review-merge-protocol.md` so pre-push reviewer routing has an explicit environment matrix:
   - Claude Code Agent path.
   - Codex inline path using `tool_search` to discover/load `multi_agent` or `spawn_agent`.
   - codex-sub-agent path, which must not recursively spawn reviewers unless explicitly assigned.
   - CLI fallback path when subagent tooling is unavailable or fails.
2. Keep the Codex-family and Claude-family reviewer requirements intact. Document that an independent subagent reviewer is a sidecar quality gate, and only counts as family coverage when the invoked reviewer is explicitly from that family.
3. Update `.claude/skills/handle-issue/SKILL.md` and `.claude/skills/handle-pr/SKILL.md` to point agents at the protocol's subagent-first reviewer routing before push.
4. Update dogfood/Trellis workflow guidance so Codex inline sessions discover `spawn_agent` with `tool_search` when the environment hints at subagent capability.
5. Add a regression-style scripts test that asserts the process docs contain the new discovery/routing language and do not silently regress to CLI-only reviewer wording.

## Grill-With-Docs Challenge

- `CONTEXT.md` terms: this is workflow-defined mode / small PR auto-merge / hard dependency handling, not a new domain term. No glossary update needed.
- SPEC boundary: SPEC §1 and §3.2 place ticket/PR business logic in the workflow prompt and agent tooling; this issue changes docs/skills only and must not add worker phases, gates, config, or orchestrator behavior.
- Runbook consistency: `batch-issue-processing.md` delegates review and merge gates to `pr-review-merge-protocol.md`, so the protocol remains the single source of truth. The skills should reference it rather than duplicate a long reviewer algorithm.
- Test surface: `scripts/*_test.go` already checks process docs and workflow examples with string/regex assertions. A focused docs test is appropriate.
- Risk: over-defining subagents could make agents think family coverage is optional. The text must explicitly preserve Codex + Claude family coverage.

## Acceptance Criteria

- [x] Pre-push docs say available multi-agent/subagent tooling should be used before CLI fallback for sidecar code-review/quality-check work.
- [x] Codex inline sessions have explicit `tool_search` discovery guidance for `multi_agent` / `spawn_agent`.
- [x] Docs distinguish Codex inline, codex-sub-agent, Claude Code Agent, and CLI fallback paths.
- [x] Reviewer protocol preserves Codex-family plus Claude-family coverage, with subagent review described as complementary unless the subagent is explicitly one of those families.
- [x] Regression-style doc/process test added or updated.
- [x] PR remains within budget and has no SPEC-sensitive worker/orchestrator changes.

## Outcome

- Branch/worktree: `fix/658-multi-agent-reviewers` / `/Users/yvan/.codex/worktrees/658/aiops-platform`
- PR: #662, `https://github.com/xrf9268-hue/aiops-platform/pull/662`
- Head: `b8e181de0e75ac3df31d6e96752705414e45b7af`
- State: merged. PR #662 was manually merged at `2026-06-05T12:29:26Z`, merge commit `eff46e0faad5a6b6d3c8f49af84814c94b9b8e74`. GitHub Codex review failed twice with connector `An unknown error occurred` after triggers `4631453015` and `4631497084` before the manual merge.
- Clean gates before deferral: full local gate, mutation checks, subagent reviewer `Plato`, Codex-family local JSON reviewer, Claude-family local JSON reviewer, CI run `27014282779`, metadata run `27014788948`, GraphQL reviewThreads `[]`, warning audit with no `warning:` lines, and size gate `within budget`.

## Verification Plan

- `go test ./scripts`
- Full local gate before push:
  - `gofmt -l $(git ls-files '*.go')`
  - `go mod tidy && git diff --exit-code -- go.mod go.sum`
  - `go vet ./...`
  - `go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2 run --config=.golangci.yml --issues-exit-code=0`
  - `go test -run '^TestProductionGoFilesStayWithinSizeBudget$' -count=1 ./scripts`
  - `go test -race -covermode=atomic ./...`
  - `go build ./cmd/worker ./cmd/tui`

## Out of Scope

- Changing worker/orchestrator behavior.
- Changing GitHub Codex remote review semantics.
- Implementing #655, #656, #659, #660, or #661 in this PR.
