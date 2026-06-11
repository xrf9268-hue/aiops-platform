# web-todo-app full lifecycle test with packaged release binary

## Goal

用最新打包发布的 worker 二进制（release v0.1.0, darwin_arm64）对一个真实目标仓库
（新设计的 web todo app）执行完整 Symphony 生命周期测试：tracker 轮询 → 候选选取 →
工作区准备 → codex agent 运行 → agent 自行开 PR / 迁移 issue 状态 → reconcile →
全部 issue 跑完。验证打包产物在真实负载下端到端可用。

## What I already know

* 最新 release：v0.1.0（2026-06-10），资产含 `aiops-platform_v0.1.0_darwin_arm64.tar.gz`。
* e2e 套件使用 `gitea/gitea:1.26.1-rootless` 容器 → 本测试同样用本地 Gitea，不碰真实 Linear。
* 宿主机是 16GB Mac mini，内存受限（memory: project_e2e_host_memory_constrained）——codex 子进程是内存大头，必须压低并发。
* worker 是 scheduler/runner + tracker reader；PR 创建与 issue 状态迁移由 agent 通过 dynamic tools 完成（SPEC §1 / #76）。
* `active_states` 必须排除 agent 的 handoff/done 状态，否则触发 SPEC §7.1 无界续跑（memory: project_active_states_must_exclude_handoff_states）。
* codex pin = 0.137（`CodexProtocolVersion`），doctor 会对版本不符告警。

## Decisions (ADR-lite)

* **Tracker = 本地 Docker Gitea**：与 e2e 基线一致、可控、可重复；Linear 会污染真实工作区。
* **Agent = 真实 codex（codex-app-server）**：用户要求"实际的"完整生命周期；mock 无法验证 agent 侧 PR/状态迁移。并发上限取低值（≤2）。
* **二进制 = release 打包产物**，不用 `go run`/本地 build——这正是本测试的对象。

## Requirements

* 设计一个 web todo app（简单、自包含、issue 可并行拆分），目标仓库初始为最小骨架 + WORKFLOW.md。
* 在 Gitea 上创建 ≥10 个 issue，覆盖 app 的功能切片。
* 用 v0.1.0 打包二进制运行 worker，跑完所有 issue 的完整生命周期。
* 记录并核验：每个 issue 的 dispatch → agent run → PR 创建 → issue 状态迁移 → reconcile 行为。

## Acceptance Criteria

* [x] worker 二进制为 release v0.1.0 资产解包所得（校验版本输出）。
* [x] Gitea 上 ≥10 个 issue 全部被 worker 认领并跑完生命周期。
* [x] 每个完成的 issue 有对应 PR（agent 创建，非 worker）。
* [x] 无 stuck run / 无无界续跑（active_states 配置正确）。
* [x] 全程记录测试报告（issue → run → PR → 状态 对照表）。

## App 设计：web-todo（Go stdlib + vanilla JS）

技术选型动机：codex 在沙箱内可能无网络，**零外部依赖**是硬约束 —— Go stdlib HTTP
服务器 + JSON 文件持久化 + 原生 HTML/CSS/JS 前端，无 npm/无第三方 Go 模块，
`go test ./...` 即可验证。

* `cmd/server/` — Go HTTP 服务器：静态文件 + `/api/todos` REST
* `internal/store/` — Todo 模型 + JSON 文件存储
* `web/` — index.html / app.js / style.css

### Issue 清单（12 个）

| # | 标题 | 范围 |
|---|------|------|
| 1 | Scaffold Go HTTP server and project layout | cmd/server, web/ 占位, go.mod |
| 2 | Todo data model and JSON file store with tests | internal/store CRUD + 单测 |
| 3 | REST API: list and create todos | GET/POST /api/todos |
| 4 | REST API: update, toggle and delete todos | PATCH/DELETE /api/todos/{id} |
| 5 | Frontend: render todo list and add form | web/index.html + app.js |
| 6 | Frontend: toggle complete and delete interactions | app.js 事件绑定 |
| 7 | Frontend: filter all/active/completed | 过滤状态 + URL hash |
| 8 | Frontend: edit todo title in place | 双击编辑 |
| 9 | Responsive CSS and dark mode | style.css + prefers-color-scheme |
| 10 | API validation and error handling | 空标题 400、未知 id 404、坏 JSON 400 + 测试 |
| 11 | Todo due dates with overdue highlighting | 模型字段 + API + UI |
| 12 | README and Makefile | run/test/build 文档化 |

依赖关系靠**串行推进 + 及时合并 PR** 化解（并发 ≤2，PR 到达后尽快合并，参照
docs/runbooks/batch-issue-processing.md 的 one-issue-per-PR 协议）。

## Out of Scope

* 不修改 aiops-platform 产品代码（除非测试暴露 bug，另开 issue 跟踪）。
* 不做 PR 自动合并的批处理优化（按需手动/脚本合并以推进生命周期即可）。
* 不测 Linear tracker。

## Technical Notes

* 研究材料见 `research/`。
* runbooks: batch-issue-processing.md, codex-app-server-docker.md, local-dev.md。
