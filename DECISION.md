# Decision: stop Go port; adopt a Symphony fork as the new base

> **Status:** In progress. The high-level decision (stop Go port, switch to a
> fork) is made. The specific fork to adopt is open and depends on
> [`docs/research/symphony-fork-evaluation.md`](docs/research/symphony-fork-evaluation.md).
>
> **Date:** 2026-05-13
> **Branch where this was decided:** `claude/handle-issue-3DyNV`
> **See also:** [`AGENTS.md`](AGENTS.md), [`DEVIATIONS.md`](DEVIATIONS.md)

## TL;DR

`aiops-platform` was started as a from-scratch Go implementation of OpenAI
Symphony. After deep reading of the upstream `SPEC.md`, the Elixir reference
implementation, the OpenAI announcement post, and two practitioner accounts
([George](docs/research/2026-05-george-symphony-electron-rewrite.md) and
[Addy Osmani](docs/research/2026-05-addy-osmani-harness-engineering.md)),
the Go implementation accumulated **nine** SPEC deviations
([D1–D9](DEVIATIONS.md)). Four are `Reverting`, one is `P0`, and the project
"can't even run end to end yet" per the operator's assessment.

Under the operator's stated criteria —

> "实现错误,违反规范才是最大问题,都要推倒重来。"
> "时间是重要的但不急。"
> "现在都是 Claude code, codex 来接手维护写代码,人只是一个导航员。"

— the right move is **not** to spend 2–4 weeks rewriting the Go orchestrator
to match the Elixir reference. The right move is to **stop the Go port and
adopt one of the existing working Symphony implementations as the new
upstream**, then layer Gitea support and the harness-engineering gates we
have already proven valuable (policy / verify / secret-scan) on top.

## Context

When this project was started, the assumption was that a Go-native
implementation would be the cleanest path. The harness-engineering gates
under `internal/policy`, `internal/workspace.RunVerify`, and the secret-scan
hook are genuine value-adds; the runner abstraction is direction-correct;
the workflow loader is mostly right.

What turned out to be wrong was the **orchestrator core**. Reading
`SPEC.md` §1 and the Elixir
[`orchestrator.ex`](https://github.com/openai/symphony/blob/main/elixir/lib/symphony_elixir/orchestrator.ex)
made it clear that:

- Scheduling state belongs in process memory, not a Postgres table (D6, #73).
- Triggers are polling-only, not webhook-based (D7, #74).
- `WORKFLOW.md` is a single source, not a three-path search (D4, #72).
- PR creation, git push, and ticket state writes belong to the **agent**,
  not the orchestrator (D8, P0, #76).
- Reconciliation runs on every tick to stop active runs when their tracker
  state changes (D9, #78), in addition to a startup pass (D3, #68).
- The agent runner uses long-running JSON-RPC over stdio (`codex app-server`),
  not one-shot `codex exec` (D1, #64).

These are not surface-level corrections. They reshape the worker loop, the
runner protocol, the trigger ingress, and the queue. Re-implementing the Go
core to match `orchestrator.ex` is essentially writing a Go version of the
Elixir reference — which is itself an artifact OpenAI published as the
reference, not a stand-alone product to maintain.

## Decision

**Stop the Go port. Adopt a working Symphony implementation as the new
upstream base, then add Gitea support and the harness-engineering gates on
top.**

The specific fork to adopt is **open** and tracked in
[`docs/research/symphony-fork-evaluation.md`](docs/research/symphony-fork-evaluation.md).
Initial scan suggests the strongest candidates are:

- **`junhoyeo/contrabass`** (Go, Codex + Claude, ~475 commits, packaged via
  Homebrew, active as of 2026-05-10). Best fit if its SPEC alignment passes
  a quick code-read review.
- **`openai/symphony`** (Elixir, canonical). Strongest SPEC fidelity by
  definition; more work to add Claude and Gitea on top.
- **`mksglu/hatice`** (TypeScript, Claude Code SDK, GitLab adapter present).
  Worth a look if Claude-Code-native integration matters more than SPEC
  fidelity.

See the evaluation doc for the full table and the verification checklist.

## Why not just finish the Go port

Considered. Rejected. The two strongest reasons:

1. **Verification surface.** A Go port has to be verified against the Elixir
   reference module-by-module, behavior-by-behavior. Under the "human is
   navigator, AI does the writing" model, that doubles the review cost on
   every change: read the AI's Go diff, then cross-check against the
   corresponding Elixir source. A fork (or a community port that already
   passes muster) shrinks the verification surface to just the deltas we
   add — Gitea adapter, harness gates port. The reviewer's job becomes
   "does this delta match the local convention" rather than "does this
   diff faithfully reproduce reference behavior."

2. **Maintenance gradient.** A Go port has to track every upstream change
   as a fresh porting task. A fork picks up upstream changes by merge.
   Over months, the cumulative porting debt for a from-scratch Go
   implementation grows linearly; the maintenance burden of a fork is
   roughly flat. Under the "AI does maintenance" model this still matters
   — a smaller surface is easier for AI to keep correct, and easier for
   the human navigator to review.

The "Go is more familiar" argument that motivated the original choice no
longer applies: the operator's stated maintenance model is that AI agents
(Claude Code, Codex) write and maintain code, and the human role is
navigation. AI fluency in Go vs. TypeScript vs. Elixir vs. Rust is a real
gradient (Go and TS highest, Elixir lower), but it does not justify
maintaining 3-5k LOC of orchestrator behavior to a known reference when
the alternative is to keep just the deltas.

Full alternatives discussed in
[`docs/research/symphony-fork-evaluation.md`](docs/research/symphony-fork-evaluation.md).

## What this means for this repository

This repo will be **archived** once a new base is chosen and the migration
landings below complete. Until then:

- The branch `claude/handle-issue-3DyNV` carries the in-progress
  SPEC-alignment documentation work and **stays open** as the historical
  record. Do not force-push or rewrite it.
- `main` continues to receive doc-only changes (research mirrors, this
  decision, evaluation drafts) but **does not** receive new architectural
  code. The nine open alignment issues (#14, #64, #67, #68, #70, #72,
  #73, #74, #76, #78) will not be worked in this repo; they will be
  re-filed (or closed as "wontfix — moved to fork") in the new repo.
- The harness-engineering posture written into `AGENTS.md` and the
  evaluation work in `DEVIATIONS.md` are the durable artifacts. Both
  move to the new repo.

### What transfers to the new repo

| Asset | Where it goes | Why it transfers |
|---|---|---|
| `examples/WORKFLOW.md` | New repo's `examples/` | Prompt content is language-agnostic |
| `docs/research/*` (4 files) | New repo's `docs/research/` | The decision basis itself |
| `AGENTS.md` §SPEC alignment, §Harness engineering principles | New repo's `AGENTS.md` | The posture that will keep the fork from drifting |
| `DEVIATIONS.md` structure | New repo's `DEVIATIONS.md` (likely empty initially) | Vocabulary for tracking new deviations if/when they emerge |
| Issues #71 (Gitea label state machine) | New repo issue | Gitea support is still needed |
| Policy / verify / secret-scan logic | Re-implemented in new repo's runtime | Harness-engineering gates that pass the "behavior first" test |

### What is discarded

Everything under `internal/queue/`, `migrations/`, `cmd/trigger-api/`,
`internal/triggerapi/`, `internal/gitea/webhook*.go`, the orchestrator
state-write paths in `internal/worker/runtask.go` (`OnClaim`,
`OnPRCreated`, `CreatePR`, `BuildPRBody`), and the entire Postgres
dependency. These either violate SPEC or only existed to compensate for
the Go port architecture.

The current `internal/runner/`, `internal/workspace/`, `internal/workflow/`,
`internal/policy/` packages are useful **as a reference for the harness
gates we want re-implemented in the new base** — they are not transferred
as code, but the test suite is a behavioral spec for the re-implementation.

## Open question

**Which fork.** The evaluation doc has the candidates, the comparison
table, and a verification checklist. The operator picks after reading
the evaluation; the deciding factors are likely Gitea-distance and
Claude-distance from each candidate.

## Next steps

1. **Read** [`docs/research/symphony-fork-evaluation.md`](docs/research/symphony-fork-evaluation.md)
   and pick a primary candidate.
2. **Spend 1–2 hours code-reading** the chosen candidate against `SPEC.md`
   to confirm its alignment claims. The evaluation doc has the checklist.
3. **Stand up the new repo** (fork the chosen base; rename or namespace
   appropriately).
4. **Migrate** the assets listed above. AI-led; human approves the
   migration commits.
5. **Land Gitea + harness gates** on the new base. Track work in the
   new repo's issues.
6. **Archive this repo.** README updated with a pointer to the new repo;
   open issues closed with `state_reason: not_planned` and a link to
   the equivalent issue in the new repo (or to "subsumed by upstream").

This document stays in place at `DECISION.md` at the repo root after
archive so anyone landing here understands what happened and where the
project went.
