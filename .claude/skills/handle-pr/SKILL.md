---
description: Audit and ship a GitHub PR through SPEC-aligned review rounds against upstream openai/symphony. Manual invoke only.
argument-hint: "[pr-number]"
disable-model-invocation: true
allowed-tools: Bash(git *) Bash(ls *) Bash(grep *) Bash(find *) Bash(go *) Bash(gofmt *) Bash(gh *) Bash(codex *) Bash(claude *) Agent(codex:codex-rescue) Agent(feature-dev:code-reviewer)
---

# Handle PR #$ARGUMENTS

本仓库是 OpenAI Symphony SPEC 的 Go 端口，SPEC 对齐是硬要求。本 skill 覆盖 **已有 PR 的 SPEC 对齐审查轮**。

> **审查 / 合并协议是共享的。** commit-first、pre-push 双 reviewer、`@codex review` 每 push 收敛、GraphQL review threads 判定、size-gate 三态 + PR-body checklist、合并/auto-merge 门槛与 hard stops、回归+变异测试纪律统一在
> [`docs/runbooks/pr-review-merge-protocol.md`](../../../docs/runbooks/pr-review-merge-protocol.md)。本 skill 只写 **PR 审查阶段差异**，到 push/审查/合并步骤照那份协议执行。

## Upstream 已就位

!`[ -d /tmp/symphony-upstream ] || git clone --depth 1 https://github.com/openai/symphony.git /tmp/symphony-upstream 2>&1 | tail -3`

参考路径：`/tmp/symphony-upstream/SPEC.md` + `/tmp/symphony-upstream/elixir/lib/symphony_elixir/*.ex`。SPEC 文本歧义时直接 grep Elixir 源码找原算法行号，不要用 WebFetch 总结。

## 当前 main HEAD

!`git fetch origin main 2>&1 | tail -1 && git log --oneline origin/main -1`

## PR 审查阶段差异

0. **并行 owner 探测（开始时 + 每次 push 前）**：按协议 §9 探测活跃 owner；判定为 owned 即进入 §9 的 increment-only 协作模式，信号清单、观察窗、接管判据、push 被拒处理、触发去重全部以 [`docs/runbooks/pr-review-merge-protocol.md`](../../../docs/runbooks/pr-review-merge-protocol.md#9-concurrent-sessions-on-one-pr) §9 为唯一来源，不在此复述。**模式自主选定并通报用户，不询问**。

1. **读 AGENTS.md** 的 "SPEC alignment" 和 "Cross-cutting checklist when porting from the Elixir reference" 两节作为审查清单。**每个 finding 至少归到一类**：
   - 算法级 SPEC 偏差（vs upstream `handle_*` 逐分支比对）
   - 跨模块一致性漏镜像（grep 同概念其他 consumer，列出 aiops-platform 扩展 routing / fan-out / capacity caps / eligibility filter 是否都镜像了）
   - Go 运行时硬化（followup goroutine 是否 `context.WithTimeout` / `defer recoverPanic` / `Timer.Stop()`——Elixir BEAM 隐式给的保证 Go 必须显式补）
   - 测试是否安慰剂（assertion 真的读到新代码改的字段吗？）
   - 错位机制（在 §1 scheduler/runner 边界的错误一侧）：worker/orchestrator 侧「消费 agent 产物」的 phase/gate/artifact/config，先 grep Elixir 有没有等价物——没有就是过度设计信号，判**删除**（非搬进 prompt、非只记 DEVIATIONS），归宿默认是 WORKFLOW prompt（worker post-turn phase 在 push 后只能 flag 不能 prevent，且抢跑 D9 reconcile-cancel / §16.5 self-stop——#557 即此；AGENTS.md 原则 6；#557/#561 拆的就是这类）

2. **修一轮、提交一轮、push 前双审一轮**：按协议 §2–§3 对稳定 head SHA 派 Codex + Claude Code 双 reviewer，并先执行协议里的 **subagent-first reviewer routing**；具体 reviewer-routing 细节以 [`docs/runbooks/pr-review-merge-protocol.md`](../../../docs/runbooks/pr-review-merge-protocol.md) 为唯一来源。HIGH/MEDIUM/Critical 先验证技术正确性，再决定修复 / 反证 / 延后；不要盲从，也不要把 finding 当噪音略过。若 finding 证明计划本身错了，同轮更新代码、测试、SPEC/deviation 文档和 PR body。每 push 后按协议 §4 跑 `@codex review` 收敛、§5 用 GraphQL 处理 review threads。

3. **Deferred 偏差必须开 issue**：标 `area:spec-alignment`，body 含 upstream 行号引用 + acceptance criteria（AGENTS.md rule 2）。决定延后就**当场**告知用户并立即开 issue，别攒到收尾汇报。

4. **Scope 分离**：治理 / 文档改动从 main 开新分支单独 PR，不要塞进 fix PR。

5. **PR body 是活账本**（协议 §7）：每次重大 push 后更新 head SHA、验收项、验证命令、mutation check、CI、双 reviewer、`@codex review`、thread、size-gate 三态 checklist、deferral 状态，避免 stale body 误导合并判断。最后一次 body 更新后重新等 `PR Metadata` 终态；`@codex review` 的 clean issue comment 只作为人工可审计信号，自动化不可解析自然语言。

## 默认行为

- 工作分支：系统会告诉你具体名字。
- Claude Code 若同一工具动作连续 2 次 malformed / stall / 中断，停止第 3 次原地重试，升级给 `codex:codex-rescue`，附 PR 号、当前 head/base、目标动作、失败 transcript、已验证事实；这是运行时故障，不是 review finding 的技术裁定。
- 合并、force-push、auto-merge 门槛、hard stops：全部按协议 §8。merge 前必须等用户明确许可；批次/scope 显式授权下的 opt-in 自动合并见 [`docs/runbooks/batch-issue-processing.md`](../../../docs/runbooks/batch-issue-processing.md)，授权不跨批次/scope 沿用。
- **自主优先**：协议 §8 hard stops（以协议原文清单为唯一来源，不在此枚举）之外的决策——finding 修复/反证/延后、reviewer 调度、rebase/增量取舍、body/thread 维护——自主做并简要通报，不向用户提问；finding 证据已能定论的不做多选题。
- 中文回复，简洁；每次只汇报变化不复述。
