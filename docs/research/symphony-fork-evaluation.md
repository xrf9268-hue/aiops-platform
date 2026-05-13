# Symphony fork evaluation

> **Status:** Initial scan; **SPEC alignment claims have NOT been verified
> by code-read**. The READMEs are summarized below for triage; before
> committing to any candidate, spend 1–2 hours reading its actual
> orchestrator/runner/tracker source against `SPEC.md`.
>
> **Date:** 2026-05-13
> **Context:** [`DECISION.md`](../../DECISION.md) — why we are forking
> instead of completing the Go port.

## Operator's hard requirements

Carried over from this repo's posture:

1. **Codex AND Claude runners** — both, eventually. Codex is the canonical
   Symphony agent; Claude Code is what the operator runs day-to-day.
2. **Gitea** — primary tracker for personal/internal repos. (Linear is
   nice-to-have for inter-team work; GitHub Issues is a secondary path.)
3. **SPEC alignment is non-negotiable** — see
   [`AGENTS.md` §SPEC alignment is a hard requirement](../../AGENTS.md#spec-alignment-is-a-hard-requirement).
4. **Harness-engineering posture** — earned rules, behavior-first
   components, see
   [`AGENTS.md` §Harness engineering principles](../../AGENTS.md#harness-engineering-principles).
5. **AI-as-maintainer** — pick a base that AI agents can sustain. Lower
   verification surface preferred over greenfield familiarity.

## Candidates

### Candidate A — `openai/symphony` (Elixir, canonical)

**Source:** https://github.com/openai/symphony

| Dimension | Status |
|---|---|
| Language / runtime | Elixir / BEAM |
| Tracker support | Linear only |
| Agent runners | Codex `app-server` only |
| SPEC alignment | Definitionally aligned (this is the reference) |
| Activity | Reference, not maintained as a product per the announcement |
| Setup story | Manual: clone, `mise`, configure |
| Packaging | None (run from source) |
| Gitea distance | High — need a `Tracker.Behaviour` adapter from scratch |
| Claude distance | High — need a `claude/app_server.ex` analog |
| AI-fluency for maintenance | Lower than Go/TS (Elixir is a niche training corpus) |

**Pros:**
- Zero day-1 SPEC deviations. Anything we add is delta we own.
- The Elixir reference's `orchestrator.ex` is the artifact every other
  candidate is implicitly graded against.
- BEAM/OTP gives us supervision semantics for free; no need to
  re-implement that part of the harness.

**Cons:**
- Two large additions required (Gitea adapter, Claude runner) before
  the operator can run their primary use case.
- Niche language reduces AI per-line throughput on additions, though
  not on pattern-matching against existing modules.
- BEAM deployment is a new ops surface (Docker fine, but releases /
  observability / hot-reload all differ from Go binary norms).

### Candidate B — `odysseus0/symphony` (Elixir, friendly fork)

**Source:** https://github.com/odysseus0/symphony

| Dimension | Status |
|---|---|
| Language / runtime | Elixir / BEAM (fork of A) |
| Tracker support | Linear only |
| Agent runners | Codex only |
| SPEC alignment | A + a few polish patches; assumed close |
| Activity | ~10 commits on top of upstream |
| Setup story | `npx skills add odysseus0/symphony -s symphony-setup -y` then "set up Symphony for my repo" |
| Packaging | Skill-based onboarding |
| Gitea distance | Same as A |
| Claude distance | Same as A |
| AI-fluency for maintenance | Same as A |

**Pros over A:**
- Better first-run experience via the setup skill.
- "Cheaper Linear API calls", corrected sandbox for git, native Linear
  media upload — small QoL fixes the operator would otherwise re-derive.

**Cons:**
- Adds a community dependency for marginal gains. If upstream merges
  the patches, A is strictly better. If it does not, we inherit the
  fork's maintenance.
- Same Gitea / Claude distance as A.

**Verdict:** Use A as the base and cherry-pick what is useful from B;
do not adopt B as the upstream root unless setup ergonomics are the
critical factor.

### Candidate C — `mksglu/hatice` (TypeScript, Claude Code SDK)

**Source:** https://github.com/mksglu/hatice

| Dimension | Status |
|---|---|
| Language / runtime | TypeScript / Node |
| Tracker support | Linear, GitHub Issues, GitLab (CE/EE, self-hosted) |
| Agent runners | Claude Code Agent SDK |
| SPEC alignment | Re-implementation, not a fork; **claims explicit alignment** but no deviations doc |
| Activity | ~16 commits |
| Setup story | `npm install` then `npx tsx bin/hatice.ts start` (demo mode works without API keys) |
| Packaging | npm/npx |
| Gitea distance | Low — GitLab REST adapter is structurally close to Gitea's REST API |
| Claude distance | Zero — native |
| AI-fluency for maintenance | High (TypeScript is one of the strongest AI corpora) |

**Pros:**
- Native Claude Code integration via the official Agent SDK; we do
  not maintain a Claude runner ourselves.
- GitLab adapter probably ports to Gitea with limited delta —
  Gitea's REST API is GitLab-shaped, more so than Linear's GraphQL.
- TS is a high-fluency target for AI maintenance.
- Demo mode without API keys is a strong harness-engineering signal
  (the maintainer cares about onboarding ergonomics).

**Cons:**
- Re-implementation, not a fork. SPEC alignment claim is unverified.
- Smaller community / activity than D below.
- Codex is **not** supported — would need to be added if the operator
  wants both. (Less likely needed in practice if Claude Code is the
  daily driver.)
- ~16 commits is a younger codebase; risk of half-built corners.

### Candidate D — `junhoyeo/contrabass` (Go, Charm TUI stack)

**Source:** https://github.com/junhoyeo/contrabass

| Dimension | Status |
|---|---|
| Language / runtime | Go (+ Charm TUI) |
| Tracker support | Linear, GitHub Issues, Internal Board (filesystem-backed) |
| Agent runners | Codex `app-server`, oh-my-claudecode, OpenCode, oh-my-opencode |
| SPEC alignment | Re-implementation, not a fork; **alignment claim is unverified** |
| Activity | v0.4.1 (2026-05-10), 475 commits, 137 ⭐ / 15 forks, **active** |
| Setup story | `brew install junhoyeo/contrabass/contrabass` or pre-built binary |
| Packaging | Homebrew + GitHub Releases (macOS/Linux, amd64/arm64) |
| Gitea distance | Low — GitHub Issues adapter is structurally close to Gitea |
| Claude distance | Zero — `oh-my-claudecode` is shipped |
| AI-fluency for maintenance | High (Go) |

**Pros:**
- **Already supports both Codex and Claude Code as runners.** Eliminates
  the largest piece of work that A/B would require.
- Active and packaged: 475 commits, fresh release, Homebrew. The most
  "real product" of the four candidates.
- Go ecosystem matches the operator's existing dev environment.
- Internal Board (filesystem tracker) is a useful local-dev affordance
  the canonical reference does not have.
- GitHub Issues adapter is structurally similar to Gitea; the Gitea
  adapter is the closest delta of any candidate.

**Cons:**
- Re-implementation, not a fork. SPEC alignment claim must be
  verified by code-read.
- Charm TUI is a stack choice that may or may not match the operator's
  intended runtime mode (headless service vs. interactive TUI).
- Older than the announcement post would suggest the SPEC is settled
  on — possible drift if the maintainer made decisions before some
  SPEC clarifications landed.

## Comparison summary

| | A: openai/symphony | B: odysseus0/symphony | C: mksglu/hatice | **D: junhoyeo/contrabass** |
|---|---|---|---|---|
| Codex | ✅ | ✅ | ❌ | ✅ |
| Claude | ❌ (add) | ❌ (add) | ✅ | ✅ |
| Gitea | ❌ (full add) | ❌ (full add) | ❌ (adapt from GitLab, **near**) | ❌ (adapt from GitHub Issues, **near**) |
| SPEC alignment | Reference | ≈ Reference | Claimed, unverified | Claimed, unverified |
| Activity | Reference | Low | Low | **High** |
| Packaging | None | Skill | npm | **Homebrew + binaries** |
| AI fluency (language) | Low | Low | High | High |
| Risk surface for the operator's use case | High (build Claude + Gitea) | High (same) | Medium (verify SPEC + add Codex + adapt Gitea) | **Low (verify SPEC + adapt Gitea)** |

## Recommendation

**Primary:** Candidate **D — `junhoyeo/contrabass`**, contingent on the
SPEC-alignment verification below passing. It is the only candidate that
matches all of the operator's hard requirements (Codex + Claude + a tracker
that is structurally close to Gitea) without requiring a runner to be
added. It is also the most production-ready of the four (Homebrew,
binaries, active releases).

**Backup:** Candidate **A — `openai/symphony`**, if D fails the alignment
verification. We get maximum SPEC fidelity and inherit OTP's supervision
semantics for free, at the cost of building a Claude runner and a Gitea
adapter ourselves.

**Not recommended:** B (strictly dominated by A for the operator's
requirements) and C (adopting a TS re-implementation costs us the SPEC
fidelity argument while still requiring Codex to be added).

## Verification checklist for the chosen candidate

Before adopting any candidate as the new upstream root, spend 60–90
minutes confirming:

1. **Orchestrator state model.** Is scheduling state in process memory
   (per SPEC §2.1 / Elixir `orchestrator.ex`), or is there a hidden
   database / queue table? Grep for `Repo`, `Ecto`, `pgx`, `database/sql`,
   or any persistent-store driver.
2. **Trigger model.** Polling on a tick, or webhooks? Confirm there is
   no HTTP ingress receiving issue updates (per SPEC §triggers / D7).
3. **WORKFLOW.md discovery.** Single path at repo root? Or a multi-path
   search (per the D4 anti-pattern)?
4. **Boundary at SPEC §1.** Does the orchestrator open PRs / push
   commits / write ticket state directly? Or are those agent actions
   via advertised tools (per D8)? Look for `git push`, PR-creation HTTP
   calls, or tracker-state-mutation calls outside the agent's tool
   dispatch path.
5. **Per-tick reconciliation.** Does the poll loop re-fetch in-flight
   issue states and cancel the runner context if an issue moves to a
   terminal/ineligible state (per SPEC §2.1 / D9)?
6. **Agent runner protocol.** `codex app-server` (long-running
   JSON-RPC over stdio) or one-shot `codex exec` (per D1)?
7. **Dynamic tool advertisement.** Does the runner expose a
   `linear_graphql` (or equivalent) dynamic tool that holds the
   tracker token in-process and proxies GraphQL/REST calls without
   leaking credentials to the subprocess (per SPEC §tools and the
   blog's token-isolation requirement)?
8. **Workspace boundary.** Are agent commands constrained to operate
   under `workspace.root` (per SPEC §safety)?

If the candidate passes 1–7 cleanly and either passes 8 or documents
how it intends to (and we are comfortable with the threat model), adopt.
If it fails 1–4, treat as a fundamental SPEC deviation and prefer A.
If it fails only 5–8, adopt and file deviation issues on the new repo,
following the same pattern that produced this repo's `DEVIATIONS.md`.

## Open questions for the operator

- **Tracker priority.** If the operator's day-to-day driver is Gitea and
  Linear is occasional, the adapter-distance metric above is correct.
  If Linear is primary, both A and D need a Gitea adapter eventually
  but the timing is different — A starts Linear-only and adds Gitea
  later; D starts with GitHub Issues + can adapt to Gitea soon.
- **Runner priority.** Codex-only is acceptable for the first phase if
  Claude support can be added later (in which case A becomes more
  competitive). If both are wanted from day 1, D is the clear pick.
- **TUI vs. headless.** D ships a Charm TUI. If the operator wants a
  background service with structured logs and no terminal UI, the TUI
  may be unwanted weight (deletable but accounts for some of the LOC).

## Next step

Once the candidate is picked, the next document to write is the new
repo's `MIGRATION.md` describing exactly which files from this repo
move to the new repo and where they land. That doc is out of scope
here — it depends on the fork's directory layout.
