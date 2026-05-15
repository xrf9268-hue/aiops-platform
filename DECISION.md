# Decision: continue the Go port in this repo; SPEC.md is the contract

> **Status:** Active. Reverses the 2026-05-13 "stop the Go port; adopt a Symphony fork" decision.
>
> **Date:** 2026-05-15
> **Branch where this was decided:** `docs/reverse-decision-continue-go-2026-05-15`
> **See also:** [`AGENTS.md`](AGENTS.md), [`DEVIATIONS.md`](DEVIATIONS.md), [`docs/audits/2026-05-15-spec-vs-go-gap-audit.md`](docs/audits/2026-05-15-spec-vs-go-gap-audit.md)
> **Earlier version:** preserved in git history at commit `4f9e98d` ("docs: record decision to stop Go port and adopt a Symphony fork"). Read in conjunction with this document for the full reasoning chain.

## TL;DR

`aiops-platform` was started as a Go implementation of OpenAI Symphony. The
2026-05-13 decision was to stop the Go port and adopt a Symphony fork as the
new upstream base. That decision is reversed.

`aiops-platform` continues as a Go implementation of Symphony, **in this
repository**. `SPEC.md` is the contract. The Elixir reference implementation
is one author's interpretation of `SPEC.md`, useful as a disambiguation oracle
when SPEC text is ambiguous, but it is **not** the artifact we align to.

The 9 known deviations (D1–D9) plus 15 new deviations surfaced by the
[2026-05-15 SPEC.md vs Go gap audit](docs/audits/2026-05-15-spec-vs-go-gap-audit.md)
(D10–D24) become the work backlog. AI agents (Claude Code, Codex) close them
against `SPEC.md` as the review target, with the Elixir reference available as
a disambiguation oracle.

## Background

The 2026-05-13 decision to fork rested on three arguments:

1. **Forking gives day-1 SPEC fidelity.**
2. **Verification surface doubles for the human under a Go port** (review the
   AI's Go diff, then cross-check against Elixir).
3. **Maintenance gradient**: a Go port has to track every upstream change as
   a fresh porting task; a fork picks up upstream changes by merge.

After running the full audit (see audit doc + PR #82) and re-examining the
premises, all three arguments are weakened enough that they no longer carry
the decision.

## Decision

**Continue the Go port in this repository. `SPEC.md` is the contract. Close
D1–D24 systematically.**

Calibration from the audit:

- ~58 SPEC `MUST` / `REQUIRED` clauses examined; 6 aligned today.
- 9 known deviations (D1–D9) confirmed; D1, D3, D9 understate scope; D4
  description is stale (revert never landed).
- 15 new deviations (D10–D24), of which 7 are SPEC-`MUST` failures (D10,
  D11, D12, D13, D16, D20, D21).
- 12 silent-area categories (entire features unimplemented).
- ~800 of the current ~3,500 non-test LOC are reusable as behavioral spec;
  the rest will be rewritten during alignment.
- 4-week scope closes peripheral gaps; core loop still SPEC-non-aligned at
  that point.
- 12-week scope ≈ full SPEC alignment.

This work happens through AI agents under the operator-as-navigator model
([`AGENTS.md` §Harness engineering principles](AGENTS.md#harness-engineering-principles)).

## Why we reversed the fork decision

### 1. "Forking gives day-1 SPEC fidelity" — false framing

`SPEC.md` is the contract, not the `openai/symphony` repo. OpenAI has stated
the Elixir repo is a reference implementation and will not be maintained as
a product. Forking it gets us code that one author once interpreted as
SPEC-aligned — it does not get us SPEC fidelity by construction. Any forked
code still needs to be audited against `SPEC.md`. The "fork starts at zero
deviations" claim conflates repo with spec.

### 2. "Verification surface doubles for the human" — invalid under the workflow model

The operator's stated workflow model is that AI agents (Claude Code, Codex)
write code AND review code; the human role is navigation and outcome
judgment, not line-by-line review of AI diffs. Cross-language verification
(Go behavior vs Elixir behavior) is an AI task that costs AI tokens, not
human attention. The original framing treated verification as a human
bottleneck; it is not.

Same-language pattern matching is still easier for AI than cross-language
behavioral equivalence, so this consideration is not zero — but it is a
gradient of AI cost, not a hard cap on what is feasible.

### 3. "Maintenance gradient — upstream changes cost less in a fork" — moot

OpenAI has stated the Elixir repo will not be maintained as a product.
There are no future upstream changes to inherit by merge. Both paths (Go
port and Elixir fork) become "we own this code" from day one. The
gradient flattens to zero.

### What is left

- **AI per-token throughput** on Go is higher than on Elixir (training
  corpus). Under "AI does the writing", this favors Go.
- **Existing Go code (~800 LOC)** is salvageable infrastructure
  (`internal/policy`, `internal/workspace.RunVerify`, `internal/workspace.secretscan`,
  workflow loader, mock runner, `--print-config`).
- **No language switch tax** — neither the operator nor AI needs to take on
  Elixir/BEAM operational surface.
- **Elixir reference remains accessible** as a disambiguation oracle (via
  the public repo) without requiring us to maintain it.

## Scope

### In scope

- Closing D1–D24 against `SPEC.md`, in some order (see Open question).
- Treating `SPEC.md` as the review target. Where `SPEC.md` is ambiguous,
  the Elixir [`orchestrator.ex`](https://github.com/openai/symphony/blob/main/elixir/lib/symphony_elixir/orchestrator.ex)
  and related modules are the tie-breaking oracle.
- Refreshing `DEVIATIONS.md` to include D10–D24 alongside D1–D9.
- Keeping the harness-engineering posture in `AGENTS.md` and applying its
  "name the behavior it delivers" test to anything that survives `Reverting`
  status.

### Out of scope

- Forking `openai/symphony` or any third-party reimplementation.
- Maintaining behavioral equivalence with the Elixir reference at the
  module level. The Elixir code is a disambiguation oracle, not a porting
  target — Go modules are organized for Go, not for line-for-line
  correspondence with Elixir.
- Anything that fails `AGENTS.md`'s SPEC-alignment-is-a-hard-requirement
  test without an explicit accepted-deviation entry.

## Open question

**Sequencing.** With 24 deviations on the backlog, the order matters.
Two candidate sequences:

1. **Bottom-up structural first.** Start with D21 (single-source orchestrator
   state) and D6 (Postgres queue removal), since most other deviations
   depend on the orchestrator-state model. Then D10/D11 (workflow file
   model), D13 (workspace key), D1 (app-server). Then peripheral.

2. **Top-down behavioral first.** Start with isolated, low-coupling gaps
   that exercise the harness end-to-end (D17 template engine, D11 reload,
   D20 Linear pagination, D14 stall detection). Build operator confidence
   that the loop produces useful PRs at all, then tackle structural.

The operator picks; both are defensible.

## Next steps

1. **File D10–D24 as tracked issues** against this repo (no longer "moved to
   fork repo"). Cross-reference the audit doc and PR #82 for evidence.
2. **Update `DEVIATIONS.md`** to include D10–D24 rows alongside D1–D9, fix
   D1/D3/D9 scope wording, mark D4's "Reverting" status as not-yet-landed,
   correct D2's framing (writes happen from the wrong side, not "missing").
3. **Pick sequencing** (Open question above).
4. **Begin closing deviations.** AI-led; human approves at the outcome level.

## Historical note

The 2026-05-13 fork decision was a reasonable read of the state at that
moment (9 known deviations, no audit yet, the fork option not stress-tested).
The audit's concrete file:line evidence and the operator's clarifications on
the workflow model (AI does writing AND review) and SPEC's role (`SPEC.md` is
the artifact, not the repo) changed the calculus enough to flip the
decision. The earlier document is preserved in git history at `4f9e98d`.
