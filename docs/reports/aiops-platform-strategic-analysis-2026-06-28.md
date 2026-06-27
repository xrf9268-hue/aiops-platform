# aiops-platform 战略分析与 issue 候选

日期：2026-06-28
仓库：`/home/echoy/src/aiops-platform`
分析分支：`codex/strategic-analysis-2026-06-28`
基线：`origin/main`
基线 HEAD：`024deae chore(deps): bump actions/setup-go from 6.4.0 to 6.5.0 (#1011)`
最近发布版本：`v0.1.10`

## 结论摘要

`aiops-platform` 不是一个普通的“调用 agent 改代码”脚本，而是一个 Symphony 风格的常驻 AI coding operations loop。它把 issue tracker 作为工作队列和状态事实源，持续轮询 Linear、Gitea 或 GitHub，把合格 issue 准备成确定性的 Git workspace，再用 `WORKFLOW.md` 生成 prompt 并启动 agent。平台负责调度、隔离、观察、重试、reconcile、状态 API 和 dashboard；代码修改、验证、push、PR、tracker 写回仍然由 agent 自己通过工具完成。

在 Claude Code dynamic workflows、subagents、skills、hooks、Claude `/goal` 以及 Codex Goal mode 已经很强的背景下，`aiops-platform` 的价值不应该再表述为“通用多 agent 编排器”。许多 session/thread 内编排和目标保持能力已经由基础工具覆盖或增强。更清晰的定位是：

- 本地自托管的 issue-to-PR 运行层；
- tracker-first 的无人值守 ready queue；
- per-issue workspace 隔离、并发、重试、取消和 reconcile；
- Linear/Gitea/GitHub 的本地可控集成；
- 对 agent 的 tracker token 隔离和动态工具代理；
- repo-owned 的 PR gate、maker/reviewer 分权工作流、运行证据和 dashboard。

总体判断：`aiops-platform` 没有被 Claude `/goal`、Codex Goal mode 或 Claude Code dynamic workflows 淘汰，但它应该主动收窄定位。一次性复杂任务、代码库审计、迁移、研究、session 内 subagent fan-out 通常更适合 Claude Code dynamic workflows；需要长期把 tracker 里的 ready issues 连续转成可审 PR 时，`aiops-platform` 仍然有独立价值。

## 当前项目画像

本地仓库呈现出比较完整的运行时形态：

- `cmd/worker` 是主二进制入口，负责加载 `WORKFLOW.md`、启动 tracker client、orchestrator actor、state API、dashboard 和 runtime poll loop。
- `internal/orchestrator` 以单 goroutine actor 作为状态写入权威，围绕 running、blocked、claimed、retry、completed、codex totals 等状态组织调度。
- `internal/workspace` 负责 deterministic workspace，包含 mirror cache、worktree ownership lock、path containment、safe remove 和 untracked artifact 保留策略。
- `internal/runner` 支持 `mock`、`codex-app-server` 和 `claude` runner。当前主力 SPEC runner 是 `codex-app-server`，Claude runner 更接近 shell command wrapper。
- `internal/tracker` 定义共享 tracker 抽象并包含 Linear/GitHub client；`internal/gitea` 实现 Gitea tracker client 和 label-state helpers。worker 以读和判定为主，写回由 agent 工具或 workflow 约束完成。
- `internal/stateapi` 和 dashboard/TUI 提供运行态观察面。
- 文档和 runbooks 占比很高，说明项目资产不仅是 Go 代码，还包括治理协议、验证报告、PR gate、dogfood 流程和失败经验。
- 最新 `main` 已包含 GitHub maker/reviewer auto-merge E2E harness：运行 maker/reviewer `WORKFLOW.md` 的 worker 实例分别启动对应 agent；maker agent 负责实现、测试、push 和 PR handoff，reviewer agent 使用独立 workspace root、独立 `AIOPS_MIRROR_ROOT`、独立 `GH_CONFIG_DIR` 和 expected login 做审查、请求 Rework、批准并启用 GitHub native CI-gated auto-merge，且只在 GitHub 确认 PR merged 后关闭 issue。

这个架构的强项是“把真实 tracker 队列变成可观察、可重试、可审计的本地工程运行时”。最新 maker/reviewer harness 进一步证明：项目的价值正在从单 worker issue-to-PR 扩展到 repo-owned governance loop。它的弱项也很明确：单进程内存状态、受信任执行环境假设、复杂工作流仍然需要靠文档、GitHub branch protection 和 agent 守则共同约束，而不是由平台自身完全证明。

## 与 Claude Code `/goal` 和 Codex Goal mode 的区别

Claude Code `/goal` 是 session-scoped 的目标保持机制。用户设置完成条件后，每个 turn 结束会由一个快速评估模型判断目标是否已经满足；如果未满足，Claude 会继续下一轮。它适合让当前会话朝一个可验证终点持续推进，也可以处理当前 session 能访问和证明的 labeled issue backlog；但它的事实来源仍是会话中已经呈现的证据。

截至 2026-06-28，官方文档说明 `/goal` 要求 Claude Code v2.1.139 或更高版本，并且依赖 trusted workspace 和 hooks 系统；当 hooks 被禁用或 managed hooks 策略不允许时，命令不可用。

Codex 官方 manual 也描述了 Goal mode：goal 是一个跨较长任务保持的 persistent objective，goal 文本同时作为起始 prompt 和 completion criteria；Codex 会用它决定下一步做什么以及任务是否完成。Codex 的 `/goal` 可在 Codex app、IDE extension 和 CLI 中使用；如果 slash command 中没有 `/goal`，需要启用 `features.goals`。在 app 中，goal 进度显示在 composer 上方，并提供 pause、resume、edit、clear 控件；CLI 中也支持 `/goal pause`、`/goal resume`、`/goal clear`，且 goal objective 上限为 4,000 字符，长指令应放入文件后让 goal 指向该文件。

关键区别如下：

| 维度 | `aiops-platform` | Claude Code `/goal` | Codex Goal mode |
|---|---|---|---|
| 工作单元 | tracker issue -> workspace -> PR handoff | 当前 Claude Code session 的目标 | 当前 Codex thread/task 的目标 |
| 状态事实源 | tracker、workspace、runner event、state API、filesystem reconciliation | 会话上下文中的对话证据 | Codex thread context、tool output、goal text 和任务进度 |
| 持久性 | 常驻 worker，可重启后通过 tracker/filesystem 重建 | session-scoped，resume/continue 可恢复目标但计数和基线重置 | goal attached to active thread；可 pause/resume/edit/clear，但仍是 Codex thread 内目标 |
| 判断方式 | poller、tracker refresh、actor state、runner exit、reconcile | 小模型评估目标是否满足 | Codex 使用 goal 作为 prompt 和 completion criteria 来决定下一步和是否完成 |
| 工具能力 | worker 可以轮询 tracker、准备 workspace、启动 runner、暴露 API | goal evaluator 不调用工具、不读文件、不运行命令 | Codex agent 可继续使用当前 thread/环境允许的工具，goal 本身不是外部 tracker poller |
| 并发模型 | 多 issue 并发，受 workflow capacity 控制 | 单 session 的持续推进机制 | 单 thread/task 的持续目标机制 |
| 最适合 | 长期 ready queue、无人值守 issue-to-PR、状态可观测 | 当前 session 内的可验证目标、可证明的小型/中型 backlog、避免半途停下 | Codex 内较长任务、清晰 definition of done、需要 pause/resume/edit 的工作 |
| 主要风险 | 执行环境、安全边界、状态持久化、agent 遵守协议 | 目标定义不清、评估证据不足、会话漂移 | goal 写得不可测、thread context/compaction 影响、把 thread 目标误当作服务级队列 |

结论：Claude `/goal` 和 Codex Goal mode 都能让一次 session/thread 更有韧性，尤其适合把“完成条件”写清楚并持续推进。但它们本身仍不是常驻 tracker poller、workspace manager、state API 或 PR operations loop。`aiops-platform` 可以吸收 goal 的思想，用在 issue acceptance criteria、runner prompt 和人工 handoff 证据上；但不应该在 worker 内复制 Claude 或 Codex 的 goal 机制。

## 与 Claude Code dynamic workflows 的区别

Claude Code dynamic workflows 是 Claude 根据任务生成并执行的 JavaScript workflow。它把循环、分支、中间结果、并发 agent fan-out 等逻辑移出 Claude 上下文，放到 workflow script 变量和运行时里。

截至 2026-06-28，官方文档说明的关键边界是：

- 要求 Claude Code v2.1.154 或更高版本；Pro 计划需要在 `/config` 中启用 dynamic workflows。
- 适用于付费计划以及 Anthropic API、Amazon Bedrock、Google Cloud Vertex AI、Microsoft Foundry 等 provider。
- 单次 run 最多 16 个并发 agents、1,000 个 agents 总量。
- workflow script 本身没有直接 filesystem 或 shell access；它通过 agents 使用工具。
- 暂停/停止后的 resume 只在同一 Claude Code session 内有效；退出 Claude Code 后同一 run 会重新开始。
- workflow subagents 以 `acceptEdits` 运行并继承 tool allowlist；文件编辑自动批准，shell、web、MCP 等未 allowlist 的工具仍受 permission 规则影响。

实践上，Claude `/goal` 或 Codex Goal mode 更偏单方向深度收敛，workflow 更偏横向 fan-out、交叉验证和综合；`aiops-platform` 则应把价值放在长期 tracker queue、运行证据和 PR handoff，而不是复制 session/thread 内的目标保持或临时 fan-out 能力。

关键区别如下：

| 维度 | `aiops-platform` | Claude Code dynamic workflows |
|---|---|---|
| 编排对象 | tracker issue 生命周期和 per-issue runner | session 内的任务图、循环、分支、subagent fan-out |
| 生命周期 | 常驻服务，持续轮询外部 tracker | Claude Code session 内运行，可保存为 command |
| 状态边界 | tracker + worker memory + workspace + state API | workflow script 变量 + Claude session |
| 外部系统 | Linear/Gitea/GitHub/worktree/dashboard | 由 agents 通过工具访问 |
| 最适合 | 多 issue 长期队列、PR handoff、dogfood 验证 | 一次性研究、审计、迁移、批量分析、复杂探索 |
| 安全边界 | worker 自己承担 runner/sandbox/workspace/token 约束 | Claude Code runtime 和 tool allowlist 承担边界；workflow subagents 以 `acceptEdits` 运行，文件编辑自动批准 |
| 价值证明 | 可用 issue-to-PR 指标和运行证据证明 | 可用任务完成质量和探索效率证明 |

结论：dynamic workflows 会削弱 `aiops-platform` 作为“通用编排器”的吸引力，但不会直接替代 tracker-first 的常驻 queue runner。最合理的方向是把 dynamic workflows 视为上游能力或对照组：复杂一次性工作交给 Claude workflows，长期 tracker backlog 交给 `aiops-platform`。

## 现阶段价值评估

### 仍然有价值的部分

- **常驻性**：它不是一次会话，而是一个可以挂在本地机器、工作站或小服务器上的 worker。
- **tracker-first**：issue 状态、labels、dependencies、blocked/rework/human-review 等流程约束是调度输入，而不是 prompt 里的软建议。
- **workspace discipline**：每个 issue 有可预测工作目录、cache、锁和清理策略。
- **observability**：state API、dashboard、TUI、trace evidence 和 validation reports 是 Claude `/goal` 或 Codex Goal mode 本身不提供的运行面。
- **local control**：Gitea、本地 repo、systemd/launchd/Docker compose、离线 mock loop 等能力对个人和小团队很有意义。
- **protocol ownership**：AGENTS、WORKFLOW、runbooks、PR gate、maker/reviewer auto-merge harness 和 dogfood 流程把“怎么让 agent 干活、何时算完成、用什么证据证明完成”变成 repo 内可审查资产。

### 需要收缩或重新表述的部分

- 不应该强调“我能做多 agent 编排”，因为 Claude Code dynamic workflows 已经更自然地覆盖很多 session 内编排场景。
- 不应该把 Claude `/goal` 或 Codex Goal mode 当竞争对象。它们是 session/thread 完成条件机制，`aiops-platform` 是 service-level issue runner。
- 不应该过早追求横向扩展和企业级多租户。当前架构更像 personal/small-team harness。
- 不应该让 worker/orchestrator 承担 push/PR/merge 的写操作。最新 GitHub maker/reviewer 模式的方向是正确的：写操作由受 workflow 约束的 agent 身份完成，worker 保持调度和观察边界。

### 推荐定位语

建议 README 或文档中采用类似表述：

> `aiops-platform` is a local, tracker-first AI coding operations runtime: it turns ready issues into isolated agent workspaces, supervises execution, and exposes the evidence needed to review, retry, or hand off one issue as one PR.

中文表述可以是：

> `aiops-platform` 是一个本地自托管、tracker-first 的 AI 编码运行时：它把 ready issue 转成隔离 workspace，监督 agent 执行，并产出足够的运行证据，让每个 issue 能以一个可审 PR 的形式交付、重试或人工接管。

## 可选后续议题（非路线图）

下面两项不是默认待办，也不是下一步路线图。它们只是当维护者明确决定把这份定位判断落地成 GitHub issue 时，最小、最清晰的候选。没有明确 owner、需求和验收意图前，不应因为报告里列了候选就创建 issue。筛选标准是：直接服务 `aiops-platform` 的核心定位；能降低后续决策成本；不把外部 agent 原生能力接进 worker；不为了“看起来完整”而新增实现型 backlog。

### 1. Clarify aiops-platform positioning against Claude Code workflows, Claude `/goal`, and Codex Goal mode

建议标签：`type:docs`, `area:docs`, `area:research`, `priority:p1`

背景：

Claude Code dynamic workflows、Claude `/goal` 和 Codex Goal mode 已经覆盖了大量 session/thread 内自动化场景。当前 README 已经说明项目是 personal-productivity、tracker-first、issue-to-workspace-to-agent-to-PR 的 Symphony-style loop；真正缺口是没有把这个定位和这些原生 agent 能力做显式对照。

建议范围：

- 在 README 或 `docs/architecture.md` 增加 positioning/decision matrix。
- 明确四类工具边界：`aiops-platform`、Claude Code dynamic workflows、Claude Code `/goal`、Codex Goal mode。
- 明确写出“不集成 dynamic workflows 到 worker，不实现自己的 goal loop，不把 session/thread 内编排当作平台能力”。
- 给出 3 到 5 个“应该用 aiops-platform”和“不应该用 aiops-platform”的例子。
- 引用 Claude 官方 workflows/goal 文档和 OpenAI Codex manual，明确 goal 偏 session/thread 内深度收敛，workflow 偏 session 内 fan-out，`aiops-platform` 偏服务级 tracker queue。

验收标准：

- README 或 `docs/architecture.md` 包含 decision matrix：`aiops-platform`、Claude Code dynamic workflows、Claude Code `/goal`、Codex Goal mode。
- 文档包含 3 到 5 个适合 `aiops-platform` 的例子，以及 3 到 5 个不适合 `aiops-platform` 的例子。
- Claude 官方 docs 和 OpenAI Codex manual 都有引用链接和引用日期。
- 文档明确 dynamic workflows 可以作为人工使用的外部研究/拆解工具，但不是 worker plugin、runner mode 或平台集成目标。
- 文档明确建议 dynamic workflows 通常用于一次性复杂探索，`aiops-platform` 用于长期 tracker backlog。

### 2. Promote the GitHub maker/reviewer harness into a reusable governance template

建议标签：`type:docs`, `area:workflow`, `priority:p1`

背景：

最新 `main` 已有 `docs/runbooks/github-maker-reviewer-automerge-e2e.md`、`examples/github-maker-WORKFLOW.md` 和 `examples/github-reviewer-automerge-WORKFLOW.md`。这证明项目已经形成更强的治理模式：maker 不能审自己的 PR，reviewer 用独立身份和 workspace 审查并启用 GitHub native CI-gated auto-merge。当前形态偏 release-validation runbook，下一步应该把它提炼成用户可复用模板。

这个候选仍然值得保留，因为它不是“新增能力”，而是把已经存在且已经验证过的 maker/reviewer dogfood 模式整理成清晰边界：worker 负责调度和观察，agent 身份负责 PR 操作，GitHub branch protection 负责最终 gate。

建议范围：

- 新增较短的 production governance guide，和长 E2E runbook 分离。
- 提炼 maker/reviewer 的最小配置、身份隔离、branch protection、required checks、label state machine 和失败恢复。
- 明确 maker/reviewer 模式的生成/验证闭环：maker 生成，reviewer 和 GitHub branch protection 提供独立验证与停止条件。
- README 增加从普通 GitHub workflow 升级到 maker/reviewer 模式的链接。
- 保留长 E2E runbook 作为验证证据和深度演练。

验收标准：

- governance guide 包含 GitHub identities、distinct `GH_CONFIG_DIR`、distinct workspace root、distinct `AIOPS_MIRROR_ROOT`、expected login、branch protection required checks、auto-merge setting、state labels、evidence checklist、failure recovery。
- guide 明确 worker/orchestrator 不写 PR、不 approve、不 merge、不 close issue。
- guide 链接示例 workflows、preflight scripts、长 E2E runbook 和 evidence checklist。

## 明确不纳入本报告候选

以下方向本报告不建议拆成 issue；不是“以后再开”的 backlog，而是现阶段不应把它们放进项目路线：dynamic workflow 集成或 handoff runbook、自建 Claude/Codex goal loop、goal-aware runner、worker 自动 push/open PR/merge、Postgres/分布式队列、泛 onboarding、没有真实运行数据支撑的 metrics taxonomy 或 pair-aware preflight。

## 落地原则

这份报告本身可以作为决策材料合并，不要求立即创建后续 issue。若要落地，先由维护者从上面两项里明确选择一个，并确认它服务当前项目节奏；做完后停止扩张。只有真实 dogfood 运行反复暴露同一类问题时，才重新评估是否需要新增工程 issue。

## 参考资料

- 本仓库：`README.md`、`AGENTS.md`、`DEVIATIONS.md`、`docs/architecture.md`、`docs/security-posture.md`、`docs/runbooks/*`、`.github/workflows/*`。
- Claude Code workflows: <https://code.claude.com/docs/en/workflows.md>
- Claude Code goal: <https://code.claude.com/docs/en/goal.md>
- OpenAI Codex manual, Goal mode: <https://developers.openai.com/codex/prompting#goal-mode>
- OpenAI Codex manual, app `/goal`: <https://developers.openai.com/codex/app/commands#set-or-manage-a-goal-with-goal>
- OpenAI Codex manual, CLI `/goal`: <https://developers.openai.com/codex/cli/slash-commands#set-or-view-a-task-goal-with-goal>
