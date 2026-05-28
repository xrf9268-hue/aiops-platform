---
description: Audit and ship a GitHub PR through SPEC-aligned review rounds against upstream openai/symphony. Manual invoke only.
argument-hint: "[pr-number]"
disable-model-invocation: true
allowed-tools: Bash(git *) Bash(ls *) Bash(grep *) Bash(find *) Bash(go *) Bash(gofmt *) Bash(gh *) Bash(codex *) Bash(claude *) Agent(codex:codex-rescue) Agent(feature-dev:code-reviewer)
---

# Handle PR #$ARGUMENTS

本仓库是 OpenAI Symphony SPEC 的 Go 端口，SPEC 对齐是硬要求。本 skill 覆盖 **已有 PR 的 SPEC 对齐审查轮**。

> **审查 / 合并协议是共享的。** commit-first、pre-push 双 reviewer（含 Codex `codex exec review` 的 flag 互斥与 fallback）、`@codex review` 每 push 收敛、GraphQL review threads 判定、size-gate 三态 + PR-body checklist、合并/auto-merge 门槛与 hard stops、回归+变异测试纪律统一在
> [`docs/runbooks/pr-review-merge-protocol.md`](../../../docs/runbooks/pr-review-merge-protocol.md)。本 skill 只写 **PR 审查阶段差异**，到 push/审查/合并步骤照那份协议执行。

## Upstream 已就位

!`[ -d /tmp/symphony-upstream ] || git clone --depth 1 https://github.com/openai/symphony.git /tmp/symphony-upstream 2>&1 | tail -3`

参考路径：`/tmp/symphony-upstream/SPEC.md` + `/tmp/symphony-upstream/elixir/lib/symphony_elixir/*.ex`。SPEC 文本歧义时直接 grep Elixir 源码找原算法行号，不要用 WebFetch 总结。

## 当前 main HEAD

!`git fetch origin main 2>&1 | tail -1 && git log --oneline origin/main -1`

## PR 审查阶段差异

1. **读 AGENTS.md** 的 "SPEC alignment" 和 "Cross-cutting checklist when porting from the Elixir reference" 两节作为审查清单。**每个 finding 至少归到一类**：
   - 算法级 SPEC 偏差（vs upstream `handle_*` 逐分支比对）
   - 跨模块一致性漏镜像（grep 同概念其他 consumer，列出 aiops-platform 扩展 routing / fan-out / capacity caps / eligibility filter 是否都镜像了）
   - Go 运行时硬化（followup goroutine 是否 `context.WithTimeout` / `defer recoverPanic` / `Timer.Stop()`——Elixir BEAM 隐式给的保证 Go 必须显式补）
   - 测试是否安慰剂（assertion 真的读到新代码改的字段吗？）

2. **修一轮、提交一轮、push 前双审一轮**：按协议 §2–§3 对稳定 head SHA 派 Codex + Claude Code 双 reviewer，HIGH/MEDIUM/Critical 先修再 amend 重审；一般 2–3 轮收敛。每 push 后按协议 §4 跑 `@codex review` 收敛、§5 用 GraphQL 处理 review threads。

3. **Deferred 偏差必须开 issue**：标 `area:spec-alignment`，body 含 upstream 行号引用 + acceptance criteria（AGENTS.md rule 2）。决定延后就**当场**告知用户并立即开 issue，别攒到收尾汇报。

4. **Scope 分离**：治理 / 文档改动从 main 开新分支单独 PR，不要塞进 fix PR。

5. **PR body 是活账本**（协议 §7）：每次重大 push 后更新 head SHA、验收项、验证命令、mutation check、CI、双 reviewer、`@codex review`、thread、size-gate 三态 checklist、deferral 状态，避免 stale body 误导合并判断。

## 默认行为

- 工作分支：系统会告诉你具体名字。
- 合并、force-push、auto-merge 门槛、hard stops：全部按协议 §8。merge 前必须等用户明确许可；批次/scope 显式授权下的 opt-in 自动合并见 [`docs/runbooks/batch-issue-processing.md`](../../../docs/runbooks/batch-issue-processing.md)，授权不跨批次/scope 沿用。
- 中文回复，简洁；每次只汇报变化不复述。
