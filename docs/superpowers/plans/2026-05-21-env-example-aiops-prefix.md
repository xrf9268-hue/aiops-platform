# `.env.example` AIOPS_ prefix Implementation Plan

> **For agentic workers:** Use superpowers:subagent-driven-development or superpowers:executing-plans. Steps use `- [ ]` checkboxes.

**Goal:** Rename `WORKFLOW_PATH` to `AIOPS_WORKFLOW_PATH` in `.env.example` so operators copying the template get a value the worker actually reads. Add a one-line convention comment to prevent future drift.

**Architecture:** Single-file edit. No Go code changes. No Docker/Compose changes (already correct).

**Tech Stack:** dotenv-format text. Audit via `grep`.

**Spec:** [`docs/superpowers/specs/2026-05-21-env-example-aiops-prefix-design.md`](../specs/2026-05-21-env-example-aiops-prefix-design.md)

**Issue:** [#195](https://github.com/xrf9268-hue/aiops-platform/issues/195)

**Fork-routing reminder:** Standard fork CI PR + cross-fork upstream PR with `--no-maintainer-edit`.

---

## Task 1: Edit `.env.example`

**Files:**
- Modify: `.env.example`

- [ ] **Step 1: Replace the body**

Final state of `.env.example`:

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

- [ ] **Step 2: Verify**

```bash
grep -n 'WORKFLOW_PATH' .env.example
```

Expected: a single line with `AIOPS_WORKFLOW_PATH=examples/WORKFLOW.md`. No bare `WORKFLOW_PATH=` line remains.

```bash
grep -rn 'WORKFLOW_PATH' --include='*.go' --include='*.md' --include='*.yml' .
```

Expected:
- `.env.example` → only the AIOPS_-prefixed line.
- `README.md`, `deploy/docker-compose.yml`, `cmd/worker/main.go`, tests → all use `AIOPS_WORKFLOW_PATH`.
- No file still references the bare `WORKFLOW_PATH=`.

---

## Task 2: Local sanity gate

**Files:** none.

- [ ] **Step 1: Confirm no Go test depends on the bare env name**

```bash
go test -race ./... 2>&1 | tail -5
```

Expected: all packages pass. (No code change was made; this just confirms nothing was implicitly coupled to the bare name.)

- [ ] **Step 2: gofmt** (formality)

```bash
gofmt -l $(git ls-files '*.go') && echo gofmt-clean
```

Expected: no output (no Go files were touched).

---

## Task 3: Commit + dual-PR + codex + merge

Branch: `worktree-fix-195-env-example`.

- [ ] **Step 1: Commit**

```bash
git add .env.example docs/superpowers/specs/2026-05-21-env-example-aiops-prefix-design.md docs/superpowers/plans/2026-05-21-env-example-aiops-prefix.md
git commit -m "$(cat <<'EOF'
fix(env): rename WORKFLOW_PATH -> AIOPS_WORKFLOW_PATH in .env.example (#195)

The worker reads AIOPS_WORKFLOW_PATH (cmd/worker/main.go:90) and the
Compose stack already sets it (deploy/docker-compose.yml:10). Only
.env.example shipped the bare WORKFLOW_PATH, so operators copying the
template and running the worker outside Compose silently lost their
explicit workflow path and fell back to the cwd default ./WORKFLOW.md
— usually missing.

Rename the entry. Add a comment block documenting the AIOPS_ prefix
convention so future additions don't reintroduce the drift.

The optional compat warning in the worker is intentionally not
implemented (see spec): adding code that reads a deprecated env name
just to warn perpetuates the misnomer.

Refs #195
EOF
)"
```

- [ ] **Step 2: Push + open dual PRs**

```bash
gh auth switch -u xrf-9527
git push -u origin worktree-fix-195-env-example
gh pr create --repo xrf-9527/aiops-platform --base main --head worktree-fix-195-env-example \
  --title "[fork CI] fix(env): rename WORKFLOW_PATH -> AIOPS_WORKFLOW_PATH in .env.example (#195)" \
  --body "Fork-internal PR. Single-line rename in .env.example. No Go changes."

gh auth switch -u xrf9268-hue
gh pr create --repo xrf9268-hue/aiops-platform --base main \
  --head "xrf-9527:worktree-fix-195-env-example" \
  --title "fix(env): rename WORKFLOW_PATH -> AIOPS_WORKFLOW_PATH in .env.example (#195)" \
  --body "Closes #195. <fork-pr-url>" \
  --no-maintainer-edit
```

- [ ] **Step 3: Request `@codex review`, wait for 👀 to clear, address feedback serially**

- [ ] **Step 4: Merge with `--match-head-commit`, sync fork main, close fork PR, delete branch**
