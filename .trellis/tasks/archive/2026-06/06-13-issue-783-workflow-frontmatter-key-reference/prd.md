# PRD: issue #783 — docs: complete WORKFLOW.md front-matter key reference

Issue: https://github.com/xrf9268-hue/aiops-platform/issues/783 (P2, docs-only)
方案以 issue 正文为准。证据已于 2026-06-13 现场复核全部成立。

## Problem

README 的 defaults 表（README.md:268-296）刻意只映射 SPEC §6.4 cheat-sheet，
是部分视图。以下活跃配置键只存在于 Go 源码，运维者无处查阅：

- `tracker.team_key` — config.go:148；消费于 internal/runner/tools.go:275
  （linear_graphql current-issue mutation guard 的 workflowStates 查询按 team 限定）。
- `sandbox.network_interface` — config.go:427；validate.go:124-126
  （`sandbox.network=allowlist` 时必填，Firejail --netfilter 挂接的宿主网卡）。
- `codex.turn_timeout_ms`/`read_timeout_ms`/`stall_timeout_ms` — config.go:367-369，
  默认 3600000/5000/300000（config.go:497-499）；运行时语义在
  codex_app_server.go:429-430（turn 墙钟）、codex_app_server_transport.go:154-159
  （单行读预算）、codex_app_server_turn.go:56-62（stall 检测，0 关闭）。

`grep` 复核：这五个键在 README/docs/examples 中唯一命中是历史审计文档
docs/audits/2026-05-15-spec-vs-go-gap-audit.md（快照，不算 operator-facing）。

## Changes

1. 新增 `docs/runbooks/workflow-frontmatter-reference.md`：穷尽式 key reference
   （key、type、default、一行行为、validation 规则），按 config.go 的 12 个
   top-level block 分节；外加加载语义（prompt-only、env 展开 `$VAR`/`${VAR}`
   整值、`~/` 展开）、deprecated alias 表（base_url/poll_interval_ms/
   gitea project_slug/workspace.hooks.*）、removed-keys 表（reject.go 拒绝集）。
2. README defaults 表导语补一句链接到 full reference（cheat-sheet 保持 SPEC
   映射视图不动 — 一概念一来源）。
3. AGENTS.md "Where to read next" 不动（README runbook 链接已覆盖发现路径，
   #784 会处理索引 hygiene；避免两 PR 改同一节）。

## Verdict（原则 7）

- `tracker.statuses.{in_progress,human_review,rework}`：解析+默认但全仓零消费者
  （struct 注释自称 transitional，SPEC §1 把 tracker 写操作划给 agent 侧）。
  Reference 如实记"currently unconsumed"，另开 area:tech-debt follow-up issue
  建议删除，不在本 docs PR 动 config.go（避免触发 PR-metadata SPEC 门禁）。
- claude.* 共享 CommandConfig 的 codex-only 字段（approval_policy 等）：如实记
  "parsed but only consumed by the codex app-server runner; claude.linear_graphql
  is rejected at load"。

## Acceptance criteria（照 issue）

- [ ] docs/runbooks/ 下一页完整 front-matter key reference。
- [ ] README §6.4 cheat-sheet 原样保留并链接 full reference。
- [ ] config.go 每个 YAML key 要么出现在 reference，要么注明 intentionally
      internal（unexported 字段无 YAML tag，不是 key，无需列）。

## Gate

docs-only：gofmt/vet/test 全套照跑（无 Go 变更应全绿）；PR body 带
`Closes #783`；within budget（1 新文件 + README 一行级改动）。
