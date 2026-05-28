# PR review & merge protocol

The single source of truth for how any pushed head in this repo is tested,
reviewed, and merged. `handle-issue`, `handle-pr`, and
[`batch-issue-processing.md`](batch-issue-processing.md) all reference this
file instead of restating it — update the protocol here, in one place
(clean-code rule 3, one source of truth per concept; see
[`AGENTS.md`](../../AGENTS.md)).

Every rule below is earned by a specific observed failure; the consuming
docs name the runs (e.g. the 2026-05 eight- and ten-issue batches). Do not
generalize beyond the evidence.

## 1. Test discipline (regression + mutation verification)

The canonical rule lives in [`AGENTS.md`](../../AGENTS.md) clean-code rule 6
(regression test + mutation against the committed artifact); this section is the
operational procedure.

1. **Every fix ships a regression test.** A test that would still pass with the
   production fix reverted is a placebo and does not count as done.
2. **Mutation-verify against the committed artifact, not the working tree.** A
   green local test only proves the working tree passes; it does **not** verify
   the artifact you push unless the tree equals the pushed HEAD.
3. **Correct mutation procedure — commit first, then mutate:**
   1. Commit (or amend) the fix so it is in HEAD.
   2. Break the key production line (edit/`sed`/`perl`).
   3. Run the matching test — it **must FAIL**.
   4. `git checkout -- <file>` to restore (idempotent now that the fix is
      committed — checkout restores the fix instead of losing it).
   5. Confirm `git status` is clean and `git diff HEAD` is empty before moving
      on, so "tested" equals "shipped".
4. **Fire-and-forget negative assertions need a deterministic barrier** (e.g. a
   sibling still in the terminal state that makes "did not happen" observable);
   never sell a probabilistic barrier as race-free.
5. **Run `go test -race` for concurrency changes.**

## 2. Review the committed range before every push

1. **Refresh `origin/main`, then commit/amend first**, staging only explicit
   paths (never `git add -A` / `git add .`). The review artifact is a stable
   head SHA, never a half-finished working tree or a stale base.
2. The diff under review is always `git diff origin/main...HEAD`.

## 3. Pre-push dual diff-only reviewers

Dispatch **two independent reviewers** before every push; both see only the
committed diff and are **not told your conclusion**. Require severity-tagged
findings (keep each review ≤700 words) and a final verdict —
`MERGE-READY` / `NEEDS-CHANGES` / `BLOCKED`. Give each a complete brief: head
SHA, changed files, intent (issue/PR), relevant SPEC/upstream pointers, and
AGENTS.md policy.

- **Codex reviewer.** When the main agent is Claude Code, **prefer** the
  `codex:codex-rescue` subagent from
  [codex-plugin-cc](https://github.com/openai/codex-plugin-cc) (`Agent` tool,
  `subagent_type: "codex:codex-rescue"`); pass the full brief in the prompt. It
  wraps `codex exec review`'s CLI flag mutex (`--base` / `--commit` / `PROMPT`
  are pairwise exclusive — see `codex exec review --help`) and returns
  structured output via `codex:codex-result-handling`. **Only when** the main
  agent is Codex itself or the plugin is unavailable, fall back to
  `codex exec review --base origin/main --title "…"` — the `review` subcommand
  takes no custom PROMPT and runs Codex's generic review heuristic, so
  repo-specific context (issue intent, SPEC refs, acceptance criteria) is lost.
  If the fallback still needs a repo-specific prompt, use plain `codex exec`
  (free PROMPT, diff as a `<stdin>` block) at the cost of the `review`
  subcommand's built-in review structure.
- **Claude Code reviewer.** Prefer the `Agent` tool with
  `subagent_type: "feature-dev:code-reviewer"`. In a restricted environment use
  a hardened `claude -p` that receives the diff on stdin and cannot read the
  mutable working tree or inherit the session:
  `--permission-mode bypassPermissions --no-session-persistence --tools "" --max-turns 2`.

Triage:

- **HIGH / MEDIUM / Critical block the push.** Fix, amend, rerun the local
  gate and both reviewers. Keep an unfixed item only if the human explicitly
  signs off on the risk before push.
- **LOW / P3:** fix when small, localized, and regression-like; otherwise record
  the decision in review notes and copy it into the PR body once the PR exists.
- **A failed reviewer run is not approval.** Retry with a smaller diff-only
  prompt. If one reviewer family stays unavailable, get an equivalent successful
  diff-only review from the same family or explicit human sign-off before push,
  and record it (review notes → PR body). Never push on a single reviewer.
- **Review depth matches blast radius.** Two reviewers are the minimum for a
  pushed head; destructive/concurrent paths need repeated adversarial rounds
  until findings stop, while pure incremental or documentation changes need one
  clean dual-review round.

## 4. `@codex review` is a per-push, post-push gate

Run it **on every pushed head**, not once per PR.

1. `gh pr comment <pr> --body "@codex review"`; record that trigger
   comment's id.
2. **Poll** its reactions summary —
   `gh api repos/<owner>/<repo>/issues/comments/<id> --jq '.reactions.eyes'`
   (the issue-comment object carries the `.reactions` counts; no separate
   `/reactions` endpoint needed). Codex review is not a check run and reactions
   have no watch/subscribe API (REST is GET/POST/DELETE only), so polling is the
   only option. **`eyes==0` alone is not done** (it may not have started): wait
   for 👀 to **appear then disappear**, **and** for a positive completion
   signal — Codex posted a review/comment for that head, or 👍'd the trigger.
3. **CI green** uses native `gh pr checks <pr> --watch --fail-fast` (blocks to
   completion) — never `sleep`-poll.
4. Local review **does not replace** this gate: the GitHub Codex bot and the
   stop-time Codex gate catch defect classes local reviewers miss. If Codex is
   externally unavailable (e.g. account usage limit), compensate with an
   equivalent independent dual review and disclose it in the PR body.

## 5. Review threads — judge with GraphQL

Flat review comments and `latestReviews` are not enough (they lag the fresh
trigger). Query `reviewThreads` via GraphQL. Resolve a thread only when one
holds:

- the code actually fixed the thread's issue, or
- the thread is already outdated **and** a fresh Codex trigger is clean, or
- the human asked you to handle the PR, the fresh trigger completed, and a
  thread-aware recheck confirms it is no longer actionable.

**Merge requires zero unresolved, non-outdated actionable threads.** After
merging a PR with non-trivial bot activity, sanity-check the next
`Capture unresolved reviews` workflow run / linked follow-up issues — that
workflow is a backstop, not a merge substitute.

## 6. Size gate is a merge gate, not an LOC-reduction mandate

The size-gate rule — the three states (`within budget` /
`size-gated: justified overage` / `size-gated: split recommended`) and the
principle that correctness/coverage/safety take precedence over LOC compliance
— is canonical in [`AGENTS.md`](../../AGENTS.md) (the `policy.max_changed_files`
/ `policy.max_changed_loc` bullet). Classify every PR into exactly one state and
disclose it in the body; never delete meaningful tests or weaken coverage to
fit the budget.

Merge-flow application of those states:

- `within budget` — standard auto-merge path.
- `size-gated: justified overage` — **not auto-mergeable**; provide a human
  sign-off bundle (why the scope is necessary, head SHA, local verification, CI
  state, bot-review state, unresolved-thread state, residual risk).
- `size-gated: split recommended` — **hard stop, split into smaller PRs**; do
  not seek sign-off in lieu of splitting.

Read the effective budget and `policy.deny_paths` from the repo's `WORKFLOW.md`;
do not trust a hardcoded list (defaults are 12 files / 300 LOC and deny
`infra/**`, `deploy/**`, `db/migrations/**`, `secrets/**`, but workflows differ
— e.g. the `github-local` example uses 20 / 600).

## 7. PR body is a living ledger

After every material push, refresh the body: head SHA, acceptance criteria,
verification commands, mutation check, CI conclusion/run id, dual-reviewer
verdicts, `@codex review` trigger id + reaction/thread state, size-gate
classification (one of the three states; rationale if over budget), and
deferral/follow-up links. A stale body misleads the merge decision.

Include a size-gate checklist (exactly one box checked):

```markdown
### Size gate
- [ ] `within budget` — diff fits effective `policy.max_changed_files` /
      `max_changed_loc`
- [ ] `size-gated: justified overage` — rationale: <why correctness/coverage/
      safety/perf justifies the extra LOC; not auto-mergeable; needs human
      size-gate sign-off>
- [ ] `size-gated: split recommended` — rationale: <which concerns to split
      into separate PRs; hard stop, do not seek sign-off in lieu of splitting>
```

## 8. Merge

- **Merge only after explicit human permission.** Squash into the agreed base
  (repo convention) with a commit message describing the **final state**, not a
  round-by-round log; delete the branch.
- Force-push uniformly with `--force-with-lease=<branch>:<known-sha>`.
- **Authorized auto-merge is opt-in and scope-bounded** — "you may merge the
  PRs for issues #N–#M once they pass the gate", never a standing grant.
  Approving one merge does not authorize the next. When authorized, merge only
  when **all** hold:
  1. CI green on the head commit (not a stale run).
  2. `@codex review` clean for the current head (converged trigger, GraphQL
     thread state clean) — no unresolved HIGH/MEDIUM and zero unresolved,
     non-outdated threads.
  3. Every acceptance criterion met, or each gap deferred to a tracked, linked
     issue.
  4. Classified `within budget` and changes no `policy.deny_paths`.
  5. Branch protection's required reviews satisfied.
  6. The agreed merge method (default squash) into the agreed base.

  **Native auto-merge enforces only required status checks (CI) and branch
  protection — not gates 2–4.** Confirm gates 2–4 yourself immediately before
  enabling auto-merge; any new push re-opens them (re-confirm before
  re-enabling).

  **Hard stops — always require human sign-off even under an auto-merge grant:**
  force-pushing/merging into `main` out of band or any history rewrite; editing
  `go.mod`'s `go` directive or touching `policy.deny_paths`; a
  `size-gated: justified overage` PR (flag, don't merge without size-gate
  sign-off); a `size-gated: split recommended` PR (split, don't seek sign-off);
  anything the human's instructions put off-limits. When at the edge of the
  grant, use `AskUserQuestion`. Stop the moment the human revokes the grant.
