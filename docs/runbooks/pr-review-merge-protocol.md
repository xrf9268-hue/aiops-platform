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

For issue work that required a negative-constraint preflight, the **first**
pre-push reviewer brief must also include that guardrail: required behavior,
negative constraints, opaque boundaries, and the decision to omit arbitrary text
opaquely unless the issue explicitly asks for structured parsing with fixtures.
Ask reviewers to challenge the implementation direction and boundary before
spending their review budget on parser or redaction edge cases.

Subagent review is the **default** in main interactive sessions when the tool
contract exposes a default-on reviewer path. Interactive Claude Code uses the
Agent tool and a Codex-family subagent (`codex:codex-rescue`); Codex inline uses
that same policy only when its `spawn_agent` tool contract exposes a default-on
reviewer path for the session. Run the subagent dual review **without asking** —
the operator's standing preference is the authorization. The per-request
authorization ask was retired in #900 as pure ceremony for default-on reviewer
paths (the operator invariably opted in, so the question only added a
round-trip and contradicted the autonomy-first default). The #891 lesson still
holds in the *other* direction: never *skip* an available subagent reviewer.
Within this default, pick one sidecar reviewer (normal-risk) vs parallel
specialized reviewers (high-risk diff) by **blast radius**, per "review depth
matches blast radius" below — that escalation is automatic, not a second
authorization to ask for. Three paths narrow this default:

- **Opt out in-turn.** A current-turn phrase such as "CLI review only" or
  "subagent review disabled" switches that request to the bounded CLI fallback.
- **Runtime contract block.** If Codex inline exposes `spawn_agent` only behind
  an explicit current-request subagent/delegation precondition, record the
  contract-blocked fallback and use the bounded CLI fallback. This is a runtime
  tool-contract limit, not a standing-preference question to ask again.
- **Sub-agent self-exemption.** When the current session is already a spawned
  sub-agent, obey the parent assignment and do not recursively spawn reviewers
  unless the parent task explicitly asks for that.

Best-practice reviewer modes:

| Mode | Use when | Required handling |
| --- | --- | --- |
| Subagents unavailable / opted out / contract-blocked | The Agent tool, requested reviewer subagent, or Codex inline `spawn_agent` path is unavailable; the Codex inline tool contract does not expose a default-on reviewer path for this request; the current role forbids spawning (for example, already inside `codex-sub-agent`); the subagent call fails; or the operator opted out in-turn | Discover availability when relevant, record why subagents were not used, and use the bounded CLI fallback. **Do not** ask a standing-preference authorization question in main interactive sessions with a default-on reviewer path — subagent review is the default there (#900) |
| One sidecar subagent (default, normal-risk) | Interactive Claude Code or Codex inline with a default-on reviewer subagent path available, and the diff is low-risk/docs-only or normal-risk | Run one independent read-only diff reviewer sidecar focused on correctness, safety/security, and test gaps, plus the existing Codex-family and Claude-family gates. No authorization phrase required |
| Parallel specialized subagents | The diff touches high-risk security, sandbox, filesystem deletion, concurrency, tracker mutation, workflow, or orchestrator behavior | Split read-only reviewers by concern, such as security/safety, tests/mutation gaps, races/lifecycle, and maintainability/scope, then merge their findings into the same dual-family gate |

Reviewer routing is environment-dependent:

- **Claude Code Agent.** Subagent dual review runs by default here (no
  authorization phrase needed). Prefer the Agent tool subagents named below for
  family-specific review. Use the hardened CLI fallback only when the Agent
  tool or requested subagent is unavailable or fails, or the operator opted out
  in-turn.
- **Codex inline.** When the environment hints that subagents or multi-agent
  tools are available, call `tool_search` for `multi_agent` / `spawn_agent`
  before CLI fallback. If `spawn_agent` is available and its tool metadata
  exposes a default-on reviewer path, use it for an independent final-diff
  reviewer or quality-check sidecar by default unless the operator opts out
  in-turn. Record the agent id/verdict in review notes. If the tool metadata
  requires an explicit current-request subagent/delegation ask and the request
  does not include one, do not spawn; record the contract-blocked fallback and
  use the bounded CLI fallback. Do not wait for a human reminder to discover the
  tool.
- **codex-sub-agent.** If the current session is already a spawned sub-agent,
  obey the parent assignment and the sub-agent notice first. Do not recursively
  spawn reviewers unless the parent task explicitly asks for that; return the
  requested review or implementation result to the parent.
- **CLI fallback.** Use CLI reviewers only after the relevant subagent path is
  unavailable, fails, or is inappropriate for the current role. Record the
  discovery/fallback evidence in review notes and later in the PR body.

Subagent review complements the two-family gate. It does not replace the
required Codex-family and Claude-family coverage unless the invoked subagent is
explicitly the Codex-family or Claude-family reviewer for that environment.

- **Codex reviewer.** When the main agent is Claude Code, **prefer** the
  `codex:codex-rescue` subagent from
  [codex-plugin-cc](https://github.com/openai/codex-plugin-cc) (`Agent` tool,
  `subagent_type: "codex:codex-rescue"`); pass the full brief in the prompt. It
  wraps the Codex CLI review contract and returns structured output via
  `codex:codex-result-handling`. Invoke it **foreground/synchronous** for this
  gate — an `Agent` call without `run_in_background`, and no `--background` — so
  the review returns inline: the rescue runtime's `task` blocks and returns by
  default, whereas `--background` detaches a `task-worker` whose result, once the
  spawning Claude session ends, is no longer surfaced by default
  `/codex:status`/`/codex:result` discovery (session-scoped) and is recoverable
  only via the retained job id (#743). Reserve `--background` for long
  fire-and-forget work fetched with `/codex:result` in the same session.
  **Only when** no authorized and appropriate
  Codex-family subagent path is available, fall back to plain `codex exec
  --output-schema` with the repo-specific review prompt and diff on stdin.
  Do not use `codex exec review --base` for this gate: the `review` subcommand
  cannot accept the repo-specific prompt/schema that the machine-validated local
  review contract requires.
- **Claude Code reviewer.** Prefer the `Agent` tool with
  `subagent_type: "feature-dev:code-reviewer"`. In a restricted environment use
  a hardened `claude -p` that receives the diff on stdin and cannot read the
  mutable working tree or inherit the session:
  `--permission-mode bypassPermissions --no-session-persistence --tools "" --output-format json --json-schema <schema> --max-turns 6`.
  Read `.structured_output` from Claude's JSON wrapper as the review JSON.

Triage:

- **First validate the finding technically.** Compare it against the current
  head, the issue plan, SPEC/Elixir references, and adjacent code paths before
  changing code. A wrong review comment should be answered with evidence; a
  right comment that exposes a plan flaw should update code, tests, and any
  SPEC/deviation docs in the same review round.
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

1. Post the trigger with a workspace-bound identity, resolving the token
   first so a missing token fails loud instead of silently falling back to the
   default account:

   ```bash
   trigger_token="$(gh auth token --user bytevane)" \
     && GH_TOKEN="$trigger_token" gh pr comment <pr> --body "@codex review"
   ```

   The `&&` chain is what makes it fail-closed in an interactive shell: `gh`
   treats an empty `GH_TOKEN` as unset and silently falls back to the default
   (deactivated-workspace) identity.

   Record that trigger comment's id plus the head SHA, base SHA, and base
   branch at trigger time. **The trigger comment's GitHub identity selects the
   Codex workspace** — the bot resolves the ChatGPT workspace from whoever
   posts the comment, not from the repo or the App installation. On this
   machine only `bytevane` is bound to an active workspace; a trigger from
   `xrf9268-hue` or `YYvanYang` fails with
   `This workspace is deactivated. Select an active workspace and try again.`
   (proven 2026-06-11 on bytevane/aiops-platform#17: the same head got two
   deactivated errors and then a clean review, with the commenter as the only
   variable). If that error appears, re-trigger as `bytevane` — do not fall
   back, do not mirror the PR to a fork (earlier "fork doesn't bypass it"
   evidence was confounded by always commenting as `xrf9268-hue`). Keep
   `xrf9268-hue` as the active `gh` account; pass the bytevane token per
   command via `GH_TOKEN`. `scripts/local-pr-follow-through.sh` applies the
   same rule via `AIOPS_CODEX_TRIGGER_USER` (default `bytevane`).
2. **Detect completion by the canonical predicates — not a reaction/login
   string.** Codex review is not a check run and reactions have no
   watch/subscribe API, so polling is the only option, but poll for the *right*
   object and identify the bot by *stable identity*. These are the single source
   of truth (the script + helper implement them; agents must not re-derive
   fragile filters):
   - **Identity — one constant.** Codex is `id == 199175422`
     (`login == "chatgpt-codex-connector[bot]"`, `type == "Bot"`). Match by the
     numeric `id`; the `[bot]`-suffixed login is easy to drop and differs across
     endpoints, so a bare-login filter silently matches nothing (the #870 trap).
     Fail closed on conflicts: `type != Bot` or the login present over a
     wrong/absent id (possible spoof) → reject; an `id`-match with a drifted
     login (App reinstall) is the only "proceed, log". Pinned once in
     `scripts/codex_review_signal.py`.
   - **Findings (reliable, head-bound).** The only structured signal tied to a
     specific head is a Codex **review object** with `commit_id == <head>` and
     `submitted_at` ≥ the trigger. That is the completion + attribution signal
     ("Codex reviewed *this* head"). The empirical co-occurrence: a PR can carry
     a findings review for an early head *and* a clean `+1` for the final head,
     so "has a review object" is never "current head has findings" — always scope
     by `commit_id`.
   - **Merge blocker (broad).** ANY unresolved, non-outdated `reviewThread` —
     any author, humans included — blocks merge (see §5). The review-object
     predicate above is for completion/attribution, **not** a second merge gate.
   - **Processing (advisory only).** 👀 eyes is transient — it clears on
     completion — so it is never a gate; log it, do not wait on it.
   - **No reliable clean signal → no unattended clean-merge.** A clean review
     leaves no head-bound structured artifact: the PR-body `+1` is
     idempotent/PR-level (re-reacting keeps the original timestamp, so it can't
     bind a head) and the clean issue comment carries `Reviewed commit:` only
     ~⅓ of the time (matching its verdict prose is the banned NL path). So
     unattended automation **cannot** assert "clean" and must not auto-merge on
     `+1`/eyes/comment. Absence of a review object within the window is
     **NOT-CONFIRMED** (clean-or-not-reviewed, indistinguishable to the script);
     it hands to a human, never auto-merges. A human (or an explicitly-authorized
     agent) may audit the clean `+1`/comment and merge — that audited artifact is
     not an unattended-script trigger.

   **Poll-loop implementation constraints** (earned: the PR #774 second-round
   watch stalled silently for 16+ rounds, 2026-06-12). The interactive shell
   on this machine is zsh, whose builtin `echo` interprets backslash escapes
   by default (`man zshbuiltins`) — piping rich-text JSON through
   `echo "$var"` turns the `\n` escapes inside review/comment `body` fields
   into real control characters and breaks every JSON parser downstream,
   while small scalar JSON survives, making the breakage look intermittent.
   Therefore: keep the predicate inside `--jq` and emit only small
   scalars/flags (as the commands above already do); use `printf '%s' "$var"`
   when a variable must carry JSON; and never silence the producer's stderr
   inside a poll loop — print an `ERR:` line on fetch/parse failure, because
   a watcher whose failure mode is silence is indistinguishable from "still
   waiting".
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

**Merge requires zero unresolved, non-outdated actionable threads.** A resolved
thread is closed even when it is still non-outdated; do not resurrect it merely
because the diff line still exists. After merging a PR with non-trivial bot
activity, sanity-check the next
`Capture unresolved reviews` workflow run / linked follow-up issues — that
workflow is a backstop, not a merge substitute.

## 6. Size gate is a merge gate, not an LOC-reduction mandate

The size-gate rule — the three states (`within budget` /
`size-gated: justified overage` / `size-gated: split recommended`) and the
principle that correctness/coverage/safety take precedence over LOC compliance
— is canonical in [`AGENTS.md`](../../AGENTS.md) (the size-gate bullet).
Classify every PR into exactly one state and
disclose it in the body; never delete meaningful tests or weaken coverage to
fit the budget.

Merge-flow application of those states:

- `within budget` — standard auto-merge path.
- `size-gated: justified overage` — **not auto-mergeable**; provide a human
  sign-off bundle (why the scope is necessary, head SHA, local verification, CI
  state, bot-review state, unresolved-thread state, residual risk).
- `size-gated: split recommended` — **hard stop, split into smaller PRs**; do
  not seek sign-off in lieu of splitting.

The size budget is a review guideline (≤12 changed files / ≤300 changed LOC),
not worker-enforced config — the `policy.max_changed_*` gate was removed in
#561. Off-limits paths now live in the repo's `WORKFLOW.md` prompt (SPEC §3.2)
rather than a `policy.deny_paths` config key; read them there (commonly
`infra/**`, `deploy/**`, `db/migrations/**`, `secrets/**`, but workflows differ).

## 7. PR body is a living ledger

After every material push, refresh the body: head SHA, acceptance criteria,
verification commands, mutation check, CI conclusion/run id, dual-reviewer
verdicts, `@codex review` trigger id + reaction/thread state, size-gate
classification (one of the three states; rationale if over budget), and
deferral/follow-up links. A stale body misleads the merge decision.

The final body edit is itself part of the gate: it can start a fresh
`PR Metadata` run. After the last body update, re-read the status rollup and
wait for any new metadata run to reach a terminal passing state before calling
remote checks final. When you inspect CI/metadata logs for warnings, record the
warning audit alongside the final head SHA rather than relying on an earlier
run.

Include a size-gate checklist (exactly one box checked):

```markdown
### Size gate
- [ ] `within budget` — diff fits the ~12-file / ~300-LOC review guideline
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
  4. Classified `within budget` and touches no off-limits paths.
  5. Branch protection's required reviews satisfied.
  6. The agreed merge method (default squash) into the agreed base.

  **Native auto-merge enforces only required status checks (CI) and branch
  protection — not gates 2–4.** Confirm gates 2–4 yourself immediately before
  enabling auto-merge; any new push re-opens them (re-confirm before
  re-enabling).

  Gate 2 distinguishes **who** is merging:
  - A **human or explicitly-authorized agent** may establish "clean" by auditing
    Codex's clean `+1`/comment for the current head (§4) — natural-language
    judgment is allowed for *this* actor.
  - The **unattended script** (`local-pr-follow-through.sh`) may merge **only on
    positive structured confirmation**: a head-bound Codex review object (§4)
    whose threads — and all threads — are resolved. It has no reliable clean
    signal, so a NOT-CONFIRMED (clean-or-not-reviewed) result and a human-audited
    clean artifact **never** trigger a script merge; the script hands those to a
    human (non-zero "human action required" exit). This is the only unattended
    merge path, and in practice it retires unattended auto-merge-on-clean.

  **Hard stops — always require human sign-off even under an auto-merge grant:**
  force-pushing/merging into `main` out of band or any history rewrite; editing
  `go.mod`'s `go` directive or touching off-limits paths; a
  `size-gated: justified overage` PR (flag, don't merge without size-gate
  sign-off); a `size-gated: split recommended` PR (split, don't seek sign-off);
  anything the human's instructions put off-limits. When at the edge of the
  grant, use `AskUserQuestion`. Stop the moment the human revokes the grant.

## 9. Concurrent sessions on one PR

**Earned by:** PR #768 (2026-06-12). The authoring cloud session and a local
`handle-pr` session both responded to the same `@codex review` findings;
three consecutive pushes raced (403 classification, headerless-403 probe,
size-budget split). Every locally re-derived equivalent was discarded; only
the increments the owner lacked (per-endpoint test pins, `ErrRange`
saturation, discriminator unit tests, a refutation record) landed. The waste
was rework; the risk was the `reset --hard` / cherry-pick churn each race
forced.

**Single-driver rule.** A PR has at most one driving session at a time.
Before taking any action on an existing PR — and again **before every push**
— probe for an active owner:

1. **Identity signals:** the PR body links a `claude.ai/code/session_*` (or
   equivalent agent session); commits are authored by an agent identity
   (e.g. `Claude <noreply@anthropic.com>`).
2. **Liveness signals:** a push, review-thread reply, or PR-body edit within
   the last ~15 minutes; reviewer-bot findings being answered minutes after
   they appear. **Count only activity that is not your own**: the probe
   detects *another* active session, so exclude pushes whose head SHA you
   just published and comments/body edits/thread replies you authored this
   session — a driver must never back off from its own footprint.

Both signal classes present → treat the PR as **owned** and apply the
collaboration policy below autonomously. Identity signals persist for the
life of the PR, so they are never sufficient alone — the liveness window is
the gate that distinguishes owned-now from agent-authored-but-idle. Inform
the human which mode was chosen; do not block waiting for an answer.

**Increment-only collaboration policy (owned PR):**

- **Observe first.** When a new external finding lands (reviewer bot, CI
  failure), do not start fixing immediately. Watch the remote for one short
  window (~10 minutes; this is the per-finding reaction window — distinct
  from the ~15-minute liveness window above, which classifies the PR as
  owned in the first place). If the owner pushes a fix: adopt it — fetch,
  reset the local view to the remote head, verify their fix against the
  finding, and contribute only what is still missing (tests, pins, doc
  contracts, refutation records). Take over only when the window passes
  with **no non-own liveness signal at all** — a verified fetch showing no
  new remote push *and* no owner thread reply or body edit; any owner
  activity inside the window restarts it (the owner may answer the bot
  first and push later). Then take over from the current remote head and
  run the normal per-push gate.
- **Remote is the base, always.** On any push rejection, never push over an
  owner head you have not incorporated, and never re-apply a local
  equivalent over the owner's version: fetch, diff remote vs local, keep
  the remote implementation, re-derive only the missing increments on top,
  and re-run the gates (§1–§5) on the merged head. Publishing a rewritten
  head after adopting the remote as base stays on §8's sanctioned
  mechanism — `--force-with-lease=<branch>:<just-fetched-sha>`, where the
  lease pinning the SHA you just incorporated is what guarantees no unseen
  owner work is clobbered; what §9 forbids is the blind force-push that
  discards it.
- **No duplicated triggers.** Before posting `@codex review` or editing the
  PR body, check whether the owner already did for the current head; the
  ledger must merge both sessions' histories, not overwrite either.

**Issue-phase corollary** (for `handle-issue`): before implementing, check
the issue for an existing open PR (`Closes #N` search / linked PRs) and for
an active remote `fix/<n>-*` branch — list with
`git ls-remote origin 'refs/heads/fix/<n>-*'`, then judge liveness with
`gh api 'repos/<owner>/<repo>/activity?ref=refs/heads/<branch>&per_page=1' --jq '.[0] | {time: (.timestamp // .pushed_at), who: (.actor.login // .pusher.login)}'`
(`ls-remote` carries no timestamps, and a commit's `committer.date` is
when it was *created*, not when it was *pushed* — an old commit pushed
seconds ago must still read as live; the activity endpoint records
per-ref push time plus the actor, which also feeds the
own-footprint exclusion above; "active" = pushed within ~15 minutes). An open PR → switch to the PR-phase flow with this probe. An
**active** branch without a PR means an owner is mid-flight before opening
its PR — do not adopt it and start driving (that recreates the two-driver
race outside the PR-phase guard); observe instead, re-probing on the same
cadence as the per-finding window, until either the PR appears (→ PR-phase
flow) or the branch goes idle past the liveness window (→ adopt it as base
instead of re-implementing). No PR and no live branch → free to start.
