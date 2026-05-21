# Compose SSH scoping Implementation Plan

> **For agentic workers:** Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement task-by-task. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Stop `deploy/docker-compose.yml` from mounting the operator's entire `~/.ssh` into the worker container. Mount only a dedicated deploy keypair + `known_hosts`, both env-var-overridable.

**Architecture:** Pure compose + docs change. No Go code, no Dockerfile changes. The worker container still runs as root (Phase 2 deferred). The new mount is two file-level binds, defaulting to `deploy/ssh/id_ed25519` and `deploy/ssh/known_hosts` (which the operator generates locally).

**Tech Stack:** Docker Compose v2 env-var substitution (`${VAR:-default}`), Markdown documentation.

**Spec:** [`docs/superpowers/specs/2026-05-21-compose-ssh-scoping-design.md`](../specs/2026-05-21-compose-ssh-scoping-design.md)

**Issue:** [#221](https://github.com/xrf9268-hue/aiops-platform/issues/221)

**Fork-routing reminder:** Same as #219 / #225 — fork CI PR via `xrf-9527`, cross-fork upstream PR via `xrf9268-hue`, `--no-maintainer-edit` on the upstream side. See memory `project_pr_via_fork.md`.

---

## Task 1: Replace the bind mount

**Files:**
- Modify: `deploy/docker-compose.yml` (worker service `volumes` block)

- [ ] **Step 1: Edit the mount**

Locate (currently lines 11-15):

```yaml
    volumes:
      - workspaces:/workspaces
      - ../examples:/app/examples:ro
      - ~/.ssh:/root/.ssh:ro
```

Replace with:

```yaml
    volumes:
      - workspaces:/workspaces
      - ../examples:/app/examples:ro
      # Scoped SSH credentials — only the dedicated deploy keypair + its known_hosts
      # are exposed to the agent process. See deploy/ssh/README.md and
      # docs/security-posture.md "Docker Compose SSH key isolation".
      - ${AIOPS_SSH_KEY_PATH:-./ssh/id_ed25519}:/root/.ssh/id_ed25519:ro
      - ${AIOPS_SSH_KNOWN_HOSTS_PATH:-./ssh/known_hosts}:/root/.ssh/known_hosts:ro
```

- [ ] **Step 2: Lint with `docker compose config`**

Operators may not have docker available locally; if `docker compose` is on PATH, validate:

```bash
( cd deploy && docker compose config 2>&1 | head -30 )
```

Expected: no errors. The output should show the substituted paths in the worker volumes list. If docker is unavailable, skip — CI's `Docker image build` job validates the file is parseable.

- [ ] **Step 3: Verify the legacy mount is gone**

```bash
grep -n '~/.ssh\|/root/.ssh' deploy/docker-compose.yml
```

Expected: only the two new lines mentioning `/root/.ssh/id_ed25519` and `/root/.ssh/known_hosts`. **No** `~/.ssh:/root/.ssh:ro` line.

---

## Task 2: Scaffold `deploy/ssh/` directory

**Files:**
- Create: `deploy/ssh/.gitkeep` (empty file)
- Create: `deploy/ssh/README.md`

- [ ] **Step 1: Create the directory placeholder**

```bash
mkdir -p deploy/ssh
touch deploy/ssh/.gitkeep
```

- [ ] **Step 2: Write `deploy/ssh/README.md`**

```markdown
# Deploy SSH credentials

This directory holds a **dedicated** SSH keypair the worker container uses to
push and clone from Gitea / GitHub. It is mounted into the worker by
`deploy/docker-compose.yml`. The operator's broader `~/.ssh` is no longer
exposed (see [#221] and `docs/security-posture.md`).

[#221]: https://github.com/xrf9268-hue/aiops-platform/issues/221

## One-time setup

```bash
cd deploy
ssh-keygen -t ed25519 -f ssh/id_ed25519 -C aiops-worker-deploy-key -N ''
ssh-keyscan -H <your-gitea-host> >> ssh/known_hosts
# Then add ssh/id_ed25519.pub as a deploy key in the target Gitea/GitHub repo.
```

## Overrides

Set `AIOPS_SSH_KEY_PATH` and/or `AIOPS_SSH_KNOWN_HOSTS_PATH` in your `.env`
(or shell) to point at a different keypair location. Paths are resolved
relative to `deploy/docker-compose.yml`.

```dotenv
# Example: keep the deploy key under XDG state instead of inside the repo.
AIOPS_SSH_KEY_PATH=${HOME}/.local/state/aiops/id_ed25519
AIOPS_SSH_KNOWN_HOSTS_PATH=${HOME}/.local/state/aiops/known_hosts
```

## Safety

- The repository's root `.gitignore` ignores everything in this directory
  except this README and `.gitkeep`. **Never** commit a private key.
- Use a **dedicated** keypair scoped to the workflow's repo set. Do not
  reuse your personal `~/.ssh/id_*` here — that defeats the purpose of #221.
- Rotate the keypair periodically; on rotation, regenerate `known_hosts` if
  the destination's host key changed.
```

---

## Task 3: Guard private keys in `.gitignore`

**Files:**
- Modify: `.gitignore` (root)

- [ ] **Step 1: Append a section**

At the end of `.gitignore`, append:

```
# Deploy SSH scaffolding (#221)
deploy/ssh/*
!deploy/ssh/.gitkeep
!deploy/ssh/README.md
```

- [ ] **Step 2: Verify**

```bash
git check-ignore -v deploy/ssh/id_ed25519 2>&1
git check-ignore -v deploy/ssh/.gitkeep 2>&1
git check-ignore -v deploy/ssh/README.md 2>&1
```

Expected:
- `id_ed25519` → ignored (matches `deploy/ssh/*`).
- `.gitkeep` → NOT ignored (negated by `!deploy/ssh/.gitkeep`).
- `README.md` → NOT ignored (negated).

`git check-ignore` exits 0 for ignored, 1 for not ignored. If you script the check, treat exit code 1 as success for the negated entries.

---

## Task 4: Update `docs/security-posture.md`

**Files:**
- Modify: `docs/security-posture.md`

- [ ] **Step 1: Insert a new subsection after "Current sandbox model" and before "Trust boundary"**

Find the line `## Trust boundary` and insert before it:

```markdown
## Docker Compose SSH key isolation

`deploy/docker-compose.yml` does **not** bind-mount the operator's full
`~/.ssh` directory into the worker container. Doing so was the prior
default and exposed the operator's entire SSH key set, `known_hosts`, and
`config` to the agent process — a single prompt-injection or malicious
dependency that read `/root/.ssh/id_*` could exfiltrate every keypair on
the host.

Today the worker container receives only two file-level binds:

| Host path (default) | Container path |
| --- | --- |
| `deploy/ssh/id_ed25519` | `/root/.ssh/id_ed25519:ro` |
| `deploy/ssh/known_hosts` | `/root/.ssh/known_hosts:ro` |

Both paths are overridable through environment variables in the operator's
`.env`:

```dotenv
AIOPS_SSH_KEY_PATH=...
AIOPS_SSH_KNOWN_HOSTS_PATH=...
```

Operators generate the dedicated keypair under `deploy/ssh/` with
`ssh-keygen` and add the public key as a Gitea / GitHub deploy key on the
target repository. See `deploy/ssh/README.md` and
`docs/runbooks/local-dev.md` for the step-by-step setup.

**Threat reduced, not eliminated.** The worker container still runs as
root (no `USER` directive in `Dockerfile`). A successful container
breakout or a write-side compromise can still misuse the mounted deploy
key — but the key's blast radius is bounded to the repos that deploy key
authorises, not every repo on the operator's host. Dropping root inside
the container is tracked as a separate hardening step.
```

- [ ] **Step 2: Verify heading hierarchy stays sensible**

```bash
grep -nE '^#{1,3} ' docs/security-posture.md | head -20
```

Expected: the new `## Docker Compose SSH key isolation` appears once, between `## Current sandbox model` and `## Trust boundary`.

---

## Task 5: Update `docs/runbooks/local-dev.md`

**Files:**
- Modify: `docs/runbooks/local-dev.md`

- [ ] **Step 1: Replace the SSH bullet (currently line 205)**

Find:

```markdown
- The worker bind-mounts `~/.ssh` read-only into the container. SSH clone URLs require a working key on the host, with the Gitea host already in `~/.ssh/known_hosts`.
```

Replace with:

```markdown
- The worker container mounts a **dedicated** SSH keypair at
  `/root/.ssh/id_ed25519` — not your entire `~/.ssh`. Set it up once:

  ```bash
  cd deploy
  ssh-keygen -t ed25519 -f ssh/id_ed25519 -C aiops-worker-deploy-key -N ''
  ssh-keyscan -H <your-gitea-host> >> ssh/known_hosts
  ```

  Then register `deploy/ssh/id_ed25519.pub` as a Gitea / GitHub deploy key on
  the target repository. The keypair lives outside version control thanks to
  the root `.gitignore`. Override the path with `AIOPS_SSH_KEY_PATH` /
  `AIOPS_SSH_KNOWN_HOSTS_PATH` in `.env` if you keep the key elsewhere. See
  `docs/security-posture.md` ("Docker Compose SSH key isolation") for the
  rationale — this scoping closed the broad credential exposure described in
  issue #221.
```

- [ ] **Step 2: Verify the file still parses sensibly**

```bash
grep -nE '~/.ssh|AIOPS_SSH_KEY_PATH|deploy/ssh' docs/runbooks/local-dev.md
```

Expected: the new content references `deploy/ssh` and `AIOPS_SSH_KEY_PATH`. No surviving "bind-mounts `~/.ssh`" sentence.

---

## Task 6: Local validation gate

**Files:** none.

- [ ] **Step 1: `docker compose config` smoke test (if docker available)**

```bash
( cd deploy && \
  ssh-keygen -t ed25519 -f /tmp/aiops-test-key -C aiops-test -N '' >/dev/null 2>&1 && \
  ssh-keyscan -H github.com > /tmp/aiops-test-known 2>/dev/null && \
  AIOPS_SSH_KEY_PATH=/tmp/aiops-test-key AIOPS_SSH_KNOWN_HOSTS_PATH=/tmp/aiops-test-known \
    docker compose config 2>&1 | grep -A2 'aiops-test-key' | head -5 )
rm -f /tmp/aiops-test-key /tmp/aiops-test-key.pub /tmp/aiops-test-known
```

Expected: the override paths appear in the resolved compose output. If `docker compose` isn't installed, skip — CI's docker job will catch syntactic regressions.

- [ ] **Step 2: Go suite unchanged but re-run for safety**

```bash
gofmt -l $(git ls-files '*.go') && echo gofmt-clean
go test -race -covermode=atomic ./... 2>&1 | tail -10
```

Expected: gofmt clean, all packages pass (no Go changes were made, but the suite confirms nothing else regressed).

- [ ] **Step 3: `git check-ignore` regression**

```bash
echo placeholder > deploy/ssh/should-be-ignored.key
git status --short deploy/ssh/ | grep -v '^.. deploy/ssh/should-be-ignored.key' && rm deploy/ssh/should-be-ignored.key
```

Expected: `should-be-ignored.key` does not appear in `git status` output (proves the gitignore guard works). Cleanup the test file.

---

## Task 7: Commit + push + dual-PR + codex + merge

Follow the same shape as `docs/superpowers/plans/2026-05-21-release-workflow-fix.md` Tasks 4-7. Branch will be `worktree-fix-221-compose-ssh`. Suggested commit message:

```
fix(security): scope deploy/docker-compose SSH mount to a dedicated keypair (#221)

deploy/docker-compose.yml previously mounted the operator's entire ~/.ssh
into the worker container at /root/.ssh:ro. The container runs as root and
the agent process inside is the most exposed component in this system, so
a single prompt-injection or malicious-dependency event reading
/root/.ssh/id_* could exfiltrate every SSH keypair on the operator's host.
The :ro flag prevents writes but does not prevent reads.

Replace the full-tree mount with two file-level binds, defaulting to
deploy/ssh/{id_ed25519,known_hosts} and overridable via AIOPS_SSH_KEY_PATH
and AIOPS_SSH_KNOWN_HOSTS_PATH. Add deploy/ssh/{.gitkeep,README.md} as
the operator-facing scaffolding and extend root .gitignore so private
keys can never sneak into a commit while the README and placeholder stay
tracked.

Phase 2 (drop root inside the worker container) and Phase 3 (Compose
profile opt-in) from the issue body are deferred:

  * Phase 2 changes workspace bind-mount ownership semantics and needs
    E2E coverage with real Gitea before landing safely.
  * Phase 3 puts SSH behind a Compose profile, but the worker is
    documented to require SSH for git push/clone; making operators
    remember --profile agent-ssh every time adds friction without
    security benefit beyond Phase 1.

Track those as follow-ups.

Docs:
  - docs/security-posture.md gains "Docker Compose SSH key isolation"
    with the threat model, the new mount table, and an explicit
    "threat reduced, not eliminated" callout because root in the
    container remains.
  - docs/runbooks/local-dev.md replaces the "bind-mounts ~/.ssh" bullet
    with the ssh-keygen + ssh-keyscan + deploy-key setup.

Refs #221
```

Then: push as `xrf-9527`, open fork CI PR, switch to `xrf9268-hue`, open cross-fork PR with `--no-maintainer-edit`, request `@codex review`, wait for the trigger-comment 👀 to clear AND for a clean review/completion signal, address any feedback serially, merge with `--match-head-commit <SHA>`, sync fork main via `gh api -X POST /repos/xrf-9527/aiops-platform/merge-upstream -f branch=main`, close fork PR, delete branch.

---

## Self-review checklist

- [x] Every step shows the actual file content or command.
- [x] No `TBD` / "fill in" / "similar to Task N".
- [x] Spec coverage:
  - Replace mount → Task 1.
  - Scaffolding + README → Task 2.
  - Gitignore guard → Task 3.
  - security-posture.md update → Task 4.
  - local-dev.md update → Task 5.
- [x] Decision rationale on deferring Phase 2/3 lives in the commit body so reviewers do not need to re-derive it.
