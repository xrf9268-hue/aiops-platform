# Baseline (#521) `gocognit`/`funlen` nolint risk-ranking

**Date:** 2026-06-07
**Audit base:** `aiops-platform` working tree at `main` (commit `7802a3f`).
**Issue:** [#673](https://github.com/xrf9268-hue/aiops-platform/issues/673).
**Scope:** function-level complexity debt only. File-level oversized-Go
burn-down is tracked separately in [#661](https://github.com/xrf9268-hue/aiops-platform/issues/661)
and is **not** blocked by this report.

## Method

Inventoried every production (non-test) `//nolint:gocognit[,funlen] // baseline
(#521)` directive (`grep` over `*.go`, excluding `*_test.go`). Grouped by
subsystem from the file path. Then read the 18 highest-risk functions in full
and assessed each for: actual behavior, which trust/mutation boundary it sits
on, a risk tier, and — critically — whether decomposition would reduce **real**
complexity / improve testability of the risky logic (`genuine`) or merely
shuffle cohesive branchy code to dodge the linter (`cosmetic-only`). (The
assessment tables below also list 4 further functions — the 3 `reconcile.go`
key helpers and `findLinearWorkpadCommentID` — tiered from their call sites and
marked "(not individually read)"; they are not part of the 18.) AGENTS.md
clean-code rule 7 forbids cosmetic decomposition of cohesive functions, so the
decomposability verdict, not the raw `gocognit` number, drives the selection.

**Risk model** (issue #673, highest first): security boundary > external-process
lifecycle > filesystem mutation > network/API mutation > long-lived state
mutation. Display-only and config-load are lowest.

## Summary

- **61** production baseline nolints across **32** files.
- #521 (closed) intentionally repaid only the 10 heaviest `gocognit >= 30`
  hotspots and left milder findings out of scope; these 61 are the remainder.
- Concentration is in two clusters that also carry the highest risk: the agent
  **sandbox/isolation** layer (`internal/runner/sandbox.go` + env builders) and
  the agent **tool-surface / GraphQL mutation gate** (`internal/runner/tools.go`
  + `linear_graphql_parse.go`).
- Most of the 61 are **cohesive** functions where the branch density is the
  contract (env allowlist ladders, the TOCTOU-resistant secret-write sequence,
  GraphQL lexer token switches, sandbox arg builders). Only a minority have a
  **genuine** seam worth extracting; the rest should keep the directive and be
  hardened in place / table-tested rather than split.

### Count by subsystem

| # | Subsystem | Files | Nolints | Risk tier (cluster) |
|---|-----------|-------|---------|---------------------|
| A | Runner: agent sandbox & env isolation | `runner/sandbox.go` (5), `runner/env.go` (1), `workspace/subprocess_env.go` (1) | 7 | **critical/high** — security boundary |
| B | Runner: agent tool surface & GraphQL gate | `runner/tools.go` (2), `runner/linear_graphql_parse.go` (3), `runner/workpad.go` (1), `workflow/validate.go` (1) | 7 | **critical/high** — security/trust boundary |
| C | Workspace / filesystem / git | `workspace/safe_remove.go` (1), `workspace/artifacts.go` (2), `worker/reconcile.go` (3) | 6 | **critical/high** — FS mutation + secrets |
| D | Worker: runner lifecycle | `worker/runtask.go` (1) | 1 | **high** — external process |
| E | Orchestrator state & scheduling | `orchestrator/workflow_runtime.go` (3), `orchestrator/poller.go` (2), `orchestrator/runtime_poller.go` (1), `orchestrator/state.go` (1) | 7 | medium — long-lived state |
| F | Tracker API clients | `tracker/github.go` (3), `gitea/tracker_client.go` (2), `tracker/linear.go` (2), `gitea/client.go` (1), `gitea/label_state.go` (1) | 9 | medium/low — network-API **reads** |
| G | Dashboard / state API / TUI | `cmd/worker/statehttp_server.go` (4), `cmd/worker/stateapi.go` (3), `cmd/tui/main.go` (2) | 9 | low (display/read) — **except** `isLoopbackHTTPHost` = high |
| H | Workflow loader / config / template | `workflow/template.go` (2), `expand.go`/`codex_schema.go`/`env.go`/`loader.go`/`config.go`/`resolver.go` (1 each) | 8 | low — config load (boot) |
| I | Doctor / boot | `doctor/doctor.go` (5), `cmd/worker/main.go` (2) | 7 | medium/low — external-process probes (boot-time) |

## High-risk function assessments

Read in full; tier and decomposability are grounded in the code, not the name.

### Cluster A — sandbox & env isolation (`internal/runner`, `internal/workspace`)

| Function | file:line | Tier | Decompose | Note |
|----------|-----------|------|-----------|------|
| `firejailCommand` | `sandbox.go:217` | critical | **genuine** | Security boundary + external process + FS (temp netfilter). Most failure-prone state in the file (cleanup-on-error flag, shell `rm` wrapper, `Cancel`/`WaitDelay`). Extract `buildFirejailNetArgs` + `wireFirejailCleanup` to isolate the leak-prone temp-file lifecycle. Dual `gocognit,funlen`. |
| `bubblewrapCommand` | `sandbox.go:154` | critical | borderline | The bwrap arg list *is* the boundary contract; splitting it fragments the contract. Only genuine seam: the credential-mount loop, duplicated in `firejailCommand` → shared `appendCredentialMounts`. |
| `applySandbox` | `sandbox.go:16` | critical | borderline | Top of the isolation dispatch (linux precondition, allowlist/backend coupling, `ensurePathWithinRoot` containment from #670). Optional `validateSandboxPreconditions` extract for guard testability; only ~50 lines and cohesive. |
| `sandboxEnv` | `sandbox.go:101` | high | **genuine** | Secret/credential boundary into the sandbox (allowlist + deny filter). Extract `carryWorkerInjectedGoCache` to isolate the subtle "only worker-injected GOCACHE/GOMODCACHE passes" rule (#544 defaults, #548 review) for direct leak-vs-no-leak unit tests. |
| `writeFirejailNetfilter` | `sandbox.go:314` | high | cosmetic-only | Egress policy (default DROP + per-CIDR ACCEPT) + FS write + CIDR trust boundary, but single-responsibility ~35 lines, no second caller. Harden in place; do not split. |
| `agentEnvWithLookup` | `env.go:42` | critical | cosmetic-only | Default-deny token allowlist keeping `GH_TOKEN`/`LINEAR_API_KEY`/configured tracker key out of the agent process (#76/#227). Guard ladder is cohesive; table-test, don't split. |
| `subprocessEnv` | `subprocess_env.go:28` | critical | cosmetic-only | Same default-deny allowlist for hook/verify subprocesses (#227). Near-duplicate of `agentEnvWithLookup` (a cross-package dedup *opportunity*, not a within-function split). |

### Cluster B — agent tool surface & GraphQL mutation gate

| Function | file:line | Tier | Decompose | Note |
|----------|-----------|------|-----------|------|
| `call` | `tools.go:401` | critical | **genuine** | THE agent-controlled authorization boundary: parses untrusted GraphQL, rejects subscriptions, enforces §15.5 `allow_mutations`/`allowed_mutations` + the current-issue guard. Extract `authorizeMutation` (the whole mutation `case`) into a per-reason table-testable gate. |
| `dispatch` | `tools.go:532` | high | borderline | Executes the network mutation with the orchestrator-held token; reached by ungated `callRaw` too, so the redaction (`redactToolSecrets`)/timeout/response-cap path is a token-isolation boundary (#76/#298/#287/#405). Only genuine seam: extract the success-path mutation-audit/post-stop-sink branch (e.g. `fireMutationAudit`). |
| `countGraphQLOperations` | `linear_graphql_parse.go:252` | high | cosmetic-only | Anti-smuggling "exactly one operation" enforcer. Flat single-pass lexer; the count comes from enumerating token cases, not nesting. Dual `gocognit,funlen`; cohesive. |
| `consumeName` | `linear_graphql_parse.go:161` | high | borderline | Computes `op.Kind` + `op.FieldName` the gate keys on. Optional `classifyOperationHeader` extract; only worthwhile with characterization tests first. |
| `skipGraphQLString` | `linear_graphql_parse.go:53` | high | cosmetic-only | Shared lexer primitive; a desync mis-classifies a hidden mutation as a read. Cohesive 25-line scanner; splitting scatters one lexer rule. |
| `isLinearGraphQLMutationName` | `workflow/validate.go:289` | low | cosmetic-only | Pure GraphQL-Name syntax check at config load (not an authorization decision). Trivially table-testable already. |
| `findLinearWorkpadCommentID` | `runner/workpad.go:117` | medium | (not individually read) | Workpad comment lookup; medium — protocol parse, not a mutation gate. |

### Cluster C — workspace / filesystem / git

| Function | file:line | Tier | Decompose | Note |
|----------|-----------|------|-----------|------|
| `SafeRemove` | `safe_remove.go:36` | critical | borderline | Gates `os.RemoveAll` behind containment + post-symlink re-check (§9.5/§15.2). Containment is **already** extracted to `assertContained`; the remainder is a cohesive feed-forward sequence. At most a small `resolveRootForCompare` extract for the macOS `/var` symlink edge. |
| `WriteSensitiveArtifact` | `artifacts.go:45` | critical | cosmetic-only | TOCTOU/symlink/hardlink-resistant secret write (`O_NOFOLLOW`, pre/post hardlink recount, 0600). Ordering is load-bearing; splitting breaks the open-handle continuity that defeats the race. Keep cohesive. |
| `EnsureSensitiveArtifactExcludes` | `artifacts.go:114` | high | **closed by #882** | Two independent jobs behind one name: (1) `info/exclude` append, (2) pre-commit hook install (`core.hooksPath`). #882 extracted `ensureSensitiveArtifactExcludeFile` + `installSensitiveArtifactHook`, removing the baseline while preserving the append/newline + git-config behavior. |
| `reworkWorkspaceKeyPrefixes`, `workspaceKeysForRawIssueKeys`, `sanitizeLegacyWorkspaceKey` | `reconcile.go:454/517/543` | medium | (not individually read) | Compute the keys that drive reconcile keep/remove decisions (feed `SafeRemove`); cohesive sanitizer/matcher logic. `reworkWorkspaceKeyPrefixes` was just simplified under #679 but is still gocognit 13. |

### Cluster D — runner lifecycle

| Function | file:line | Tier | Decompose | Note |
|----------|-----------|------|-----------|------|
| `RunRunnerWithTimeout` | `worker/runtask.go:434` | high | **genuine** | Owns the deadline context that kills the agent subprocess and maps its exit onto SPEC run-attempt phases/events; a misclassification corrupts retry policy and the reconcile-cancel race (#543/#557). Extract `classifyRunnerError` (the `if runErr != nil` taxonomy block). Dual `gocognit,funlen`. |

### Cluster G — one security predicate among display code

| Function | file:line | Tier | Decompose | Note |
|----------|-----------|------|-----------|------|
| `isLoopbackHTTPHost` | `cmd/worker/statehttp_server.go:402` | high | borderline | Parses an attacker-controllable `Host` header for the no-auth state-API loopback gate (checked with `RemoteAddr`). Optional `normalizeHostFromHostport` extract isolates the bypass-prone parsing; ~26 lines, fail-closed. The other Cluster G nolints are read/display handlers (low). |

## First batch — selected high-risk targets

Selected for **both** high risk **and** a `genuine` decomposition seam (so the
follow-up PR repays real complexity/testability rather than dodging the linter).
Each is its own behavior-preserving, characterization-test-first PR (see
"Follow-up PR protocol"). Ordered by risk × leverage:

1. **`tools.go:call` → extract `authorizeMutation`** (critical; the §15.5 agent
   mutation authorization boundary). Highest stakes: an over-permissive bug
   writes to the tracker with the orchestrator-held token. Table-test every
   rejection reason in isolation.
2. **`sandbox.go:firejailCommand` → `buildFirejailNetArgs` + `wireFirejailCleanup`**
   (critical; sandbox confinement + leak-prone temp-file lifecycle; dual
   `gocognit,funlen`). Also lift the shared credential-mount loop out of
   `firejailCommand`/`bubblewrapCommand` into `appendCredentialMounts` — the
   iteration/validation is identical, but the helper must take the per-backend
   flag shape (`--ro-bind f f` for bwrap vs `--read-only=f` + `--whitelist=f`
   for firejail), so it abstracts emission rather than being a literal lift.
3. **`runtask.go:RunRunnerWithTimeout` → extract `classifyRunnerError`** (high;
   external-process exit taxonomy that feeds the reconcile-cancel race
   #543/#557; dual `gocognit,funlen`). Decision-table unit tests for
   stall/timeout/reconcile-cancel/failure/success.
4. **Done in #882:** `artifacts.go:EnsureSensitiveArtifactExcludes` → `ensureSensitiveArtifactExcludeFile` + `installSensitiveArtifactHook` (high; secret-leak guard; two genuinely independent jobs).
5. **`sandbox.go:sandboxEnv` → extract `carryWorkerInjectedGoCache`** (high;
   isolates the #548 "only worker-injected GOCACHE passes" leak rule for direct
   testing).

### Do **not** decompose (keep the directive; harden in place / table-test)

These are critical/high **but cohesive** — splitting would fragment a
load-bearing contract or scatter a security filter for no testability gain
(clean-code rule 7): `WriteSensitiveArtifact`, `agentEnvWithLookup`,
`subprocessEnv`, `bubblewrapCommand` (beyond the shared credential-mount seam),
`writeFirejailNetfilter`, the GraphQL lexer primitives (`countGraphQLOperations`,
`skipGraphQLString`), and `isLinearGraphQLMutationName`. Strengthen these with
table-driven tests on the existing function instead.

### Lower priority (defer)

Clusters E (orchestrator state — well-characterized), F (tracker **read**
clients — the orchestrator is a tracker reader per SPEC §1, so these are not
mutation paths), G (display/read handlers, except `isLoopbackHTTPHost`), H
(boot-time config load), and the boot half of I. Touch these only when they
block a file-level #661 split or a feature change in the same file.

## Follow-up PR protocol (per #673 acceptance criteria + AGENTS.md rule 6)

For each selected target, one PR that:

1. Adds a **characterization test** first that pins current behavior of the
   risky logic (e.g. each `authorizeMutation` rejection reason; each
   `classifyRunnerError` exit class), with `Foo(%q) = %v; want %v` failure
   messages.
2. Performs the **behavior-preserving** extraction.
3. Is **mutation-verified against the committed artifact**: commit, break the
   extracted logic, confirm the test fails, `git checkout` to restore, confirm
   the tree matches HEAD.
4. Removes the `baseline (#521)` directive **only** when the function genuinely
   no longer needs the exception (re-run the `gocognit`/`funlen` gate without it
   and confirm it passes); otherwise leave it.
5. Does not block #661 file-size work.
