---
description: Take a GitHub issue end-to-end — SPEC/upstream investigation, implement with TDD, then ship to a PR ready for handle-pr / gh-pr-follow-through. Manual invoke only.
argument-hint: "[issue-number]"
disable-model-invocation: true
allowed-tools: Bash(git *) Bash(ls *) Bash(grep *) Bash(find *) Bash(go *) Bash(gofmt *) Bash(gh *)
---

# Handle Issue #$ARGUMENTS

本仓库是 OpenAI Symphony SPEC 的 Go 端口，SPEC 对齐是硬要求。本 skill 覆盖 **issue → 开 PR** 阶段；PR 之后交给 `handle-pr`（SPEC 对齐审查轮）+ `gh-pr-follow-through`（盯到 merge-ready）。处理 issue #$ARGUMENTS（xrf9268-hue/aiops-platform）按下面流程。

## Upstream 就位

!`[ -d /tmp/symphony-upstream ] || git clone --depth 1 https://github.com/openai/symphony.git /tmp/symphony-upstream 2>&1 | tail -2`

参考：`/tmp/symphony-upstream/SPEC.md` + `/tmp/symphony-upstream/elixir/lib/symphony_elixir/*.ex`。SPEC 文本歧义时直接 grep Elixir 源码找原算法/分支，不要用 WebFetch 总结。

## 当前 issue + main HEAD

!`gh issue view $ARGUMENTS --json number,title,labels,body --jq '"#\(.number) \(.title)\nlabels: \(.labels|map(.name)|join(", "))\n\n\(.body)"' 2>&1 | head -60`
!`git fetch origin main 2>&1 | tail -1 && git log --oneline origin/main -1 | cat`

## 流程（按序）

### 1. 读 issue
读 labels（`area:spec-alignment` / `priority:pN` / `type:*`）与正文。**把正文的 Acceptance criteria 复选框当作 definition-of-done**——每条都要满足或显式说明为何不在范围内。

### 2. SPEC / upstream 调研（写代码之前）
- 读 SPEC.md 相关章节 + 对应 Elixir 模块（`orchestrator.ex` / `codex/app_server.ex` / `tracker.ex` / `config/schema.ex`），歧义以 Elixir 为准。
- **审查相邻路径**（AGENTS.md「Cross-cutting checklist」item 1）：grep 你要改的 SPEC 概念符号，列出其它 consumer；aiops 扩展（service routing `selectRoutedCandidates`、multi-tracker fan-out、per-state capacity caps、eligibility filter、reconcile hooks）要么也在你的新路径生效，要么写明为何不同。**踩坑实例**：blocked vs running 清理路径只兑现了一半契约。
- **DEVIATIONS.md 决策门**（D1–D30）：本 issue 是关闭既有 deviation、需新开 deviation、还是回退？不要为了让差异消失而新造「deliberate extension」。

### 3. 分支 + 实现
- 从 `main` 开 `fix/<n>-<slug>`（如 `fix/331-active-transition-workspace-cleanup`）。
- **显式补上 Elixir 隐式的 BEAM 保证**（checklist item 2）：followup goroutine 包 `context.WithTimeout`；每个 `go func`/`time.AfterFunc` 上 `defer recoverPanic` 或走 `safeGo`；重置 timer 前先 `Timer.Stop()`。
- 算法对齐 upstream 分支（如终态清理仅在 terminal 转换、引用 `orchestrator.ex` 行号）。

### 4. 测试纪律
- **每个修复配回归测试 + 变异测试**：删掉新代码关键行 → 新测试必须 FAIL；恢复 → PASS。安慰剂测试是最隐蔽的陷阱。
- fire-and-forget followup 的负向断言要有**确定性 barrier**（如再放一个仍处终态的 sibling 让「未发生」可观测）；别把概率性 barrier 写成「race-free」。
- 并发改动跑 `go test -race`。

### 5. 本地门禁（必须，且与 CI 一致）
```bash
gofmt -l $(git ls-files '*.go')          # 必须为空
go mod tidy && git diff --exit-code -- go.mod go.sum
go vet ./... ; go test -race ./... ; go build ./...
```
**暂存只 add 明确路径，绝不 `git add -A` / `git add .`**（会把 `.codex/` 等本地未跟踪文件卷入 PR）。commit 前 `git status --short` 核对只暂存了预期文件。

### 6. 开 PR + 审查环（关键）
1. 开 **一个** PR 对应该 issue，body 引用 issue（`Closes #N`）。治理/文档类改动**单开 PR**，不要塞进 fix PR。
2. **每次 push 都 `@codex review` 并等它收敛**——不是每个 PR 一次，是每个 commit。`gh pr comment <pr> --body "@codex review"` → 轮询 trigger comment 的 reactions 直到 `eyes==0` → 再查 `reviewThreads` 有无新的未解决 actionable thread。本地审查**不能替代**它（GitHub Codex bot 与 stop-time Codex gate 抓到本地 Claude reviewer 漏掉的缺陷类）。
3. **并行本地审查提速**：同时派 Claude general-purpose subagent + Codex（`codex:codex-rescue`）审 `git diff origin/main...HEAD`，盲审、附 severity + verdict。两者抓不同缺陷类。（注：本环境 codex-rescue 沙箱可能被 bwrap/netns 限制；那就以 stop-time gate + GitHub @codex 为 Codex 信号。）
4. 每条 finding 归入 ≥1 类（算法偏差 / 跨模块一致性 / Go runtime hardening / 安慰剂测试），然后修掉或**开 follow-up issue 延后**（标 `area:spec-alignment`，body 含 upstream 行号引用 + acceptance criteria；伞 issue #67）。
5. **审查深度匹配 blast radius**：破坏性/并发路径要穷尽对抗式审查；纯增量序列化改动一轮即可。
6. 收敛后交给 `gh-pr-follow-through` 盯 CI + 线程到 merge-ready。

### 7. 合并
- **必须等用户明确许可**再合并。
- squash + 删分支；commit message 写**最终状态**，不要按轮次罗列。
- 强推统一 `--force-with-lease=<branch>:<known-sha>`。

## 反模式备忘（踩过的坑）
- 把已有的等待（worker-exit `Done`）耦合到异步 cleanup → 假超时 / 砍断 hook。把不变量守在正确的层。
- 确定性共享路径上的 check-then-delete TOCTOU → recheck 关不掉，需单一权威（actor）串行化的锁。
- 设了状态标志却无清除路径 → 瞬时抖动触发误删；标志应每 tick 按当前观测重算。
- 销毁性操作用 live snapshot 重算配置（如 workspace root）而非用创建时捕获的值 → 热重载下静默退化。
- 修复 A 引入缺陷 B（Done-gating → 超时耦合；ConfirmRemove → TOCTOU）：每次「修复」后重新审整条路径。
- 一个契约在姊妹路径上只兑现一半（running vs blocked；reconcile-poll vs §16.5 self-stop）。

## 默认行为
- 中文回复，简洁；每次只汇报变化，不复述。
- worker 永不 push/合并 PR 或写 tracker 状态（D8/#76）。
- Go 版本由 go.mod 锁定，别顺手改 `go` 指令。
