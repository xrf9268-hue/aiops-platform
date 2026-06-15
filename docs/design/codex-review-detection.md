# Design: align codex-review completion detection with codex's actual signaling

Status: accepted — implemented (`scripts/codex_review_signal.py`,
`scripts/local-pr-follow-through.sh`, runbook §4/§8 + automation/batch docs)
Issue: #870
Scope: the local follow-through tooling + the protocol/runbook docs that decide
when a GitHub `@codex review` is **done** and whether it is **clean** or has
**findings**. No change to the agent runner / SPEC paths.

## Problem

The completion-detection logic for GitHub Codex reviews is built on an
**inaccurate model of how Codex signals**, so it does not reliably detect a
finished review:

1. `scripts/local-pr-follow-through.sh` (`wait_for_github_codex_review`) gates
   "clean review done" on a `+1` reaction **by `user.login == "chatgpt-codex-connector"`**
   on the **`@codex review` trigger comment** (`issues/comments/{trigger_id}/reactions`).
   Two independent errors:
   - **Wrong object.** Codex's `+1` lands on the **PR body**
     (`issues/{pr}/reactions`), not on the trigger comment. On PR #869 (driven
     with five manual `@codex review` comments) every trigger comment had zero
     reactions, while the PR body carried a Codex `+1`.
   - **Wrong identity.** The bot login is `chatgpt-codex-connector[bot]`
     (`type=Bot`, `id=199175422`) on **every** object (reviews, issue comments,
     reactions); the bare-`[bot]`-less literal never matches.
   Net effect: the unattended `+1` gate **never fires**, so the script polls
   until timeout and fails closed even on a clean review — the auto-merge path
   it exists for is effectively dead.
2. `docs/runbooks/pr-review-merge-protocol.md` §4 and
   `docs/runbooks/github-local-automation.md` describe the same wrong model
   (`+1` from `chatgpt-codex-connector` on the trigger; 👀/👍 as the gate),
   propagating the habit.
3. Agents (incl. this author) hand-roll `select(.user.login=="chatgpt-codex-connector")`
   jq filters against the **reviews/issue-comments** endpoints, which return
   `…[bot]` — so the filter silently matches nothing and reports false
   "no review yet", repeatedly (the #869 session burned several cycles on this).

## Evidence

Official contract — [OpenAI Codex GitHub integration](https://developers.openai.com/codex/integrations/github)
plus the boilerplate Codex posts in its own review body:
- Acknowledges with a 👀 (eyes) reaction; "posts a review on the pull request,
  just like a teammate"; flags only P0/P1; **"If Codex has suggestions, it will
  comment; otherwise it will react with 👍."** No documented poll-able
  completion signal.

GitHub API — review object has `user{login,id,type}`, `state`
(`COMMENTED`/`APPROVED`/`CHANGES_REQUESTED`), `commit_id`, `body`,
`submitted_at`; there is a `pull_request_review` webhook. Reactions have **no
webhook** (poll only); the reaction `user` is a simple-user with `id`/`type`.

Empirical — sweep of the **150 most recent merged PRs** in this repo
(`issues/{pr}/reactions`, `pulls/{pr}/reviews`, `issues/{pr}/comments`, filtered
by `user.id==199175422`):

| Signal | Object | Count |
|---|---|---|
| `+1` (👍) | **PR body** `issues/{pr}/reactions` | **85 / 150 PRs** |
| 👀 (eyes), residual | PR body | 1 (transient; clears on completion) |
| REVIEW objects (findings) | `pulls/{pr}/reviews` (`COMMENTED`) + inline `reviewThreads` | 92 |
| "find any major issues" | `issues/{pr}/comments` | 135 |
| `+1` on the **`@codex review` trigger comment** | `issues/comments/{id}/reactions` | **0 / 379 triggers** |

Bot identity is `login=chatgpt-codex-connector[bot]`, `type=Bot`,
`id=199175422` on all object types (confirmed incl. the eyes reaction `user`).

Per-PR **co-occurrence** (same 150 PRs; R = a findings `review` object, C = a
clean issue-comment, P = a body `+1`):

```
RCP=30  RC=5  RP=1  R-only=4  CP=51  C-only=20  P-only=3  none=36
```

- **D-Q1 answered:** of 71 "clean" PRs (clean-comment, no findings review),
  **51 carry a body `+1` and 20 do not** — so the body `+1` covers **~72%** of
  clean reviews; **~28% are clean via the NL comment only**.
- `none=36` → **~24% of PRs get no Codex signal at all** (Codex skipped them /
  auto-review off / usage limit) — detection must not wait forever for a review
  that will never come.
- `RCP=30` → a PR accumulates a findings `review` (early head) **and** a clean
  comment/`+1` (final head) over its life — so "has a review object" is not
  "current head has findings"; findings MUST be scoped to `commit_id==<head>`.

> Methodology note: an earlier sweep checked reactions on the **trigger
> comments** and wrongly concluded "Codex never leaves `+1`". The `+1` is on the
> **PR body**. The lesson — *verify you are querying the right object before
> aggregating* — is itself part of why this design centralizes the predicates.

## Codex's actual signaling model (this repo)

| Phase | Object / signal | Notes |
|---|---|---|
| Processing | 👀 eyes on the trigger comment **or** PR body | **transient**, cleared on completion — unreliable as a gate |
| Clean (structured) | **`+1` by Codex on the PR body** | present on 85/150 PRs |
| Clean (text) | issue comment "…find any major issues…" | natural language |
| Findings | REVIEW object with `commit_id==head` + unresolved non-outdated `reviewThreads` | P0/P1 only |

## Goals / non-goals

Goals
- Detect **findings** reliably and by **stable identity**; surface (not
  auto-trust) human-audited **clean** evidence. There is no reliable structured
  clean signal (D1/D5), so "detect clean" is explicitly NOT a goal.
- Query the **right object** for each signal.
- Keep the safety invariant: **unattended automation must not parse natural
  language** to decide merge-readiness (`github-local-automation.md`).
- One **single source of truth** for the predicates; tooling + docs + agents
  reference it instead of re-deriving fragile filters.

Non-goals
- Changing what Codex does, or the agent runner / SPEC paths.
- Auto-merging on anything other than the existing gate
  ([protocol §8](../runbooks/pr-review-merge-protocol.md#8-merge)).

## Design

### D1. Canonical detection predicates (single source of truth)

Document these in `pr-review-merge-protocol.md` §4 as THE predicates; the script
and `inspect_pr_state.py` implement them; the skills reference §4.

**Key data-driven constraint (round-2 review + sweep): Codex emits no reliable,
structured, current-head-bound CLEAN signal.**
- The PR-body `+1` is PR-level and **idempotent** — re-reacting keeps the
  original `created_at`, so it cannot be tied to a specific head (review R2-#1).
- The clean issue comment carries `Reviewed commit: <sha>` only **49/135** of the
  time; matching its verdict prose is the banned NL path anyway.
- The only reliably head-bound structured artifact is the **findings review
  object** (`commit_id==head`; 91/92 also name the commit).

So "this head has **findings**" is reliably detectable; "this head is **clean**"
is **not** reliably detectable for unattended automation (absence of a review
object is ambiguous: not-yet-reviewed vs reviewed-clean).

Predicates:
- **Bot identity — one documented constant:** `id == 199175422` (authoritative)
  with `login == "chatgpt-codex-connector[bot]"` / `type == "Bot"` as secondary
  cross-checks. Define ONCE (named constant / config). Conflict handling, all
  **fail-closed + logged**: id-match/login-differ (likely app reinstall) is the
  only "proceed, log"; login-match/id-differ (possible spoof) → reject;
  `type != Bot` → reject; neither present → reject. Never match a bare login
  without `[bot]`. (review R1-#4 / R2-#4.)
- **Findings (reliable, head-bound):** a Codex `review` object with
  `commit_id == <head>` and `submitted_at` ≥ the current head's trigger.
- **Merge blocker (broad, all authors):** ANY unresolved non-outdated
  `reviewThread` — any author, incl. humans — blocks merge. Keep the existing
  all-thread gate; never narrow to Codex-only. (review R1-#5.)
- **Processing (advisory only):** 👀 eyes — transient (1/150 residual), never a
  gate.
- **No-review backstop:** ~24% of PRs get no Codex signal; time-bound the wait.
  But the timeout result is **NOT-CONFIRMED**, not a definitive "Codex did not
  review" — the script cannot distinguish clean-done from never-ran (see D5).
- **Clean — NOT structurally auto-confirmed:** unattended automation must not
  assert "clean" from `+1` / eyes / comment (none reliable + head-bound). Clean
  is established by a human (or an explicitly-authorized agent) auditing the
  Codex clean comment / `+1` — consistent with the repo default that **merge is
  a human action** (`batch-issue-processing.md`) and the earned "no NL parsing
  for unattended merge" rule (`github-local-automation.md`). (review R2-#1.)
- **Two merge paths, kept distinct (review R4-#1):**
  - **Unattended-eligible — the ONLY path the script may auto-merge:** a Codex
    review object for the **current head** with all of *its* threads resolved
    (a positive, head-bound confirmation that Codex reviewed this head and the
    findings are addressed) **AND** zero unresolved non-outdated `reviewThreads`
    overall.
  - **Human-merge — default for everything else:** NOT-CONFIRMED
    (clean-or-not-reviewed) and any human-audited clean signal are **human**
    decisions; the script never auto-merges on them. A human-recorded "audited
    clean" artifact is **not** an unattended-script trigger.

### D2. `local-pr-follow-through.sh`

- **Remove** the dead trigger-comment `bot_plus_one` clean gate. Do **not**
  replace it with a PR-body `+1` gate — the `+1` is idempotent / PR-level and
  cannot confirm the current head (review R2-#1).
- The unattended script keeps only the **reliable** responsibilities: detect
  findings (Codex review object `commit_id==head`, `submitted_at` ≥ trigger) and
  block on the all-thread merge gate; head/base binding + drift-abort unchanged.
  It does **not** auto-merge on a self-asserted clean (it cannot reliably
  determine it); clean-merge stays the human / authorized-audit decision.
- Identity = the single named constant; fail closed + log on every mismatch
  class above. (review R2-#4.)
- Rewrite `scripts/local_pr_follow_through_test.go` from substring assertions to
  fixture-driven, mutation-verified predicate tests (review R1-#6 / R2-#5):
  identity match + each conflict class fails closed; findings review-object with
  `commit_id==head` detected and `commit_id!=head` ignored; all-author thread
  blocker not narrowed; identity cross-check failure (id-match/login-differ vs
  login-match/id-differ) exercised; no-signal timeout → **NOT-CONFIRMED** and
  does NOT auto-merge; multi-head re-review keys off the current head.

### D3. Docs (all updated atomically with the script)

- `pr-review-merge-protocol.md` **§4** + `github-local-automation.md`: replace
  the trigger-comment-`+1` / bare-login model with D1 — the **identity
  constant**, the head-bound **findings review object**, the broad **all-thread
  merge blocker**, transient eyes, and the **explicit statement that Codex
  provides no reliable structured clean signal**, so unattended does not
  auto-merge on "clean". Keep the earned "unattended must not parse Codex NL"
  rule as-is; the clean comment / `+1` stay human-audited.
- `pr-review-merge-protocol.md` **§8 (merge)** + `batch-issue-processing.md`
  **"Authorized auto-merge"** (review R4-#1): state the single unattended-merge
  rule — auto-merge fires ONLY on the unattended-eligible path (head-bound Codex
  review object, its threads + all threads resolved); a NOT-CONFIRMED result or
  a human-audited clean artifact **never** triggers an unattended/script merge.
  Authorized auto-merge stays opt-in and scope-bounded as today; it does not
  gain a clean-artifact trigger.

### D4. Out of scope (confirmed)

- `.github/scripts/capture-unresolved-reviews.mjs` — captures **all** unresolved
  threads regardless of author (login only used for display). No change.

### D5. Script terminal behavior and the clean-detection consequence (review R3-#1)

Because there is no reliable structured clean signal, the unattended script
**cannot distinguish "reviewed clean" from "not yet reviewed"** — both present
as "no findings (yet)". The design owns this rather than papering over it:

- The script classifies only what it can prove: **FINDINGS** = a Codex review
  object (`id==199175422`, `commit_id==head`, `submitted_at ≥ trigger`) and/or
  unresolved non-outdated `reviewThreads`. Everything else is
  **NOT-CONFIRMED** (clean-or-not-reviewed) — it must NOT be reported as a
  definitive "Codex did not review" (the script can't know that), and must NOT
  be treated as clean.
- **Auto-merge on a NOT-CONFIRMED result is forbidden.** The current silent
  `AIOPS_AUTO_MERGE=1` default (`local-pr-follow-through.sh:10`) contradicts
  "clean-merge is human"; gate it so the clean/NOT-CONFIRMED path **hands to a
  human** (matches the repo default `batch-issue-processing.md`). Auto-merge may
  only proceed on positive structured confirmation, which for *clean* does not
  exist.
- **Consequence (decided, best-practice — flag to operator):** unattended
  **auto-merge-on-clean is effectively retired**, because Codex emits no
  reliable clean signal. The unattended script's value narrows to: detect
  findings, block on any unresolved thread, drive to threads-resolved — then a
  human confirms Codex reviewed and merges. Rejected alternative (B): accept the
  human-audited clean comment/`+1` as an *unattended* signal — that is exactly
  the NL/best-effort path the earned safety rule forbids.
- The **findings review-object** predicate is for *completion/attribution*
  ("did Codex review this head?"), NOT a second merge block — the all-thread
  blocker already blocks findings regardless of how Codex surfaces them
  (review R3-#2). Keep both, with that division of labor explicit.
- **NOT-CONFIRMED terminal contract (review R4-LOW)** — make the handoff
  actionable, not a silent recurring timeout indistinguishable from a network
  failure:
  - **non-zero exit status** meaning "human action required" (distinct from a
    hard error/network exit);
  - a single **structured log/audit line**: PR number, head SHA, base SHA,
    trigger id, the observed signal (review-object? threads? clean artifact?
    none), and any open-thread ids;
  - **suppress re-triggering** the same NOT-CONFIRMED on the next poll until the
    head SHA changes (a new push), so it doesn't spin.
  - a test asserts this exit status + payload.

## Open questions / decisions

- **D-Q1 — DECIDED: no reliable structured clean signal → unattended does not
  auto-confirm clean.** Data killed both candidate clean gates: `+1` is
  PR-level + idempotent (round-2 #1); the clean comment carries `Reviewed
  commit:` only 49/135 and otherwise needs NL parsing. So clean-merge stays a
  human / authorized-audit decision (the repo default). Reliable structured
  automation is limited to findings (review object `commit_id==head`, 91/92) +
  the all-thread blocker.
- **D-Q2 — MOOT:** the body-`+1` gate is dropped, so "where does the manual-
  trigger `+1` land / is it fresh" no longer matters for any automated gate.
- **D-Q3: a `pull_request_review` webhook** could cut latency for the
  review-object leg (reactions have no webhook). Do NOT redesign around
  check-runs (review R2-#7). Hybrid is optional, later.
- **D-Q4: version/repo sensitivity** — the identity (`id==199175422`) and the
  "findings = review object" assumption are empirical for the current Codex
  version on this repo; record the contract next to the identity constant and
  re-validate if Codex changes signaling.

## Test / verification plan

- Rewrite `local_pr_follow_through_test.go` from substring assertions to
  fixture-driven, mutation-verified predicate tests: identity constant + each
  conflict class fails closed; findings review object `commit_id==head` detected
  vs `commit_id!=head` ignored; all-author thread blocker not narrowed; timeout
  classifies as NOT-CONFIRMED (not "did not review") and does NOT auto-merge;
  multi-head re-review keys on the head.
- A doc-consistency check so §4 / `github-local-automation.md` / the script stay
  in step (one source of truth).

## Review log

- **R1 — codex (local, grounded + adversarial), commit 9a4422e:**
  NEEDS-CHANGES, 7 findings (#1 safety-boundary erosion, #2 stale/non-per-head
  `+1`, #3 unresolved manual-trigger endpoint, #4 hardcoded id portability,
  #5 findings-vs-merge-blocker conflation, #6 weak test plan, #7 keep polling /
  no check-run redesign). All accepted; revised in commit d6e60b9.
- **R2 — codex (local, grounded + adversarial), commit d6e60b9:**
  NEEDS-CHANGES. **R2-#1 (decisive):** the PR-body `+1` is idempotent/PR-level,
  so the freshness gate can't bind it to a head — the whole `+1` clean-gate is
  structurally unsound. Plus R2-#2..#5 (D1/D2 inconsistency, D-Q2 exit criteria,
  identity AND/OR conflict classes, test gaps). A follow-up sweep then showed the
  clean comment carries `Reviewed commit:` only **49/135** of the time, killing
  the comment-as-head-marker fallback too. **Resolution (this revision):** drop
  the unattended clean gate entirely; reliable structured automation = findings
  (review object `commit_id==head`) + all-thread blocker; clean-merge is
  human-audited. This collapses R2-#1/#2 and D-Q2, and tightens identity (#4) and
  tests (#5).
- **R3 — codex (local, grounded + adversarial), commit 3bfef29:**
  NEEDS-CHANGES. Most "NOT RESOLVED" items were the reviewer comparing the
  *current* script/runbooks/tests to the design — i.e. the not-yet-done
  implementation (D2/D3/test plan), not design defects. Genuine design findings
  accepted: **R3-#1** — dropping clean auto-detect leaves the script unable to
  distinguish clean-done from not-reviewed; the doc must own this and the
  `AIOPS_AUTO_MERGE` clean-path must hand to a human (added as **D5**). **R3-#2**
  — the findings review-object is completion/attribution, not a second merge
  block (clarified in D5). Net: unattended auto-merge-on-clean is retired
  (no reliable clean signal); scope confirmed not overbuilt.
- **R4 — codex (local, grounded + adversarial), commit 9d15d0b:**
  NEEDS-CHANGES, all polish/consistency (no new structural or premise flaws;
  force-push race verified safe via `commit_id==head`; hardcoded-id acceptable
  under D-Q4). Accepted: **R4-#1 (HIGH)** D3 must also update protocol §8 +
  batch auto-merge, and the merge-ready path must be crisp — unattended merges
  ONLY on a head-bound review-object-all-resolved; a human-audited clean artifact
  never triggers a script merge (D1 two-paths + D3 + D5). **R4-#2 (HIGH)** stale
  "detect clean" / "did not review" wording removed from Goals/D2. **R4-LOW**
  NOT-CONFIRMED terminal contract (non-zero exit, structured log, re-trigger
  suppression, test) added to D5. Design judged structurally sound; remaining
  work is implementation (D2/D3/tests).
