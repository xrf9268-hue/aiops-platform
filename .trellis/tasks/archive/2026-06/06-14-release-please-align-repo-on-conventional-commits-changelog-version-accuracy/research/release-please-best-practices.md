# Research: release-please best-practice config + Conventional Commits enforcement (single-package Go binary)

- **Query**: Best-practice release-please config + Conventional Commits enforcement for a squash-merge, single-package Go binary repo; compare our config to upstream defaults/dogfood; recommend changelog-sections; pick a PR-title-lint mechanism; define the allowed type list; assess migration risk to existing tags + open release PR.
- **Scope**: mixed (release-please v17.9.0 source at `/tmp/release-please-src`, this repo's config/workflows, upstream GitHub repos via `gh`)
- **Date**: 2026-06-14

## TL;DR verdicts (bring-the-verdict, not a menu)

1. **Hide `chore` and `refactor`.** Upstream's default (`src/util/filter-commits.ts`) and every per-language strategy hide `chore`, `docs`, `style`, `refactor`, `test`, `build`, `ci`. Our config makes `chore` + `refactor` **visible**, which is why the live CHANGELOG is polluted with "archive Trellis task" / "dead-code sweep" entries (see `CHANGELOG.md` `0.1.1`/`0.1.2` "Miscellaneous Chores" blocks and open PR #803). This is the single biggest divergence.
2. **Enforce at the PR-title layer** with `amannn/action-semantic-pull-request` (v6.1.1, commit `48f256284bd46cdaab1048c3721360e808335d50`). Because the repo squash-merges and (must) "Default to PR title for squash merge commits", the squash subject == PR title == what release-please parses. Linting PR titles is the correct and standard layer; commitlint-on-push does not see the squash subject and is the wrong layer here.
3. **Allowed type list**: the canonical Angular/commitizen set — `feat, fix, perf, refactor, docs, style, test, build, ci, chore, revert`. Do **not** add `deps` (not a commitlint default type; see §4).
4. **Migration is safe.** release-please's changelog updater is **prepend-only** — it never rewrites past entries. Changing `changelog-sections`/hiding `chore` affects **future** releases only. The open release PR (0.1.3) is regenerated on the next `push: main` run, so its body re-renders under the new sections automatically; no manual surgery, no retroactive rewrite of v0.1.0–v0.1.2.

---

## 1. Config comparison vs upstream

### Upstream dogfood config (`/tmp/release-please-src/release-please-config.json`)

```json
{
  "$schema": "https://raw.githubusercontent.com/googleapis/release-please/main/schemas/config.json",
  "release-type": "node",
  "include-component-in-tag": false,
  "packages": { ".": { "extra-files": ["src/index.ts"] } }
}
```

Key observation: **upstream ships NO `changelog-sections`** — they rely on the built-in default in `src/util/filter-commits.ts`. That default is the authority for "what's hidden":

```js
// src/util/filter-commits.ts  (DEFAULT_CHANGELOG_SECTIONS)
{type: 'feat',     section: 'Features'},
{type: 'fix',      section: 'Bug Fixes'},
{type: 'perf',     section: 'Performance Improvements'},
{type: 'revert',   section: 'Reverts'},
{type: 'chore',    section: 'Miscellaneous Chores',     hidden: true},
{type: 'docs',     section: 'Documentation',            hidden: true},
{type: 'style',    section: 'Styles',                   hidden: true},
{type: 'refactor', section: 'Code Refactoring',         hidden: true},
{type: 'test',     section: 'Tests',                    hidden: true},
{type: 'build',    section: 'Build System',             hidden: true},
{type: 'ci',       section: 'Continuous Integration',   hidden: true},
```

So upstream's answer to "is `chore` hidden or visible?" → **hidden** (and `refactor` hidden too). Every per-language strategy that overrides the default (`src/strategies/go-yoshi.ts`, `java.ts`, `php.ts`, `r.ts`) keeps `chore`/`refactor`/`docs`/`style`/`test`/`build`/`ci` hidden; only `php.ts` un-hides `chore`. The Go default (`go-yoshi`) hides them all. `filterCommits` semantics: a hidden type is dropped UNLESS the commit is a `BREAKING CHANGE`, in which case it still surfaces (`isBreaking && hiddenSections.includes(commit.type)`).

### Per-key divergence table (our config vs best practice)

| Key | Our value | Best practice | Verdict |
|---|---|---|---|
| `release-type` | `go` | `go` | ✅ keep |
| `include-component-in-tag` | `false` | `false` (single package → tag = `vX.Y.Z`, no `name-vX.Y.Z`) | ✅ keep |
| `bump-minor-pre-major` | `true` | recommended pre-1.0 (breaking → minor, not major) | ✅ keep |
| `bump-patch-for-minor-pre-major` | `true` | recommended pre-1.0 (feat → patch, not minor) | ✅ keep |
| `initial-version` | `0.1.0` | fine; already past it (tags v0.1.0–v0.1.2 exist) | ✅ keep (now inert) |
| `bootstrap-sha` | `0cc8cc4b…` | upstream docs (`customizing.md`) call this "an uncommon use case [that] should generally be avoided"; it only mattered for the very first release and is inert now that releases exist | ⚠️ **drop** — see §5; harmless but no longer earning its keep |
| `changelog-sections.chore` | **visible** ("Miscellaneous Chores") | **hidden** | ❌ **change → hidden** |
| `changelog-sections.refactor` | **visible** ("Code Refactoring") | **hidden** | ❌ **change → hidden** |
| `changelog-sections.style` | **absent** | hidden | ➕ add (hidden) for completeness/parity with default |
| `changelog-sections` (feat/fix/perf/revert) | visible | visible | ✅ keep |
| `changelog-sections` (docs/test/build/ci) | hidden | hidden | ✅ keep |
| `packages` | `{".":{}}` | `{".":{}}` | ✅ keep (single-package) |

### Best-practice keys we are MISSING — adopt or not?

All exist in `schemas/config.json`. Verdicts for a single-package Go binary repo:

| Key | What it does | Adopt? |
|---|---|---|
| `pull-request-title-pattern` | Customizes the Release PR title. Default produces `chore(main): release 0.1.3` (confirmed: open PR #803 title). | **No.** Default is fine and is what the enforcement action must allow-list anyway (`chore` type). Adopting a custom pattern only adds a thing the PR-title linter must special-case. |
| `pull-request-header` | First line of the Release PR body (default: ":robot: I have created a release *beep* *boop*"). | **No.** Cosmetic; not earning its keep. |
| `changelog-host` | Rewrites changelog links to a GH Enterprise host. | **No.** We're on github.com. |
| `commit-search-depth` | How many commits back to scan (default 500). | **No.** Our history since v0.1.2 is tiny; default is ample. |
| `release-search-depth` | How many past releases to consider (default 400). | **No.** 3 tags total. |
| `separate-pull-requests` | One Release PR per component. | **No.** Single package → meaningless. |
| `include-v-in-tag` | Keep `v` prefix in tag (default `true`). | **No.** Default already gives `v0.1.x`, which `release.yml`'s `on: push: tags` expects. Leave at default. |
| `extra-files` | Stamp the version into additional files (e.g. a Go `version.go`, a README badge). | **Maybe** — only if there's a hand-maintained version string outside the tag. The repo already does ldflags version stamping (commit 19e3c8f "version observability — --version, ldflags stamping"), so the binary version comes from the git tag at build time, not a checked-in constant. If no in-tree constant needs syncing, **skip**. Worth a one-line check for a `Version = "..."` literal; if found, add it here so it can't drift. |

Net: the only config changes that matter are **(a) hide `chore` + `refactor`, (b) optionally add `style` hidden for parity, (c) optionally drop the now-inert `bootstrap-sha`.** Everything else is already aligned or correctly omitted.

---

## 2. Recommended `changelog-sections` (concrete JSON)

Canonical titles come from `src/changelog-notes.ts` `DEFAULT_HEADINGS` and `DEFAULT_CHANGELOG_SECTIONS`. Recommended block (visible: feat/fix/perf/revert; hidden: everything else — matches upstream default exactly, so this is "make our explicit config equal the upstream default"):

```json
"changelog-sections": [
  { "type": "feat",     "section": "Features" },
  { "type": "fix",      "section": "Bug Fixes" },
  { "type": "perf",     "section": "Performance Improvements" },
  { "type": "revert",   "section": "Reverts" },
  { "type": "refactor", "section": "Code Refactoring",       "hidden": true },
  { "type": "chore",    "section": "Miscellaneous Chores",   "hidden": true },
  { "type": "docs",     "section": "Documentation",          "hidden": true },
  { "type": "style",    "section": "Styles",                 "hidden": true },
  { "type": "test",     "section": "Tests",                  "hidden": true },
  { "type": "build",    "section": "Build System",           "hidden": true },
  { "type": "ci",       "section": "Continuous Integration", "hidden": true }
]
```

Note: because this is byte-for-byte the upstream default, you could also **delete `changelog-sections` entirely** and inherit the default (that's literally what release-please dogfoods). Keeping it explicit is fine if you want the file to be self-documenting — but if you keep it, keep it equal to the default so it doesn't silently drift on a future release-please upgrade.

Should `chore`/`refactor`/`docs` be hidden? **Yes, all three** — they're hidden in the upstream default and in the Go strategy. Rationale: a CHANGELOG is a user-facing "what changed for me" document; chores (task archival, dependency housekeeping), refactors (no behavior change), and docs don't change the binary's behavior for an operator. Breaking changes of any type still surface via the `isBreaking` override in `filterCommits`.

---

## 3. Enforcing Conventional Commits on a squash-merge repo

### Why PR-title linting is the right layer

This repo squash-merges. With GitHub's repo setting **"Default to PR title for squash merge commits"** enabled, the squash commit *subject* = the PR title. release-please parses commit subjects on `main`. Therefore the one string that must be a valid Conventional Commit is **the PR title**. Linting the PR title validates exactly the artifact release-please will consume.

`commitlint`-on-push (e.g. a `push`/`pre-commit` commitlint hook) lints the *branch* commits, which are thrown away by squash — it does **not** see or constrain the squash subject. It's the wrong layer for a squash-merge repo and would produce false confidence. **Use PR-title linting.**

### Recommended action: `amannn/action-semantic-pull-request`

- Latest release **v6.1.1** (published 2025-08-22), commit **`48f256284bd46cdaab1048c3721360e808335d50`** (lightweight tag → this IS the commit SHA; resolved via `gh api .../git/refs/tags/v6.1.1`).
- README states its purpose verbatim: "ensures that your pull request titles match the Conventional Commits spec." Used by Electron, Vite, Vercel/ncc, Firebase, AWS — well-maintained, widely adopted.
- README installation step 1 literally instructs: configure squash & merge **and tick "Default to PR title for squash merge commits"** — i.e. this action is designed for exactly our setup.
- Default `types` = the commitizen/conventional-commit-types set (feat/fix/docs/style/refactor/perf/test/build/ci/chore/revert), so the default already matches §4. We pass `types` explicitly anyway to pin the contract.

### Concrete workflow (SHA-pinned to this repo's convention)

Pin style matches existing workflows (e.g. `actions/checkout@df4cb1c0…  # v6`). Create `.github/workflows/pr-title-lint.yml`:

```yaml
name: PR Title Lint

# Squash-merge uses the PR title as the squash commit subject (repo setting
# "Default to PR title for squash merge commits"). release-please parses that
# subject, so the PR title is the artifact that must be a Conventional Commit.
on:
  pull_request_target:
    types: [opened, reopened, edited, synchronize]

permissions:
  pull-requests: read

concurrency:
  group: ${{ github.workflow }}-${{ github.event.pull_request.number }}
  cancel-in-progress: true

jobs:
  validate-pr-title:
    name: Validate PR title (Conventional Commits)
    runs-on: ubuntu-latest
    if: github.repository == 'xrf9268-hue/aiops-platform'
    timeout-minutes: 5
    steps:
      - uses: amannn/action-semantic-pull-request@48f256284bd46cdaab1048c3721360e808335d50 # v6.1.1
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
        with:
          types: |
            feat
            fix
            perf
            refactor
            docs
            style
            test
            build
            ci
            chore
            revert
          requireScope: false
          # PR subjects are free-form sentences; don't force a leading-lowercase
          # rule (release-please renders them verbatim). Add a subjectPattern
          # only if a casing convention is later adopted.
```

Notes / interactions:

- **`pull_request_target` + `synchronize`**: the action README and GitHub docs note that if the check is **required for merge**, you must include `synchronize` in the trigger types so the check re-runs on each push (required checks must run on the latest SHA). Included above. If you make this a required check, wire it into `.github/governance/main-ruleset.json` like the other required checks.
- **`pull_request_target` vs `pull_request`**: `pull_request_target` runs with the base-branch config and works for fork PRs (it only needs `pull-requests: read`, no secrets). This matches how the repo already accepts external contributions. Caveat the README calls out: config changes (e.g. editing `types`) only take effect for PRs **after** they land on `main`.
- **Default-to-PR-title interaction**: the action validates the title; the GitHub repo setting is what makes that title *become* the squash subject. Both are needed. If the setting is OFF and a single-commit PR is merged, GitHub may use the branch commit subject instead — the action's legacy `validateSingleCommit`/`validateSingleCommitMatchesPrTitle` options guard that case, but the README explicitly says the repo setting is preferred over those options. **So: enable the repo setting; don't bother with the single-commit options.**
- **Required GitHub repo setting** (Settings → General → Pull Requests): enable "Allow squash merging", set default commit message to **"Pull request title"** (a.k.a. "Default to PR title for squash merge commits"). Reference: GitHub changelog 2022-05-11 "default to PR titles for squash merge commit messages" (linked from the action README).

### Alternative considered

`commitlint` + `wagoid/commitlint-github-action` on `pull_request`: lints branch commits, not the squash subject — wrong layer for squash-merge. Rejected.

---

## 4. Allowed type list (for AGENTS.md)

Canonical Angular/commitizen set (source: `commitizen/conventional-commit-types/index.json`, which is the action's documented default and the conventionalcommits.org reference set). One-line guidance:

| Type | When to use | CHANGELOG |
|---|---|---|
| `feat` | A new user-facing feature / capability. (Triggers a minor bump; here a patch pre-1.0.) | ✅ Features |
| `fix` | A bug fix. (Triggers a patch bump.) | ✅ Bug Fixes |
| `perf` | A code change that improves performance without changing behavior. | ✅ Performance Improvements |
| `revert` | Reverts a previous commit. | ✅ Reverts |
| `refactor` | A code change that neither fixes a bug nor adds a feature (e.g. the file-decomposition PRs #830/#831). | hidden |
| `docs` | Documentation-only changes (README, runbooks, AGENTS.md). | hidden |
| `style` | Formatting / whitespace / non-semantic code style; no behavior change. | hidden |
| `test` | Adding or correcting tests only. | hidden |
| `build` | Build system / external build deps (Dockerfile, go.mod tooling, release tarball packaging). | hidden |
| `ci` | CI config and scripts (`.github/workflows/**`, governance rulesets). | hidden |
| `chore` | Housekeeping that doesn't touch `src`/tests (Trellis task archival, version bumps, misc). | hidden |

`BREAKING CHANGE:` footer or `type!:` marks a breaking change — bumps major (minor pre-1.0 here, given `bump-minor-pre-major`) and surfaces in the changelog even for an otherwise-hidden type.

**Do NOT add `deps`.** It is *not* in the commitlint/Angular default type set, so adding it to the action's `types` list and the docs invites titles the standard tooling elsewhere rejects. release-please *does* recognize `deps` as a `DEFAULT_HEADING` ("Dependencies", see `src/changelog-notes.ts`), but that only matters if you opt into it everywhere. The aligned move is to file dependency updates under `chore(deps): …` or `build(deps): …` (the convention Dependabot/Renovate emit), keeping the type list to the standard 11. Scope (`deps`) carries the intent; type stays standard.

---

## 5. Migration note — existing tags + open release PR

**Will changing `changelog-sections` / hiding `chore` retroactively rewrite past CHANGELOG entries?** → **No.** release-please's changelog updater is strictly **prepend/insert**, never a rewrite. From `src/updaters/changelog.ts` `updateContent`:

```js
const lastEntryIndex = content.search(this.versionHeaderRegex); // first existing version header
const before = content.slice(0, lastEntryIndex);
const after  = content.slice(lastEntryIndex);
return `${before}\n${this.changelogEntry}\n${after}`.trim() + '\n';
```

It splices the newly-rendered entry in front of the existing version blocks and re-emits the rest **verbatim**. It never re-parses or re-renders historical sections. So the "Miscellaneous Chores" blocks already written into `0.1.1`/`0.1.2` stay exactly as they are; only releases cut *after* the config change use the new sections. (To clean up the already-published entries you'd hand-edit `CHANGELOG.md` — a separate, optional cosmetic commit, not something the config change does.)

**Risk to the open release PR (0.1.3, branch `release-please--branches--main`):** Low / self-healing.
- The Release PR body is **regenerated on every `push: main`** run of `release-please.yml` (it force-updates the release branch). After the config change merges, the next push re-renders 0.1.3's notes under the new sections — the current "Miscellaneous Chores" block (the three "archive … Trellis task" / "dead-code sweep" chores in PR #803's body) will simply **drop out**, leaving the `feat` entry. No manual edit of the PR is needed; do not hand-edit the release branch.
- The **version number is unaffected** by `changelog-sections` — section visibility is presentation only; bump computation uses commit *types* (feat/fix/breaking), which are unchanged. 0.1.3 stays 0.1.3.
- Sequencing: merge the config change to `main` *before or together with* the PR-title lint enablement; the next release-please run picks it up. There is no state migration and no tag rewrite.

**`bootstrap-sha` cleanup (optional, low-risk):** now that v0.1.0–v0.1.2 are tagged, release-please walks back to the last released tag, not `bootstrap-sha`. Per `customizing.md`, `bootstrap-sha`/`last-release-sha` are "uncommon … should generally be avoided." Removing it is safe (it's inert once a release exists). Keep it only if you want a documented floor for history scans; otherwise drop it to reduce config surface.

---

## Sources

- release-please v17.9.0 source (vendored at `/tmp/release-please-src`):
  - `src/util/filter-commits.ts` — `DEFAULT_CHANGELOG_SECTIONS` (authoritative hidden/visible defaults) + `filterCommits` breaking-change override.
  - `src/changelog-notes.ts` — `DEFAULT_HEADINGS` (canonical section titles; includes `deps`→"Dependencies").
  - `src/updaters/changelog.ts` — `updateContent` (prepend-only proof for §5).
  - `src/strategies/go-yoshi.ts` — Go strategy keeps chore/refactor/docs/etc hidden.
  - `release-please-config.json` — upstream dogfood config (no `changelog-sections`).
  - `schemas/config.json` — full key list / descriptions for §1 "missing keys".
  - `docs/customizing.md` — `bootstrap-sha`/`last-release-sha` "should generally be avoided"; search-depth defaults.
- `amannn/action-semantic-pull-request` — README (purpose, squash setup instructions, `types`/`requireScope`/`validateSingleCommit` options, `pull_request_target` + `synchronize` required-check note). Latest release **v6.1.1**, commit **`48f256284bd46cdaab1048c3721360e808335d50`** (via `gh release view` + `gh api .../git/refs/tags/v6.1.1`).
- `commitizen/conventional-commit-types/index.json` — the action's default type list + one-line descriptions used in §4.
- conventionalcommits.org v1.0.0 — the spec the action and release-please both target (type/scope/`!`/BREAKING CHANGE semantics).
- GitHub changelog "Default to PR titles for squash merge commit messages" (2022-05-11), linked from the action README — the repo setting that makes PR title == squash subject.
- This repo: `release-please-config.json`, `CHANGELOG.md` (v0.1.1/v0.1.2 chore pollution), `.github/workflows/release-please.yml` (App-token + v5 action pin), `.github/workflows/ci.yml` (SHA-pin convention), open Release PR #803 (title `chore(main): release 0.1.3`).

## Caveats / Not Found

- I did **not** confirm whether an in-tree Go version constant exists that would warrant `extra-files` — the repo uses ldflags stamping (commit 19e3c8f), which suggests the version is injected at build time from the tag, not a checked-in literal. Recommend a quick `grep -rn 'Version *= *"0\.' --include='*.go'` before deciding on `extra-files`; if it returns nothing, skip the key.
- The "open release PR #803" referenced in the task brief: `gh pr view 803` returns the 0.1.3 Release PR body, but `gh pr list --search "release-please in:title" --state open` returned `[]` at query time (title is `chore(main): release 0.1.3`, not matching the search term). The PR exists and is the standard release branch; the number in the brief is correct.
- The v6.1.1 tag is a **lightweight** tag (the `git/tags/{sha}` annotated-tag lookup 404'd), so `48f256284bd46cdaab1048c3721360e808335d50` is the commit SHA directly — correct for pinning. v5.5.3 (commit `0723387faaf9b38adef4775cd42cfd5155ed6017`) is the prior widely-pinned LTS if a v6 incompatibility surfaces, but v6.1.1 is recommended.
