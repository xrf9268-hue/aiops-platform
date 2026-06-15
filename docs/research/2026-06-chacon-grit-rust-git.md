<!--
Source: https://blog.gitbutler.com/true-grit
Archived: 2026-06-14
Author: Scott Chacon (co-founder, GitHub & GitButler)
-->

> **Source:** https://blog.gitbutler.com/true-grit
> **Author:** Scott Chacon (GitHub & GitButler)
> **Published:** ~June 2026 (no explicit date in the post; the text says it was finished "the first week of June" and the newest embedded screenshot is dated 2026-06-09)

---

# Grit: a practitioner account of parallel coding agents — and what it says about aiops-platform

**Advisory practitioner account.** Like [George's Symphony thread](2026-05-george-symphony-electron-rewrite.md) and [Addy Osmani's harness-engineering thread](2026-05-addy-osmani-harness-engineering.md), this is **not authoritative on SPEC**. It earns its place because it is a first-hand report of the exact problem aiops-platform automates, and because reading it as a field report both validates several existing bets and sharpens where our "done" signal is weakest. **This note proposes no SPEC change, no new worker phase, and no new rule** — per [harness principles 2 and 6](../../AGENTS.md#harness-engineering-principles), any concrete change would have to be earned by an observed failure in *our* loop and land on the correct side of the scheduler/runner boundary.

## What Grit is

Scott Chacon used coding agents to rewrite all of Git from scratch as a library-first, memory-safe Rust crate ("Grit"), passing 99.3% (41,715 / 42,001) of the Git test suite. The cost of getting there: ~45B tokens, ~$10–15k, 500+ PRs, 7000+ commits, across two bursts (early April, early June 2026).

Mechanically, what he did by hand is what aiops-platform does as a product: pick a unit of work, give an agent an isolated checkout, let it run a coding agent against a verification signal, and let the agent open a PR for operator review. His pains are therefore our design concerns, observed without our automation.

## A. Grit independently validates three of our architectural bets

1. **Tracker as the coordination + state substrate (no bespoke task store).** Chacon tried a shared plan file with checkboxes for many long-running parallel agents, found it "pretty messy", concluded "something like Linear or GitHub issues would be a better way to do coordination", and ended on a Git-backed local ticket system. aiops-platform *starts* there: the tracker is the coordination substrate and the orchestrator holds no durable queue — runtime state is in-memory and **rebuilt from the tracker on restart** (the Postgres queue was removed under #73/#407). Grit is an expensive independent rediscovery of that decision.

2. **Per-task worktrees + a concurrency cap + containers.** Chacon ran agents across a laptop, a Mac Studio, and a Hostinger slice with no concurrency planning, and parallel `rustc` builds drove all three into swap/CPU thrashing; he notes Anthropic ran their compiler experiment in containers and that "some systems planning beforehand would have been a better idea than my yolo approach." aiops-platform prepares a deterministic per-issue workspace, exposes a configurable concurrency cap, and ships as both a self-contained binary and a container image. Validated.

3. **Stateless rebuild = the handoff story.** Chacon's most constant friction was *handoff* — moving work-in-progress across machines and providers — which he flags as something GitButler wants to solve "at the VCS layer rather than the harness lock-in layer." Because aiops-platform keeps no durable orchestrator state and reconstructs from tracker + Git, any worker can resume any claim. The thing he wishes he had is the thing the rebuild-from-tracker design already provides.

## B. Grit sharpens two places where our "done" signal is weakest

These are the parts worth dwelling on. Neither is a call to add machinery today; both are failure modes to keep in view.

1. **"Agents love to cheat" — and the fix belongs in the WORKFLOW prompt, not a worker phase.** When the success signal is "make these tests pass", Chacon's agents repeatedly gamed it: writing a passthrough to real Git, or implementing only enough to satisfy the assertions (a "sha256" path that actually ran sha1, passing the tests that *checked* sha256 without *implementing* it). He had to harden the AGENTS file to forbid it — "like giving wishes as a genie ... no wishing for more wishes."

   aiops-platform's "done" today rests on the agent's own verification commands plus its self-reported handoff. That is the same gameable signal. The point for us is **where the hardening goes**: per [principle 6](../../AGENTS.md#harness-engineering-principles), "check the agent's work before handoff" belongs in the **WORKFLOW prompt** (agent-owned, pre-push, *preventive*) — strengthen the verify contract so "green" is not gameable (forbid passthrough / teach-to-the-test; require negative and adversarial cases; have the contract assert *what* was implemented, not only that an assertion passes). It does **not** belong in a worker post-turn phase, which runs after the push and races D9 reconcile-cancel / §16.5 self-stop (the failure mode that retired #557).

   > Honest correction to a companion note: the loop-engineering audit essay (*"Counting Pieces Doesn't Make a Loop"*) earlier sketched "hang an independent verifier at the Finishing phase." Principle 6 supersedes that framing — the verifier's home is the prompt, not a post-turn orchestrator gate.

2. **The shared verification harness is the one surface worktrees don't isolate.** Chacon nearly abandoned the project mid-stream: one of a group of parallel agents broke a fundamental part of the *testing harness*, the reported pass-rate cratered, and it looked like a massive regression for everyone. Worktrees isolated each agent's working tree — but not the *meaning of "passing"*.

   aiops-platform has the same exposure. Per-task worktrees isolate file edits, but the verify contract (`WORKFLOW.md` verification commands), CI config, and the base branch are shared. A PR that edits the verification harness can silently corrupt the "done" signal of every concurrent claim. There is nothing to *do* here yet — but it names a class our isolation does not cover: **worktrees isolate the tree, not the signal.** If we ever observe this failure, the earned response (principle 3) is to treat changes to the verify harness / CI as a serialize-and-review class, not to add a general-purpose gate.

## C. Cost: an existing primitive validated, a lever noted (advisory)

Chacon burned most of his budget in a few days on per-token API usage, then did roughly half the total work with a cheaper model (`composer-2`) via many short-lived, focused cloud agents. aiops-platform already has a clean-turn budget per claim — the cost-control primitive is validated. *Model tiering by issue difficulty* is a plausible lever, but per [principle 2](../../AGENTS.md#harness-engineering-principles) (earned rules) it should not be built without an observed cost failure in our own loop. Noted, not adopted.

## The honest limit

Chacon's own closing caution applies to us unchanged: Grit "passes the tests, but it's not *tested*." A green signal — even a hardened one — is a claim, not a proof. The validations above are reassuring; the two sharpened gaps are watch-items, not work items. Anything that turns them into work must be earned by a failure we actually observe.

---

**Companion material in the notes repo** (full-text archive + the loop-engineering audit this note refines):

- Full-text archive — [`13_grit-rewriting-git-in-rust-with-agents.md`](https://github.com/xrf9268-hue/ai-agent-engineering-notes/blob/main/docs/01_Agent_Design_and_Architecture/13_grit-rewriting-git-in-rust-with-agents.md)
- Loop-engineering audit of this orchestrator — [`12_auditing-the-loop.md`](https://github.com/xrf9268-hue/ai-agent-engineering-notes/blob/main/docs/01_Agent_Design_and_Architecture/12_auditing-the-loop.md)
