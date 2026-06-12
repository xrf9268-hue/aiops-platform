# PRD: issue #782 — e2e 正向回路：脚本化 agent 推分支、开 PR、翻 label

Issue: https://github.com/xrf9268-hue/aiops-platform/issues/782 (P1, type:test)
方案以 issue 正文为准。机制核实见 research/e2e-harness-facts.md。

## Problem

`test/e2e` 只断言负边界（worker 没推分支、没开 PR）；mock runner 不提交不写
tracker——agent 推分支/开 PR/翻 `aiops/*` label、worker 观察 handoff 并
reconcile/清理的正半环路，全仓没有任何自动化测试（v0.1.0 时仅手工验证过一次）。

## Design（裁决）

新 e2e 测试 `TestGiteaScriptedAgentLoop_PositiveHandoff`（建议名）放入现有
Gitea 套件，复用 `runGiteaWorkerTask` 的装配模式但用**脚本化 agent**：

1. **载体**：`agent.default: claude` + `claude.command: <测试生成的 sh 脚本>`
   → `ShellRunner` 在 workspace 内 `sh -c "<command> < .aiops/PROMPT.md"`。
   脚本由测试写到 TempDir（0755），token/baseURL/owner/repo/issue 号直接烘焙
   进脚本文本（agentEnv deny list 不透传 GITEA_TOKEN，这正是 SPEC §1 的点）。
2. **脚本动作**（`set -euo pipefail`，任一步失败→runner error→测试可见）：
   - `git checkout -b agent/<issue>`；写一个文件；`git -c user.name=... -c
     user.email=... commit`；
   - `git push <带 bot basic-auth 的 cloneURL> HEAD:agent/<issue>`
     （显式 URL 推送，不依赖 workspace remote 的凭证形态）；
   - `curl -sf -X POST {base}/api/v1/repos/{o}/{r}/pulls` 开 PR
     （head=agent/<issue>, base=main）；
   - curl 把 issue 的 `aiops/todo` 翻成 `aiops/done`（labels API）。
3. **fixture**：新增 `test/e2e/fixtures/scripted-agent.md`（kind: gitea、
   agent.default: claude、claude.command 占位符由测试替换/或模板化注入；
   沿用 mock-happy.md 的 clone_url 验证器注记）。
4. **断言**（正负都保留）：
   - 任务成功（events.taskSucceeded）；
   - 远端存在 `agent/<issue>` 分支（getBranch == true）；
   - 恰好 1 个 open PR，head 为 agent 分支（worker 没另开）；
   - issue 标签为 `aiops/done`（agent 翻的，worker 没回写）；
   - worker 的 `ai/<n>` work branch 没被推送（保留 happypath 负断言）；
   - 用 `NewPollerWithReconciliation` 再 PollOnce：terminal 后 workspace
     目录被清理、`orch.Snapshot().Running == 0`（"observes handoff,
     reconciles, cleans"）。
5. 缺的 Gitea 测试床 helper（如 getIssueLabels、createPR 用不到——脚本自己
   curl）只加在 e2e build-tag 文件里。

## Acceptance criteria（来自 issue）

- [ ] 现有 Gitea 套件里有 scripted-agent e2e 变体：fixture 扮演的 agent 真实
      commit、push 分支、经 Gitea API 翻 `aiops/*` label（来自 workspace 内）。
- [ ] 断言：worker 观察 handoff、reconcile、清理 workspace，且自身从不写
      tracker 状态或 PR（负断言保留）。
- [ ] 纯测试侧（fixture 扮演 agent）——无 SPEC §1 边界变更。

## Constraints

- 只动 `test/e2e/**`（含 fixtures）。`gitea.go`（无 _test 后缀但带 e2e tag）
  若需扩展，先验证 `TestProductionGoFilesStayWithinSizeBudget` 是否计入它；
  若计入且超限，helper 放进新的 `*_test.go`。
- 本地必须实跑 `go test -tags e2e -race -timeout 15m ./test/e2e/...` 全绿
  （需 Docker；e2e 不在 CI 门禁内，PR body 记录本地证据：命令 + 输出尾部）。
- mutation-verify（规则 6，对已提交产物）：例如把脚本的 push 步骤去掉 →
  分支断言必须红；把 label 翻转去掉 → label/清理断言必须红。
- 分支 `fix/782-e2e-scripted-agent-loop`，PR body 带 `Closes #782`；
  生产 diff≈0（全测试侧）→ within budget。
