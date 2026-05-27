---
description: Audit and ship a GitHub PR through SPEC-aligned review rounds against upstream openai/symphony. Manual invoke only.
argument-hint: "[pr-number]"
disable-model-invocation: true
allowed-tools: Bash(git *) Bash(ls *) Bash(grep *) Bash(find *) Bash(go *) Bash(gofmt *) Bash(gh *) Bash(codex *) Bash(claude *) Agent(codex:codex-rescue) Agent(feature-dev:code-reviewer)
---

# Handle PR #$ARGUMENTS

本仓库是 OpenAI Symphony SPEC 的 Go 端口，SPEC 对齐是硬要求。处理 PR #$ARGUMENTS（xrf9268-hue/aiops-platform）按以下流程。

## Upstream 已就位

!`[ -d /tmp/symphony-upstream ] || git clone --depth 1 https://github.com/openai/symphony.git /tmp/symphony-upstream 2>&1 | tail -3`

参考路径：`/tmp/symphony-upstream/SPEC.md` + `/tmp/symphony-upstream/elixir/lib/symphony_elixir/*.ex`。SPEC 文本歧义时直接 grep Elixir 源码找原算法行号，不要用 WebFetch 总结。

## 当前 main HEAD

!`git fetch origin main 2>&1 | tail -1 && git log --oneline origin/main -1`

## 必做的几件不常规的事

1. **读 AGENTS.md** 的 "SPEC alignment" 和 "Cross-cutting checklist when porting from the Elixir reference" 两节作为审查清单。每个 finding 至少归到一类：
   - 算法级 SPEC 偏差（vs upstream `handle_*` 逐分支比对）
   - 跨模块一致性漏镜像（grep 同概念其他 consumer，列出 aiops-platform 扩展 routing / fan-out / capacity caps / eligibility filter 是否都镜像了）
   - Go 运行时硬化（followup goroutine 是否 `context.WithTimeout` / `defer recoverPanic` / `Timer.Stop()`——Elixir BEAM 隐式给的保证 Go 必须显式补）
   - 测试是否安慰剂（assertion 真的读到新代码改的字段吗？）

2. **修一轮、提交一轮、push 前双审一轮**：
   - 先 `git fetch origin main`，再 commit/amend；审稳定 head SHA，不审未提交工作区或陈旧 base
   - 同时派 Codex reviewer + Claude Code reviewer，均只看 `git diff origin/main...HEAD`
   - **Codex reviewer 选哪条路径**：主 agent 是 Claude Code 时，**首选** [codex-plugin-cc](https://github.com/openai/codex-plugin-cc) 提供的 `codex:codex-rescue` subagent（通过 `Agent` 工具 `subagent_type: "codex:codex-rescue"` 调用，prompt 里完整给出 PR head SHA / 改动文件 / SPEC/upstream 引用 / AGENTS.md 政策 / 验收项），它封装了 `codex exec review` 的 CLI flag 互斥（`--base`/`--commit`/`PROMPT` 三者两两互斥，详见 `codex exec review --help`）并通过 `codex:codex-result-handling` 结构化输出。**仅当**主 agent 是 codex 自身或插件不可用时，回退到 bash `codex exec review --base origin/main --title "..."` — 注意 review 子命令不接受自定义 PROMPT，跑的是 codex 默认 review heuristic（generic correctness/style 审查），repo-specific 上下文（issue 意图、SPEC 引用、验收项）丢失。若 fallback 路径仍需传 repo-specific prompt，改用普通 `codex exec` 子命令（自由 PROMPT，把 diff 作为 `<stdin>` 块），代价是失去 review 子命令的内置 review 结构
   - Claude Code reviewer 用 `Agent` 工具 `subagent_type: "feature-dev:code-reviewer"`（首选），或在受限环境用 hardened `claude -p`：必须接收 stdin diff，并加 `--permission-mode bypassPermissions --no-session-persistence --tools "" --max-turns 2` 等约束，避免读取可变工作区或继承会话
   - prompt brief 完整（PR head SHA、文件路径、upstream 参考路径、AGENTS.md 政策、验收项）
   - **不透露你的结论**
   - 要求 ≤700 字 + severity 标注 + 末尾 `MERGE-READY / NEEDS-CHANGES / BLOCKED` 判决
   - HIGH/MEDIUM/Critical 先修复、amend、重跑本地门禁和双审；只有用户在 push 前明确签核接受风险时才可保留未修复项。LOW/P3 若小而真实就修，否则在 PR body 记录不阻塞理由
   - reviewer 失败不算通过；先改用更小的 diff-only prompt 重试。若某一路 reviewer 持续不可用，必须拿到同族等价 diff-only 审查通过或 push 前人工签核，并先记在 review notes，PR 存在后写入 PR body，不能只靠单 reviewer push
   - 一般 2-3 轮收敛

3. **Mutation test 验证新测试有效**：删掉新代码的关键行，跑新测试，确认 fail；恢复，确认 pass。安慰剂测试是最隐蔽的陷阱。

4. **Deferred 偏差必须开 issue**：标 `area:spec-alignment`，body 含 upstream 行号引用 + acceptance criteria。AGENTS.md rule 2 要求。决定延后就**当场**告知用户并立即开 issue，别攒到收尾汇报。

5. **Scope 分离**：治理 / 文档改动从 main 开新分支单独 PR，不要塞进 fix PR。

6. **Fresh `@codex review` 是 post-push gate**：每次 push 后在 PR 评论
   `@codex review`，记录 trigger comment id，等 👀 先出现再消失，并要求
   Codex 在该 head 贴 review/comment 或给 trigger 👍。`latestReviews` 可能
   滞后，不能单独作为新 head clean 的证据。

7. **Review threads 用 GraphQL 判定**：flat comments 不够。只在以下条件之一
   成立时 resolve thread：
   - 代码确实修复了该 thread 的问题
   - thread 已 outdated，且 fresh Codex trigger clean
   - 用户明确要求处理该 PR，fresh trigger 已完成，且 thread-aware recheck 确认该 thread 不再 actionable
   合并前必须零 unresolved、non-outdated actionable thread。

8. **Size gate 是 merge gate，不是质量上限，更不是 LOC 削减强制**：
   correctness / safety / performance / coverage 修复带来的 LOC 增量优先于
   预算合规。**严禁为了压 LOC 删测试、弱化 state-machine 覆盖、跳过 race 覆盖
   或留下已知缺陷**。把每个 PR 准确归入三态之一，并在 body 写明：
   - `within budget`：diff 在预算内。
   - `size-gated: justified overage`：因 correctness / safety / performance / coverage
     修复超出预算且无法拆分成原子单元。**禁止自动合并**，body 写清超限原因、head SHA、
     验证、CI、bot review、thread 状态和剩余风险，等人工 size-gate 签核后再合。
   - `size-gated: split recommended`：因 scope creep / 无关清理 / 实际可拆分的
     关注点超出预算。**Hard stop**——拆成多个小 PR，不要拿签核当替代。

9. **PR body 是活账本**：每次重大 push 后更新 head SHA、验收项、验证命令、
   mutation check、CI、双 reviewer、`@codex review`、thread、size-gate
   classification（必须三态之一；超限时附说明）、deferral/follow-up 状态，
   避免 stale body 误导后续合并判断。PR body 必带一行 size-gate checklist：

   ```markdown
   ### Size gate
   - [ ] `within budget` — diff fits effective `policy.max_changed_files` /
         `max_changed_loc`
   - [ ] `size-gated: justified overage` — overage rationale: <why correctness/
         coverage/safety/perf justifies the extra LOC; not auto-mergeable; needs
         human size-gate sign-off>
   - [ ] `size-gated: split recommended` — split rationale: <which concerns
         to split into separate PRs; hard stop, do not seek sign-off in lieu
         of splitting>
   ```

## 默认行为

- 工作分支：系统会告诉你具体名字
- 合并方式：squash（本仓库惯例），commit_message 写最终状态不要按轮次罗列
- 强推统一 `--force-with-lease=<branch>:<known-sha>`
- merge 前必须等用户明确许可；例外：用户给了**按批次、按 scope 的显式授权**时，走 `docs/runbooks/batch-issue-processing.md` 的 opt-in 自动合并流程（全门槛 + hard stops）。授权不跨批次/scope 沿用
- 即便有 auto-merge 授权，也只有在本地门禁、CI、fresh `@codex review`、GraphQL threads、size gate、branch protection 全部满足时才可合并；启用 GitHub 原生 auto-merge 前必须先确认这些非 check gates 已经 clean。size gate 必须分态处理：只有 `within budget` 可走 auto-merge；`size-gated: justified overage` 必须等人工 size-gate 签核（不可 auto-merge）；`size-gated: split recommended` 是 hard stop，必须拆 PR，不要拿签核当替代
- 批处理多个 PR 时的并行与状态清单纪律见 `docs/runbooks/batch-issue-processing.md`
- 中文回复，简洁；每次只汇报变化不复述
