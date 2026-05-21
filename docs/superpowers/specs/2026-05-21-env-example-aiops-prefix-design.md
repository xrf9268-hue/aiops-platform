# `.env.example` AIOPS_ prefix alignment — design

**Date:** 2026-05-21
**Issue:** [#195](https://github.com/xrf9268-hue/aiops-platform/issues/195) — [P1][bug] `.env.example` ships `WORKFLOW_PATH` but the worker reads `AIOPS_WORKFLOW_PATH` (silent misconfig)

## Problem

`.env.example:6` defines `WORKFLOW_PATH=examples/WORKFLOW.md`. The worker only honors `AIOPS_WORKFLOW_PATH` (`cmd/worker/main.go:90`). The Docker Compose file is correct (`deploy/docker-compose.yml:10` sets `AIOPS_WORKFLOW_PATH` directly). Operators who copy `.env.example` and run the worker outside Compose silently lose their explicit workflow path and fall back to the cwd default `./WORKFLOW.md` — usually missing.

`grep WORKFLOW_PATH` across the repo confirms the worker code, README quick-start (`README.md:133`), tests (`cmd/worker/main_test.go`), and `deploy/docker-compose.yml` all use the `AIOPS_` prefix. `.env.example` is the sole drifter.

## Decision

Rename `WORKFLOW_PATH` → `AIOPS_WORKFLOW_PATH` in `.env.example`. Add a one-line comment header documenting the `AIOPS_` prefix convention so future additions follow it.

### Scope rejected

- **Worker warning when `WORKFLOW_PATH` is set without `AIOPS_WORKFLOW_PATH`**: issue body marks this optional. The misconfig affects operators copying `.env.example` — fixing the file gives them the right name on next copy; existing operators with a hand-edited `.env` already moved past the example. Adding a compat warning means new code reading a not-spec-aligned env name, perpetuating the misnomer. Skip.
- **Rename `WORKSPACE_ROOT` → `AIOPS_WORKSPACE_ROOT`**: a separate naming-convention issue. The worker code reads `WORKSPACE_ROOT` without prefix (`internal/worker/config.go:26`), so `.env.example` IS consistent with the reader today — the bug here is only `WORKFLOW_PATH`. Changing `WORKSPACE_ROOT` is a behavior change (renaming the env key the code reads), not a doc-alignment change, and out of scope for #195.

## Change

`.env.example`, replace the file body so the workflow-path line matches the code reader and the prefix convention is stated:

```dotenv
# Configuration for cmd/worker and the transitional pollers.
# Worker-specific overrides use the AIOPS_ prefix (see cmd/worker/main.go).
# Vars without the prefix are read by transitional / shared infrastructure
# (queue, pollers, Compose stack).
DATABASE_URL=postgres://aiops:aiops@localhost:5432/aiops?sslmode=disable
GITEA_BASE_URL=http://gitea.local
GITEA_TOKEN=replace-with-gitea-bot-token
LINEAR_API_KEY=replace-with-linear-personal-key
WORKSPACE_ROOT=/tmp/aiops-workspaces
AIOPS_WORKFLOW_PATH=examples/WORKFLOW.md
```

The comment block explains why some vars carry the prefix and others don't, so future additions don't reintroduce the same drift.

## Verification

- `grep -n 'WORKFLOW_PATH' .env.example` returns only `AIOPS_WORKFLOW_PATH=...`.
- `grep -rn 'WORKFLOW_PATH' --include='*.go' --include='*.md' --include='*.yml'` shows no operator-facing surface still uses the bare name.
- `gofmt`, `go test ./...`, binary builds are unaffected (no Go change).

## Acceptance criteria checklist

- [ ] `.env.example` uses `AIOPS_WORKFLOW_PATH`.
- [ ] No `WORKFLOW_PATH` references remain in `.env.example` or operator docs (audited via grep; the only operator-facing reference is the renamed line itself).
- [ ] Optional worker compat warning: **not implemented** — see "Scope rejected".

## Refs

- Issue: https://github.com/xrf9268-hue/aiops-platform/issues/195
- Code reader: `cmd/worker/main.go:90`
- Compose stack: `deploy/docker-compose.yml:10`
- README quick-start: `README.md:133`
