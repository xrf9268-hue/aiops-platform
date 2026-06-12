# Research: e2e scripted-agent positive loop (#782)

现场核实日期：2026-06-12。基线 origin/main@3313f01。

## e2e harness 现状

- `test/e2e/`（build tag `e2e`）共 4 个测试函数，全部 Gitea + mock runner；
  `TestGiteaMockLoop_HappyPath`（happypath_test.go:24-58）只断言负边界：
  worker 没推 work branch、没开 PR。
- harness 是**进程内**装配，不是二进制：`orchestrator.New` +
  `orchestrator.WorkerTaskDispatcher{Config: worker.Config{WorkspaceRoot,
  MirrorRoot, Workflow}}` + `gitea.NewTrackerClient` + `NewPoller(...).PollOnce`。
- `runGiteaWorkerTask`（happypath_test.go:165+）：createRepo → putFile
  WORKFLOW.md → createIssue → ensureLabels/addIssueLabels(aiops/todo) →
  装配 → PollOnce → pollUntil taskSucceeded。fixture 经
  `writeE2EServiceWorkflow(t, fixtureContent, cloneURL)` 注入 clone_url。
- reconcile 测试（同文件:59+）用 `NewPollerWithReconciliation(client, orch,
  ReconciliationConfig{ActiveStates, TerminalStates, WorkerExitTimeout})`，
  label 翻转后再 PollOnce 触发取消。
- Gitea 测试床 helpers（gitea.go）：`bed.gitea.botUser/botToken/baseURL`、
  `createRepo`（返回带认证的 cloneURL）、`putFile`、`createIssue`、
  `ensureLabels`、`addIssueLabels`、`replaceIssueLabels`、`getBranch`、
  `listOpenPRs`、`getIssueLabels`(待确认，没有就加)。

## 脚本化 agent 的载体（裁决）

`internal/runner/runner.go:123`：`case "claude": return ShellRunner{Name:
"claude"}`。`ShellRunner.Run`（shell.go:22-74）：
`exec.CommandContext(ctx, "sh", "-c", command+" < .aiops/PROMPT.md")`，
`cmd.Dir = in.Workdir`（即 workspace），env 走 `agentEnv`（passthrough
deny list——GITEA_TOKEN 不透传）。
→ fixture 设 `agent.default: claude` + `claude.command: <测试写的脚本路径>`，
脚本即"agent"。**token/baseURL/issue 号由测试烘焙进脚本文本**（test-side，
不依赖 env passthrough）。`claude.command` 支持整值 $VAR 展开（expand.go），
但直接写字面路径最简单。注意 workspace 内 `.aiops/PROMPT.md` 存在，
sh -c 的 stdin 重定向要求它在（workspace prepare 阶段写入）。

## 脚本要做的事（issue 验收）

workspace 内：`git checkout -b agent/<n>` → 改文件 → `git -c user.email=...
-c user.name=... commit` → `git push origin agent/<n>`（remote origin 的
cloneURL 已带 bot basic-auth，无需另配凭证——待实现时验证 mirror/clone
机制是否保留认证 URL；若 origin 指向本地 mirror，则 push 用测试烘焙的
认证 URL 显式推 gitea）→ `curl -sf -X POST {base}/api/v1/repos/{o}/{r}/pulls`
开 PR → `curl -sf -X PUT/DELETE labels` 把 aiops/todo 翻成 aiops/done
（或 human-review，按 reviewer 流派；用 done 可顺带触发 terminal 清理）。
脚本任何一步失败要 `set -e` 退出非零 → runner error → 测试失败可见。

## 断言（正负都保留）

1. 任务成功（taskSucceeded 事件路径，复用 happypath 模式）。
2. 远端存在 agent 分支（`getBranch(owner, repo, "agent/<n>")` == true）。
3. 恰好 1 个 open PR 且 head 是 agent 分支（worker 自己没开第二个）。
4. issue label 现为 aiops/done（agent 翻的；worker 没改回/没另写）。
5. worker 负边界保留：worker 的 `ai/<n>` work branch 未被推送
   （happypath_test.go:44 同款断言）。
6. handoff 观察 + 清理：label 进 terminal 后用 NewPollerWithReconciliation
   再 PollOnce，断言 workspace 目录被清理（WorkspaceRoot 下该 task 目录
   消失）+ orch Snapshot Running==0。（"worker observes the handoff,
   reconciles, cleans the workspace"）

## 边界

纯测试侧（fixture + 脚本扮演 agent），零生产代码改动 → 无 SPEC §1 问题，
PR 模板 SPEC alignment 勾第一项；测试文件不计 size budget。
若 helper 缺（getIssueLabels 等）只加在 test/e2e/gitea.go（build tag e2e，
也不计预算——待验证 build tag 文件是否被 budget 测试排除；它按
non-test .go 计数，gitea.go 无 _test 后缀但有 e2e build tag——需现场跑
TestProductionGoFilesStayWithinSizeBudget 验证）。

## 运行

`go test -tags e2e -race -timeout 15m ./test/e2e/...`，需 Docker
（gitea/gitea:1.26.1-rootless）。e2e 不在 CI 默认门禁里（CI gate 清单无
-tags e2e 行）——本地必须全程跑通并在 PR body 记录证据。
