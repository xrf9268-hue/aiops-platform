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

- **Bot identity — one documented constant, never a scattered magic number:**
  the Codex bot is `login == "chatgpt-codex-connector[bot]"` / `id == 199175422`
  / `type == "Bot"`. Define it ONCE (a named constant / config value referenced
  by the script, tests, and docs), match the **full `[bot]` login or the id**,
  and **fail closed + log on mismatch**. Treat it as a re-validatable contract
  (an app reinstall could change the id). Never match a bare login without
  `[bot]`. (codex review #4.)
- **Findings (per current head) — distinct from the merge blocker:** a Codex
  `review` object with `commit_id == <head>`. Used to answer "did Codex review
  *this* head, with suggestions?" — NOT the merge gate. (codex review #5.)
- **Clean (structured, unattended-safe):** a Codex `+1` reaction on the **PR
  body** (`issues/{n}/reactions`) whose `created_at` is **after** the current
  head's review trigger — **freshness**, so a stale `+1` from an earlier clean
  head cannot pass a head that has since gained findings. (codex review #2.)
- **Clean (text) — human-audited ONLY, not an unattended gate:** the
  "…find any major issues…" issue comment. The earned safety rule
  (`github-local-automation.md`) stands: unattended automation must **not** rely
  on it. The ~28% of clean reviews that leave only this comment and no fresh
  `+1` **fail closed to a human**. *(This REVERSES the draft's "fixed-template =
  structured" refinement — codex review #1: it eroded an earned safety boundary
  on the strength of an undocumented Codex template.)*
- **Processing (advisory):** 👀 eyes on the trigger comment or PR body —
  transient (1/150 residual), never a completion gate.
- **No-review backstop:** ~24% of PRs get no Codex signal; the gate must
  time-bound the wait and surface "Codex did not review" rather than hang.
- **Merge blocker (unchanged, intentionally broader than "findings"):** ANY
  unresolved non-outdated `reviewThread` — **any author**, including human
  reviewers — blocks merge. Keep the existing all-thread gate; do not narrow it
  to Codex-only. (codex review #5.)
- **Merge-ready** = a fresh structured clean signal for the current head (or a
  Codex review-object for the head whose threads are all resolved) **AND** zero
  unresolved non-outdated `reviewThreads` overall.

### D2. `local-pr-follow-through.sh`

- Replace the trigger-comment `bot_plus_one` check with a **PR-body** `+1`
  check by the bot-identity constant, **gated on freshness** (`reaction.created_at`
  after the recorded head trigger; abort on head/base drift as today).
- Unattended clean-completion = **fresh PR-body `+1`** only. This recovers the
  ~72% of clean reviews that carry a `+1`; the remaining ~28% (clean comment, no
  `+1`) **fail closed to a human** — we do NOT add the issue-comment path to the
  unattended gate (codex review #1).
- Keep the existing all-thread merge blocker and the head/base binding unchanged.
- Identity as a single named constant (codex review #4); fail closed + log on
  mismatch.
- Rewrite `scripts/local_pr_follow_through_test.go` from substring assertions to
  fixture-driven predicate tests (codex review #6): correct object
  (`issues/{pr}/reactions`), identity, **freshness**, head/base drift, **stale
  `+1`** (earlier head), **no-signal timeout**, and multi-head re-review.

### D3. Docs

- `pr-review-merge-protocol.md` §4 + `github-local-automation.md`: replace the
  trigger-comment-`+1` / bare-login model with D1 (PR-body fresh `+1` by the
  identity constant, review objects + the broad all-thread merge blocker,
  transient eyes). Keep the earned "unattended must not parse Codex NL" rule
  **as-is** — the clean-comment path stays human-only.

### D4. Out of scope (confirmed)

- `.github/scripts/capture-unresolved-reviews.mjs` — captures **all** unresolved
  threads regardless of author (login only used for display). No change.

## Open questions / decisions

- **D-Q1 — DECIDED (post codex review #1): unattended stays `+1`-only,
  fail-closed on the ~28%.** Data: 51/71 clean PRs carry a body `+1`; 20/71 are
  clean-comment-only. The draft proposed reclassifying the fixed-template comment
  as "structured" to recover the tail; that eroded an earned safety boundary on
  an undocumented template, so it is **rejected**. The ~28% tail fails closed to
  a human (safe; no false auto-merge).
- **D-Q2 — MUST resolve with data before implementing the body-`+1` gate
  (codex review #3):** confirm at scale that the `+1` lands on the PR body for
  *manual* `@codex review` comment triggers (not only auto-review-on-open), and
  that freshness (`created_at` vs trigger time) is well-defined. #869 (manual)
  put it on the body; widen the sample before coding D2.
- **D-Q3: a `pull_request_review` webhook** could cut latency for the
  review-object leg, but reactions have no webhook (poll-only) — do NOT redesign
  around check-runs (codex review #7). Hybrid is optional, later.
- **D-Q4: version/repo sensitivity** — empirical for the current Codex version
  on this repo; identity + `+1` behavior must be re-validated if Codex changes.
  Record the contract next to the identity constant and re-check on signal
  changes.

## Test / verification plan

- Resolve D-Q2 (manual-trigger `+1` location + freshness) with a widened sweep
  before finalizing D2.
- Rewrite `local_pr_follow_through_test.go` from substring assertions to
  fixture-driven predicate tests, mutation-verified per gate: correct object,
  identity constant, **freshness**, head/base drift, **stale `+1`**,
  **no-signal timeout**, multi-head re-review.
- A doc-consistency check so §4 / `github-local-automation.md` / the script stay
  in step (one source of truth).

## Review log

- **codex (local, grounded + adversarial) — 2026-06-15, commit 9a4422e:**
  NEEDS-CHANGES, 7 findings (#1 safety-boundary erosion, #2 stale/non-per-head
  `+1`, #3 unresolved manual-trigger endpoint, #4 hardcoded id portability,
  #5 findings-vs-merge-blocker conflation, #6 weak test plan, #7 keep polling /
  no check-run redesign). **All accepted**; this revision addresses #1, #2,
  #4, #5, #6, #7 and converts #3 into a pre-implementation data gate (D-Q2).
