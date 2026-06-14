# Journal - yvan (Part 1)

> AI development session journal
> Started: 2026-06-05

---

## 2026-06-06 — P2 batch: #665 / #668 / #675 / #676

Cleared the open **P2** queue, one issue per PR. All four shipped green; the
human merges. The shared per-PR review/merge/test discipline lives in
[`docs/runbooks/pr-review-merge-protocol.md`](../../../docs/runbooks/pr-review-merge-protocol.md)
and the batch-orchestration delta in
[`docs/runbooks/batch-issue-processing.md`](../../../docs/runbooks/batch-issue-processing.md);
this entry records what *this run* delivered and the lessons it earned.

### Shipped

| Issue | PR | What |
|---|---|---|
| #665 | #691 | Contain git mirror paths vs clone-URL traversal — reuse the shared `sanitizeComponent`, add a `pathContainedUnder` backstop. |
| #668 | #692 | Bound the `linear_graphql` / `gitea_issue_labels` dynamic-tool proxies: per-request `context.WithTimeout`, enforce the Gitea response-size cap (was read-but-discarded), redact the token from **every** body in a tool result. |
| #675 | #693 | Bound the §18.1 terminal-cleanup state-refresh retry with `RetryScheduler` failure backoff + a give-up cap (was a fixed-1s forever loop). |
| #676 | #694 | Direct unit coverage for `MaskCloneURL` — which exposed a real credential leak (raw return on `url.Parse` error) → **fail closed** via a `scheme://userinfo@` string fallback. DEVIATIONS.md D36. |

### Why the review discipline paid off (keep doing it)

Independent / adversarial review caught real defects that same-family
first-party review missed — the AGENTS.md cross-cutting item 4 lesson, live:

- **#668** — the adversarial-review workflow's secret-leak lens found a **HIGH**
  2xx-unparseable token leak and a **MEDIUM** success-path leak after my own
  review was clean. Root cause: I'd redacted only non-2xx bodies; the criterion
  is "no token in *any* tool result". Fix-the-class.
- **#675** — an independent **Codex** pass caught a **P2 data race** (off-actor
  cleanup retry read `o.scheduler` while `UpdateRetryScheduler` swaps it
  on-actor) after **three** clean Claude reviews. Fixed with `schedulerMu` +
  `currentScheduler()`, mirroring `candidateLister` / `retryTerminalResolver`.
- **#676** — writing the "test-only" coverage *itself* exposed the leak, and the
  workflow caught the merge-blocking PR-metadata gate before push.

### Earned this run (durable; also saved to assistant memory)

1. **PR-metadata gate, `config.go`:** ANY change to `internal/workflow/config.go`
   is SPEC-sensitive by a pure filename match (`validate-pr-metadata.mjs`) — even
   a non-key security fix. Resolve by adding/refreshing a **DEVIATIONS.md** row
   and checking the DEVIATIONS option (established practice: D34/#628, D25/#573).
   Relocating code out of `config.go` does not avoid the gate for that PR.
2. **"Test-only" issues can expose real bugs.** Fix them in the same PR
   (fix + test), don't ship coverage that documents a live bug. #676's leak +
   #469/#483 are the precedent.
3. **Mutation-test against the committed artifact; commit/stash before any
   `git checkout` restore** — it silently reverts uncommitted edits (hit twice
   in a prior run).
4. **Verify parser/edge outputs empirically** (a throwaway probe test) before
   hard-coding `want` values — `url.Parse` and `filepath` have surprising edges
   (scp form *errors*; `://`-only authority detection).
5. **Independent Codex when origin `@codex` quota is down:** fork to the
   `bytevane` account, `gh repo sync` its main, open a same-repo PR there,
   comment `@codex review`, bring findings back to the origin PR. (Switch
   accounts with `gh auth switch`; switch back after.)
6. **Merge autonomy under conditions** (operator preference, 2026-06-06): to cut
   unnecessary human burden, **auto-merge then continue** when a PR is *fully*
   green AND implemented strictly per the authoritative sources (SPEC.md / the
   Elixir reference / AGENTS.md) and official best practices AND every review
   (adversarial workflow + `code-reviewer` + independent Codex) converged clean
   with no remaining findings AND there is nothing debatable, uncertain, or
   needing a judgment call. **Pause and report** for the human only on a genuine
   open question: an adjudicated finding (e.g. #675's continuation-drop where I
   *rejected* the suggested fix), a scope/intent/safety fork, a
   `size-gated: justified overage` needing sign-off, an unresolved review thread,
   or anything you would want a second opinion on. When in doubt, pause — the
   bar for autonomous merge is "nothing here a reasonable reviewer would question."

### Continuation prompt for the next session

Paste this to resume. Queue order is my recommendation; adjust as priorities shift.

```
Continue the aiops-platform batch issue processing (prior session cleared the P2s:
#665/#668/#675/#676 merged). ultracode mode. One issue per PR; the human merges.
Follow docs/runbooks/pr-review-merge-protocol.md + batch-issue-processing.md, plus
the journal entry .trellis/workspace/yvan/journal-1.md (2026-06-06) and the saved
assistant memories (codex-review-every-push, bot-review-via-bytevane-fork,
mutation-testing-git-checkout-trap, pr-metadata-gate-config-go, no-git-add-all).

Remaining queue (suggested order):
- P3: #670 (sandbox root containment for "../"-relative paths, security — do first)
  -> #669 (escape Gitea owner/repo URL segments) -> #671 (validate Codex app-server
  numeric IDs before int) -> #672/#677 (Linear/Gitea polling N+1 blocker lookups)
  -> #678 (remove dead orchestrator tracker-write residue: Transitioner /
  MoveIssueToState / AddComment) -> #679 (drop pre-#229 'form 3' rework-key
  fallback in reconcile) -> #673 (risk-rank remaining #521 gocognit/funlen nolints)
- SPEC-alignment backports (no priority label, upstream Symphony parity):
  #682 (tracker.required_labels dispatch gate), #683 (issue_url on state-API rows),
  #684 (networkAccess opt-in doc)
- deferred (skip unless asked): #464, #547

Per-issue pipeline (don't skip steps):
1. gh issue view <N>; explore the affected code; read SPEC/AGENTS.md; mirror an
   existing pattern.
2. Branch off latest main.
3. Tests: probe real outputs before writing `want` for parser/edge cases.
4. Commit BEFORE mutation testing. Mutate each new behavior -> its test must FAIL
   -> git checkout restore -> confirm tree==HEAD.
5. Full local gate: gofmt -l; go vet ./...; go mod tidy clean; golangci-lint;
   file-size budget test; go test -race -covermode=atomic ./...; python unittests.
6. Adversarial-review Workflow (lenses chosen per issue) + per-finding verify.
   ADJUDICATE findings yourself — reject unsafe "fixes" (e.g. #675's
   reschedule-on-give-up), apply the safe ones.
7. pr-review-toolkit:code-reviewer on the post-fix diff.
8. Push; open draft PR with the full template (Closes #N, SPEC-alignment 1-of-3,
   size budget, testing checklist). If you touched internal/workflow/config.go,
   add a DEVIATIONS.md row and check the DEVIATIONS option.
9. Independent Codex on the bytevane fork (origin @codex quota down). Fold
   findings back; re-trigger until clean.
10. Document the Codex result on the origin PR. MERGE POLICY: if the PR is fully
    green AND implemented strictly per the authoritative sources (SPEC.md / the
    Elixir reference / AGENTS.md) and official best practices AND all reviews
    converged clean with no remaining findings AND nothing is debatable,
    uncertain, or needs a judgment call (an adjudicated finding, a scope/intent/
    safety fork, a "size-gated: justified overage" needing sign-off, an
    unresolved thread) -> `gh pr ready` then `gh pr merge <N> --squash
    --delete-branch` yourself and go to the next issue. Otherwise mark ready,
    report the open question, and pause for the human. When in doubt, pause.

Start with #670. Auto-merge the clean, uncontroversial PRs and keep going;
pause and report only when something is debatable, uncertain, or needs sign-off.
```

## 2026-06-07 — SPEC-alignment backports: #682 / #683 / #684

Finished the 2026-06-06 queue's backport tail (the P2 fixes #670–#673/#677–#679
had already merged). All three closed; `main` advanced to `92c5d67`.

### Shipped
- **#682 (PR #706)** — `tracker.required_labels` opt-in gate (SPEC §4.1.1/§6.4,
  upstream #88). First cut gated dispatch/retry/reconcile but had a production
  no-op (`reconciliationConfigForWorkflow` dropped the field) and, per the Codex
  bot, two P2 gaps: the §16.5 per-turn **continue** gate and **out-of-page**
  reconcile couldn't see label removal (labels were sourced only from the active
  listing). Operator chose "complete it (size-gated)." Final design (verified by a
  design Workflow's 2 adversarial passes + a Codex plan review, no fatal flaws):
  new `tracker.IssueState{State,Labels}` refresh contract carries labels across
  all 3 trackers; continue gate folds the label check into `Active` (no runner
  change); `appendLabelIneligibleActive` replaced by one `refreshedIssueIsInactive`
  helper that also covers out-of-page in-flight issues; Linear label cap 50→250.
  `size-gated: justified overage` (signed off).
- **#683 (PR #707)** — `issue_url` on running/retrying/blocked state-API rows
  (SPEC §13.7 SHOULD, upstream #89). Projection-only (URL already in
  `*Entry.Issue.URL`). Bonus: extracted the StateView projection structs into
  `internal/orchestrator/views.go`, burning state.go 1062→1002 (a #521/#661 split,
  baseline ratcheted down). Both reviews clean first pass.
- **#684 (PR #708)** — docs-only `networkAccess` note (upstream #65). Bot found 4
  real defects across 3 rounds in the YAML snippet (incomplete `workspaceWrite`
  fields → load error; wrong for the Docker `danger-full-access` profile;
  read-only→workspace-write escalation). Fixed snippets verified via
  `--print-config`.

### Earned this run (durable; also saved to assistant memory)
1. **Mutation-test the wiring seam, not just the leaf** — #682 retry-fire +
   production builder and #683's label-carry were unwired no-ops the leaf tests
   passed. Now AGENTS.md Clean-code rule 11.
2. **Validate doc config snippets through the real loader** before committing —
   the #684 3-round bot saga. A docs PR carrying a config snippet is not low-risk.
3. **Bot review is posted as a PR review + inline comments**, not issue comments —
   poll `/pulls/<n>/reviews` + `/comments` by head SHA (a wrong endpoint cost a
   20-min false timeout). Updated the codex-review-every-push memory.
4. **PR-metadata gate trips on a new orchestrator/worker file even for a pure
   refactor extraction** (views.go) — satisfy with the Elixir-reference option.

### Follow-up
- **#705 (open)** — Linear `labels(first:N)` cap could false-cancel an issue with
  a required label beyond the cap. Mitigated by the 250 raise; kept open for true
  label pagination.
- Deferred #464 / #547 untouched, as planned.


## Session 1: Dashboard Worker Status v2 layout (PR #731)

**Date**: 2026-06-10
**Task**: Dashboard Worker Status v2 layout (PR #731)
**Branch**: `main`

### Summary

Ported the Claude Design lean/Worker Status v2.html handoff to cmd/worker/dashboard: regrouped Running/Retrying/Blocked into the main column in KPI order, subgrid-aligned the Reconcile roll-up with serif counts, single-line Tokens header, retargeted the >=641px scrollbar guard, removed dead .grid-2 CSS. Vitest 17/17 with new mutation-verified layout assertions. Codex review unavailable ('This workspace is deactivated' — account-level; fork mirrors xrf-9527 and bytevane#16 both hit it, no repo-side bypass), merged #731 per adversarial-self-review + green-CI fallback with user approval. Learned: YYvanYang token lacks workflow scope; default SSH key = xrf-9527; xrf9268-hue now has Write on bytevane; git push --dry-run does not validate write permission.

### Main Changes

(Add details)

### Git Commits

| Hash | Message |
|------|---------|
| `5ec324b` | (see git log) |

### Testing

- [OK] (Add test results)

### Status

[OK] **Completed**

### Next Steps

- None - task complete


## Session 2: Automated release: release-please + App token, v0.1.0 shipped

**Date**: 2026-06-10
**Task**: Automated release: release-please + App token, v0.1.0 shipped
**Branch**: `main`

### Summary

为发版流水线补上前半段：release-please v5（manifest 模式，release-type go，App token 认证）在 push main 时维护 Release PR，合并后 cut tag + Release；release.yml 最后一步 create→upload；aiops-platform-release[bot] 加入 PR Metadata 豁免（mutation-verified）。GitHub App（ID 4017341）经 manifest 流程创建（用户仅点 Create/Install 两次），secrets 已设置。端到端验收通过：PR #735 合并 → Release PR #736 自动出现（CHANGELOG 正确、CI 正常跑）→ 合并后 v0.1.0 tag/Release 自动创建并触发 release.yml → 四平台 tar.gz + SBOM 挂上 Release。codex bot 审查不可用（workspace deactivated），以 trellis-check 对抗审查替代（verdict: merge-ready）。

### Main Changes

(Add details)

### Git Commits

| Hash | Message |
|------|---------|
| `87f1392` | (see git log) |
| `bf74cf4` | (see git log) |

### Testing

- [OK] (Add test results)

### Status

[OK] **Completed**

### Next Steps

- None - task complete


## Session 3: v0.1.0 packaged-binary full lifecycle test: 12-issue web-todo app, PASS

**Date**: 2026-06-11
**Task**: v0.1.0 packaged-binary full lifecycle test: 12-issue web-todo app, PASS
**Branch**: `main`

### Summary

Ran the v0.1.0 darwin_arm64 release worker against a local Gitea (docker, e2e-mirror setup) with real codex agents (0.137.0 pin) on a freshly designed 12-issue web-todo target repo. All 12 issues completed the full Symphony lifecycle (agent-side push/PR/label handoff, operator merge+done loop, 3 successful rework cycles, 1 stall-timeout retry, reconcile cancels verified). Final main independently verified: gofmt/vet/test green, live CRUD+validation correct. Filed #739 (Gitea Depends-on blocker gate keyed to literal Todo state, dead on Gitea) and #740 (freed slot dispatches stale candidates; handoff counter taxonomy). Full report + findings + reusable setup research archived in the task dir.

### Main Changes

(Add details)

### Git Commits

| Hash | Message |
|------|---------|
| `1a81e17` | (see git log) |

### Testing

- [OK] (Add test results)

### Status

[OK] **Completed**

### Next Steps

- None - task complete


## Session 4: Issue #765: mirror legacy-refspec heal + staging cleanup (PR #774 merged)

**Date**: 2026-06-12
**Task**: Issue #765: mirror legacy-refspec heal + staging cleanup (PR #774 merged)
**Branch**: `main`

### Summary

Handled issue #765 end-to-end via /handle-issue: re-assert remote.origin.fetch (--replace-all) on the existing-mirror refresh path to heal pre-#764 partial mirrors, and reap .git.staging on failed first clones via moved-gated deferred cleanup. Reworked the startRef-fallback test (upstream-branch deletion) after the re-assert made the unset-refspec construction a placebo. 3 regression tests, all mutation-verified; two pre-push dual-review rounds clean; GitHub codex P2 (multi-valued refspec wedge) validated and fixed; CI green; squash-merged as c45b323 with user approval.

### Main Changes

(Add details)

### Git Commits

| Hash | Message |
|------|---------|
| `c45b323` | (see git log) |

### Testing

- [OK] (Add test results)

### Status

[OK] **Completed**

### Next Steps

- None - task complete


## Session 5: Batch: release-readiness issues #780/#781/#782 → PRs #800/#801/#804 merged

**Date**: 2026-06-13
**Task**: Batch: release-readiness issues #780/#781/#782 → PRs #800/#801/#804 merged
**Branch**: `main`

### Summary

Processed the 2026-06-12 release-readiness audit batch serially per batch-issue-processing + pr-review-merge-protocol with authorized auto-merge. #780→PR#800: README/example tracker.api_key wiring fixed, day1-runbook deleted, examples wiring guard test added. #781→PR#801: doctor Gitea/GitHub preflight converged over 9 review rounds to driving the worker's own tracker clients (shared constructors gitea.BaseURLFromEnv / tracker.NewGitHubClientFromEnv); /user probe dropped with evidence (least-privilege false negatives); masked errors, JSON validation, per-listing deadline. #782→PR#804: scripted-agent e2e covering the positive issue→PR loop (ShellRunner seam, de-placebo'd GITEA_TOKEN guard mutation-verified via os.Environ() inheritance, credential-free push via http.extraHeader, draft_pr fixture consistency). Follow-up filed: #802 (pre-existing e2e lint debt). Spec guide updated: preflight-mirrors-consumer checklist in cross-layer-thinking-guide.

### Main Changes

(Add details)

### Git Commits

| Hash | Message |
|------|---------|
| `ce5157c` | (see git log) |

### Testing

- [OK] (Add test results)

### Status

[OK] **Completed**

### Next Steps

- None - task complete


## Session 6: Batch #783/#784/#785: release-readiness docs (key reference, staleness/links, GitHub onboarding)

**Date**: 2026-06-13
**Task**: Batch #783/#784/#785: release-readiness docs (key reference, staleness/links, GitHub onboarding)
**Branch**: `chore/batch-783-785-bookkeeping`

### Summary

Three docs PRs merged under authorized auto-merge; follow-ups #808 filed; two process lessons captured

### Main Changes

## 2026-06-13 release-readiness docs batch (#783/#784/#785)

Serial batch (all three shared README.md — soft overlap → serialized per the batch runbook). One issue → one branch → one PR; full shared gate per PR (local CI gate → dual local diff-only review → push → CI green → @codex review convergence → thread closure); authorized auto-merge applied.

- #783 → PR #809 (merged bcbe28b): new docs/runbooks/workflow-frontmatter-reference.md — exhaustive front-matter key reference; README §6.4 cheat-sheet links to it. Discovered tracker.statuses.* is parsed but consumed nowhere → filed #808 (area:tech-debt). Review rounds fixed 3 Codex MEDIUMs (Linear pagination is a fixed 200-page cap and ignores tracker.pagination_max_pages — README cheat-sheet row was also wrong and fixed in the same PR; by-state cap violations are load errors, not drops; explicit workspace.root beats AIOPS_WORKSPACE_ROOT).
- #784 → PR #810 (merged 343fee3): reworded the two stale "worker enforces policy" runbook claims to post-D33 reality; linked the orphaned ADR 0002 + Gitea bot runbook from README indexes.
- #785 → PR #811 (merged a0c1270): README "GitHub issue-state labels" section (open/closed/all vs label-as-state, aiops:ready convention, open-PR claim skip), read-only token-scope sentence, per-tracker example links in quick start. @codex review found 2 real P2s: the quick start still exported the Linear AIOPS_WORKFLOW_PATH; and the reconciliation bullet over-promised — githubResolveState falls back to plain `open` on label removal, so only closing the issue cancels an in-flight run under the recommended config. Both fixed in ad64c0f.

Process lessons (saved to memory): poll all three codex completion signals (PR reviews + bot issue comments + trigger reactions) — narrowing to the last observed form misread "converged with findings" as "quota exhausted" on #811; use the gh-pr-follow-through skill for PR closure instead of hand-rolled wait loops.


### Git Commits

| Hash | Message |
|------|---------|
| `bcbe28b` | (see git log) |
| `343fee3` | (see git log) |
| `a0c1270` | (see git log) |

### Testing

- [OK] (Add test results)

### Status

[OK] **Completed**

### Next Steps

- None - task complete


## Session 7: dashboard: worker-status version chip + favicon (Symphony #90)

**Date**: 2026-06-14
**Task**: dashboard: worker-status version chip + favicon (Symphony #90)
**Branch**: `main`

### Summary

Pulled the Worker Status v2 design draft (hosted /v1/design/h share link → gzip tarball), ported two additions into cmd/worker's embedded React/Vite dashboard: a topbar version chip from /api/v1/state.version, and a v3 brand-mark favicon (committed 128x128 PNG, embedded, served at /favicon.png with a sha256[:12] content-digest cache-bust templated into index.html+fallback.html, mirroring upstream Symphony #90). Mutation-verified Go + vitest. PR #834 merged (squash), issue #833 closed; full local CI gate + remote checks green; codex review (via bytevane) found no issues.

### Main Changes

(Add details)

### Git Commits

| Hash | Message |
|------|---------|
| `661cf47` | (see git log) |

### Testing

- [OK] (Add test results)

### Status

[OK] **Completed**

### Next Steps

- None - task complete


## Session 8: release-please: align repo on Conventional Commits (PR #838)

**Date**: 2026-06-14
**Task**: release-please: align repo on Conventional Commits (PR #838)
**Branch**: `main`

### Summary

Fixed release-please dropping real work from CHANGELOG/version bumps (it only acts on Conventional Commit types; repo squash-merged freeform area: titles). Removed the changelog-sections override (hide chore/refactor), added a SHA-pinned PR-title lint (amannn/action-semantic-pull-request on pull_request) as a required check, set squash_merge_commit_title=PR_TITLE, switched dependabot prefixes to chore(deps). Source-verified via cloned release-please. @codex review caught + I fixed: dependabot deps:->chore(deps) (P2), pull_request_target->pull_request head-SHA bug (P1), and a deps-non-releasable policy point (reply+resolved with source evidence). Pre-push dual review (Claude+Codex) ran 2 rounds. Imported the live ruleset post-merge. Follow-ups: #839 drift-check, #841 changelog cleanup, #842 deps release policy. Lesson: never push on a single reviewer; codex:codex-rescue stalls on network SHA verification.

### Main Changes

(Add details)

### Git Commits

| Hash | Message |
|------|---------|
| `03b51d3` | (see git log) |

### Testing

- [OK] (Add test results)

### Status

[OK] **Completed**

### Next Steps

- None - task complete
