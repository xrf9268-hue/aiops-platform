# Design: align codex-review completion detection with codex's actual signaling

Status: draft (for review)
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
- Detect, reliably and by **stable identity**: processing, findings, clean.
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
- **No-review backstop:** ~24% of PRs get no Codex signal; time-bound the wait
  and surface "Codex did not review" rather than hang.
- **Clean — NOT structurally auto-confirmed:** unattended automation must not
  assert "clean" from `+1` / eyes / comment (none reliable + head-bound). Clean
  is established by a human (or an explicitly-authorized agent) auditing the
  Codex clean comment / `+1` — consistent with the repo default that **merge is
  a human action** (`batch-issue-processing.md`) and the earned "no NL parsing
  for unattended merge" rule (`github-local-automation.md`). (review R2-#1.)
- **Merge-ready (human / authorized agent):** Codex has reviewed the current
  head (a review object for the head, or an audited clean signal) **AND** zero
  unresolved non-outdated `reviewThreads`.

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
  blocker not narrowed; no-signal timeout surfaces "Codex did not review";
  multi-head re-review keys off the current head.

### D3. Docs

- `pr-review-merge-protocol.md` §4 + `github-local-automation.md`: replace the
  trigger-comment-`+1` / bare-login model with D1 — the **identity constant**,
  the head-bound **findings review object**, the broad **all-thread merge
  blocker**, transient eyes, and the **explicit statement that Codex provides no
  reliable structured clean signal**, so unattended does not auto-merge on
  "clean". Keep the earned "unattended must not parse Codex NL" rule as-is; the
  clean comment / `+1` stay human-audited.

### D4. Out of scope (confirmed)

- `.github/scripts/capture-unresolved-reviews.mjs` — captures **all** unresolved
  threads regardless of author (login only used for display). No change.

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
  vs `commit_id!=head` ignored; all-author thread blocker not narrowed; no-signal
  timeout surfaces "Codex did not review"; multi-head re-review keys on the head.
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
