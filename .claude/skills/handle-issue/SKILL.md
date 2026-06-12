---
description: Take a tracked GitHub issue in aiops-platform from triage through SPEC/upstream investigation, TDD implementation, and local gates to an open PR, then hand off to handle-pr / gh-pr-follow-through. Use when starting work on an issue here — "处理下一个 issue", "修 #N", "实现这个 issue", "handle issue 331". Manual invoke only. Do NOT use when a PR already exists (use handle-pr / gh-pr-follow-through) or for trivial changes needing no SPEC/upstream investigation.
argument-hint: "[issue-number]"
disable-model-invocation: true
allowed-tools: Bash(bash .claude/skills/handle-issue/scripts/bootstrap.sh *) Bash(git *) Bash(ls *) Bash(grep *) Bash(find *) Bash(go *) Bash(gofmt *) Bash(gh *) Bash(codex *) Bash(claude *) Agent(codex:codex-rescue) Agent(feature-dev:code-reviewer)
metadata:
  pattern: inversion+pipeline+reviewer
  phase: issue→PR (hands off to handle-pr / gh-pr-follow-through)
---

# Handle Issue #$ARGUMENTS

本仓库是 OpenAI Symphony SPEC 的 Go 端口，SPEC 对齐是硬要求。本 skill 覆盖 **issue → 开 PR** 阶段；PR 之后交给 `handle-pr`（SPEC 对齐审查轮）+ `gh-pr-follow-through`（盯到 merge-ready）。

> **审查 / 合并协议是共享的。** pre-push 双 reviewer、`@codex review` 收敛、GraphQL review threads、size-gate 三态、合并门槛、回归+变异测试纪律统一在
> [`docs/runbooks/pr-review-merge-protocol.md`](../../../docs/runbooks/pr-review-merge-protocol.md)。本 skill 只写 **issue→PR 阶段差异**，到 push/审查/合并步骤直接照那份协议执行，不在此复述。

## 何时用 / 不该用
- **用**：要开始处理本仓库一个已建的 GitHub issue（"处理下一个 issue"、"修 #N"、"实现这个 issue"）。
- **不该用**：PR 已存在 → 用 `handle-pr` + `gh-pr-follow-through`；无需 SPEC/upstream 调研的琐碎改动；非 GitHub 的 tracker。

## 上下文就位（issue 全文 + upstream 镜像 + main HEAD）

!`bash .claude/skills/handle-issue/scripts/bootstrap.sh "$ARGUMENTS" 2>&1`

`scripts/bootstrap.sh`（`set -euo pipefail`，fail-fast）依次做：校验参数为数字 issue 号（坏参数 exit 2，与后续 fetch 失败区分）→ 校验/刷新 SPEC 上游镜像 `/tmp/symphony-upstream`（origin 不是 symphony 就重新 clone；`pull --ff-only`/`clone` 失败即中止，不留陈旧镜像）→ 打印 issue **全文**（pin `--repo`，不截断）→ `git fetch origin main` 后打印 main HEAD（fetch 失败即中止，无管道掩盖状态）。

开工前完整读 issue 正文，尤其逐条 Acceptance criteria。SPEC 文本歧义时直接 grep `/tmp/symphony-upstream/elixir/lib/symphony_elixir/*.ex` 找原算法/分支，不要用 WebFetch 总结。

## 流程（按序）

### 1. 读 issue + 并行 owner 探测（协议 §9 issue-phase 推论）
读 labels（`area:spec-alignment` / `priority:pN` / `type:*`）与正文。**把正文的 Acceptance criteria 复选框当作 definition-of-done**——每条都要满足或显式说明为何不在范围内。

动手前先执行协议 §9 的 **issue-phase 探测**，判定命令与处置路由全部以 [`docs/runbooks/pr-review-merge-protocol.md`](../../../docs/runbooks/pr-review-merge-protocol.md#9-concurrent-sessions-on-one-pr) §9 为唯一来源，照其结果执行。**模式自主选定并通报，不询问。**

### 2. SPEC / upstream 调研（写代码之前）
- 读 SPEC.md 相关章节 + 对应 Elixir 模块（`orchestrator.ex` / `codex/app_server.ex` / `tracker.ex` / `config/schema.ex`），歧义以 Elixir 为准。
- **审查相邻路径**（AGENTS.md「Cross-cutting checklist」item 1）：grep 你要改的 SPEC 概念符号，列出其它 consumer；aiops 扩展（service routing `selectRoutedCandidates`、multi-tracker fan-out、per-state capacity caps、eligibility filter、reconcile hooks）要么也在你的新路径生效，要么写明为何不同。**踩坑实例**：blocked vs running 清理路径只兑现了一半契约。
- **要在 worker/orchestrator 侧新增「消费 agent 产物」的 phase/gate/artifact/config（verify、secret-scan、run-summary、diff policy、push、PR、tracker 写…）前，先 grep Elixir 参考有没有等价物**（AGENTS.md 原则 6）。**upstream 没有 = 过度设计信号，不是功能缺口**：默认删除，别搬进 prompt 也别只记一行 DEVIATIONS。「交接前检查 agent 产物」的归宿是 WORKFLOW prompt（agent 自有、push 前、预防式），不是 worker post-turn phase（push 后只能 flag，且抢跑 D9 reconcile-cancel / §16.5 self-stop——#557 即此）。本仓库已建了又拆 6+ 次（#73/#407、#74、#76、#557/#561）。
- **返工高发区先写短计划再动手**：如果要碰 codex schema / dynamic tools、worker post-turn gates、queue/webhook/PR-push/tracker-write 移除路径等历史上反复返工或删了重做的子系统，先在 worktree 内写清当前上游证据、要保留/删除的不变量、相邻路径 blast radius、测试/变异验证点，再改代码。计划给出技术结论，不把 SPEC 已经能裁定的问题做成多选题。
- **DEVIATIONS.md 决策门**：研究到结论再提（AGENTS.md 原则 7）——别把「关闭既有 / 新开 / 回退」当多选题甩给用户，SPEC + Elixir 参考通常已能定论（最常见结论：upstream 缺失且 SPEC 归在别处的扩展应删除）。别为了让差异消失而新造「deliberate extension」。

### 3. 分支 + 实现
- 分支基底按步骤 1 的 §9 探测结果定；仅当探测结论为"自由开工"时才从 `main` 开 `fix/<n>-<slug>`（如 `fix/331-active-transition-workspace-cleanup`）。
- **显式补上 Elixir 隐式的 BEAM 保证**（checklist item 2）：followup goroutine 包 `context.WithTimeout`；每个 `go func`/`time.AfterFunc` 上 `defer recoverPanic` 或走 `safeGo`；重置 timer 前先 `Timer.Stop()`。
- 算法对齐 upstream 分支（如终态清理仅在 terminal 转换、引用 `orchestrator.ex` 行号）。
- 测试纪律（回归 + 变异 + fire-and-forget 的确定性 barrier + `-race`）见协议 §1；**别把"本地变绿"当成验证了发布物**。

### 4. 本地门禁（必须，CI 主门禁 + go vet）
```bash
gofmt -l $(git ls-files '*.go')          # 必须为空
go mod tidy && git diff --exit-code -- go.mod go.sum
go vet ./...
go test -race -covermode=atomic ./...
go build ./cmd/worker ./cmd/tui
```
**暂存只 add 明确路径，绝不 `git add -A` / `git add .`**（会把 `.codex/` 等本地未跟踪文件卷入 PR）。commit 前 `git status --short` 核对只暂存了预期文件。

### 5. 提交 → 双审 → 开 PR
1. 按协议 §2–§3 commit-first + pre-push 双 reviewer；先执行协议里的 **subagent-first reviewer routing**，具体 reviewer-routing 细节以 [`docs/runbooks/pr-review-merge-protocol.md`](../../../docs/runbooks/pr-review-merge-protocol.md) 为唯一来源。review finding 先按当前 head、issue 计划、SPEC/Elixir 参考和相邻路径验证技术正确性，再修复 / 反证 / 延后。§4 每 push 跑 `@codex review` 收敛，§5 处理 review threads。
2. push 后开 **一个** PR 对应该 issue，body 引用 issue（`Closes #N`），列验收项、验证命令、变异验证、风险/deferral；PR body 是活账本（协议 §7）。
3. **每条 finding 归入 ≥1 类**：算法偏差 / 跨模块一致性 / Go runtime hardening / 安慰剂测试；然后修掉或**开 follow-up issue 延后**（标 `area:spec-alignment`，body 含 upstream 行号引用 + acceptance criteria；伞 issue #67）。
4. **Deferred 偏差必须开 issue**（AGENTS.md rule 2）：决定延后就**当场**告知用户并立即开 issue，别攒到收尾汇报。
5. **Scope 分离**：治理/文档类改动从 main 开新分支**单独 PR**，不要塞进 fix PR。
6. 收敛后交给 `gh-pr-follow-through`（私有 `xrf9268-hue/yy-skills`；云端容器通常没装）盯 CI + 线程到 merge-ready。**该 skill 不可用时就地内联**：`gh pr checks <pr> --watch --fail-fast` 等 CI → GraphQL `reviewThreads` 解决所有未决 actionable thread → 最后一次 PR body 更新后等新的 `PR Metadata` 终态并做 warning audit → merge-ready。期间推了修复就对新 head 重跑协议 §3–§5。

### 6. 合并
按协议 §8：**必须等用户明确许可**；squash + 删分支，commit message 写最终状态；`--force-with-lease=<branch>:<known-sha>`。批次/scope 显式授权下的 opt-in 自动合并见 [`docs/runbooks/batch-issue-processing.md`](../../../docs/runbooks/batch-issue-processing.md)。

## 反模式备忘（踩过的坑）
- **pre-release 别加 back-compat**：别名 / 双发同一数据 / 保留旧 wire 名都是技术债，要单独清理 PR（#338 双发 `last_codex_at`+`last_event_at` → 清理 #342）。SPEC 重命名就全量原子改名，内部 Go 标识符也对齐 SPEC 词汇，注释只解释 why（AGENTS.md「These rules apply to every PR」§1–§5）。仓库内消费者用旧名就在同一 PR 改掉。
- 把已有的等待（worker-exit `Done`）耦合到异步 cleanup → 假超时 / 砍断 hook。把不变量守在正确的层。
- 确定性共享路径上的 check-then-delete TOCTOU → recheck 关不掉，需单一权威（actor）串行化的锁。
- 设了状态标志却无清除路径 → 瞬时抖动触发误删；标志应每 tick 按当前观测重算。
- 销毁性操作用 live snapshot 重算配置（如 workspace root）而非用创建时捕获的值 → 热重载下静默退化。
- 修复 A 引入缺陷 B（Done-gating → 超时耦合；ConfirmRemove → TOCTOU）：每次「修复」后重新审整条路径。
- 一个契约在姊妹路径上只兑现一半（running vs blocked；reconcile-poll vs §16.5 self-stop）。

## 示例（两条真实路径）
**看似简单实则有坑 — #328（PR #338）**：`/api/v1/state` 缺 SPEC §13.7.2 的 `rate_limits` + `last_event_at`。一个 commit、一轮 review 就合了——但它**双发** `last_codex_at`（旧名）+ `last_event_at`（spec 名）当 back-compat，违反 pre-release「无 back-compat shim / 一个概念一个真相源 / 名字对齐 domain」规则，结果要靠清理 PR #342 删掉别名。→ 教训：SPEC 重命名就**全量原子改名**（struct 字段 + JSON tag + dashboard + runbook + test 一次改完），小改动也逃不过这些规则。

**硬路径 — #331（PR #339，多轮级联）**：running issue mid-run 转终态时未清理 workspace（§18.1）。破坏性 + 并发路径，经历 P1 数据丢失竞态 → P2 超时耦合（前一个修复引入的）→ P2 陈旧标志 → TOCTOU → P2 root 不匹配 的级联，每轮 `@codex review` 抓到新缺陷（含修复引入的）。每个修复配回归 + 变异测试；路由模式与 §16.5 self-stop 两处缺口延后到 #340/#341。→ 破坏性/并发路径要穷尽对抗式审查，且**每个 push 都重新 @codex review**。

## 验证（完成判定）
- 本地门禁全绿（§4）；新测试经变异验证（协议 §1）。
- 每个已 push 的 head 过了 pre-push 双 reviewer + fresh `@codex review` 收敛 + GraphQL threads 无未决 actionable thread（协议 §3–§5）。
- CI 全绿：`gh pr checks <pr> --watch --fail-fast` 阻塞到完成。
- issue 的每条 Acceptance criteria 满足或显式延后到 tracked issue。
- 用户明确许可后才合并（协议 §8）。

## 默认行为
- **自主优先**：协议 §8 hard stops（以协议原文清单为唯一来源，不在此枚举）之外的决策——SPEC/upstream 裁定、finding 修复/反证/延后、分支与增量取舍、body/thread 维护——自主做并简要通报，不向用户提问；证据已能定论的不做多选题（AGENTS.md 原则 7）。
- 中文回复，简洁；每次只汇报变化，不复述。
- Claude Code 若同一工具动作连续 2 次 malformed / stall / 中断，停止第 3 次原地重试，升级给 `codex:codex-rescue`，附当前 head、目标动作、失败 transcript、已验证事实；这是运行时故障，不是技术 finding 通过或失败。
- worker 永不 push/合并 PR 或写 tracker 状态（D8/#76）。
- Go 版本由 go.mod 锁定，别顺手改 `go` 指令。
- **批处理多个 issue 时**（`/goal` over a set）：并行/串行依赖判定、deferral 时机、live ledger、pause/resume、opt-in 授权合并等批处理纪律全部见 [`docs/runbooks/batch-issue-processing.md`](../../../docs/runbooks/batch-issue-processing.md)，不在此复述（与本 skill 顶部「不复述共享协议」一致，避免内联规则与 runbook 漂移）。
