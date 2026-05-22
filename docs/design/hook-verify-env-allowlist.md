# Hook & verify subprocess env allowlist (#227)

## Problem

`workspace.runWorkspaceHookCommand` and `workspace.RunVerify` construct
`exec.CommandContext("sh", "-lc", script)` without setting `cmd.Env`.
Go's stdlib `exec.Cmd` inherits the parent's environment when `Env` is
nil, so every hook script and every verify command runs with full
access to the worker process's environment — `LINEAR_API_KEY`,
`GITHUB_TOKEN`, `GITEA_TOKEN`, `SSH_AUTH_SOCK`, and any other secret in
the operator's `.env`.

SPEC §15.4 frames hooks as "fully trusted configuration", but §15.5
recommends narrowing client-side credentials to the minimum the
workflow needs. Inheriting the agent's tracker API key into an
`after_create` hook violates that recommendation: a hook does not need
the tracker token to do its job, and a malicious or buggy WORKFLOW.md
could `env > /tmp/dump` and exfiltrate.

## Decision

Hooks and verify commands build their subprocess environment from an
explicit allowlist rather than inheriting. Operators can opt specific
additional vars in via two new schema fields:

```yaml
hooks:
  env_passthrough: [CARGO_HOME, GIT_AUTHOR_NAME]
verify:
  env_passthrough: [CARGO_HOME, NPM_CONFIG_USERCONFIG]
```

### Baseline allowlist

Both hook and verify subprocesses always inherit, when set:

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

`hooks.env_passthrough` and `verify.env_passthrough` are separate
fields, not a shared one. Hooks and verify run at different lifecycle
points and typically need different env: hooks may want
git-identity vars, verify may want build-tool caches (`CARGO_HOME`,
`GOMODCACHE`). Operators set each list explicitly to the union of vars
those scripts actually consume.

A name in `env_passthrough` that is not set in the worker's env is
silently dropped (no error, no empty `NAME=` entry) so passthrough
lists can include "may or may not be present" vars without spurious
config-validation noise.

### Why allow-list rather than deny-list

A deny-list (e.g. "drop `*_TOKEN` and `*_KEY`") would let new secrets
named differently leak through — the model is fail-closed for unknown
names. The allowlist matches the precedent set by `sandbox.env_allowlist`
in `WorkspaceConfig` (config.go:307).

## Migration

Existing workflows that depended on a specific env var being inherited
must add it to `hooks.env_passthrough` or `verify.env_passthrough`.
Concretely: any hook that read `$LINEAR_API_KEY`, `$GITHUB_TOKEN`,
`$GITEA_TOKEN`, etc. directly will now see an empty string. The
workflow loader does not flag this — it's a runtime behavior change
visible in hook output.

The release note (`docs/security-posture.md`, "What is defended today")
spells out the new behavior so operators reviewing security can audit
it.

## Out of scope

- Schema validation for env-var name shape: stdlib does the right thing
  for malformed names already; the loader does not need a regex.
- Setting `cmd.Env` for the agent subprocess itself: SPEC §15.3 already
  treats the agent as a trusted-config-driven actor, and the existing
  `sandbox.env_allowlist` covers the supported reduction.
- Allowing per-hook (rather than per-WorkspaceHooks) passthrough:
  per-hook overrides could come later if real workflows need them;
  YAGNI for now.

## Implementation

1. Add `EnvPassthrough []string` field to `WorkspaceHooks` and
   `VerifyConfig` (workflow/config.go).
2. New `workspace.subprocessEnv(passthrough []string)` helper builds
   `[]string{"K=V", ...}` from baseline allowlist + passthrough.
3. `RunWorkspaceHook` grows an `envPassthrough []string` parameter;
   passes it to `runWorkspaceHookCommand` which sets `cmd.Env`.
4. `RunVerify` reads `wf.Verify.EnvPassthrough` and sets `cmd.Env` on
   each verify command.
5. `internal/worker/runtask.go` plumbs `wcfg.Hooks.EnvPassthrough`
   into the `RunWorkspaceHook` call.
6. Tests: hook subprocess env does not contain `LINEAR_API_KEY` by
   default; verify subprocess env does not contain `LINEAR_API_KEY` by
   default; explicit passthrough surfaces a named var.
