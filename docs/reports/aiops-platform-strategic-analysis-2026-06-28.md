# aiops-platform strategic analysis and issue candidates

日期：2026-06-28
仓库：`/home/echoy/src/aiops-platform`
分析分支：`codex/strategic-analysis-2026-06-28`
基线：`origin/main`
基线 HEAD：`024deae chore(deps): bump actions/setup-go from 6.4.0 to 6.5.0 (#1011)`
最近发布版本：`v0.1.10`

## 结论摘要

`aiops-platform` 不是一个普通的“调用 agent 改代码”脚本，而是一个 Symphony 风格的常驻 AI coding operations loop。它把 issue tracker 作为工作队列和状态事实源，持续轮询 Linear、Gitea 或 GitHub，把合格 issue 准备成确定性的 Git workspace，再用 `WORKFLOW.md` 生成 prompt 并启动 agent。平台负责调度、隔离、观察、重试、reconcile、状态 API 和 dashboard；代码修改、验证、push、PR、tracker 写回仍然由 agent 自己通过工具完成。

在 Claude Code dynamic workflows、subagents、skills、hooks 以及 `/goal` 已经很强的背景下，`aiops-platform` 的价值不应该再表述为“通用多 agent 编排器”。这块能力正在被基础工具快速吸收。更清晰的定位是：

- 本地自托管的 issue-to-PR 运行层；
- tracker-first 的无人值守 ready queue；
- per-issue workspace 隔离、并发、重试、取消和 reconcile；
- Linear/Gitea/GitHub 的本地可控集成；
- 对 agent 的 tracker token 隔离和动态工具代理；
- repo-owned 的 PR gate、maker/reviewer 分权工作流、运行证据和 dashboard。

总体判断：`aiops-platform` 没有被 `/goal` 或 Claude Code dynamic workflows 淘汰，但它应该主动收窄定位。一次性复杂任务、代码库审计、迁移、研究、session 内 subagent fan-out 更适合 Claude Code dynamic workflows；需要长期把 tracker 里的 ready issues 连续转成可审 PR 时，`aiops-platform` 仍然有独立价值。

## 当前项目画像

本地仓库呈现出比较完整的运行时形态：

- `cmd/worker` 是主二进制入口，负责加载 `WORKFLOW.md`、启动 tracker client、orchestrator actor、state API、dashboard 和 runtime poll loop。
- `internal/orchestrator` 以单 goroutine actor 作为状态写入权威，围绕 running、blocked、claimed、retry、completed、codex totals 等状态组织调度。
- `internal/workspace` 负责 deterministic workspace，包含 mirror cache、worktree ownership lock、path containment、safe remove 和 untracked artifact 保留策略。
- `internal/runner` 支持 `mock`、`codex-app-server` 和 `claude` runner。当前主力 SPEC runner 是 `codex-app-server`，Claude runner 更接近 shell command wrapper。
- `internal/tracker` 抽象 Linear、Gitea、GitHub。worker 以读和判定为主，写回由 agent 工具或 workflow 约束完成。
- `internal/stateapi` 和 dashboard/TUI 提供运行态观察面。
- 文档和 runbooks 占比很高，说明项目资产不仅是 Go 代码，还包括治理协议、验证报告、PR gate、dogfood 流程和失败经验。
- 最新 `main` 已包含 GitHub maker/reviewer auto-merge E2E harness：maker worker 负责实现、测试、push 和 PR handoff，reviewer worker 使用独立 workspace 与 `GH_CONFIG_DIR` 做审查、请求 Rework、批准并启用 GitHub native CI-gated auto-merge，且只在 GitHub 确认 PR merged 后关闭 issue。

这个架构的强项是“把真实 tracker 队列变成可观察、可重试、可审计的本地工程运行时”。最新 maker/reviewer harness 进一步证明：项目的价值正在从单 worker issue-to-PR 扩展到 repo-owned governance loop。它的弱项也很明确：单进程内存状态、受信任执行环境假设、复杂工作流仍然需要靠文档、GitHub branch protection 和 agent 守则共同约束，而不是由平台自身完全证明。

## 与 Claude Code `/goal` 的区别

Claude Code `/goal` 是 session-scoped 的目标保持机制。用户设置完成条件后，每个 turn 结束会由一个快速评估模型判断目标是否已经满足；如果未满足，Claude 会继续下一轮。它适合让当前会话朝一个可验证终点持续推进。

关键区别如下：

| 维度 | `aiops-platform` | Claude Code `/goal` |
|---|---|---|
| 工作单元 | tracker issue -> workspace -> PR handoff | 当前 Claude Code session 的目标 |
| 状态事实源 | tracker、workspace、runner event、state API、filesystem reconciliation | 会话上下文中的对话证据 |
| 持久性 | 常驻 worker，可重启后通过 tracker/filesystem 重建 | session-scoped，resume/continue 可恢复目标但计数和基线重置 |
| 判断方式 | poller、tracker refresh、actor state、runner exit、reconcile | 小模型评估目标是否满足 |
| 工具能力 | worker 可以轮询 tracker、准备 workspace、启动 runner、暴露 API | goal evaluator 不调用工具、不读文件、不运行命令 |
| 并发模型 | 多 issue 并发，受 workflow capacity 控制 | 单 session 的持续推进机制 |
| 最适合 | 长期 ready queue、无人值守 issue-to-PR、状态可观测 | 单次任务的完成条件保持、避免半途停下 |
| 主要风险 | 执行环境、安全边界、状态持久化、agent 遵守协议 | 目标定义不清、评估证据不足、会话漂移 |

结论：`/goal` 可以让一个 Claude Code session 更有韧性，但它不是 tracker queue、workspace manager、state API 或 PR operations loop。`aiops-platform` 可以吸收 `/goal` 的思想，用在 agent session 的完成条件上，但不应该复制 `/goal` evaluator。

## 与 Claude Code dynamic workflows 的区别

Claude Code dynamic workflows 是 Claude 根据任务生成并执行的 JavaScript workflow。它把循环、分支、中间结果、并发 agent fan-out 等逻辑移出 Claude 上下文，放到 workflow script 变量和运行时里。workflow 可以在同一 session 中恢复，也可以保存为 command；当前文档描述的能力包括最多 16 个并发 agents、单次运行最多 1,000 个 agents，script 自身没有 filesystem/shell access，但 agents 可以使用工具。

关键区别如下：

| 维度 | `aiops-platform` | Claude Code dynamic workflows |
|---|---|---|
| 编排对象 | tracker issue 生命周期和 per-issue runner | session 内的任务图、循环、分支、subagent fan-out |
| 生命周期 | 常驻服务，持续轮询外部 tracker | Claude Code session 内运行，可保存为 command |
| 状态边界 | tracker + worker memory + workspace + state API | workflow script 变量 + Claude session |
| 外部系统 | Linear/Gitea/GitHub/worktree/dashboard | 由 agents 通过工具访问 |
| 最适合 | 多 issue 长期队列、PR handoff、dogfood 验证 | 一次性研究、审计、迁移、批量分析、复杂探索 |
| 安全边界 | worker 自己承担 runner/sandbox/workspace/token 约束 | Claude Code runtime 和工具权限承担主要边界 |
| 价值证明 | 可用 issue-to-PR 指标和运行证据证明 | 可用任务完成质量和探索效率证明 |

结论：dynamic workflows 会削弱 `aiops-platform` 作为“通用编排器”的吸引力，但不会直接替代 tracker-first 的常驻 queue runner。最合理的方向是把 dynamic workflows 视为上游能力或对照组：复杂一次性工作交给 Claude workflows，长期 tracker backlog 交给 `aiops-platform`。

## 现阶段价值评估

### 仍然有价值的部分

- **常驻性**：它不是一次会话，而是一个可以挂在本地机器、工作站或小服务器上的 worker。
- **tracker-first**：issue 状态、labels、dependencies、blocked/rework/human-review 等流程约束是调度输入，而不是 prompt 里的软建议。
- **workspace discipline**：每个 issue 有可预测工作目录、cache、锁和清理策略。
- **observability**：state API、dashboard、TUI、trace evidence 和 validation reports 是 Claude `/goal` 本身不提供的运行面。
- **local control**：Gitea、本地 repo、systemd/launchd/Docker compose、离线 mock loop 等能力对个人和小团队很有意义。
- **protocol ownership**：AGENTS、WORKFLOW、runbooks、PR gate、maker/reviewer auto-merge harness 和 dogfood 流程把“怎么让 agent 干活”变成 repo 内可审查资产。

### 需要收缩或重新表述的部分

- 不应该强调“我能做多 agent 编排”，因为 Claude Code dynamic workflows 已经更自然地覆盖很多 session 内编排场景。
- 不应该把 `/goal` 当竞争对象。`/goal` 是 session 完成条件机制，`aiops-platform` 是 service-level issue runner。
- 不应该过早追求横向扩展和企业级多租户。当前架构更像 personal/small-team harness。
- 不应该让 worker/orchestrator 承担 push/PR/merge 的写操作。最新 GitHub maker/reviewer 模式的方向是正确的：写操作由受 workflow 约束的 agent 身份完成，worker 保持调度和观察边界。

### 推荐定位语

建议 README 或文档中采用类似表述：

> `aiops-platform` is a local, tracker-first AI coding operations runtime: it turns ready issues into isolated agent workspaces, supervises execution, and exposes the evidence needed to review, retry, or hand off one issue as one PR.

中文表述可以是：

> `aiops-platform` 是一个本地自托管、tracker-first 的 AI 编码运行时：它把 ready issue 转成隔离 workspace，监督 agent 执行，并产出足够的运行证据，让每个 issue 能以一个可审 PR 的形式交付、重试或人工接管。

## 建议优先创建的 issues

下面这些不是随手列的 TODO，而是基于当前项目边界、Claude Code 新能力、最新 `main` 的 maker/reviewer harness 和现有风险面筛出的高价值 issue 候选。建议先开前 6 个，后 3 个可以作为 design/experiment backlog。

### 1. Clarify aiops-platform positioning against Claude Code workflows and `/goal`

建议标签：`docs`, `product`, `priority:p0`

背景：

Claude Code dynamic workflows 和 `/goal` 已经覆盖了大量 session 内自动化场景。如果项目继续以“agent orchestrator”泛泛定位，容易被误解为和 Claude Code 正面竞争。项目真正强项是 tracker-first、常驻、issue-to-PR、本地可控运行层。

建议范围：

- 在 README 或 `docs/architecture.md` 增加 positioning/decision matrix。
- 明确三类工具边界：`aiops-platform`、Claude Code dynamic workflows、Claude Code `/goal`。
- 给出 3 到 5 个“应该用 aiops-platform”和“不应该用 aiops-platform”的例子。
- 引用 Claude 官方 workflows 和 goal 文档。

验收标准：

- 新文档能回答“有了 Claude workflows 和 `/goal` 后为什么还需要本项目”。
- README 中的定位不再只停留在 generic orchestrator。
- 文档明确建议 dynamic workflows 用于一次性复杂探索，`aiops-platform` 用于长期 tracker backlog。

### 2. Add a documented Claude Code dynamic workflow handoff pattern

建议标签：`docs`, `workflow`, `priority:p1`

背景：

dynamic workflows 很适合代码库审计、批量 issue 评估、迁移方案生成和多 subagent fan-out。`aiops-platform` 不需要复制这些能力，但应该定义如何把 dynamic workflow 的产出转成可执行 issue queue。

建议范围：

- 新增 runbook：`docs/runbooks/claude-workflow-handoff.md`。
- 定义一个推荐流程：dynamic workflow 做审计/拆解 -> 输出 issue 草案 -> 人类筛选/加 label -> `aiops-platform` 消费 ready issue。
- 明确不把 workflow script 直接作为 worker plugin 执行，避免混淆权限边界。
- 给出示例：一次 repo audit 如何产出 5 个可交给 worker 的 issues。

验收标准：

- runbook 包含输入、输出、人工 gate、失败处理和安全注意事项。
- 示例 issue 包含标题、背景、验收标准、建议 label。
- 文档说明 dynamic workflow 和 `WORKFLOW.md` 的职责差异。

### 3. Add operator value metrics to the state API and dashboard

建议标签：`enhancement`, `observability`, `priority:p1`

背景：

项目价值最终需要用 issue-to-PR 的运营指标证明，而不是只靠“能跑”。当前 state API 和 dashboard 已经能观察运行态，但还可以更直接地展示平台相对手动 Claude session 的价值。

建议范围：

- 增加或整理 per-issue lifecycle metrics，例如 `ready_to_dispatch_duration`、`dispatch_to_terminal_duration`、`blocked_duration`、`retry_count`、`rework_count`、`cancel_reason`、`runner_exit_reason`、`continuation_count`。
- dashboard 增加轻量 summary，不必做复杂 analytics。
- trace evidence manifest 或 validation report 可以引用这些指标。
- maker/reviewer 模式下应能区分 maker handoff、reviewer rework、review approval、auto-merge enabled、merged confirmed、issue closed 等阶段。

验收标准：

- state API 返回每个 issue 的关键生命周期时间点或可计算字段。
- dashboard 能看出队列吞吐、失败原因和人工接管点。
- 单元测试覆盖指标在 dispatch、retry、cancel、terminal transitions 下的更新。

### 4. Promote the GitHub maker/reviewer harness into a reusable governance template

建议标签：`docs`, `github`, `governance`, `priority:p1`

背景：

最新 `main` 已有 `docs/runbooks/github-maker-reviewer-automerge-e2e.md`、`examples/github-maker-WORKFLOW.md` 和 `examples/github-reviewer-automerge-WORKFLOW.md`。这证明项目已经形成更强的治理模式：maker 不能审自己的 PR，reviewer 用独立身份和 workspace 审查并启用 GitHub native CI-gated auto-merge。当前形态偏 release-validation runbook，下一步应该把它提炼成用户可复用模板。

建议范围：

- 新增较短的 production governance guide，和长 E2E runbook 分离。
- 提炼 maker/reviewer 的最小配置、身份隔离、branch protection、required checks、label state machine 和失败恢复。
- README 增加从普通 GitHub workflow 升级到 maker/reviewer 模式的链接。
- 保留长 E2E runbook 作为验证证据和深度演练。

验收标准：

- 新用户能在不阅读全文 E2E 的情况下理解 maker/reviewer 模式何时适用。
- 文档明确 worker/orchestrator 不写 PR、不 approve、不 merge、不 close issue。
- governance guide 链接到示例 workflows、preflight scripts 和 evidence checklist。

### 5. Define a minimum safe runner profile for real-agent mode

建议标签：`security`, `runner`, `priority:p1`

背景：

当前项目明确是 trusted environment posture，bubblewrap/firejail 是可选能力。随着 real agent runner 增多，需要一个更明确的“最小安全运行档位”，避免用户误把它当成强沙箱。

建议范围：

- 在 `docs/security-posture.md` 或 runbook 中定义 `mock`、`local trusted`、`sandboxed local` 三个安全档位。
- `--doctor` 或 startup warning 检查 real-agent mode 下的关键配置：workspace root、token source、sandbox setting、network assumptions、tracker write boundary、maker/reviewer identity separation。
- 明确哪些风险不能由当前平台解决。

验收标准：

- 文档中有清晰的安全档位和推荐用途。
- real-agent mode 缺少关键安全配置时有可见 warning 或 doctor failure。
- 测试覆盖配置诊断逻辑，避免把 warning 静默吞掉。

### 6. Persist a lightweight run manifest for issue lifecycle evidence

建议标签：`enhancement`, `reliability`, `priority:p1`

背景：

orchestrator 状态主要在内存中，重启后依赖 tracker polling 和 filesystem reconciliation。这个设计符合当前 personal/small-team 模型，但对于复盘、价值指标和异常诊断，轻量持久化 manifest 会很有帮助。

建议范围：

- 不引入数据库。
- 在 per-issue workspace 或 trace evidence 目录写入 append-only/atomic manifest。
- 记录 issue id、source、workflow fingerprint、base revision、runner type、dispatch time、terminal state、exit reason、artifact links。
- startup reconciliation 可以读取 manifest 辅助展示历史，但不把它作为唯一状态事实源。

验收标准：

- manifest 文件格式稳定并有 schema 文档。
- worker restart 后 dashboard 能区分“历史 terminal evidence”和“当前 active state”。
- 单元测试覆盖 manifest 写入失败、部分写入、重复 dispatch 的行为。

### 7. Document a goal-aware Claude runner experiment without reimplementing `/goal`

建议标签：`design`, `runner`, `priority:p2`

背景：

`/goal` 的价值是让单个 Claude session 围绕完成条件持续推进。`aiops-platform` 可以研究如何在 Claude runner prompt 或 command wrapper 中使用 goal-like completion criteria，但不应该在 worker 内复制 Claude 的 evaluator。

建议范围：

- 新增 design note，说明 goal-aware runner 的目标和非目标。
- 明确 worker 只负责传递 issue acceptance criteria、观察 runner exit 和收集证据。
- 评估 Claude CLI 是否适合在 runner 中显式使用 `/goal`，以及和 non-interactive mode 的兼容性。

验收标准：

- design note 明确“不在 worker 里实现 goal evaluator”。
- 包含至少一个实验计划：同一个 issue 用普通 Claude runner 和 goal-aware prompt 对比。
- 结论能指导是否进入实现。

### 8. Split daily-user onboarding from dogfood/PR-merge protocol docs

建议标签：`docs`, `onboarding`, `priority:p2`

背景：

当前 runbooks 非常丰富，但新用户可能会把 dogfood merge protocol、GitHub maker/reviewer E2E、trace harness 和 daily workflow 混在一起。应该给个人用户一条更短路径：mock loop -> one real issue -> dashboard -> PR handoff。

建议范围：

- 新增或改写 `docs/runbooks/personal-daily-workflow.md` 的开头路径。
- 把 dogfood PR merge 规则和 GitHub maker/reviewer auto-merge E2E 作为进阶链接，而不是首屏负担。
- 明确“worker 不 push/不 PR/不 merge”的边界。

验收标准：

- 新用户 15 分钟内能知道最小配置和第一条 issue 如何跑。
- dogfood/merge protocol 仍可被发现，但不会遮蔽基础用法。
- 文档包含 mock mode 和 real runner mode 的分叉说明。

### 9. Run a comparative validation: aiops-platform vs dynamic workflows vs `/goal`

建议标签：`validation`, `research`, `priority:p2`

背景：

目前对价值边界的判断主要来自架构分析。可以设计一个小型对照实验，用同一组任务分别通过 `aiops-platform`、Claude Code dynamic workflow、Claude Code `/goal` 跑，记录各自的适用性和成本。

建议范围：

- 选 3 类任务：单 issue 修复、多文件审计、批量 issue 拆解。
- 记录完成时间、人工干预次数、可审证据、失败恢复、PR handoff 清晰度。
- 输出 validation report，不要求改代码。

验收标准：

- `docs/validation/` 下有对照报告。
- 报告给出明确建议：哪些任务进入 aiops queue，哪些任务交给 Claude workflow 或 `/goal`。
- 结论被 README 或 runbook 的 decision matrix 引用。

## 不建议现在开的 issues

- “把 aiops-platform 做成通用 Claude workflow 替代品”：价值边界太散，容易和 Claude Code 原生能力竞争。
- “在 worker 中实现自己的 `/goal` evaluator”：会复制 Claude 的会话层能力，而且 worker 没有完整上下文。
- “让 worker 自动 push/open PR/merge”：会破坏当前清晰边界。现有 maker/reviewer 方向应继续保持 agent 身份分权、GitHub branch protection 和 native auto-merge，而不是 worker-side merge helper。
- “引入 Postgres/分布式队列”：当前 personal/small-team 价值还没有证明到需要承担这类复杂度。

## 推荐执行顺序

1. 先开 Issue 1，修正项目定位。
2. 同时开 Issue 2 和 Issue 3，把 Claude workflows 的关系和价值指标落地。
3. 接着开 Issue 4，把 GitHub maker/reviewer harness 从 release-validation runbook 提炼成可复用治理模板。
4. Issue 5 补安全档位和 real-agent doctor。
5. Issue 6 作为可靠性增强进入实现 backlog。
6. Issue 7 到 9 作为 design/validation backlog，用于避免战略判断停留在直觉层面。

## 参考资料

- 本仓库：`README.md`、`AGENTS.md`、`DEVIATIONS.md`、`docs/architecture.md`、`docs/security-posture.md`、`docs/runbooks/*`、`.github/workflows/*`。
- Claude Code workflows: <https://code.claude.com/docs/en/workflows.md>
- Claude Code goal: <https://code.claude.com/docs/en/goal.md>
