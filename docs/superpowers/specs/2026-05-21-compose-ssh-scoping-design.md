# Docker Compose SSH mount scoping — design

**Date:** 2026-05-21
**Issue:** [#221](https://github.com/xrf9268-hue/aiops-platform/issues/221) — [P1][security] deploy/docker-compose.yml mounts host ~/.ssh into worker container

## Problem

`deploy/docker-compose.yml:15` bind-mounts the entire operator `~/.ssh` directory into the worker container at `/root/.ssh:ro`. The worker container runs as root and the agent process inside the worker reads issue bodies, executes generated code, and may load third-party content. A single `cat /root/.ssh/id_*` exfiltration step leaks every SSH key the operator possesses.

`:ro` only prevents writes; it does nothing about reads.

## Decision: Phase 1 only

Phase 1 from the issue body (scope the mount to a specific key + known_hosts file, exposed through env-var-overridable paths) is implemented in this PR. Phase 2 (drop root inside the container) and Phase 3 (opt-in Compose profile) are deferred.

### Why Phase 1 alone is enough for the acceptance bar

The issue's acceptance criteria are three bullets, all satisfied by Phase 1:

- "Default Compose stack no longer mounts the operator's entire `~/.ssh` directory." → Phase 1 replaces the full-tree mount with two file-level binds.
- "Documentation in `docs/security-posture.md` calls out the previous risk and the new scoped-key path." → Phase 1.
- "`docs/runbooks/local-dev.md` updated with the new key-scoping setup." → Phase 1.

### Why defer Phase 2 (drop root)

Adding `USER aiops` to the Dockerfile changes ownership semantics on workspace bind mounts, the persisted `workspaces` named volume, git operations from inside the container, and any hooks running in the workspace. Without E2E testing against real Gitea + a real workspace cache, the regression surface is wide. Track as a separate issue if the maintainer wants it.

### Why defer Phase 3 (opt-in Compose profile)

The worker is documented to require SSH for git clone/push (`docs/runbooks/local-dev.md:204-205`). Putting the SSH mounts behind a Compose profile means every operator who actually wants the worker to work must remember `--profile agent-ssh`. That is friction without security benefit, since the worker without SSH access cannot complete its core workflow.

## Change

### `deploy/docker-compose.yml`

Replace, inside the `worker` service `volumes:` block:

```yaml
- ~/.ssh:/root/.ssh:ro
```

with:

```yaml
- ${AIOPS_SSH_KEY_PATH:-./ssh/id_ed25519}:/root/.ssh/id_ed25519:ro
- ${AIOPS_SSH_KNOWN_HOSTS_PATH:-./ssh/known_hosts}:/root/.ssh/known_hosts:ro
```

Defaults point at `deploy/ssh/id_ed25519` and `deploy/ssh/known_hosts` (resolved relative to the compose file's location at `deploy/`). Operators with a different layout set `AIOPS_SSH_KEY_PATH` and `AIOPS_SSH_KNOWN_HOSTS_PATH` in their `.env` / shell environment.

A SSH-`config`-file mount is *not* added — operators who need custom SSH config can extend the volumes list themselves. Default mount stays minimal.

### `deploy/ssh/` scaffolding (new)

- `deploy/ssh/.gitkeep` — empty placeholder so the directory exists in checkout.
- `deploy/ssh/README.md` — operator instructions: generate a dedicated keypair with `ssh-keygen -t ed25519 -f deploy/ssh/id_ed25519 -C aiops-worker-deploy-key`, populate `known_hosts` via `ssh-keyscan <gitea-host> > deploy/ssh/known_hosts`, never commit any `*.key` / `*.pem` / private file.

### `.gitignore` (root)

Add carve-out so private keys can never sneak in but the placeholder + README stay tracked:

```
deploy/ssh/*
!deploy/ssh/.gitkeep
!deploy/ssh/README.md
```

(The existing `*.key` / `*.pem` ignores already catch most accidents; the explicit `deploy/ssh/*` ignore is belt-and-braces, also catching `id_ed25519` / `id_rsa` style filenames without an extension.)

### `docs/security-posture.md`

Add a new subsection (after "Current sandbox model", before "Trust boundary") titled "Docker Compose SSH key isolation". Content: (1) describe the previous `~/.ssh` full mount as a deprecated risk, (2) document the scoped two-file bind, (3) document the env-var override knobs.

### `docs/runbooks/local-dev.md`

Replace the line 204-205 SSH bullet with multi-step setup walking through `ssh-keygen` + `ssh-keyscan` + optional env-var override. Add a "Why a dedicated deploy key" sentence pointing at security-posture.md.

## Non-goals

- Drop root inside the worker container — deferred (Phase 2).
- Make SSH mount opt-in via a Compose profile — deferred (Phase 3).
- Provide automated key rotation — out of scope.
- Bundle a sample/test keypair — security hazard; operators generate their own.

## Verification

```bash
# Stack-up smoke test with default scoped key
( cd deploy && \
  ssh-keygen -t ed25519 -f ssh/id_ed25519 -C aiops-test -N '' && \
  ssh-keyscan -H github.com > ssh/known_hosts 2>/dev/null && \
  docker compose config | grep -A4 'volumes:' | head -15 )

# Expected: the worker volumes block lists
#   - ssh/id_ed25519
#   - ssh/known_hosts
# instead of ~/.ssh

# Override path test
AIOPS_SSH_KEY_PATH=/tmp/my-key AIOPS_SSH_KNOWN_HOSTS_PATH=/tmp/my-known docker compose -f deploy/docker-compose.yml config | grep '/tmp/'
```

(`docker compose config` is the local validation tool — it resolves env-var substitution and prints the effective compose definition.)

CI gate: `gofmt`, `go test ./...`, `go build ./cmd/{worker,linear-poller,gitea-poller}` are unaffected (no Go changes). Docker image build CI step continues to use `Dockerfile` which is also unchanged.

## Acceptance criteria checklist

- [ ] Default Compose stack mounts only the dedicated key + known_hosts file (not the whole `~/.ssh`).
- [ ] `AIOPS_SSH_KEY_PATH` and `AIOPS_SSH_KNOWN_HOSTS_PATH` env vars override the default mount sources.
- [ ] `deploy/ssh/` directory exists in checkout with a `.gitkeep` and `README.md`; `.gitignore` prevents accidental key commits.
- [ ] `docs/security-posture.md` documents the previous risk and the new posture.
- [ ] `docs/runbooks/local-dev.md` walks operators through the new setup.

## Refs

- Issue: https://github.com/xrf9268-hue/aiops-platform/issues/221
- Current mount: `deploy/docker-compose.yml:15`
- Dockerfile (still runs as root, scope of Phase 2): `Dockerfile:1-15`
- SPEC §15 / §15.5 — harness hardening guidance
