# Issue #661: burn down oversized Go file baselines

## Goal

Track issue #661, the file-level baseline burn-down debt that follows the #657
file-size gate.

## Outcome — COMPLETE (2026-06-10, issue closed)

All five baseline files paid down via per-file sub-issue + behavior-preserving
decomposition PRs; `scripts/file_size_budget_test.go`'s baseline map is now
**empty** (kept with a why-comment as the reviewed home for future exceptions).

| File | Was → Now | Sub-issue / PR |
|------|-----------|----------------|
| `internal/workflow/loader.go` | 852 → 123 | PR #686 (pre-session) |
| `internal/runner/codex_app_server.go` | 877 → 672 + transport.go | #722 / PR #723 |
| `internal/orchestrator/state.go` | 1002 → 716 + snapshot/operator-stop | #724 / PR #725 |
| `internal/doctor/doctor.go` | 1294 → 553 + codex/tracker | #726 / PR #727 |
| `cmd/tui/main.go` | 904 → 252 + render/state_client | #728 / PR #729 |

## Process patterns that worked (reuse for future burn-downs)

- Per-file **sub-issue** so each PR's `Closes #N` satisfies the metadata gate
  without closing the umbrella (#686's `Closes #661` had closed it prematurely).
- Pure-move proof: sorted-multiset + ordered-subsequence comparison vs
  origin/main; final PR also did per-function byte-identity (37/37).
- Mutation-verify the budget gate each time (pad file >800 → FAIL → restore).
- Serial PR processing — every PR edits the same baseline map (conflict-prone).
- New `internal/orchestrator/` files trip the PR-metadata SPEC gate → check
  option 2 with a concrete `elixir/lib/symphony_elixir/orchestrator.ex:36`
  citation (precedent: PR #707 views.go).
- Codex family unavailable all session (quota; bot "workspace is deactivated")
  → dual review = Claude-family + refute-framed adversarial substitute,
  disclosed per PR. Dated audit snapshots (2026-06-07 nolint ranking) are NOT
  rewritten when attributions drift — decided in #727, reaffirmed in #729.

Function-level nolint debt stays scoped to #673.
