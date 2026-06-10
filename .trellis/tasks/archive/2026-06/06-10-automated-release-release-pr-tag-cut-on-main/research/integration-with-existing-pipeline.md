# Research: Integrating automated version/CHANGELOG/tag (release-please-style) with the existing tag-triggered release.yml and repo gates

- **Query**: How does an automated Release-PR + tag-cut mechanism wire into the existing `release.yml`, and how do the repo's PR gates treat a bot Release PR?
- **Scope**: mixed (repo inspection + official-docs verification)
- **Date**: 2026-06-10

## Part 1 — Repo facts

### 1.1 `.github/workflows/release.yml`

| Fact | Location |
|---|---|
| Triggers: `push: tags: ["v*.*.*"]` AND `workflow_dispatch` with required `tag` input ("Existing git tag to publish") | `release.yml:5-14` |
| **Already has the dispatch entry point an external trigger needs** — `resolve` job validates `^v[0-9]+\.[0-9]+\.[0-9]+$`, resolves `commit_sha` from the tag | `release.yml:24-75` |
| Reuses CI quality gates via `uses: ./.github/workflows/ci.yml` (workflow_call) with `ref: commit_sha` | `release.yml:77-85` |
| `release` job permissions: `contents: write`, `id-token: write`, `attestations: write` | `release.yml:94-97` |
| Builds dashboard (`npm ci && npm run build`), then worker+tui for 4 platforms (linux/darwin × amd64/arm64) | `release.yml:139-166` |
| Trivy CycloneDX SBOM over `dist/`, tar.gz packaging, `actions/attest-build-provenance` | `release.yml:168-191` |
| Final step: **`gh release create "$RELEASE_TAG" dist/*.tar.gz dist/*_sbom.cdx.json ... --verify-tag`** with boilerplate notes ("Automated aiops-platform release for $TAG"); re-verifies the tag has not moved | `release.yml:193-210` |

Repo currently has **no `v*` tags** (only `dogfood-baseline-20260526-0096c4f`) and **no `CHANGELOG.md`** — release-please's `release-type: go` creates/maintains `CHANGELOG.md` and tracks version via the tag/manifest (no version file required).

### 1.2 PR-metadata gate vs. a bot Release PR

`pr-metadata.yml` runs on `pull_request` `[opened, edited, reopened, synchronize, ready_for_review, converted_to_draft]` (`pr-metadata.yml:16-18`) and `Validate PR metadata` is a **required status check** (`main-ruleset.json:41`). For *every* non-exempt PR, `validate-pr-metadata.mjs` requires:

1. A closing keyword `Closes/Fixes/Resolves #N` in the body (`validate-pr-metadata.mjs:46-47,132-136`) — unconditional, not just for SPEC-sensitive paths.
2. All three `SPEC alignment` checklist options present from the template, exactly one checked (`:138-144`).

A release-please PR body (changelog preview) satisfies neither → **the required check fails and the Release PR can never merge** unless exempted. Release-PR file paths (`CHANGELOG.md`, `.release-please-manifest.json`, `release-please-config.json`, optional version file) do **not** hit the SPEC-sensitive patterns (`internal/workflow/config.go`, new `internal/{orchestrator,worker}/*.go` — `:22-31`), so only the two unconditional requirements bite.

**Exemption mechanism already exists**: `exemptAuthorLogins` set, currently `dependabot[bot]` / `dependabot-preview[bot]` only, deliberately scoped per-login not per-`[bot]` suffix ("a future bot has to be added deliberately" — `:56-66`). **Smallest change**: add the release bot's login (the GitHub App's `<app-slug>[bot]`, or `github-actions[bot]` if using GITHUB_TOKEN — the broader `github-actions[bot]` exemption is less precise) to that set, plus the paired test `validate-pr-metadata.test.mjs:33-37` ("exempts Dependabot logins but no one else") and the AGENTS.md sentence naming the exemption list.

### 1.3 `ci.yml` triggers and required checks on the Release PR

- `ci.yml:5-18`: `push: main`, `pull_request: main`, `workflow_dispatch`, `workflow_call` (with optional `ref` input — **workflow_call plumbing already exists**).
- Required checks on main (`main-ruleset.json:33-42`): `Go build and test`, `Security and supply-chain`, `Validate PR metadata`; `strict_required_status_checks_policy: true` (branch must be up to date — fine: release-please force-updates its PR on every main push).
- A Release PR with base `main` triggers `ci.yml` *as an event*, **but who created the PR determines whether the runs actually start** — see Part 2.

### 1.4 Branch protection / tag push

`main-ruleset.json` targets **branch refs only** (`"target": "branch"`, `ref_name include ~DEFAULT_BRANCH` — `:3-9`): squash-only PRs, review-thread resolution, 0 approvals, no force-push/deletion. **No tag ruleset exists**, so any actor with `contents: write` (including GITHUB_TOKEN or an app token) can push a `v*` tag directly; tags are not gated by PR requirements. Ruleset is applied server-side via `gh api` import (`.github/governance/README.md`), so adding the bot to required-check exemptions does not require ruleset changes — only the mjs edit.

## Part 2 — Verified token/trigger semantics (official docs)

From [GITHUB_TOKEN concepts doc](https://docs.github.com/en/actions/concepts/security/github_token), section "When GITHUB_TOKEN triggers workflow runs" (verified 2026-06-10):

> "events triggered by the GITHUB_TOKEN will not create a new workflow run, with the following exceptions:
> - **workflow_dispatch and repository_dispatch events always create workflow runs.**
> - pull_request events with the opened, synchronize, or reopened activity types: when a workflow using GITHUB_TOKEN creates or updates a pull request, the resulting pull_request event **creates workflow runs in an approval-required state**. The pull request displays a banner in the merge box, and a user with write access ... can start the runs by selecting *Approve workflows to run*."

Consequences for this repo:

- **Tag pushed by GITHUB_TOKEN does NOT trigger `release.yml`'s `push: tags`** (not in the exception list). This is the classic cascade limitation; the [release-please-action README](https://github.com/googleapis/release-please-action#other-actions-on-release-please-prs) repeats it verbatim and recommends a non-default token.
- **GITHUB_TOKEN-created Release PR**: CI/PR-metadata runs are created but stuck in *approval-required* state — a human must click "Approve workflows to run" on every PR update (every push to main refreshes the Release PR). Recurring manual friction that defeats "automated release". (This approval-required behavior is newer than the README's "will not trigger at all" wording; the concepts doc above is current.)
- **`workflow_dispatch` via API/`gh workflow run` with GITHUB_TOKEN ALWAYS creates a run** — explicitly documented exception. Requires `actions: write` on the token ([REST: create a workflow dispatch event](https://docs.github.com/en/rest/actions/workflows#create-a-workflow-dispatch-event)).
- **App-token or PAT events trigger workflows normally** (only the repository's own GITHUB_TOKEN is suppressed/approval-gated).

Reusable-workflow permission rules ([reuse workflows doc](https://docs.github.com/en/actions/how-tos/reuse-automations/reuse-workflows)): the caller job passes its GITHUB_TOKEN; through the chain "permissions can only be maintained or reduced — not elevated". So a caller job *can* grant `contents: write, id-token: write, attestations: write` to a called `release.yml`, and the called workflow's own job-level `permissions:` apply within that ceiling. OIDC (`id-token: write`) in reusable workflows is a documented, supported scenario ([OIDC with reusable workflows](https://docs.github.com/en/actions/security-for-github-actions/security-hardening-your-deployments/using-openid-connect-with-reusable-workflows)), so `attest-build-provenance` works when called via `workflow_call`. Nested chains (release-please.yml → release.yml → ci.yml) are within the allowed depth.

release-please config semantics ([manifest-releaser.md](https://github.com/googleapis/release-please/blob/main/docs/manifest-releaser.md)):

- `skip-github-release: true` — release-please creates **no GitHub Release and therefore no tag**; "Release-Please still requires releases to be tagged, so this option should only be used if you have existing infrastructure to tag these releases" (you must tag yourself or subsequent runs mis-compute).
- `draft: true` — Release created as draft; GitHub's lazy tag creation means **no tag until published**, unless `force-tag-creation: true` is also set.
- Action outputs: `release_created`, `tag_name`, `major`/`minor`/`patch` ([action README outputs](https://github.com/googleapis/release-please-action#outputs)); README's own publish pattern is `if: steps.release.outputs.release_created` then `gh release upload ${{ steps.release.outputs.tag_name }} ...`.
- "Allow GitHub Actions to create and approve pull requests" repo setting is required only when the PR is created with GITHUB_TOKEN; irrelevant with an app token.

## Part 3 — Integration options

### (a) Single-workflow gating: convert release.yml to also accept `workflow_call` + tag input; release-please workflow calls it in the same run

- Feasible: caller job can grant the three write permissions (verified above); ci.yml nesting OK.
- Cost: `release.yml` `resolve` job logic keys off `github.event_name`/`GITHUB_REF_NAME`, which under `workflow_call` reflect the **caller's** event (`push` to `main`) — needs a third input-driven mode; `concurrency` group and `run-name` expressions also need adjustment. Tag must exist before the call → still needs release-please (or a manual step) to have created the tag first in the same job.
- Pros: one run, one place to look. Cons: largest diff to a working, carefully hardened workflow; duplicate-trigger care needed (tag push by app token would *also* fire `push: tags` — with GITHUB_TOKEN-created tags it would not, which is the consistent pairing).

### (b) GitHub App token (`actions/create-github-app-token`) — existing release.yml untouched on the trigger side

- Mint a short-lived (≤1 h) installation token per run from an owned GitHub App (permissions: contents RW, pull-requests RW, issues RW for `autorelease:` labels); pass as `token:` to release-please-action.
- Release PR triggers CI + PR-metadata **normally, no approval clicks**; tag/Release created by release-please **does** fire `release.yml`'s `push: tags` unchanged.
- Remaining conflict: `gh release create` vs release-please's own Release (see Part 4) — one small release.yml edit.
- Cost: one-time app creation + 2 secrets (`APP_ID`, private key). The private key is long-lived but app-scoped/installation-scoped and tokens are per-run — this is GitHub's recommended replacement for PATs ([create-github-app-token](https://github.com/actions/create-github-app-token)).

### (c) PAT — rejected baseline

Works identically to (b) trigger-wise but is a long-lived, user-scoped credential; fine-grained PATs expire and rotate manually; classic PATs over-scope. Strictly dominated by (b).

### (d) `skip-github-release` + manual/dispatch publish

release-please only manages the Release PR; tag + Release left to a human or a separate step. Loses the "tag cut on merge" automation; release-please also needs the tag to exist to compute the next release, so a forgotten manual tag corrupts the next changelog. Only sensible as a degenerate fallback.

### (e) Minimal-change dispatch bridge: release-please with GITHUB_TOKEN, then `gh workflow run release.yml -f tag=...`

- After `release_created`, the same job runs `gh workflow run release.yml -f tag=${{ steps.release.outputs.tag_name }}` — **works with plain GITHUB_TOKEN** (workflow_dispatch is an always-trigger exception; needs `actions: write` on the job). `release.yml` already has exactly this entry point (`release.yml:9-14`); zero edits to its trigger.
- **Fatal friction**: the Release PR itself is GITHUB_TOKEN-created → required checks sit in approval-required state on every update; a human must click "Approve workflows to run" repeatedly. Also still needs the `gh release create` conflict resolved and the bot exemption (author is `github-actions[bot]`, the broadest possible exemption).

## Part 4 — The `gh release create` conflict

release-please's default flow creates the GitHub Release (which creates the tag); `release.yml:206-210` then runs `gh release create --verify-tag` → fails with "release already exists". Resolutions:

1. **Switch `release.yml` to `gh release upload $TAG dist/* --clobber`** (keep the tag-move verification at `:200-205`). Release-please owns the Release and writes real changelog notes into the Release body — strictly better than the current boilerplate "Automated aiops-platform release for $TAG". This is release-please's own documented artifact pattern.
2. release-please `draft: true` + `force-tag-creation: true`, release.yml uploads then `gh release edit --draft=false` — avoids a published-but-artifactless window (~20 min while CI+build run) at the cost of two extra config knobs and a publish step.
3. `skip-github-release: true` + self-tag in the release-please workflow, keeping `gh release create` as-is — keeps boilerplate notes, adds hand-rolled tagging logic; inferior notes, more code.

## Verdict (AGENTS.md principle 7)

**Adopt (b): GitHub App token, with conflict resolution (1).** Concretely:

1. Create a GitHub App (contents RW, pull-requests RW, issues RW), install on the repo; store `RELEASE_APP_ID` + private key secrets.
2. New `release-please.yml` on `push: main`: `actions/create-github-app-token` → `googleapis/release-please-action@v4` (`release-type: go`) with that token. Release PR gets normal CI; merge creates tag + Release with changelog notes; tag push fires the existing `release.yml` untouched.
3. Edit `release.yml` final step: `gh release create ...` → `gh release upload "$RELEASE_TAG" dist/*.tar.gz dist/*_sbom.cdx.json --clobber` (keep `--verify-tag`-equivalent tag-move check already at `:200-205`).
4. Add the app's `<slug>[bot]` login to `exemptAuthorLogins` (`validate-pr-metadata.mjs:62`) + update `validate-pr-metadata.test.mjs:33-37` + the AGENTS.md exemption sentence. No ruleset change needed.

Why not the alternatives: (e) is smaller but leaves a permanent human approve-click on every Release-PR update — not an automated release; (a) rewrites a hardened workflow's event plumbing for no capability (b) doesn't already give; (c) is a worse (b); (d) abandons the goal. If the published-release-before-artifacts window matters later, layer `draft: true` + `force-tag-creation: true` (Part 4 option 2) on top without changing the architecture.

## Sources

- GITHUB_TOKEN trigger semantics: https://docs.github.com/en/actions/concepts/security/github_token ("When GITHUB_TOKEN triggers workflow runs")
- Reusable workflows (permissions maintain/reduce, nesting): https://docs.github.com/en/actions/how-tos/reuse-automations/reuse-workflows
- OIDC in reusable workflows (id-token passthrough): https://docs.github.com/en/actions/security-for-github-actions/security-hardening-your-deployments/using-openid-connect-with-reusable-workflows
- workflow_dispatch REST (actions: write): https://docs.github.com/en/rest/actions/workflows#create-a-workflow-dispatch-event
- release-please-action README (token warning, outputs, `gh release upload` pattern, go release type): https://github.com/googleapis/release-please-action
- release-please manifest config (`draft`, `force-tag-creation`, `skip-github-release`): https://github.com/googleapis/release-please/blob/main/docs/manifest-releaser.md
- App-token action: https://github.com/actions/create-github-app-token

## Caveats / Not Found

- The "approval-required state" behavior for GITHUB_TOKEN-created PRs is the *current* docs wording (verified today); older sources (incl. the release-please README) say such PRs trigger nothing at all. Either way, plain GITHUB_TOKEN is inadequate for unattended Release PRs here.
- The exact bot login for the exemption is `<app-slug>[bot]` and is only knowable after the app is created — wire it as a deliberate single-login addition per the existing comment at `validate-pr-metadata.mjs:56-62`.
- Ruleset is server-side; confirmed only `main-ruleset.json` exists in `.github/governance/` (no tag ruleset committed), but a server-side-only tag ruleset can't be ruled out from the repo alone — verify with `gh api repos/xrf9268-hue/aiops-platform/rulesets` before first tag push.
