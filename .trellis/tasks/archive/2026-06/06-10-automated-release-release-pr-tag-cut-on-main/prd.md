# Automated release: release PR + tag cut on main

## Goal

为 aiops-platform 补上发版流水线缺失的前半段：自动维护版本号 / CHANGELOG / git tag。
现有 `.github/workflows/release.yml` 已经做好后半段（tag 触发 → 复用 CI 质量门 → 构建
worker/tui 四平台二进制 → SBOM → provenance attestation → GitHub Release），但 tag
目前只能手工打。参考 claude-quota-bar 的 release-plz 方案（Rust 专属），Go 仓库的
对应物是 release-please（Conventional Commits → Release PR → 合并后自动 tag + Release）。

## Research References

* [`research/release-please-official.md`](research/release-please-official.md) —
  release-please-action v5.0.0（2026-04，活跃维护）；本仓库应选 manifest 模式 +
  `release-type: go`（仅 CHANGELOG，无 version.txt）+ `initial-version: 0.1.0` +
  pre-1.0 bump flags；tag-only 模式不存在（tag 是随 Release 创建的）。
* [`research/integration-with-existing-pipeline.md`](research/integration-with-existing-pipeline.md) —
  GITHUB_TOKEN 双重不够用（tag push 不触发 `push: tags`；bot Release PR 的必需检查
  卡在 approval-required）；verdict = GitHub App token；`Validate PR metadata` 必需
  检查会卡死 Release PR，需把 bot login 加进 `exemptAuthorLogins`。

## Decision (ADR-lite)

**Context**: release-please 创建的 tag/PR 必须能（1）触发既有 `release.yml` 的
`push: tags`，（2）让 Release PR 正常跑 CI + PR Metadata 必需检查。默认 GITHUB_TOKEN
两条都不满足（anti-recursion 规则）。

**Decision**: 方案 (b) —— GitHub App installation token
（`actions/create-github-app-token` 每次 run 现铸短期 token），release-please 用它
开 Release PR、cut tag；既有 `release.yml` 触发侧零改动。冲突点只改一处：
`gh release create` → `gh release upload --clobber`（release-please 已创建带真实
changelog notes 的 Release，比现在的 boilerplate notes 更好）。

**Rejected**:
- (e) GITHUB_TOKEN + `gh workflow run` dispatch 桥：可行但 Release PR 每次刷新都要
  人点 "Approve workflows to run"，不是自动发版。
- (a) 把 release.yml 改成 workflow_call 单 run 串联：重写已硬化 workflow 的事件
  plumbing，没有 (b) 给不了的能力。
- (c) PAT：长期个人凭证，被 (b) 严格支配。

**Consequences**: 一次性人工成本 = 创建 GitHub App（contents RW + pull-requests RW +
issues RW）、装到本仓库、存 `APP_ID` + private key 两个 secret。private key 长期但
app/installation 限定、token 每 run 短期，是 GitHub 官方推荐的 PAT 替代。

## Requirements

* push main → release-please（App token）维护 Release PR（版本号 + CHANGELOG.md）。
* 合并 Release PR → release-please 创建 `vX.Y.Z` tag + GitHub Release（changelog
  notes）→ tag push 触发既有 `release.yml` → CI 门 → 构建/SBOM/attest →
  `gh release upload` 补上产物。
* 新增 `release-please-config.json`（manifest 模式）：`release-type: go`、
  `include-component-in-tag: false`（tag 格式 `v0.1.0`，匹配现有 `v*.*.*` 触发器）、
  `initial-version: "0.1.0"`、`bump-minor-pre-major` + `bump-patch-for-minor-pre-major`
  （0.x 阶段 breaking→minor、feat→patch）、changelog-sections 显示
  feat/fix/perf/refactor/chore、`bootstrap-sha` = 落地时的 HEAD（首个 changelog 不
  回灌全部历史）。
* `.release-please-manifest.json` = `{}`（首个 Release PR 即 v0.1.0）。
* `validate-pr-metadata.mjs` `exemptAuthorLogins` 增加 App 的 `<slug>[bot]` login，
  同步更新 `validate-pr-metadata.test.mjs` 与 AGENTS.md 豁免清单句。
* action 全部 SHA pin（`googleapis/release-please-action@45996ed1…` # v5.0.0、
  `actions/create-github-app-token@<sha>`），沿用仓库既有 pin 风格。

## Acceptance Criteria

* [x] 合并一个 feat/fix commit 后，Release PR 自动出现，版本号与 CHANGELOG 正确，
      CI + PR Metadata 检查正常运行（无 approval-required 卡点）。（#736，bootstrap-sha
      生效：CHANGELOG 只含 #735 一条）
* [x] 合并 Release PR → tag `v0.1.0` + GitHub Release 自动创建，且触发 release.yml。
* [x] release.yml 成功向该 Release 上传四平台 tar.gz + SBOM（run 27273593076 绿）。
* [x] `gh api repos/xrf9268-hue/aiops-platform/rulesets` 确认无服务端 tag ruleset
      阻塞 bot tag push（仅 branch 目标的 main merge governance）。
* [x] mutation-verify：从 `exemptAuthorLogins` 删掉 bot login，测试失败（12 pass→1 fail）。

落地记录：App `aiops-platform-release`（ID 4017341）经 manifest 流程创建于
xrf9268-hue 名下，secrets `RELEASE_PLEASE_APP_ID` / `RELEASE_PLEASE_APP_PRIVATE_KEY`
已设置；PR #735（实现）与 #736（v0.1.0 Release PR）均已合并；Docker image build
在 #736 上有一次 Docker Hub 网络超时（与变更无关，重跑通过）。

## Definition of Done

* workflow 改动 actionlint 通过、CI green
* `docs/runbooks/ci.md` 发版章节更新（含 App 创建/轮换 runbook）
* 回滚路径写明：误发 Release → `gh release delete` + tag 删除 + manifest 回退

## Out of Scope

* npm / Homebrew / Docker 镜像分发
* 1.0 之后的 bump 策略调整（届时删 pre-major flags 即可）

## Technical Notes

* 仓库现状：无 `v*` semver tag（仅 `dogfood-baseline-*`）、无 CHANGELOG.md、commit
  严格 Conventional Commits、`aiops-platform/0.x` 是平台版本（≠ codex pin 0.137）。
* `release.yml:200-205` 的 tag-move 校验保留；只替换 `:206-210` 的 create → upload。
* App slug 只有创建后才知道 → 豁免名单的具体 login 在 App 建好后填。
* 发版属仓库 CI/CD 基础设施，不在 SPEC §1 调度边界内，无 DEVIATIONS 影响；但
  PR Metadata 门禁要求 PR body 带 `Closes #N` → 实现 PR 需要先开 GitHub issue。
