# 完整生命周期测试报告 — release v0.1.0 打包二进制

**日期**: 2026-06-11 · **被测物**: `aiops-platform_v0.1.0_darwin_arm64.tar.gz` 的 `worker`
**环境**: 本机 darwin/arm64（16GB Mac mini）· Gitea 1.26.1-rootless（Docker）· codex-cli 0.137.0（= pin，ChatGPT 登录，gpt-5.5/xhigh）
**目标仓库**: `aiops-bot/web-todo` — Go stdlib + vanilla JS 的 web todo app（零外部依赖）
**配置要点**: `tracker.kind: gitea`，`active_states: [AI Ready, Rework]`，`inactive: [Human Review]`，`terminal: [Done, Canceled]`，`agent.max_concurrent_agents: 1`，codex `turn_sandbox_policy.networkAccess: true`，polling 10s

## 结论

**PASS** — 12 个 issue 全部走完完整生命周期：poll → 派发 → workspace（bare mirror + worktree `ai/<n>`）→ codex agent 实现 → agent 自行 `git push` + Gitea API 开 PR + `gitea_issue_labels` 移交 human-review → 运维 merge + `aiops/done` → 队列排空后 worker 静默（无 §7.1 续跑）。最终产物经独立验证可用。

## Issue → PR 对照（12/12 合并）

| Issue | PR | 备注 |
|---|---|---|
| #1 scaffold | #13 | 首发，4.5 分钟 |
| #2 store | #14 | |
| #3 API list/create | #15 | |
| #4 API patch/delete | #16 | |
| #5 frontend list/add | #17 | **rework ×1**（与 #16 冲突，agent 自行 merge main 解决） |
| #6 frontend toggle/delete | #20 | **stall timeout(5m) → 自动重试**后成功 |
| #7 filter | #22 | **rework ×1**（与 #21 冲突） |
| #8 edit-in-place | #23 | **rework ×1**（与 #22 冲突） |
| #9 CSS/dark mode | #19 | |
| #10 validation | #18 | 首次为陈旧派发被 reconcile 取消（F1/F3），二次成功 |
| #11 due dates | #21 | 同上 |
| #12 README/Makefile | #24 | |

**统计**: `runner_start` 18 次（12 有效 + 3 rework + 1 stall 重试 + 2 陈旧派发浪费）；codex 总消耗 32.18M tokens（输入 31.98M 含缓存 / 输出 194k），agent 运行时长 91.6 分钟；全程墙钟 ~101 分钟（12:02–13:43），串行并发=1。

## 覆盖的生命周期路径

dispatch、依赖阻塞（F1 失效→运维标签门控替代）、reconcile-cancel（×4：2 次纠错 + 2 次 handoff 尾部）、stall-timeout + RetryScheduler 重试、Rework 状态（×3，全部成功解冲突）、agent 侧 PR/标签 handoff、prompt blocked-逃生路径（#11 首轮）、队列排空后静默。

## 最终产物验证（独立 fresh clone）

`gofmt -l` 空 · `go vet ./...` 过 · `go test ./...` 两包全过 · 实测 server：POST/GET/PATCH/DELETE、空标题 400、坏日期 400、未知 id 404、due-date 序列化、JSON 文件持久化、静态页面服务 — 全部正确。

## Findings（详见 findings.md）

| # | 严重度 | 摘要 | 处置 |
|---|---|---|---|
| F1 | **bug** | Gitea 上 `Depends on #N` 完全失效：blocker gate 只认字面 `"Todo"` 状态，Gitea 标签映射产出 `"AI Ready"`；adapter 构建的 `BlockedBy`（含 #677 缓存）在 poller 处永不消费 | 建 issue（area:spec-alignment） |
| F3 | bug? | 槽位释放时从陈旧候选队列派发，不复核 tracker 当前状态；reconcile 兜底纠错但浪费了 2 次完整 agent 启动 | 建 issue（先查 Elixir 上游是否同样行为） |
| F2 | 正向 | reconcile-cancel 在打包二进制上行为正确（强制 refresh 立即生效；常规 tick ~2.5min 内收敛） | — |
| F4 | 观察 | cancel 与 agent `gitea_issue_labels` 调用竞态：标签写入在 cancel 后仍落地；无状态损坏 | 报告记录 |
| F5 | 正向 | Rework 路径 ×3 全部成功（issue body 附 rework note 即可驱动 agent 精准解冲突，不重写实现） | — |
| F6 | 正向 | stall timeout（5m 无事件）→ 自动重试成功，无 stuck state | — |
| F7 | 观察 | 状态 API `completed_total=0`、`agent_handoff_reconcile_stopped_total=0`，但实际 12 次 handoff 均以 reconcile-stop 结束 — 计数器分类未覆盖 Gitea handoff 流的实际终止形态，dashboard 语义易误读 | 报告记录，可并入 F3 issue 讨论 |

## 运维侧工件

`/tmp/aiops-lifecycle-test/`：`workdir/WORKFLOW.md`（含凭证，勿外传）、`worker.log`（全事件流）、`watch.sh`（事件触发器）、`cycle.sh`（merge→done→unblock→refresh）、`unblock.py`（F1 的标签门控 workaround）、`findings.md`、`final-verify/`（验证 clone）。Gitea 容器 `aiops-gitea` 保留现场。
