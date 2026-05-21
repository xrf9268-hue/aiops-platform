# Release workflow fix Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `.github/workflows/release.yml` build the binaries that actually exist in `cmd/` so every tag push stops failing.

**Architecture:** Replace the three hand-listed `go build` calls (one of which references the removed `./cmd/trigger-api`) with a single shell array `binaries=(worker linear-poller gitea-poller)` and a nested `for binary in "${binaries[@]}"` loop. No new files. No changes to `ci.yml` or `Dockerfile` (they are already correct).

**Tech Stack:** GitHub Actions YAML, Bash, Go 1.25 (build target; module path `github.com/xrf9268-hue/aiops-platform`).

**Spec:** [`docs/superpowers/specs/2026-05-21-release-workflow-fix-design.md`](../specs/2026-05-21-release-workflow-fix-design.md)

**Issue:** [#219](https://github.com/xrf9268-hue/aiops-platform/issues/219)

**Fork-routing reminder:** Upstream `xrf9268-hue/aiops-platform` is out of Actions minutes — every CI run lands on the fork `xrf-9527/aiops-platform`. See memory `project_pr_via_fork.md` and `reference_gh_accounts.md`.

---

## Task 1: Sanity-check ground truth before any edit

**Files:**
- Read-only: `cmd/`, `.github/workflows/release.yml`, `.github/workflows/ci.yml`, `Dockerfile`

- [ ] **Step 1: Confirm `cmd/` contents**

```bash
ls cmd/
```

Expected output (order may vary):
```
gitea-poller
linear-poller
worker
```

If `trigger-api` appears here, abort — the design assumes it is absent.

- [ ] **Step 2: Confirm the `Build release binaries` step still references the bad path**

```bash
grep -n 'cmd/trigger-api\|cmd/linear-poller\|cmd/gitea-poller\|cmd/worker' .github/workflows/release.yml
```

Expected: one line with `./cmd/trigger-api`, one with `./cmd/worker`, one with `./cmd/linear-poller`, and **no** `./cmd/gitea-poller`. If the file already factors via a loop, the task is already done — close the issue and stop.

- [ ] **Step 3: Confirm baseline `go build` works for each real binary**

```bash
for b in worker linear-poller gitea-poller; do
  go build -o /tmp/aiops-$b ./cmd/$b || echo "FAIL: $b"
done
```

Expected: no `FAIL` lines, three binaries in `/tmp/`.

```bash
rm -f /tmp/aiops-worker /tmp/aiops-linear-poller /tmp/aiops-gitea-poller
```

---

## Task 2: Edit `release.yml` to factor the binary list

**Files:**
- Modify: `.github/workflows/release.yml` (the `Build release binaries` step)

- [ ] **Step 1: Replace the three hand-listed builds with a single loop**

Open `.github/workflows/release.yml` and locate the `Build release binaries` step (currently around lines 74-86). Replace its `run:` block so that the inner build commands become a `for binary in "${binaries[@]}"` loop.

Final state of the step:

```yaml
      - name: Build release binaries
        shell: bash
        run: |
          set -euo pipefail
          mkdir -p dist
          binaries=(worker linear-poller gitea-poller)
          targets=(
            "linux amd64"
            "linux arm64"
            "darwin amd64"
            "darwin arm64"
          )
          for target in "${targets[@]}"; do
            read -r goos goarch <<< "$target"
            out="dist/aiops-platform_${{ steps.release.outputs.tag }}_${goos}_${goarch}"
            for binary in "${binaries[@]}"; do
              GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 \
                go build -trimpath -ldflags="-s -w" -o "${out}/${binary}" "./cmd/${binary}"
            done
            tar -C dist -czf "${out}.tar.gz" "$(basename "$out")"
            rm -rf "$out"
          done
```

Key changes versus before:
- `trigger-api` removed.
- `gitea-poller` added.
- The three per-binary `go build` lines collapse into one parameterised line wrapped in `for binary in "${binaries[@]}"`.
- The `binaries=( ... )` array is declared **once** above the platform loop.

- [ ] **Step 2: Verify the edit diff**

```bash
git diff -- .github/workflows/release.yml
```

Expected: a single hunk modifying only the `Build release binaries` step. No other lines changed. No trigger-api reference remains:

```bash
grep -n 'trigger-api' .github/workflows/release.yml
```

Expected: no output.

- [ ] **Step 3: Lint the YAML structure stays valid**

```bash
python3 -c 'import yaml,sys; yaml.safe_load(open(".github/workflows/release.yml")); print("ok")'
```

Expected output: `ok`.

If `python3` / `pyyaml` is unavailable, the next-best check is `gh workflow view release.yml` after push — that returns an error if the file is malformed.

- [ ] **Step 4: Smoke-build all three binaries locally for the host platform**

```bash
mkdir -p /tmp/aiops-219
for b in worker linear-poller gitea-poller; do
  CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /tmp/aiops-219/$b ./cmd/$b
done
ls /tmp/aiops-219/
```

Expected: three binaries (`gitea-poller`, `linear-poller`, `worker`).

```bash
rm -rf /tmp/aiops-219
```

- [ ] **Step 5: Optional cross-compile smoke (one non-host platform)**

If the host is `darwin/arm64`, prove the loop also works for `linux/amd64`:

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -o /tmp/aiops-linux-worker ./cmd/worker
file /tmp/aiops-linux-worker
rm -f /tmp/aiops-linux-worker
```

Expected: `file` reports `ELF 64-bit LSB executable, x86-64`.

---

## Task 3: Commit on a feature branch (still in original checkout — worktree set up by parent flow)

**Files:**
- Modify: `.github/workflows/release.yml`

- [ ] **Step 1: Confirm worktree branch is correct**

The parent /next-issue flow runs `EnterWorktree` before this task. Inside the worktree:

```bash
git branch --show-current
git log --oneline -1
```

Expected: branch name is something like `fix-219-release-workflow` (chosen by `EnterWorktree`), HEAD matches `origin/main`.

- [ ] **Step 2: Stage and commit**

```bash
git add .github/workflows/release.yml docs/superpowers/specs/2026-05-21-release-workflow-fix-design.md docs/superpowers/plans/2026-05-21-release-workflow-fix.md
git commit -m "$(cat <<'EOF'
fix(release): build existing binaries instead of removed trigger-api (#219)

cmd/trigger-api was removed under #74; the release workflow still tried
to build it, so every tag push failed before reaching the upload step.
cmd/gitea-poller (built by ci.yml and Dockerfile) was missing from the
release tarballs.

Replace the three hand-listed go build lines with a single shell array
binaries=(worker linear-poller gitea-poller) and a nested for-loop, so
the release surface matches what CI and the Docker image already ship.

Refs #219
EOF
)"
```

Expected: pre-commit hooks (if any) pass. If they fail, fix the underlying issue and create a **new** commit — never `--amend` after a hook failure (see CLAUDE.md / general project rules).

- [ ] **Step 3: Verify commit content**

```bash
git show --stat HEAD
```

Expected: 3 files changed (the workflow + the two superpowers docs).

---

## Task 4: Push to fork, open both PRs

**Context:** `xrf9268-hue` (upstream) has no Actions minutes left; CI runs on the fork `xrf-9527/aiops-platform`. See memory `project_pr_via_fork.md` and `reference_gh_accounts.md`.

**Files:** None modified — purely git/gh.

- [ ] **Step 1: Switch active gh account to the fork owner**

```bash
gh auth switch -u xrf-9527
gh auth status | grep 'Active account'
```

Expected: `Active account: true` line under the `xrf-9527` block.

- [ ] **Step 2: Push the branch to the fork (origin)**

```bash
BRANCH="$(git branch --show-current)"
git push -u origin "$BRANCH"
```

Expected: `* [new branch] <branch> -> <branch>`, upstream set to `origin/<branch>`.

- [ ] **Step 3: Open the fork-internal CI PR**

```bash
gh pr create \
  --repo xrf-9527/aiops-platform \
  --base main \
  --head "$BRANCH" \
  --title "[fork CI] fix(release): build existing binaries instead of removed trigger-api (#219)" \
  --body "$(cat <<'EOF'
Fork-internal PR to run CI for the upstream change.

- Upstream issue: https://github.com/xrf9268-hue/aiops-platform/issues/219
- Upstream PR: (to be created once this run starts)

CI must pass here because upstream is out of Actions minutes.
EOF
)"
```

Capture the URL — it goes into the upstream PR body in Step 5.

- [ ] **Step 4: Switch back to upstream owner**

```bash
gh auth switch -u xrf9268-hue
gh auth status | grep 'Active account'
```

Expected: `Active account: true` under the `xrf9268-hue` block.

- [ ] **Step 5: Open the cross-fork PR for the actual merge**

```bash
FORK_PR_URL="<paste from Step 3>"
gh pr create \
  --repo xrf9268-hue/aiops-platform \
  --base main \
  --head "xrf-9527:$BRANCH" \
  --title "fix(release): build existing binaries instead of removed trigger-api (#219)" \
  --body "$(cat <<EOF
Closes #219.

## Problem

\`.github/workflows/release.yml\` still builds \`./cmd/trigger-api\` (removed under #74) and omits \`./cmd/gitea-poller\` (built by CI + Dockerfile). Every tag push fails.

## Change

- Drop \`./cmd/trigger-api\`.
- Add \`./cmd/gitea-poller\` to the release tarballs.
- Factor the per-binary build into a single shell array \`binaries=(worker linear-poller gitea-poller)\` with a nested \`for binary\` loop, per issue body.
- No changes to \`ci.yml\` or \`Dockerfile\` — they already build the correct set.

## Verification

- Local smoke build for all three binaries (\`go build ./cmd/<name>\`) — pass.
- Fork CI run: $FORK_PR_URL
- Release-workflow dry-run on the fork via fixture tag \`v0.0.999\` — see the follow-up comment for the release URL.

Upstream PR shows "no checks reported" by design; the fork CI link above is the signal.
EOF
)"
```

Expected: a PR URL under `xrf9268-hue/aiops-platform`.

- [ ] **Step 6: Cross-link the two PRs in the upstream PR body**

If the fork PR URL was not in scope when Step 5 ran, edit the upstream PR body now to include it:

```bash
gh pr edit <upstream-pr-number> --repo xrf9268-hue/aiops-platform --body-file -
```

(piping the same body with `$FORK_PR_URL` filled in)

---

## Task 5: Wait for fork CI; if green, run release dry-run

**Files:** None.

- [ ] **Step 1: Watch fork CI**

```bash
gh pr checks <fork-pr-number> --repo xrf-9527/aiops-platform --watch
```

Expected: all checks (`go`, `e2e`, `docker`) pass. If `e2e` fails for a flaky-Docker reason unrelated to this change, document the failure and retry — do **not** silently rerun.

- [ ] **Step 2: Push fixture tag for the release dry-run**

```bash
gh auth switch -u xrf-9527
TAG="v0.0.999"
git tag "$TAG"
git push origin "$TAG"
```

(`v0.0.999` is preferred over `v0.0.0-rc-test-219` because the all-numeric `v*.*.*` glob matches it unambiguously across GitHub's tag-pattern matcher.)

- [ ] **Step 3: Watch the release workflow**

```bash
gh run list --repo xrf-9527/aiops-platform --workflow release.yml --limit 3
gh run watch --repo xrf-9527/aiops-platform <run-id-of-the-just-triggered-run>
```

Expected: workflow succeeds end-to-end. All four platform tarballs uploaded to a draft/published release at `v0.0.999`.

- [ ] **Step 4: Inspect tarball contents**

```bash
mkdir -p /tmp/aiops-release-test && cd /tmp/aiops-release-test
gh release download v0.0.999 --repo xrf-9527/aiops-platform
for f in *.tar.gz; do
  echo "=== $f ==="
  tar -tzf "$f"
done
cd -
rm -rf /tmp/aiops-release-test
```

Expected: each `.tar.gz` lists exactly three files matching `gitea-poller`, `linear-poller`, `worker` (and the containing directory).

- [ ] **Step 5: Cross-platform binary sanity (one platform)**

```bash
mkdir -p /tmp/aiops-release-bin && cd /tmp/aiops-release-bin
gh release download v0.0.999 --repo xrf-9527/aiops-platform --pattern '*linux_amd64*'
tar -xzf *.tar.gz
ls aiops-platform_v0.0.999_linux_amd64/
file aiops-platform_v0.0.999_linux_amd64/worker
cd - && rm -rf /tmp/aiops-release-bin
```

Expected: `file` shows `ELF 64-bit LSB executable, x86-64, ... statically linked`.

- [ ] **Step 6: Comment the release URL on the upstream PR**

```bash
gh auth switch -u xrf9268-hue
gh pr comment <upstream-pr-number> --repo xrf9268-hue/aiops-platform --body "Fork release dry-run on v0.0.999 succeeded end-to-end: https://github.com/xrf-9527/aiops-platform/releases/tag/v0.0.999"
```

---

## Task 6: gh-pr-follow-through on the upstream PR

**Files:** None.

- [ ] **Step 1: Invoke the gh-pr-follow-through skill on the upstream PR number**

Run the skill (`gh-pr-follow-through`) targeting `xrf9268-hue/aiops-platform#<upstream-pr-number>`. The skill drives: live `gh pr checks`, thread-aware review state (`reviewThreads`, not `.comments` — see memory `feedback_codex_polling_via_review_threads.md`), and Codex review handling.

- [ ] **Step 2: Request Codex review**

If no review bot has commented within ~5 minutes, drop `@codex review` on the upstream PR. Wait for the bot review.

- [ ] **Step 3: Address review threads serially**

Per memory `feedback_serial_pr_processing.md`, finish each thread (apply or reject with reason) one at a time; do not batch.

- [ ] **Step 4: Re-run fork CI after any code change**

If the review prompts a fix, push the fix to the same branch — the fork CI PR auto-reruns. If the release-workflow itself changes, re-run the fork release dry-run (Task 5 Steps 2–5) with a fresh tag (e.g. `v0.0.1000`).

---

## Task 7: Merge upstream + clean up

**Files:** None.

- [ ] **Step 1: Merge the upstream PR**

```bash
gh auth switch -u xrf9268-hue
gh pr merge <upstream-pr-number> --repo xrf9268-hue/aiops-platform --squash --delete-branch=false
```

(`--delete-branch=false` because the branch lives on the fork, not on upstream.)

- [ ] **Step 2: Sync fork main from upstream**

```bash
gh api -X POST /repos/xrf-9527/aiops-platform/merge-upstream -f branch=main
```

Expected: `"merge_type": "fast-forward"` or `"merge_type": "merge"`.

- [ ] **Step 3: Close the fork CI PR**

```bash
gh pr close <fork-pr-number> --repo xrf-9527/aiops-platform --comment "Merged upstream; closing fork-internal CI PR."
gh auth switch -u xrf-9527
git push origin --delete "$BRANCH"
gh auth switch -u xrf9268-hue
```

- [ ] **Step 4: Delete the dry-run tag + release**

```bash
gh release delete v0.0.999 --repo xrf-9527/aiops-platform --yes
gh auth switch -u xrf-9527
git push origin --delete v0.0.999
git tag -d v0.0.999
gh auth switch -u xrf9268-hue
```

- [ ] **Step 5: Update local main**

```bash
git checkout main
git pull --ff-only upstream main || git pull --ff-only
git status -sb
```

Expected: `## main...origin/main` clean.

- [ ] **Step 6: Exit worktree**

The parent /next-issue flow controls this; the executing skill should hand back to it. Use `ExitWorktree` with `action: "remove"` once the merge has fully landed.

---

## Self-review checklist

- [x] Every step shows actual commands or actual code, not "do the thing".
- [x] Files paths are exact (`.github/workflows/release.yml`, not "the workflow file").
- [x] No `TBD`, `TODO`, or `add appropriate ...`.
- [x] Spec coverage:
  - Drop `trigger-api` → Task 2 Step 1.
  - Add `gitea-poller` → Task 2 Step 1.
  - Factor into loop → Task 2 Step 1.
  - Dry-run on fixture tag → Task 5 Steps 2–5.
- [x] No mention of changes to `ci.yml`, `Dockerfile`, or new shared-source machinery — matches the design's "intra-`release.yml` only" decision.
- [x] Fork-routing follows memory `project_pr_via_fork.md` exactly: fork self-PR for CI, upstream PR for merge, sync after merge.
