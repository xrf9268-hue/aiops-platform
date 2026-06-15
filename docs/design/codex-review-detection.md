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

- **Bot identity:** `.user.id == 199175422` (or `.user.type=="Bot"` AND the app
  slug). **Never** match on the bare login string.
- **Findings:** a Codex `review` with `commit_id == <head>` **and** ≥1
  `reviewThread` with `isResolved==false && isOutdated==false`.
- **Clean (structured):** EITHER a Codex `+1` reaction on the **PR body**
  (`issues/{n}/reactions`) OR a Codex issue comment whose **fixed template
  header** matches (e.g. starts with `Codex Review:` / "…find any major
  issues…"). Both are keyed on `user.id==199175422`. Per D-Q1 the `+1` alone
  covers only ~72%; the two together cover ~100% of clean reviews.
  - **Refined safety rule:** "no NL parsing" means *no interpreting Codex's
    free-text prose to infer a verdict*. Matching a **fixed template string** or
    a **reaction by stable id** is structured signal, not prose interpretation,
    and is permitted for unattended use. (This refines
    `github-local-automation.md`, which currently bans the issue-comment path
    wholesale and so cannot complete the ~28% of clean reviews that have no
    `+1`.)
- **Processing (advisory):** 👀 eyes on the trigger comment or PR body — never a
  completion gate (transient; 1/150 residual).
- **No-review backstop:** ~24% of PRs get no Codex signal; the gate must
  time-bound the wait and surface "Codex did not review" rather than hang.
- **Merge-ready** = a clean signal for the current head **AND** zero unresolved
  non-outdated `reviewThreads` (findings scoped to `commit_id==<head>`).

### D2. `local-pr-follow-through.sh`

- Replace the trigger-comment `bot_plus_one` check with the **PR-body** `+1`
  check by **`user.id==199175422`**.
- Unattended clean-completion = **PR-body `+1` by Codex id** (structured; no NL
  parsing) — this restores the currently-dead auto-merge path **iff** D-Q1
  below confirms the `+1` is reliable. The "find any major issues" comment stays
  human-audited-only.
- Keep the existing review-object + `reviewThreads` findings gate and the
  head/base binding.
- Update `scripts/local_pr_follow_through_test.go` fixtures to the corrected
  object (`issues/{pr}/reactions`) and id.

### D3. Docs

- `pr-review-merge-protocol.md` §4 + `github-local-automation.md`: replace the
  trigger-comment-`+1` / bare-login model with D1 (PR-body `+1` by id, review
  objects + threads, transient eyes, the NL-parsing safety split).

### D4. Out of scope (confirmed)

- `.github/scripts/capture-unresolved-reviews.mjs` — captures **all** unresolved
  threads regardless of author (login only used for display). No change.

## Open questions (review please)

- **D-Q1 — ANSWERED (51/71 clean PRs carry a body `+1`; ~28% do not).** Resolved
  in D1 by accepting **two** structured clean signals (`+1` by id **or** fixed
  clean-template comment by id), which together cover ~100%. **Review ask:** is
  the "fixed-template match is structured, not NL-parsing" refinement of the
  safety rule acceptable, or should unattended stay `+1`-only and fail-closed on
  the ~28% (operator finishes those)? This is the central judgment call.
- **D-Q2: does the `+1` land on the PR body for *manual* `@codex review`
  comment triggers, or only auto-review-on-open?** #869 (manual) put it on the
  body, but confirm at scale.
- **D-Q3: should findings/clean detection move to the `pull_request_review`
  webhook** (push) instead of polling, for the parts that have a webhook?
  Reactions still require polling.
- **D-Q4: version/repo sensitivity** — this model is empirical for the current
  Codex version on this repo; if Codex changes signaling, the predicates (esp.
  the `+1` behavior) must be re-validated. Where to record that contract?

## Test / verification plan

- Gather the D-Q1 correlation before finalizing D2's unattended gate.
- `local_pr_follow_through_test.go`: drive the corrected predicates against
  recorded fixtures (PR-body reaction by id; review object + threads); mutation-
  verify each gate.
- A doc-consistency check so §4 / `github-local-automation.md` / the script stay
  in step (one source of truth).
