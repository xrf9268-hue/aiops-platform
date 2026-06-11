# Issue #754: Docs: promote the LEARNINGS.md memory convention from #745's closing comment into README/runbook

(PRD = issue body)

## 背景

#745（cross-run memory：repo-owned `LEARNINGS.md` + worker prompt 注入）以 not planned 关闭。机制层结论经独立复核成立：workspace 即目标 repo 的确定性克隆，repo 根的 `LEARNINGS.md` agent 本来就可读、可写、可随 PR 提交；worker 注入机制与一行 prompt 指令行为等价（`internal/worker/runtask.go:286-295` 的 prompt 组装只有 WORKFLOW 模板渲染 + directive 拼接；上游 `config/schema.ex` 无任何 prompt 字段；SPEC.md:301 明写 workflow file 本就 repository-owned）。**本 issue 不重提平台改动。**

但行为承接物——#745 关闭评论里那段 drop-in WORKFLOW.md snippet（read-before-plan / verified-facts-only 含 `verified:` 字段 / 写入随 PR review / fold duplicates）——目前只活在 closed issue 的评论里。**评论不是产品面**：不可发现、会沉。operator 真正复用的入口应该是 README 或 runbook。

## 权威依据（advisory，per AGENTS.md practitioner-accounts 条款）

- Addy Osmani《Loop Engineering.》（2026-06-08）：loop 的第六件是对话外记忆——"A markdown file, or a Linear board… The agent forgets, the repo doesn't"；"Without skills the loop re-derives your whole project from zero every cycle, with skills it kind of compounds"。本地镜像 `~/projects/ai-agent-engineering-notes/docs/01_Agent_Design_and_Architecture/10_loop-engineering.md:50`、`:78`。
- Lance Martin《Designing loops with Fable 5》（2026-06-09）：有效记忆 = **fail → investigate → verify → distill → consult** 递进链；弱模型的失败模式是堆未验证笔记且几乎不回读，处方是 "task-specific memory **instructions**"（prompt 修复，正是 snippet 的定位）；最强 run 验证覆盖率 73% 并把经验蒸馏成通用规则。本地镜像同目录 `11_designing-loops-with-fable-5.md:67-73`。
- 内部先例：本仓库自己就在实践这条链——AGENTS.md earned rules + `docs/engineering-rules-rationale.md`（每条规则带 provenance）。本 issue 只是把内部已验证的实践打包给 operator。

## 交付物

README 的 workflow-authoring 段（或 `docs/runbooks/workflow-authoring.md`，若 README 体量放不下）：

1. #745 关闭评论的 snippet 原样收编（read-before-plan；每条必须含 `verified:` 依据；写入随 PR 进 review；fold duplicates 而非追加重复）。
2. **最小条目模板**（防止用户把 LEARNINGS.md 写成 run log——Lance 文中 Sonnet 4.6 的失败模式正是"堆笔记不回读"）。字段四件套，直接对应 fail→investigate→verify→distill 链：

   ```markdown
   ## <一句话规则（rule）>
   - symptom: <观察到的现象，如 "CI 上集成测试偶发超时">
   - root-cause: <诊断出的根因，如 "测试容器默认 1 CPU，t.Parallel 互相饿死">
   - verified: <如何验证的，如 "本地 --cpus=1 复现 3/3，调到 2 CPU 后 0/10">
   - rule: <通用化规则，如 "新增集成测试必须在 1 CPU 约束下本地跑一次">
   ```

   模板要求条目是**通用规则**而非单次 run 记录；`verified:` 缺失的条目按 snippet 守卫直接禁止。
3. 职责边界表：tracker = 状态记忆（SPEC §14.3）/ `WORKFLOW.md` = 指令源 / `LEARNINGS.md` = 验证过的项目经验 cargo（随 repo 走、人审变更）。
4. token 成本与膨胀提示：条目保持通用规则而非 run 日志；评审压力即淘汰机制（worker 侧上限属 #561 已移除的 policy-gate 类，不做）。

## 验收标准

- [ ] snippet 落入 README 或 runbook，并从 #745 关闭评论回链
- [ ] 文档含最小条目模板（symptom / root-cause / verified / rule 四字段），并附至少一条真实形态的示例条目
- [ ] tracker / `WORKFLOW.md` / `LEARNINGS.md` 三方职责边界成文
- [ ] verified-facts-only 守卫文本保留（每条必须含 how-verified；明确禁裸猜测）
- [ ] 零平台代码改动

Refs：#745（关闭 verdict，snippet 原文所在）· #753（reviewer-worker 模式，同源于同一次 loop-engineering 审计的姊妹承接 issue）· #561 / D33（worker 侧 policy gate 移除先例）

---
*2026-06-11 增补：最小条目模板（四字段）——来自 Codex 评审建议。*
