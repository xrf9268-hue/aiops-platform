# PRD: issue #781 — doctor: Gitea/GitHub tracker preflight + 删除 services[] 错误文案

Issue: https://github.com/xrf9268-hue/aiops-platform/issues/781 (P1, hardening)
方案以 issue 正文为准。证据已现场核实（见 research/doctor-tracker-preflight-facts.md）。

## Problem

Gitea/GitHub operator 拿着坏 token 或漏配 `tracker.api_key` 也能通过
`worker --doctor`，只能在每次 poll 的日志错误里发现问题。doctor 对非 linear
kind 一律 WARN 跳过。另：Linear 检查的错误提示还在指向 #573 已删除的
`services[]` schema，按提示写的配置会被 loader 静默拒绝。

## Changes

1. `internal/doctor/doctor.go` `BuildReport`：`r.checkLinear(...)` 改为
   `r.checkTracker(...)`，按 `cfg.Tracker.Kind` 分派：
   - `linear` → 现有 `checkLinear` 逻辑（去掉函数内的 kind WARN 短路）；
   - `gitea` → 新 `checkGiteaTracker`；
   - `github` → 新 `checkGitHubTracker`；
   - default → WARN（loader 已拒绝未知 kind，防御性兜底）。
2. `checkGiteaTracker`（镜像 `checkLinear` 的三段结构）：
   - `tracker.api_key` 解析后为空 → FAIL "Gitea API key"，remediation 指向
     在 WORKFLOW.md 设 `tracker.api_key: $GITEA_TOKEN`；
   - mock 模式 → PASS "present; live auth skipped in mock mode"；
   - real 模式 → `GET {base}/api/v1/user`（`Authorization: token <key>`）做
     auth，`GET {base}/api/v1/repos/{owner}/{name}/labels?limit=1` 做
     repo+label visibility。base 解析镜像 `cmd/worker/main.go:499`
     （`gitea.BaseURLFromTrackerConfig` + `GITEA_BASE_URL` fallback）；
     owner/name 取 `cfg.Repo`，为空则 FAIL。
3. `checkGitHubTracker` 同构：base = `cfg.Tracker.Endpoint` →
   `GITHUB_API_BASE_URL` → `https://api.github.com`（镜像 main.go:504-509）；
   `GET {base}/user` + `GET {base}/repos/{owner}/{name}/labels?per_page=1`，
   `Authorization: Bearer <key>`。检查名用 "GitHub tracker …" 与既有
   "GitHub agent …" 区分。
4. 所有新探针沿用 `decodeLinearProjectProbe` 的 drain 模式
   （`defer tracker.DrainAndClose(resp)`，#771/#762 class）。
5. `checkLinearGraphQL` 错误文案删除 `or services[].tracker.project_slug`。

## Tests（AGENTS.md clean-code 规则 6/9/11）

- gitea/github 各覆盖：空 api_key → FAIL；mock 模式 → PASS（skip 文案）；
  real 模式 httptest：断言 Authorization 头格式（token / Bearer）、两个端点
  路径命中、PASS；非 2xx → FAIL。镜像
  `TestBuildReportRealModeAuthenticatesLinearProject` 模式。
- 错误文案变更：grep 测试里 pin 旧文案者同步更新。
- 失败输出带 input/actual/expected（规则 9）。
- mutation-verify 对已提交产物：删 kind 分派 / 删 drain / 改 auth 头，对应
  测试必须红（规则 6，提交后做）。

## Acceptance criteria（来自 issue）

- [ ] doctor 校验 gitea/github tracker kind：空解析 `tracker.api_key` → FAIL；
      `--mode=real` 下用 token 探测 tracker API（Gitea `/api/v1/user`、GitHub
      `/user`）+ repo/label visibility，镜像 Linear auth+project 检查。
- [ ] 错误信息中 `services[].tracker.project_slug` 子句删除。
- [ ] 边界注记：doctor 是 operator preflight（D31 的 #542 注记），非
      agent-output gate——无 SPEC §1 问题，无需新 DEVIATIONS 行。

## Constraints

- 只动 `internal/doctor/`；不加新 WORKFLOW.md/Config key、不加 worker/
  orchestrator phase（PR 模板 SPEC alignment 勾第一项）。
- funlen ≤80 / gocognit ≤10（新代码无 nolint）；doctor_tracker.go ≤800 行。
- 分支 `fix/781-doctor-tracker-preflight`，PR body 带 `Closes #781`，
  目标 within budget（生产 diff 预计 2 文件 ~150 LOC）。
