# Research: release-please official usage for a Go binary repo

- **Query**: release-please 最新官方用法（针对 Go 二进制仓库）— action version/pinning, go vs simple, manifest config, GITHUB_TOKEN limitation, Release creation control, artifact-attachment pattern
- **Scope**: external (official googleapis/release-please + release-please-action sources, verified against live repo APIs and `main` branch source on 2026-06-10)
- **Date**: 2026-06-10

## 1. Latest action version, pinning, maintenance status

Verified live via GitHub API on 2026-06-10:

| Repo | Latest release | Date | Status |
|---|---|---|---|
| `googleapis/release-please-action` | **v5.0.0** | 2026-04-22 | actively maintained |
| `googleapis/release-please` (CLI/library) | **v17.9.0** | 2026-06-09 (released the day before this research) | actively maintained |

- **Not deprecated, not moved (recently).** The only historical move was `google-github-actions/release-please-action` → `googleapis/release-please-action` back in 2023 (v3.7.x era). Current canonical home is the `googleapis` org. Release cadence through 2025–2026 is steady (v4.2.0 2025-03, v4.3.0 2025-08, v4.4.0 2025-10, v4.4.1 2026-04, v5.0.0 2026-04).
- **v5.0.0 breaking change is only the Node runtime**: "upgrade to node24" (#1188). No input/output/config changes vs v4. The README on `main` still shows `@v4` in examples (docs lag), but v5 is the current major.
- **Pinning**: the official README examples use major-tag pinning (`googleapis/release-please-action@v4`). GitHub's security-hardening guidance (and this repo's existing convention of SHA-pinned third-party actions) favors full-SHA pinning:
  - `v5.0.0` tag → commit `45996ed1f6d02564a971a2fa1b5860e934307cf7`
  - i.e. `uses: googleapis/release-please-action@45996ed1f6d02564a971a2fa1b5860e934307cf7 # v5.0.0`

Sources:
- https://github.com/googleapis/release-please-action (README on `main`)
- https://github.com/googleapis/release-please-action/releases/tag/v5.0.0
- https://github.com/googleapis/release-please/releases

## 2. release-type: `go` vs `simple`

From official strategy docs and source (`src/strategies/go.ts`, `src/strategies/simple.ts` on `main`):

| | `go` | `simple` |
|---|---|---|
| Official description | "Go repository, with a CHANGELOG.md" | "A repository with a version.txt and a CHANGELOG.md" |
| Files updated | `CHANGELOG.md` (created if missing). **Optionally** a Go source file via `version-file` config — replaces `const Version = "x.y.z"` (regex in `src/updaters/go/version-go.ts`); `versionFile` defaults to `''` (no version file) | `CHANGELOG.md` (created if missing) **plus** `version.txt` (default `versionFile = 'version.txt'`; `createIfMissing: false`, so the file is expected to exist) |
| go.mod touched? | No. Neither strategy reads or writes `go.mod` | No |

Exact `go` strategy source (current `main`):

```ts
export class Go extends BaseStrategy {
  readonly versionFile: string;
  constructor(options: BaseStrategyOptions) {
    super(options);
    this.versionFile = options.versionFile ?? '';
  }
  // buildUpdates: CHANGELOG.md (createIfMissing: true);
  // if versionFile set: VersionGo updater replaces
  //   /const Version = "x.y.z(-pre)?"/ → const Version = "<new>"
}
```

**Verdict for a binary-only Go module (no registry publish)**: `release-type: go` is the right fit — it is exactly "CHANGELOG.md + tag", with no `version.txt` artifact to maintain. Add `"version-file": "internal/.../version.go"` only if a `const Version = "..."` constant should track releases (the file must already contain that exact `const Version = "x.y.z"` pattern; the updater is a regex replace and does not create the file).

Sources:
- https://github.com/googleapis/release-please/blob/main/docs/customizing.md#strategy-language-types-supported
- https://github.com/googleapis/release-please/blob/main/src/strategies/go.ts
- https://github.com/googleapis/release-please/blob/main/src/strategies/simple.ts
- https://github.com/googleapis/release-please/blob/main/src/updaters/go/version-go.ts

## 3. Simple mode vs manifest mode; recommended single-package setup

Two ways to configure the action (v4/v5):

**(a) `release-type` input only** ("most straight-forward configuration option, but allows for no further customization" — action README):

```yaml
- uses: googleapis/release-please-action@v5
  with:
    release-type: go
```

**(b) Manifest config** — required for ANY advanced option (`bump-minor-pre-major`, `changelog-sections`, `initial-version`, `draft`, …). Since action v4, manifest mode is the default when `release-type` is NOT set; the action reads `release-please-config.json` + `.release-please-manifest.json` from repo root. The manifest docs explicitly say it "supports single artifact workflows just as easily" as monorepos.

```yaml
- uses: googleapis/release-please-action@v5
  with:
    token: ${{ secrets.GITHUB_TOKEN }}   # default; see section 4 for caveats
    # config-file: release-please-config.json        # default path
    # manifest-file: .release-please-manifest.json   # default path
```

Recommended single-package files (all keys verified against `schemas/config.json` on `main`):

`release-please-config.json`:

```json
{
  "$schema": "https://raw.githubusercontent.com/googleapis/release-please/main/schemas/config.json",
  "release-type": "go",
  "include-component-in-tag": false,
  "bump-minor-pre-major": true,
  "bump-patch-for-minor-pre-major": true,
  "initial-version": "0.1.0",
  "changelog-sections": [
    { "type": "feat", "section": "Features" },
    { "type": "fix", "section": "Bug Fixes" },
    { "type": "perf", "section": "Performance Improvements" },
    { "type": "refactor", "section": "Code Refactoring" },
    { "type": "chore", "section": "Miscellaneous Chores" },
    { "type": "revert", "section": "Reverts" },
    { "type": "docs", "section": "Documentation", "hidden": true },
    { "type": "test", "section": "Tests", "hidden": true },
    { "type": "build", "section": "Build System", "hidden": true },
    { "type": "ci", "section": "Continuous Integration", "hidden": true }
  ],
  "packages": {
    ".": {}
  }
}
```

`.release-please-manifest.json` (must exist at the tip of the target branch; empty is fine for first run):

```json
{}
```

Key option semantics (from `schemas/config.json` descriptions + `docs/manifest-releaser.md`):

- **`initial-version`**: "Releases the initial library with a specified version". Source-verified (`src/strategies/base.ts::initialReleaseVersion()`): with no previous release found, release-please proposes `initial-version` if set, else **`1.0.0`**. So for a repo with **no semver tags yet**, set `"initial-version": "0.1.0"` and keep the manifest `{}` → first Release PR is exactly v0.1.0. (Alternative bootstrap: put `{".": "0.1.0"}` in the manifest, which means "0.1.0 was already released" and the first PR proposes the *next* version — not what we want here.)
- **`bootstrap-sha`** (top-level, full SHA): limits how far back the first changelog collects commits; ignored after the first release PR merges. Useful since this repo has hundreds of conventional commits with no tag — without it, the first CHANGELOG entry includes the entire history.
- **`bump-minor-pre-major`**: "Breaking changes only bump semver minor if version < 1.0.0" (default false).
- **`bump-patch-for-minor-pre-major`**: "Feature changes only bump semver patch if version < 1.0.0" (default false).
- **`changelog-sections`**: array of `{type, section, hidden?}`; "Override the Changelog configuration sections". Default (when unset) is the `conventional-changelog-conventionalcommits` preset (source: `src/changelog-notes/default.ts` passes `changelogSections` into `presetFactory`): only `feat`/`fix`/`perf`/`revert` are visible; `docs`, `style`, `chore`, `refactor`, `test`, `build`, `ci` are hidden. To surface `refactor:`/`chore:` commits in the CHANGELOG (this repo's history is refactor-heavy), they must be listed with `hidden` absent/false as above.
- **`include-component-in-tag: false`** → tag format `v<version>` (e.g. `v0.1.0`) instead of `<component>-v<version>`. Matches the existing release.yml trigger `v*.*.*`. (`include-v-in-tag` defaults to `true`.)
- **`include-commit-authors`** (default false): appends author to each changelog entry, optional.

Sources:
- https://github.com/googleapis/release-please/blob/main/docs/manifest-releaser.md (Quick Start, Bootstrapping, Initial Version, configfile reference)
- https://github.com/googleapis/release-please/blob/main/schemas/config.json
- https://github.com/googleapis/release-please/blob/main/src/strategies/base.ts (`initialReleaseVersion`)
- Dogfood example: https://github.com/googleapis/release-please/blob/main/release-please-config.json (single-package, `"packages": {".": {...}}`, `include-component-in-tag: false`)

## 4. GITHUB_TOKEN limitation and official recommendations

Official action README, section "Other Actions on Release Please PRs" (verbatim):

> By default, Release Please uses the built-in `GITHUB_TOKEN` secret. However, all resources created by `release-please` (release tag or release pull request) will not trigger future GitHub actions workflows, and workflows normally triggered by `release.created` events will also not run.

This is GitHub's anti-recursion rule (docs.github.com "Triggering a workflow from a workflow"): events triggered by `GITHUB_TOKEN` do not create new workflow runs — the GitHub docs carve out `workflow_dispatch` and `repository_dispatch` as the exceptions (a workflow CAN dispatch another workflow via the API even with `GITHUB_TOKEN`).

Consequence for this repo: with the default `GITHUB_TOKEN`, the tag release-please pushes will **not** fire the existing `on: push: tags: v*.*.*` release.yml.

What the official docs recommend:

1. **PAT** (the only credential alternative the README documents): "You will want to configure a GitHub Actions secret with a Personal Access Token if you want GitHub Actions CI checks to run on Release Please PRs." The basic-config example passes `token: ${{ secrets.MY_RELEASE_PLEASE_TOKEN }}`. A PAT-created tag DOES trigger tag-push workflows. (GitHub App installation tokens behave the same way and are common practice, but the release-please README itself only mentions PATs.)
2. **Same-workflow follow-up jobs gated on outputs** (the pattern every official example uses — npm publish, artifact upload, major-tag aliasing): keep `GITHUB_TOKEN`, and run the post-release work as later steps/jobs in the *same* workflow run, conditioned on `steps.release.outputs.release_created` — no second workflow trigger needed.
3. Required workflow permissions with `GITHUB_TOKEN`:

```yaml
permissions:
  contents: write
  issues: write
  pull-requests: write
```

   Plus repo setting "Allow GitHub Actions to create and approve pull requests" (Settings > Actions > General) may be required.

Also note: with `GITHUB_TOKEN`, CI checks (PR Metadata, tests, etc.) will **not run on the Release PR itself** — same anti-recursion rule. If branch protection requires those checks, a PAT/App token is effectively mandatory for the Release-PR half too.

### Exact action outputs (v4/v5 README, root component / `path` = `.`)

| output | description |
|---|---|
| `releases_created` | `true` if any release was created |
| `release_created` | `true` if a root-component release was created |
| `tag_name` | tag of the created GitHub release (e.g. `v0.1.0`) |
| `version` / `major` / `minor` / `patch` | semver value and components |
| `sha` | SHA the release was tagged at |
| `upload_url`, `html_url` | from the Create-Release API response |
| `body` | release notes extracted from CHANGELOG.md |
| `prs_created` | `true` if any release PR was created or updated |
| `pr` / `prs` | JSON PullRequest object(s) (unset if none) |
| `paths_released` | JSON array of released paths (`[]` if none) |

Relevant inputs for splitting behavior: `skip-github-release` ("If `true`, do not attempt to create releases. This is useful if splitting release tagging from PR creation.") and `skip-github-pull-request` (inverse).

Sources:
- https://github.com/googleapis/release-please-action#github-credentials (and #outputs, #action-inputs)
- https://docs.github.com/en/actions/using-workflows/triggering-a-workflow#triggering-a-workflow-from-a-workflow

## 5. Does release-please create the GitHub Release? Can it be tag-only or draft?

- **Yes, by default it creates the GitHub Release itself**, published (not draft), with the release body taken from the generated CHANGELOG entry (the `body` output = "Release notes for the current version extracted from the CHANGELOG.md"). Tag creation happens *as part of* creating the Release via the API.
- **Draft**: config `"draft": true` (root or per-package) → "Create the GitHub release in draft mode." Caveat from manifest docs: GitHub does not create the git tag for a draft release until it is published ("lazy tag creation"), which breaks release-please's previous-release lookup; the documented companion option is `"force-tag-creation": true` to create the tag immediately.
- **Tag-only without a Release is NOT supported.** `"skip-github-release": true` skips *both* — schema text: "Skip tagging GitHub releases for this package. Release-Please still requires releases to be tagged, so this option should only be used if you have existing infrastructure to tag these releases." I.e. it assumes something *else* tags; release-please never pushes a bare tag itself.
- Relevance to the existing `release.yml` which runs `gh release create` (line ~206): if release-please creates the Release and the tag, a tag-triggered `gh release create` for the same tag would fail/conflict (release already exists). The official-pattern resolutions are (a) change the existing workflow to `gh release upload`/`gh release edit` against the release-please-created Release, or (b) `skip-github-release: true` + keep external tagging — but (b) contradicts "release-please requires releases to be tagged" unless other infra tags reliably, or (c) drive the existing workflow via its `workflow_dispatch` input (`-f tag=...`) from the release-please job (workflow_dispatch is exempt from the GITHUB_TOKEN anti-recursion rule).
- Note: release-please's generated Release notes are its own changelog rendering (changelog-type `default`); a `github` changelog-type exists that uses GitHub's generate-release-notes API instead.

Sources:
- https://github.com/googleapis/release-please/blob/main/docs/manifest-releaser.md (`draft`, `force-tag-creation`, `skip-github-release` entries)
- https://github.com/googleapis/release-please/blob/main/schemas/config.json (`draft`, `skip-github-release` descriptions)
- https://github.com/googleapis/release-please/blob/main/docs/customizing.md#changelog-types

## 6. Official artifact-attachment pattern

The action README has a dedicated section "Attaching files to the GitHub release" — the official pattern is exactly "job/steps in the same workflow, gated on `release_created`, using `gh release upload`":

```yaml
on:
  push:
    branches:
      - main
name: release-please
jobs:
  release-please:
    runs-on: ubuntu-latest
    steps:
      - uses: googleapis/release-please-action@v4
        id: release
        with:
          release-type: node
      - name: Upload Release Artifact
        if: ${{ steps.release.outputs.release_created }}
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
        run: gh release upload ${{ steps.release.outputs.tag_name }} ./artifact/some-build-artifact.zip
```

(Adapted for a build: insert `actions/checkout` + build steps between the release step and the upload, each with `if: ${{ steps.release.outputs.release_created }}` — this is the structure of the official npm-publication example.) This works with the plain `GITHUB_TOKEN` because nothing depends on the tag push *triggering* another workflow; the upload happens in the same run. For this repo it maps to: build binaries/SBOM/attestation in a follow-up job (`needs: release-please`, `if: needs.release-please.outputs.release_created == 'true'`) and `gh release upload "$tag_name" dist/*` against the release-please-created Release — replacing the current `gh release create`.

Source: https://github.com/googleapis/release-please-action#attaching-files-to-the-github-release

## Caveats / Not Found

- The action README on `main` has not been updated to `@v5` in examples; v5.0.0 exists and its only breaking change is the node24 runtime (matters for old GHES/self-hosted runners, not github.com-hosted).
- The release-please README documents only PAT as the alternative credential; GitHub App tokens (`actions/create-github-app-token`) are widespread community practice but not in the official release-please docs.
- The `workflow_dispatch`/`repository_dispatch` exemption from the GITHUB_TOKEN anti-recursion rule is from GitHub's own docs, not release-please docs; re-verify the exact docs wording if relying on option (c) in section 5.
- Default first-release version `1.0.0` and `initial-version` override were verified against `src/strategies/base.ts` on `main` (release-please v17.9.0), not against a versioned doc page.
- `simple` strategy's `version.txt` updater has `createIfMissing: false` (it silently expects the file to exist); not separately doc-stated, read from source.
