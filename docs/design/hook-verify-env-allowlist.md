# Hook subprocess env allowlist (#227)

## Problem

`workspace.runWorkspaceHookCommand` used to construct hook subprocesses
without setting `cmd.Env`. Go's stdlib `exec.Cmd` inherits the parent's
environment when `Env` is nil, so every hook script ran with full access
to the worker process's environment — `LINEAR_API_KEY`, `GITHUB_TOKEN`,
`GITEA_TOKEN`, `SSH_AUTH_SOCK`, and any other secret in the operator's
`.env`.

SPEC §15.4 frames hooks as "fully trusted configuration", but §15.5
recommends narrowing client-side credentials to the minimum the
workflow needs. Inheriting the agent's tracker API key into an
`after_create` hook violates that recommendation: a hook does not need
the tracker token to do its job, and a malicious or buggy WORKFLOW.md
could `env > /tmp/dump` and exfiltrate.

## Decision

Hooks build their subprocess environment from an explicit allowlist rather
than inheriting. Operators can opt specific additional vars in via
`hooks.env_passthrough`:

```yaml
hooks:
  env_passthrough: [CARGO_HOME, GIT_AUTHOR_NAME]
```

### Baseline allowlist

Hook subprocesses always inherit, when set:

- `PATH`
- `HOME`
- `USER`
- `LANG`
- `LC_ALL`
- `LC_CTYPE`
- `TZ`
- `TERM`

These are the minimum needed to run a POSIX shell + locate common
tooling. Tracker tokens, SSH credentials, and any `*_TOKEN` / `*_KEY`
secrets are excluded by default.

### Per-config passthrough

`hooks.env_passthrough` is a top-level workflow field for hook
subprocesses. It is not shared with verification: per SPEC §1,
verification commands are surfaced to the coding agent as prompt
instructions and are not worker-run subprocesses. The removed
`verify.env_passthrough` key is rejected at load with replacement
guidance.

A name in `env_passthrough` that is not set in the worker's env is
silently dropped (no error, no empty `NAME=` entry) so passthrough
lists can include "may or may not be present" vars without spurious
config-validation noise.

`env_passthrough` is still subject to the tracker/API credential deny
policy shared with agent subprocess env construction. Hard-coded tracker
token names such as `LINEAR_API_KEY`, `GITHUB_TOKEN`, and `GITEA_TOKEN`,
the configured `tracker.api_key` env-var name, and any env var whose
current value equals the configured `tracker.api_key` are dropped even
when listed explicitly. Tracker credentials stay behind orchestrator-owned
tools/proxies; hooks should receive narrower purpose-built credentials
instead.

### Why allow-list rather than deny-list

A deny-list (e.g. "drop `*_TOKEN` and `*_KEY`") would let new secrets
named differently leak through — the model is fail-closed for unknown
names. The allowlist matches the precedent set by `sandbox.env_allowlist`
in `WorkspaceConfig` (config.go:307).

## Migration

Existing workflows that depended on a specific env var being inherited
must add it to `hooks.env_passthrough`. Concretely: any hook that read
`$LINEAR_API_KEY`, `$GITHUB_TOKEN`, `$GITEA_TOKEN`, or a configured
`tracker.api_key` value directly will now see an empty string even if that
variable is listed in passthrough. The workflow loader does not flag this
specific deny-layer drop — it's a runtime behavior change visible in hook
output.

The release note (`docs/security-posture.md`, "What is defended today")
spells out the new behavior so operators reviewing security can audit
it.

## Out of scope

- Schema validation for env-var name shape: stdlib does the right thing
  for malformed names already; the loader does not need a regex.
- Allowing per-hook (rather than per-WorkspaceHooks) passthrough:
  per-hook overrides could come later if real workflows need them;
  YAGNI for now.

## Implementation

1. Add `EnvPassthrough []string` field to `WorkspaceHooks`
   (workflow/config.go).
2. New `envpolicy.BuildSanitizedEnv` helper builds `[]string{"K=V", ...}`
   from baseline allowlist + passthrough and applies the shared tracker
   credential deny policy.
3. `workspace.subprocessEnv(passthrough []string, cfg workflow.Config)` and
   `runner.agentEnvWithLookup(...)` share that helper so hook and agent env
   construction cannot drift.
4. `RunWorkspaceHook` takes `envPassthrough []string` plus the workflow config;
   `internal/worker/runtask.go` and cleanup paths plumb
   `wcfg.Hooks.EnvPassthrough` plus the workflow config into the call so
   tracker credential denial sees the effective tracker configuration.
5. Tests: hook subprocess env does not contain `LINEAR_API_KEY` by default;
   explicit passthrough surfaces a named non-secret var; configured
   `tracker.api_key` values are still denied on the RunTask and cleanup
   production paths.
