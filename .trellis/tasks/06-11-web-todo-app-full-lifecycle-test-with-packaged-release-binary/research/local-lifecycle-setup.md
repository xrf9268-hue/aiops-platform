# Research: 本机 darwin arm64 用打包 worker 二进制 + 本地 Docker Gitea + 真实 codex agent 跑完整 Symphony 生命周期

- **Query**: WORKFLOW.md schema / Gitea 接入 / codex 要求 / worker 启动 / e2e Gitea 启动序列 / 并发控制键
- **Scope**: internal (本仓库代码与文档)
- **Date**: 2026-06-11

---

## 0. 打包 release 二进制是什么

`.github/workflows/release.yml:145-186`：每个 release tag 构建 `worker` + `tui` 两个二进制，目标含 `darwin arm64`，打包为
`dist/aiops-platform_<tag>_darwin_arm64.tar.gz`（`CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" ./cmd/worker ./cmd/tui`），并附 SBOM + provenance attestation。解包后直接得到可执行 `worker`。

---

## 1. WORKFLOW.md front matter 全部可用键与默认值

来源：`internal/workflow/config.go`（结构体 + `DefaultConfig()` L440-506）、`internal/workflow/expand.go`（加载期默认/派生）、README.md L256-275 配置表。

### repo（config.go:126-131）
| 键 | 默认 | 备注 |
|---|---|---|
| `repo.owner` | 无 | Gitea/GitHub tracker 轮询必填（tracker_client.go:102-104） |
| `repo.name` | 无 | 同上 |
| `repo.clone_url` | 无 — **worker 运行时必填**（cmd/worker/main.go:304-312 校验 `repo.clone_url is required for poll-based worker runtime`） | 支持 `$VAR` env 展开（expand.go:119-123）；嵌入 basic-auth 即 agent 的 push+PR 凭证 |
| `repo.default_branch` | `main`（expand.go:124-126） | |

### server（config.go:101-118）
| 键 | 默认 |
|---|---|
| `server.host` | `127.0.0.1`（DefaultConfig，config.go:442） |
| `server.port` | `4000`；`-1` 禁用 HTTP/dashboard（README L265） |

### tracker（config.go:133-177; DefaultConfig config.go:454-463）
| 键 | 默认 | 备注 |
|---|---|---|
| `tracker.kind` | 无 — **必填**，允许 `gitea`/`github`/`linear`（README L267；loader 拒绝空值） |
| `tracker.api_key` | 无 | 推荐 `api_key: $GITEA_TOKEN`，loader 启动时展开（expand.go:18-25；env.go:22-32）。env 未设→启动即 fail `missing_tracker_api_key`。**worker 不直接读 GITEA_TOKEN env**（local-dev.md:80-90, 284-318） |
| `tracker.endpoint` | linear: `https://api.linear.app/graphql`；gitea/github: 空时回落 env（main.go:499 `GITEA_BASE_URL`，默认 `http://localhost:3000`） | `base_url` 是已废弃别名 |
| `tracker.active_states` | `[Todo, In Progress]`（config.go:455） | **Gitea 必须覆盖为 `[AI Ready, Rework]` 之类**，见 §2 标签映射 |
| `tracker.terminal_states` | `[Closed, Cancelled, Canceled, Duplicate, Done]`（config.go:456） | Gitea 建议 `[Done, Canceled]` |
| `tracker.inactive_states` | gitea kind 默认推断 `[Human Review]`（main.go:466-475） | 非终态暂停 |
| `tracker.required_labels` | `[]`（关） | 派单门 |
| `tracker.poll_interval_ms` | `30000`（config.go:457）；与 `polling.interval_ms` 互通（expand.go:62-68，polling 优先） |
| `tracker.pagination_max_pages` | gitea 适配器默认 20 页 ×50 条（tracker_client.go:20-23） |
| `tracker.statuses.{in_progress,human_review,rework}` | `In Progress` / `Human Review` / `Rework`（config.go:458-462） |
| `tracker.team_key` / `project_slug` | Linear 专用 |

### polling（config.go:212-214）
`polling.interval_ms` 默认 `30000`。

### workspace / hooks（config.go:228-261）
| 键 | 默认 |
|---|---|
| `workspace.root` | `<os.TempDir()>/symphony_workspaces`（config.go:75-77, 467）；支持 `~/` 展开（expand.go:143-154）。**per-boot tmp，持久化要显式设** |
| `hooks.{after_create,before_run,after_run,before_remove}` | 空；scalar/list/object 三种写法（config.go:267-296） |
| `hooks.timeout_ms` | `60000`（config.go:465, 327-329） |
| `hooks.env_passthrough` | 空（hooks 只见 POSIX baseline） |

### agent（config.go:333-345; DefaultConfig:468-475）
| 键 | 默认 | 备注 |
|---|---|---|
| `agent.default` | `mock` | 取值 `mock` / `codex-app-server` / `claude`（runner.go:115-124） |
| **`agent.max_concurrent_agents`** | `10` | **全局并发上限 — 16GB 内存约束设 1-2 的就是这个键**（main.go:357 直接喂给 `NewOrchestratorState`） |
| `agent.max_concurrent_agents_by_state` | 无 | 按状态名（大小写/空白归一）每态上限；≤0 条目被丢弃（normalize.go） |
| `agent.max_turns` | `20` | 单 session 轮数 |
| `agent.max_continuation_turns` | `20` | issue 级 clean-turn 预算，耗尽 park 为 blocked（D34） |
| `agent.max_retry_backoff_ms` | `300000` |
| `agent.timeout` | `30m`（duration 字符串如 `10m`） |

### codex（config.go:347-376; DefaultConfig:476-500）
| 键 | 默认 |
|---|---|
| `codex.command` | `codex app-server` |
| `codex.env_passthrough` | 空（baseline 仅 PATH/HOME/USER/LANG/LC_*/TZ/TERM，runner/env.go:11-20；tracker/repo token 名被策略拒绝，agent_env_policy.go:11-14） |
| `codex.approval_policy` | `granular:` 全 false = 自动拒绝所有审批（expand.go:100-115，#329） |
| `codex.thread_sandbox` | `workspace-write`（expand.go:88-90） |
| `codex.turn_sandbox_policy` | 未设时从 thread_sandbox 派生（expand.go:97-99；D32）；显式设置需带全字段 `type/writableRoots/networkAccess/excludeTmpdirEnvVar/excludeSlashTmp`（codex-app-server-docker.md:340-348） |
| `codex.turn_timeout_ms` / `read_timeout_ms` / `stall_timeout_ms` | `3600000` / `5000` / `300000` |
| `codex.linear_graphql.{allow_mutations,allowed_mutations}` | mutation 默认关（Linear 专用） |

### 其余顶层
- `claude.command` 默认 `claude`（config.go:501）
- `policy.mode` 默认 `draft_pr`（或 `analysis_only`）；path/diffstat gate 已在 #561 删除
- `sandbox.*`（worker 侧进程加固）默认 `enabled: false, backend: none, network: none`（config.go:503）
- `verify.commands` 默认空 — 只渲染进 prompt，worker 不执行（config.go:432-438）

无 front matter 的 WORKFLOW.md 合法：body 即 prompt，其余全默认（`source: prompt_only`）。

---

## 2. Gitea tracker 接入

来源：`internal/gitea/tracker_client.go`、`internal/gitea/label_state.go`、`cmd/worker/main.go:494-514`、`docs/runbooks/gitea-bot-and-branch-protection.md`、`docs/runbooks/local-dev.md:245-263`。

### 连接方式
- worker 端构造：`tracker.kind: gitea` → `gitea.NewTrackerClient(cfg.Tracker, baseURL, cfg.Repo.Owner, cfg.Repo.Name)`（main.go:498-502）。
- baseURL 优先级：`tracker.endpoint` → 遗留 `project_slug` → env `GITEA_BASE_URL` → `http://localhost:3000`（gitea/config.go:11-19; main.go:499）。
- 认证：`Authorization: token <tracker.api_key>`（tracker_client.go:430,489）。token 经 WORKFLOW.md `api_key: $GITEA_TOKEN` 注入。
- token scope：`GITEA_TOKEN` 只需 `write:issue`（含 read，供轮询 + `gitea_issue_labels` 工具）；**不需要** `write:repository`（gitea-bot-and-branch-protection.md:78-90）。

### 状态表示（Gitea 无原生状态字段 → aiops/* 标签）
`DefaultStateLabelMappings()`（label_state.go:38-47），顺序即冲突优先级：

| Gitea 标签 | workflow 状态 |
|---|---|
| `aiops/rework` | `Rework` |
| `aiops/in-progress` | `In Progress` |
| `aiops/human-review` | `Human Review` |
| `aiops/todo` | **`AI Ready`** |
| `aiops/done` | `Done` |
| `aiops/canceled` | `Canceled` |

**关键陷阱**：默认 `active_states: [Todo, In Progress]` 中的 "Todo" 不在 Gitea 映射里（`aiops/todo` 映射成 "AI Ready"），所以 Gitea workflow **必须显式** `active_states: [AI Ready]`（可加 Rework）。无 aiops/* 标签的 issue 直接被忽略（label_state.go:94-99 diagnostic `missing_aiops_state_label`）。

### 前置条件
1. repo 中创建标签 `aiops/todo`、`aiops/done`（至少），完整集合见上表。
2. 待跑 issue 打上 `aiops/todo`。
3. `repo.owner`/`repo.name` 必填（tracker_client.go:102-104）。
4. 运行中把 issue 换成 `aiops/done`/`aiops/canceled` → 下一 poll tick reconcile 取消运行（happypath_test.go:60-140 验证此行为）。
5. `active_states` 含终态时 API 查询用 `state=all`，否则 `open`（tracker_client.go:645-656）。
6. 阻塞依赖：issue body 写 `Depends on #N`（tracker_client.go:517-523）。

### 双凭证模型（gitea-bot-and-branch-protection.md:41-95）
- **`GITEA_TOKEN`（orchestrator 持有）**：轮询 + `gitea_issue_labels` 工具服务端代签；永不进 agent 子进程 env。
- **`repo.clone_url` 嵌入 basic-auth（agent 的 push+PR 凭证）**：workspace origin 即该 URL，agent `git push origin <branch>` 和 `POST /api/v1/repos/{owner}/{repo}/pulls` 都用它，需 `write:repository`。**没有 orchestrator 侧 PR 代理**——`internal/gitea.CreatePullRequest` 已定义但无生产调用路径（runbook:57-60），prompt 必须教 agent 用 clone_url 凭证 curl Gitea API 开 PR。

---

## 3. codex agent 运行要求

来源：`internal/runner/`、`docs/runbooks/codex-app-server-docker.md`、`docs/runbooks/binary-deployment.md`。

### 版本与配置键
- 选择 runner：`agent.default: codex-app-server`（runner.go:115 常量 `NameCodexAppServer`）。
- 启动命令：`codex.command`，默认 `codex app-server`。
- **版本 pin `0.137.0`**：`internal/runner/codex_version.go:10` `CodexProtocolVersion = "0.137.0"`；schema contract test 验证 runner 发出的每个 payload；doctor 对任何 ≠ pin 版本告警。本机安装应装 codex-cli 0.137.0（macOS：官方安装器或 release 包）。

### 认证（codex-app-server-docker.md:106-137）
两种模式选一：
1. **ChatGPT/Codex 登录（默认）**：`codex --login` 后凭证存 `$CODEX_HOME/auth.json`（默认 `~/.codex`），必须可写（0.137 原地刷新 token）。本机 darwin 直接复用已登录的 `~/.codex` 即可。
2. **API key**：`OPENAI_API_KEY` + WORKFLOW.md `codex.env_passthrough: [OPENAI_API_KEY]` 显式放行（worker 默认不传）。

验证：`codex login status`；`worker --doctor --mode=real --deploy=binary` 做 JSON-RPC initialize stdio 探针。
模型配置在 `$CODEX_HOME/config.toml`（如 `model = "gpt-5-codex"`），非 WORKFLOW.md。

### 沙箱（darwin 本机）
- `codex.thread_sandbox: workspace-write` 默认；macOS 用 Seatbelt（bwrap 是 Linux 问题，binary-deployment.md 的 userns 段不适用 darwin）。
- **全生命周期必须给 turn sandbox 联网**（git push / curl 开 PR / 包管理都在 turn sandbox 内跑；host-direct workspace-write 默认 `networkAccess: false`，codex-app-server-docker.md:331-348）：

```yaml
codex:
  turn_sandbox_policy:
    type: workspaceWrite
    writableRoots: []
    networkAccess: true
    excludeTmpdirEnvVar: false
    excludeSlashTmp: false
```

### Dynamic tools（agent 拿到的工具）
- `thread/start` 的 `dynamicTools` 字段携带（codex_app_server.go:510-529，实验 API，schema 必须 `--experimental` 生成）。
- `DynamicToolsForWorkflow`（tools.go:203-259）：**gitea kind 只注册一个工具 `gitea_issue_labels`**（条件：kind=gitea 且 api_key、repo.owner/name、baseURL 全非空，tools.go:242-257）。输入 `{issue_number, labels:[恰好一个 aiops/* 标签]}`，worker 进程服务端附 token 调 Gitea API，原子替换状态标签。
- Linear 的 `linear_graphql` / `linear_ai_workpad` 不适用 gitea。
- **PR 创建没有 dynamic tool** — agent 走 shell：`git push` + `curl -u <clone_url 凭证> POST /api/v1/repos/.../pulls`，由 prompt body 指挥。

### Agent 子进程环境
baseline allowlist `PATH/HOME/USER/LANG/LC_ALL/LC_CTYPE/TZ/TERM`（env.go:11-20）；`GITEA_TOKEN`/`GITHUB_TOKEN`/`LINEAR_API_KEY` 等被 `agent_env_policy.go` 硬性拒绝 passthrough。`CODEX_HOME` 非默认位置时需加进 `codex.env_passthrough`。

---

## 4. worker 启动方式

来源：`cmd/worker/main.go`、`docs/runbooks/local-dev.md`、`docs/runbooks/workspace-cache.md`、`docs/runbooks/binary-deployment.md`。

### CLI
```
worker [--port=N] [path-to-WORKFLOW.md]      # 运行模式
worker --print-config [--port=N] <workdir>   # 解析 <workdir>/WORKFLOW.md，输出 JSON（api_key 打码）；不读 AIOPS_WORKFLOW_PATH
worker --doctor [--mode=mock|real] [--deploy=binary|docker] [--go-test-dir=…] [path-to-WORKFLOW.md]
```
（main.go:24-50, 123-144, 152-166；本机二进制部署用 `--deploy=binary`）

### WORKFLOW.md 解析顺序（main.go:274-296）
1. CLI 位置参数 → 2. env `AIOPS_WORKFLOW_PATH`（遗留别名 `WORKFLOW_PATH`） → 3. **worker 进程 cwd 的 `WORKFLOW.md`** → 4. 内建默认（但运行模式因缺 `repo.clone_url` 直接报错）。
即：WORKFLOW.md 属于 **worker 侧**（你自己写一份指向目标仓库），不要求在目标仓库内 —— e2e 也是写到 tmp 再传路径（happypath_test.go:373-381）。

### Env vars
| 变量 | 作用 | 默认 |
|---|---|---|
| `AIOPS_WORKFLOW_PATH` | workflow 文件路径 | 无（fallback cwd） |
| `AIOPS_WORKSPACE_ROOT` | `workspace.root` 缺省时的 worktree 根 | 无（再 fallback `$TMPDIR/symphony_workspaces`） |
| `AIOPS_MIRROR_ROOT` | bare mirror 缓存 | macOS `~/Library/Caches/aiops-platform/mirrors`（workspace-cache.md:107-111） |
| `GITEA_BASE_URL` | `tracker.endpoint` 为空时的 base URL fallback | `http://localhost:3000`（main.go:499） |
| `GITEA_TOKEN` | 仅经 WORKFLOW.md `api_key: $GITEA_TOKEN` 生效 | — |
| `AIOPS_SERVER_HOST` / `AIOPS_STATE_API_TOKEN` | dashboard 绑定/鉴权 | 127.0.0.1 / 无鉴权（仅 loopback） |
（worker/env.go:11-18；遗留无前缀别名告警）

### 运行时观测
`http://127.0.0.1:4000/api/v1/state`、`/api/v1/{issue}`、`POST /api/v1/refresh`（头 `X-AIOPS-Refresh: true` 立即触发 poll+reconcile）、`/livez`、`/readyz`；`go run ./cmd/tui` 或解包的 `tui` 二进制。

### Workspace 布局
`$AIOPS_MIRROR_ROOT/<host>/<repo>.git`（bare mirror）+ `<workspace.root>/` 下 per-task worktree；agent 工作分支 `ai/<n>`（happypath_test.go:41）。worker 不 push 该分支（happypath_test.go:44-50 断言）。

---

## 5. e2e harness 的 Gitea 启动细节（可直接照抄）

来源：`test/e2e/gitea.go`（testcontainers，逻辑可逐条转成 docker CLI）。

镜像：`gitea/gitea:1.26.1-rootless`（gitea.go:31），暴露 3000/tcp。

启动序列：
1. **容器 env**（gitea.go:43-51）：
   - `GITEA__security__INSTALL_LOCK=true`（跳过安装向导）
   - `GITEA__security__SECRET_KEY=<random hex32>`
   - `GITEA__database__DB_TYPE=sqlite3`
   - `GITEA__server__DISABLE_SSH=true`
2. 等待 `GET /api/v1/version` 返回（90s 超时，gitea.go:52）。
3. **容器内 exec 建 admin**（env 方式 1.21+ 不生效，gitea.go:63-71）：
   `gitea admin user create --admin --username aiops-bot --password <pass> --email aiops-bot@example.invalid -c /etc/gitea/app.ini`
4. **basic-auth 换 token**（gitea.go:153-178）：
   `POST /api/v1/users/aiops-bot/tokens`，body `{"name":"e2e","scopes":["write:repository","write:admin","write:user","write:issue"]}`，取 `sha1` 字段。（生产最小化只要 `write:issue`；clone/push 另用 basic-auth 或独立 token。）
5. **建仓**：`POST /api/v1/user/repos` `{"name":…,"auto_init":true,"private":false}` → 响应 `clone_url`（gitea.go:182-203）。
6. **建标签**：`POST /api/v1/repos/{o}/{r}/labels` `{"name":"aiops/todo","color":"ededed"}`，409 视为已存在（gitea.go:262-282）。
7. **建 issue**：`POST /api/v1/repos/{o}/{r}/issues` `{"title","body"}`（gitea.go:223-244）。
8. **打标签**：`POST /api/v1/repos/{o}/{r}/issues/{n}/labels` `{"labels":["aiops/todo"]}`（gitea.go:284-298）；改状态用 `PUT` 全量替换（gitea.go:300-314）。

docker CLI 等价：
```bash
docker run -d --name gitea -p 3000:3000 \
  -e GITEA__security__INSTALL_LOCK=true \
  -e GITEA__security__SECRET_KEY=$(openssl rand -hex 32) \
  -e GITEA__database__DB_TYPE=sqlite3 \
  -e GITEA__server__DISABLE_SSH=true \
  gitea/gitea:1.26.1-rootless
# 就绪后：
docker exec gitea gitea admin user create --admin --username aiops-bot \
  --password <PASS> --email aiops-bot@example.invalid -c /etc/gitea/app.ini
```

e2e fixture 参考：`test/e2e/fixtures/gitea-worker.md`（active_states AI Ready/Rework、terminal Done/Canceled、polling 5000ms）。

---

## 6. 并发/容量控制键（16GB 约束）

- **`agent.max_concurrent_agents`**（config.go:335，默认 10）→ 设 `1`（或 2）。直接决定 orchestrator 同时运行的 agent 数（main.go:357）。
- `agent.max_concurrent_agents_by_state`（config.go:336）：可选每态细分上限。
- 辅助限流：`agent.max_turns`（单次会话轮数）、`agent.timeout`（单次 runner 调用墙钟）、`agent.max_continuation_turns`（issue 级总预算）。
- 记忆背景：内存大头是 codex 子进程（每 agent 一个 codex app-server + 其 shell 子命令），worker 本身 ~20MB。

---

## 最小可行配置清单

### WORKFLOW.md 草稿（worker 侧任意路径，如 `~/aiops-local/WORKFLOW.md`）

```yaml
---
repo:
  owner: aiops-bot
  name: todo-app
  # 嵌入凭证 = agent 的 push + 开 PR 凭证（write:repository）
  clone_url: http://aiops-bot:<PASSWORD_OR_TOKEN>@localhost:3000/aiops-bot/todo-app.git
  default_branch: main

tracker:
  kind: gitea
  endpoint: http://localhost:3000
  api_key: $GITEA_TOKEN          # 轮询 + gitea_issue_labels；需 write:issue
  active_states: [AI Ready, Rework]
  terminal_states: [Done, Canceled]
  inactive_states: [Human Review]

polling:
  interval_ms: 10000

workspace:
  root: ~/aiops-workspaces/todo-app

agent:
  default: codex-app-server
  max_concurrent_agents: 1       # 16GB 约束
  max_turns: 20
  timeout: 30m

codex:
  command: codex app-server
  thread_sandbox: workspace-write
  turn_sandbox_policy:           # darwin 本机：放行联网才能 git push / curl 开 PR
    type: workspaceWrite
    writableRoots: []
    networkAccess: true
    excludeTmpdirEnvVar: false
    excludeSlashTmp: false

policy:
  mode: draft_pr

verify:
  commands: []                   # 按目标仓库填，如 npm test
---
You are working on issue {{ task.id }} ({{ task.title }}) in {{ repo.owner }}/{{ repo.name }}, base branch {{ repo.branch }}.

{{ task.description }}

Workflow:
1. Implement and verify the change in this workspace.
2. Push your work branch: git push origin HEAD.
3. Open a pull request via the Gitea API using the origin remote credential:
   curl -s -X POST "http://localhost:3000/api/v1/repos/{{ repo.owner }}/{{ repo.name }}/pulls" \
     -u "aiops-bot:<PASSWORD_OR_TOKEN>" -H 'Content-Type: application/json' \
     -d '{"title":"…","body":"Closes #<issue>","head":"<your branch>","base":"{{ repo.branch }}"}'
4. Move the issue to review with the gitea_issue_labels tool:
   {"issue_number": <n>, "labels": ["aiops/human-review"]}
```

（注意：默认 inactive `Human Review` 会让 worker 在 review 期间不再派单——这正是防止 §7.1 continuation 死循环的正确终点状态。）

### 启动命令草稿

```bash
# 1) 解包 release
tar -xzf aiops-platform_v0.1.0_darwin_arm64.tar.gz && cd aiops-platform_v0.1.0_darwin_arm64

# 2) env（GITEA_TOKEN 经 WORKFLOW.md $GITEA_TOKEN 生效；GITEA_BASE_URL 仅当 endpoint 留空才需要）
export GITEA_TOKEN=<write:issue token>
export AIOPS_WORKFLOW_PATH=~/aiops-local/WORKFLOW.md
export AIOPS_WORKSPACE_ROOT=~/aiops-workspaces   # workspace.root 已设时仅作 fallback

# 3) 预检（本机二进制 + 真实 codex）
codex login status
./worker --doctor --mode=real --deploy=binary "$AIOPS_WORKFLOW_PATH"
./worker --print-config "$(dirname "$AIOPS_WORKFLOW_PATH")"   # 核对 resolution.source/path

# 4) 运行
./worker          # 或 ./worker ~/aiops-local/WORKFLOW.md

# 5) 观察
curl http://127.0.0.1:4000/api/v1/state
curl -X POST -H 'X-AIOPS-Refresh: true' http://127.0.0.1:4000/api/v1/refresh
./tui
```

## Caveats / Not Found

- **Gitea 没有 PR 创建 dynamic tool**：`internal/gitea.CreatePullRequest` 无生产调用路径；开 PR 完全依赖 prompt 教 agent 用 clone_url 凭证调 REST API。prompt 里凭证会出现在渲染文本中 —— 本地一次性 token 可接受，生产应换 SSH deploy key + tea/curl 用独立凭证文件。
- **Gitea 仅有 `gitea_issue_labels` 一个 tracker 写工具**，无评论工具；handoff 评论如需要也走 prompt+curl。
- 首次真实跑建议先 `agent.default: mock` 验证轮询/标签/reconcile 链路，再切 codex-app-server（AGENTS.md 安全姿势）。
- 本机 codex 版本若非 0.137.0，doctor 会 WARN；schema contract 是按 0.137 pin 的。
- `examples/gitea-WORKFLOW.md` 缺 `api_key` 行，照抄会启动成功但首次 poll 失败（local-dev.md:92-95）。
