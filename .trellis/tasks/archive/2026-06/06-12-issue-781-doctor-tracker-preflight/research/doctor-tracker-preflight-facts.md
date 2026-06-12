# Research: doctor Gitea/GitHub tracker preflight (#781)

现场核实日期：2026-06-12。全部行号对应 origin/main@3313f01。

## 现状（证据成立）

- `internal/doctor/doctor_tracker.go:26-28`：`checkLinear` 对任何非 linear 的
  `tracker.kind` 直接 WARN（"Linear smoke checks skipped"）并返回——gitea/github
  没有任何 tracker 侧 auth/visibility 检查。
- `internal/doctor/doctor.go:74-86`（`BuildReport`）检查清单只挂了
  `checkLinear` / `checkCodex` / `checkGitHubAgent` / `checkSandbox`。
- `checkGitHubAgent`（doctor_tracker.go:45+）是 agent 侧 gh/git-push 校验，
  由 `--github-issue` 门控，与 tracker auth 无关。
- `doctor_tracker.go:158`（`checkLinearGraphQL` 内）：错误文案
  `"linear project_slug is required at tracker.project_slug or services[].tracker.project_slug"`
  —— `services[]` schema 已在 #573 (DEVIATIONS D25) 移除，loader
  (`internal/workflow/reject.go`) 会拒绝按该提示写的配置。

## Worker 侧解析链（doctor 探针必须镜像）

- Gitea（`cmd/worker/main.go:499-502`）：
  `baseURL := gitea.BaseURLFromTrackerConfig(cfg.Tracker, env("GITEA_BASE_URL", "http://localhost:3000"))`；
  owner/name 取 `cfg.Repo.Owner` / `cfg.Repo.Name`。
  `BaseURLFromTrackerConfig`（internal/gitea/config.go:11-19）：endpoint →
  legacy project_slug → fallback，尾斜杠剥除。
- GitHub（main.go:504-509）：`cfg.Tracker.Endpoint`，空则
  `env("GITHUB_API_BASE_URL", "https://api.github.com")`；owner/name 同上。

## 认证头格式

- Gitea：`Authorization: token <key>`（internal/gitea/tracker_client.go:461,523）。
- GitHub：`Authorization: Bearer <key>`（internal/tracker/github.go:293,366,682）。
- Linear：原样 APIKey（internal/tracker/linear.go:663）——已有检查，不动。

## 探针端点（issue 验收标准指定）

- Gitea auth：`GET {base}/api/v1/user`；repo/label visibility：
  `GET {base}/api/v1/repos/{owner}/{name}/labels?limit=1`（一次调用同时证明
  repo 可见 + label 可读，镜像 Linear 的 auth+project 两段式）。
- GitHub auth：`GET {base}/user`；visibility：
  `GET {base}/repos/{owner}/{name}/labels?per_page=1`。

## doctor 既有模式（新代码必须沿用）

- HTTP：`r.opts.HTTPClient`（normalize 默认 `&http.Client{Timeout: 10s}`，
  doctor.go:115-116）+ `http.NewRequestWithContext`；非 2xx 响应体必须
  `defer tracker.DrainAndClose(resp)`（#771/#762 drain class，模式见
  `decodeLinearProjectProbe`，drain 回归测试在 doctor_tracker_drain_test.go）。
- 检查三态：FAIL（空 api_key，mock/real 都查）→ mock 模式 PASS
  （"present; live auth skipped in mock mode"）→ real 模式 live probe
  （`r.realMode()`，doctor.go:332）。镜像 `checkLinear` 的分支顺序。
- 测试模式：`TestBuildReportRealModeAuthenticatesLinearProject`
  （doctor_test.go:39，httptest server + Authorization 断言 + findCheck）；
  helper `writeWorkflow(t, kind, key)` / `writeWorkflowWithEndpoint` 已存在。

## 命名约定

agent 侧检查叫 "GitHub agent gh auth" / "GitHub agent git push"——tracker 侧
新检查命名必须可区分："Gitea API key"/"Gitea auth"、
"GitHub tracker API key"/"GitHub tracker auth"。

## 体积与 lint 约束

- doctor_tracker.go 现 308 行（预算 ≤800）；doctor_test.go 1252 行（测试豁免）。
- 新函数 funlen ≤80 行、gocognit ≤10（blocking gate，新代码不得加 nolint）。

## 边界裁决（issue 已写明）

doctor 是 operator preflight（D31 的 #542 注记所接受的预防层），不是
agent-output gate——无 SPEC §1 问题，无需新 DEVIATIONS 行。PR 模板
SPEC alignment 勾第一项（无新 config key、无新 worker/orchestrator
phase/gate/artifact；doctor 不在 PR Metadata 的 SPEC-sensitive 路径列表）。
