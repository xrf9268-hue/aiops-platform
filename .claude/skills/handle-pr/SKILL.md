---
description: Audit and ship a GitHub PR through SPEC-aligned review rounds against upstream openai/symphony. Manual invoke only.
argument-hint: "[pr-number]"
disable-model-invocation: true
allowed-tools: Bash(git *) Bash(ls *) Bash(grep *) Bash(find *) Bash(go *) Bash(gofmt *) Bash(gh *)
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

2. **修一轮派 subagent 独立审计一轮**：
   - general-purpose agent，背景跑
   - prompt brief 完整（PR head SHA、文件路径、upstream 参考路径、AGENTS.md 政策）
   - **不透露你的结论**
   - 要求 ≤700 字 + severity 标注 + 末尾 `MERGE-READY / NEEDS-CHANGES / BLOCKED` 判决
   - 一般 2-3 轮收敛

3. **Mutation test 验证新测试有效**：删掉新代码的关键行，跑新测试，确认 fail；恢复，确认 pass。安慰剂测试是最隐蔽的陷阱。

4. **Deferred 偏差必须开 issue**：标 `area:spec-alignment`，body 含 upstream 行号引用 + acceptance criteria。AGENTS.md rule 2 要求。决定延后就**当场**告知用户并立即开 issue，别攒到收尾汇报。

5. **Scope 分离**：治理 / 文档改动从 main 开新分支单独 PR，不要塞进 fix PR。

## 默认行为

- 工作分支：系统会告诉你具体名字
- 合并方式：squash（本仓库惯例），commit_message 写最终状态不要按轮次罗列
- 强推统一 `--force-with-lease=<branch>:<known-sha>`
- merge 前必须等用户明确许可；例外：用户给了**按批次、按 scope 的显式授权**时，走 `docs/runbooks/batch-issue-processing.md` 的 opt-in 自动合并流程（全门槛 + hard stops，优先 GitHub 原生 auto-merge），授权不跨批次/scope 沿用
- 批处理多个 PR 时的并行与状态清单纪律见 `docs/runbooks/batch-issue-processing.md`
- 中文回复，简洁；每次只汇报变化不复述
