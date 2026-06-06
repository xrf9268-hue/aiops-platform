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
