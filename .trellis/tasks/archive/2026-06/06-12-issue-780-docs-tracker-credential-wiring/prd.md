# PRD: issue #780 — docs: fix Gitea/GitHub tracker credential wiring

Issue: https://github.com/xrf9268-hue/aiops-platform/issues/780 (P0, docs-only)
方案以 issue 正文为准，不重新发明。证据已于 2026-06-12 现场复核全部成立。

## Problem

README quick start 让 Gitea/GitHub 用户裸 `export GITEA_TOKEN` / `GITHUB_TOKEN`，
但 worker 从不直接读这些 env var —— token 只通过 `WORKFLOW.md` 里显式的
`tracker.api_key: $VAR` 引用流入（`internal/workflow/expand.go` →
`internal/workflow/env.go`，整值 `$VAR`/`${VAR}` 才展开）。
`examples/gitea-WORKFLOW.md` 完全没有 `api_key` 键。照 README 走的新用户得到一个
每次 poll 都失败的 worker。

## Verified evidence (2026-06-12 复核)

- `README.md` quick start（~L80-96）：`export GITEA_BASE_URL/GITEA_TOKEN/GITHUB_TOKEN`，
  未提 `tracker.api_key`；`README.md:141` 自宣 canonical，压过已正确的 local-dev.md。
- `examples/gitea-WORKFLOW.md`：tracker 块无 `api_key`；对照
  `examples/reviewer-WORKflow.md`（`api_key: $GITEA_TOKEN` + 注释）与
  `examples/github-local-WORKFLOW.md`（`api_key: $GITHUB_TOKEN`）。
- `docs/runbooks/local-dev.md` L77-99：已有正确的 per-tracker `api_key` 映射表，
  且显式写着 "`examples/gitea-WORKFLOW.md` omits it and must be edited" ——
  修好示例后这句话变 stale，须同 PR 更新（clean-code 规则 10 扫 class）。
- `docs/day1-runbook.md`（97 行）：引导 Gitea 用户用 compose 启动，而
  `deploy/docker-compose.yml:22` 硬编码 `AIOPS_WORKFLOW_PATH: /app/examples/WORKFLOW.md`
  （Linear 示例）——端到端断裂；有效内容（/livez、/api/v1/state、workspace 检查）
  已被 README + local-dev.md 覆盖。全仓唯一引用是历史设计文档
  `docs/superpowers/specs/2026-05-09-gitea-mock-loop-validation-design.md:14`（日期快照）。

## Verdict（原则 7：带结论不带菜单）

`docs/day1-runbook.md` **删除**（acceptance 给了 "fixed or deleted" 两选；
删除依据：clean-code 规则 3 一概念一来源 + 内容全被覆盖 + 接线已断）。

## Changes

1. `examples/gitea-WORKFLOW.md`：tracker 块加 `api_key: $GITEA_TOKEN`
   （带 reviewer 示例同款"worker 持有、不进 agent env"注释，保持示例间一致）。
2. `README.md` quick start：明确 token 经 `WORKFLOW.md` 的 `tracker.api_key: $VAR`
   流入，给出 per-tracker 映射（linear→`$LINEAR_API_KEY`、gitea→`$GITEA_TOKEN`、
   github→`$GITHUB_TOKEN`），不再暗示裸 export 即生效；`tracker.endpoint` 同理
   支持 `$VAR`（expand.go 已确认）。
3. `docs/day1-runbook.md` 删除；历史设计文档中的引用保持原样（dated snapshot，
   描述当时状态，不是活链接义务）——若 CI 有 md link checker 再调整。
4. `docs/runbooks/local-dev.md`：更新 "gitea 示例缺 api_key" 的 stale 句子。

## Acceptance criteria（来自 issue）

- [ ] `examples/gitea-WORKFLOW.md` sets `api_key: $GITEA_TOKEN`.
- [ ] README quick start 指引 Gitea/GitHub 用户在 WORKFLOW.md 设
      `tracker.api_key`（映射表或指向 per-tracker 示例），不再暗示裸 export 足够。
- [ ] `docs/day1-runbook.md` fixed to current contract or deleted →（删除）。
- [ ] 不新增 worker 侧 env-var fallback（不触碰 Go 代码）。

## Constraints

- 纯 docs/example 变更，within budget。
- 一 issue 一分支一 PR：`fix/780-docs-tracker-credential-wiring`，PR body 带 `Closes #780`。
