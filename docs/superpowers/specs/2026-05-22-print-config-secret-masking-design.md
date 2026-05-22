# `worker --print-config` secret masking — design

Tracks issue #194.

## Problem

`internal/worker/print_config.go:124-131` (`maskSecrets`) only redacts `Tracker.APIKey`. Its own comment admits the gap. Since that comment was written, two additional secret-bearing surfaces have landed:

- `workflow.SandboxConfig.CredentialFiles []string` — absolute paths to credential files. The path itself is sensitive ("where the attacker should look on disk if they get shell access").
- `workflow.RepoConfig.CloneURL string` — may embed `https://user:token@host/…` basic-auth.

Operators paste `--print-config` output into issues / chat / runbooks. Today, that paste leaks these values verbatim.

A second, related problem: `configView` omits `Sandbox` entirely (`internal/worker/print_config.go:38-48`). Operators cannot use `--print-config` to verify the effective sandbox posture (`enabled`, `backend`, `network_mode`, etc.). That defeats the debugging purpose for the sandbox feature.

## Goals

1. `maskSecrets` covers every secret-bearing field on the current `workflow.Config`.
2. `configView` includes the previously-omitted `Sandbox` block so it is observable from `--print-config` (after masking).
3. New tests assert no plaintext secret reaches stdout for each secret-bearing field type.
4. The policy is locally enumerated in a single function with a doc-comment contract — "every secret-bearing field is masked here; adding a new one without updating this function is a review-blocking change". The `aiops:"secret"` struct-tag walker mentioned in the issue is rejected (see below).

## Non-goals

- Building a reflection-based struct-tag walker. The current schema has three secret-bearing fields; a hand-written mask function is shorter, easier to audit, and produces specific diagnostics. A struct-tag walker would add a serializer-shaped subsystem to harden against a hypothetical future where someone adds a secret field without reading the very function they would have to touch anyway. Rejected as over-engineering. The doc-comment + tests around `maskSecrets` are the policy.
- Masking `Codex` / `Claude` `CommandConfig` fields. Audit (see "Audit") shows none of its fields are secret-bearing today.
- Changing the on-disk schema, default config, or any consumer of `workflow.Config`. This is a printer-only fix.

## Audit of `workflow.Config`

Walking each top-level field in `internal/workflow/config.go`:

| Field | Secret-bearing? | Action |
| --- | --- | --- |
| `Server.Port` | no | keep |
| `Repo.Owner` / `Name` / `DefaultBranch` | no | keep |
| `Repo.CloneURL` | **yes** if userinfo present | strip userinfo |
| `Tracker.APIKey` | **yes** | already masked |
| `Tracker.Kind` / `ProjectSlug` / etc. | no | keep |
| `Workspace.*` | no | keep |
| `Agent.*` | no | keep |
| `Codex.Command` / `Profile` / `ApprovalPolicy` / `ThreadSandbox` / `TurnSandboxPolicy` / `TurnTimeoutMs` / `ReadTimeoutMs` / `StallTimeoutMs` | no | keep |
| `Claude.Command` / etc. | no | keep |
| `Policy.*` | no | keep |
| `Verify.*` | no | keep |
| `PR.*` | no | keep |
| `Sandbox.Enabled` / `Backend` / `NetworkMode` / `NetworkAllowlistCIDRs` / `NetworkInterface` / `EnvAllowlist` | no | **add to configView** |
| `Sandbox.CredentialFiles` | **yes** (paths) | **add to configView + mask** |

Three secret-bearing fields → three explicit mask branches. One newly-printed block.

## `CloneURL` masking semantics

`url.Parse` handles the `https://user:token@host/path` case cleanly: drop `Userinfo`. SSH-style URLs like `git@example.com:o/r.git` are not parseable as `net/url` URLs and do not embed secrets in their userinfo (the part before `@` is a username, not a token); they pass through unchanged.

Behavior table:

| Input | Output |
| --- | --- |
| `""` | `""` |
| `git@example.com:o/r.git` | `git@example.com:o/r.git` |
| `https://github.com/o/r.git` | `https://github.com/o/r.git` |
| `https://user:token@github.com/o/r.git` | `https://github.com/o/r.git` |
| `https://x-access-token:ghp_…@github.com/o/r.git` | `https://github.com/o/r.git` |
| `https://user@github.com/o/r.git` (user, no password) | `https://github.com/o/r.git` (still strip — a "username" embedded in a clone URL is by convention a token alias, e.g. `oauth2`) |
| malformed | input returned unchanged (no parse, no leak — but operators see the raw value, including any secret). The loader rejects malformed URLs upstream so this branch is defensive only. |

The "drop userinfo whenever present" rule is the safe default. The cost of stripping a legitimate non-secret username is zero (it is a debug printer); the cost of leaving a token is high.

## `CredentialFiles` masking semantics

Each entry is replaced with `"***"`. The slice length and ordering are preserved so an operator can confirm "I configured three credential files; three are loaded." The actual paths never reach stdout. Empty slice / nil slice pass through unchanged.

## Test plan (boundary-coverage)

`internal/worker/print_config_test.go` adds tests using the `=N` / `=N+1` / paired-edges rule on each secret-bearing field:

1. **`CloneURL` paired edges** — three sub-cases in one test, plus a negative assertion that the plaintext token never appears:
   - userinfo present (user+password)
   - userinfo present (user only — still stripped)
   - plain HTTPS (no userinfo — round-trips unchanged)
   - SSH-style git URL (no userinfo — round-trips unchanged)
2. **`CredentialFiles` paired edges** — length 0 (omitted), length 1, length 2 to confirm slice-element masking applies to every element; assert no path substring leaks. The "exactly-N" boundary for credentials is "exactly empty vs. exactly non-empty" — empty slice must not produce `"***"`.
3. **`Sandbox` visible in `configView`** — `enabled`, `backend`, `network`, `env_allowlist` round-trip into JSON (previously omitted entirely).
4. **Existing `Tracker.APIKey` masking** — unchanged.
5. **Schema-error path** — unchanged.

No `bufio.Scanner.Buffer(_, N)`-style cap to test here; the boundary rule applies to "set vs. unset" rather than a numeric limit.

## Implementation sequence

1. In `internal/worker/print_config.go`:
   - Add `Sandbox workflow.SandboxConfig` to `configView`.
   - Populate it in `newConfigView`.
   - Add `maskCloneURL(string) string` helper using `net/url`.
   - Extend `maskSecrets` to clear `Repo.CloneURL`, walk `Sandbox.CredentialFiles`, and (existing) mask `Tracker.APIKey`. Update the doc-comment to read as the policy contract: "every secret-bearing field on `workflow.Config` is enumerated here; adding a new one without extending this function is a review-blocking gap. Tests in `print_config_test.go` enforce this for each known field."
2. In `internal/worker/print_config_test.go`:
   - Add `TestPrintConfig_MasksRepoCloneURLUserinfo` with the four URL forms from the table above.
   - Add `TestPrintConfig_MasksSandboxCredentialFiles` covering length 0 / 1 / 2.
   - Add `TestPrintConfig_SandboxVisibleInConfigView` asserting `enabled`/`backend`/`network` survive the round-trip.
3. `go test ./internal/worker/...` green.
4. `go vet ./...` green.

## Risks

- **External tooling** consuming `--print-config` JSON now sees a `sandbox` key it did not see before. The package comment on `printConfigOutput` already calls the shape "stable" but the change is additive (new key), not breaking (no key removed, no type changed). Consumers that decode into typed structs ignore unknown keys; consumers that whitelist keys will not see `sandbox` until they opt in. Acceptable.
- **`url.Parse` performance**: `--print-config` is a one-shot debug command; parsing one URL per invocation is free.
