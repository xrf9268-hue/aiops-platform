# PRD: issue #785 — GitHub tracker onboarding parity

Issue: https://github.com/xrf9268-hue/aiops-platform/issues/785 (P2, docs-only)
方案以 issue 正文为准。证据已于 2026-06-13 现场复核全部成立。

## Verified evidence (2026-06-13 复核)

- README 有 "Gitea issue-state labels" 节（现 ~L381+）与 Linear first-run
  runbook；`tracker.kind: github` 的状态模型只存在于
  `examples/github-local-WORKFLOW.md:11-14` 的 YAML 注释。
- GitHub 状态语义在 `internal/tracker/github.go`：
  - `githubIssueQueryForState`: open/closed/all 是特殊状态；其余 state
    名按 label 匹配（实现细节待写节时精读该函数确认大小写/映射规则）。
  - open-PR claim detection: `openPullRequestClaimedIssueNumbers` +
    `collectIssueFromBatch`（跳过被 open PR 认领且仍 open 的 issue；
    丢弃伪装成 issue 的 PR）。
- README quick start 不链接 per-tracker example 文件。
- README 从未说明 GitHub token 最小 scope。tracker.api_key 只做只读轮询
  （list issues + list open PRs）→ fine-grained PAT: Issues read +
  Pull requests read + Metadata read；classic PAT: public_repo/repo。
  （写节前再对照 doctor 文案与 docs/runbooks/github-local-automation.md，
  避免与现有表述冲突。）

## Changes

1. README 加 "GitHub issue-state labels" 节，与 Gitea 节平行：
   open/closed/all 特殊状态、label-as-state 语义、aiops:ready 约定
   （引 ADR 0002）、open-PR claim skip。
2. 同节一句话最小 token scopes。
3. Quick start 链接 `examples/github-local-WORKFLOW.md` 与
   `examples/gitea-WORKFLOW.md`。

## Acceptance criteria（照 issue）

- [ ] README GitHub 节（与 Gitea 节平行）。
- [ ] 最小 token scopes 一句话。
- [ ] Quick start 链接两个 per-tracker example。

## Gate

docs-only；within budget；PR body 带 `Closes #785`。
与 #783/#784 串行（共享 README.md）；前序合并后 rebase 复跑门禁。
