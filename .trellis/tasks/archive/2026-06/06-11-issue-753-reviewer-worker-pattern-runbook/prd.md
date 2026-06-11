# Issue #753: Docs: reviewer-worker pattern — run a fresh-context checker agent on "Human Review" (behavior follow-up to #744)

(PRD = issue body)

## 背景

#744（Finishing-phase 独立 verifier gate）以 not planned 关闭，机制层结论经独立复核成立：worker 不拥有 PR 创建（#76，到 Finishing 时 PR 已存在）；worker post-turn gate 与 reconcile-cancel 竞态且只能事后 flag（#557 实测 2/3 概率被跳过，D33 五个同类 gate 已全部删除）；上游（HEAD `4cbe3a9`）无 verifier/rubric/grader 等价物。**本 issue 不重提该机制。**

但 #744 关闭评论的替代路径二——"把 `aiops/human-review` 路由给 reviewer agent run"——只有断言没有落地物：

- "Service routing" 措辞已过期：`services[]` + `selectRoutedCandidates` 在 #573（D25）移除，现状的准确形态是 D25 自己写的 "Replaceable by running one process per service"。
- 实际部署需要**第二个 worker 进程 + 独立的 reviewer `WORKFLOW.md`**（#72 后单 worker 单 workflow 源），目前没有任何 runbook 或示例。

maker/checker 拆分在状态机层已存在：maker 最远只能到 `Human Review`（pending 态），`Done` 由 checker 签发；v0.1.0 生命周期实测（12/12 合并）Rework 循环 ×3 走通——checker 是人。本 issue 把 **checker 是 fresh-context agent** 的变体文档化为部署模式——纯 docs，零平台代码。

## 权威依据（advisory，per AGENTS.md practitioner-accounts 条款）

- Addy Osmani《Loop Engineering.》（2026-06-08）：maker/checker 拆分应用于停止条件本身（"/goal … a fresh model decides if the loop is done instead of the one that did the work"）；"The model that wrote the code is way too nice grading its own homework"。本地镜像 `~/projects/ai-agent-engineering-notes/docs/01_Agent_Design_and_Architecture/10_loop-engineering.md:64`、`:90`。
- Lance Martin《Designing loops with Fable 5》（2026-06-09）：self-critique 不可靠，独立上下文 verifier 更优（"grading is done in an independent context window"）；grader 在允许停止前确认 rubric 达标。本地镜像同目录 `11_designing-loops-with-fable-5.md:34-40`。
- 注意两篇原文里 verifier 都门控**停止条件**（handoff 之前/之上），没有任何"编排器事后截 artifact"的形状——支持状态机层方案，反对 #744 原机制。

## 交付物

1. `docs/runbooks/reviewer-worker.md`：
   - 部署步骤：第二个 worker 进程 + reviewer `WORKFLOW.md`，`tracker.active_states: ["Human Review"]`（可选 `tracker.required_labels` 收窄），与 maker worker 指向同一 Gitea 项目。
   - **workspace 隔离（硬性要求）**：reviewer worker 必须使用独立 `workspace.root`。机理：`PathFor` 是 `Root/owner/repo/sourceType/sourceEventID`（`internal/workspace/manager.go:145-148`），同 issue 在两个进程算出同一路径；`reuseWorktree`（manager.go:285 起）会 `git reset HEAD -- .` + `git checkout --force -B` 强制丢弃 tracked 修改。maker 翻完 handoff 标签后、reconcile-cancel 抓到它之前（最长一个 poll 间隔，#557 记录过该尾部窗口），reviewer 一旦派发就会 force-reset maker 仍在用的 worktree，跨进程无锁可协调。
   - **bare mirror cache 共享声明**：`Manager.MirrorRoot` 默认解析到 `os.UserCacheDir`（manager.go:139-141），同主机同用户的两个 worker 共享同一 bare mirror，并发 fetch 依赖 git 自身 ref 锁、可能瞬时锁冲突。runbook 必须明确取舍（建议同样隔离，或写明依赖 git 锁 + 重试语义）。
   - **待审 diff 发现约定（可执行）**：prompt 渲染变量只含 SPEC §4.1.1 issue 快照（`internal/worker/runtask.go:43-49`），无 PR 链接；且 reviewer 自己的 worktree 是从 base ref 新建的 work branch，**maker 的改动不在本地**。runbook 须给出默认约定二选一：maker 在 issue 评论留 PR URL，或 reviewer 按 head branch（`ai/<n>`）查 Gitea API；并写明 reviewer 需 `git fetch origin <work-branch>` 或经 API 拿 diff。
   - 状态流转：maker → `aiops/human-review` → reviewer 按 rubric 审 diff → `aiops/done`（通过）/ `aiops/rework`（打回，复用既有 Rework 重派发路径，实测已走通）。
   - 对齐声明：零平台改动、纯配置 + prompt；与 D25 "one process per service"、#72 单 workflow 源一致。
2. 示例 reviewer `WORKFLOW.md`：rubric 驱动、review-only（不写码不 push，verdict 即标签翻转）。
   - "只审不写"约束放 **prompt 文本**——`policy.mode: analysis_only` 不适用：其 directive 是 plan-artifact/no-handoff 契约（`internal/worker/runtask.go:676`），会连标签翻转一起禁掉。
   - **sandbox 按两档给出**：纯 diff 审 → read-only（verdict 路径不受影响：`gitea_issue_labels` 是 orchestrator 代理的动态工具，不走沙箱 FS/网络）；rubric 含跑 build/test → workspace-write（`go build`/`go test` 需写 cache/tmp）+ prompt 约束不 push。
3. **待验证**的单进程替代（验证通过才写入 runbook）：maker 的 WORKFLOW prompt 指示在翻 handoff 标签前，经 repo 内 `.codex/agents/` 起独立上下文 grader 子代理（CMA Outcomes 形状）。headless `codex app-server` 下子代理可用性未验证；验证不通过则在 runbook 明确标注 unsupported + 原因。
4. 顺带修复：AGENTS.md cross-cutting checklist 仍引用已删除的 `selectRoutedCandidates`（#573/D25 移除），扩展示例需更新为现存符号。

## 验收标准

- [ ] runbook 存在，其中所有 config 片段经 `go run ./cmd/worker --print-config` 实际校验（doc snippet 必须过真实 loader）
- [ ] runbook 把独立 `workspace.root` 写成硬性要求并给出碰撞机理（PathFor 同路径 + reuseWorktree force-reset + handoff 尾部窗口）；对 bare mirror cache 的共享/隔离做出明确声明
- [ ] runbook 给出可执行的"待审 diff 发现"默认约定（issue 评论 PR URL / 按 head branch 查 API 二选一），并写明 reviewer worktree 不含 maker 改动、需 fetch 或 API 取 diff
- [ ] 示例 reviewer `WORKFLOW.md` 随 runbook 提供，含 rubric 段与 review-only 约束；sandbox 配置按两档（read-only / workspace-write+跑测试）给出，并注明 verdict 经代理工具不受沙箱限制
- [ ] 单进程 grader 子代理选项：验证通过并文档化，或明确标注 unsupported + 原因
- [ ] AGENTS.md `selectRoutedCandidates` 过期引用修复
- [ ] 全程零平台代码改动（docs/示例 only；无 DEVIATIONS 行；不触发 PR Metadata 的 SPEC 敏感路径门）

Refs：#744（关闭 verdict）· #76 / #557 / D33（边界先例）· #573 / D25（service routing 移除）· v0.1.0 生命周期报告（`.trellis/tasks/archive/2026-06/06-11-web-todo-app-full-lifecycle-test-with-packaged-release-binary/report.md`，12/12 + Rework ×3）

---
*2026-06-11 增补：workspace.root 隔离（含 mirror cache 声明）、diff 发现约定、sandbox 两档取舍——来自 Codex 评审 + 代码核实（manager.go / runtask.go 行号见上）。*
