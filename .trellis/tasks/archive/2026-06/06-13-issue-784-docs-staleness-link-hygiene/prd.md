# PRD: issue #784 — docs staleness + link hygiene

Issue: https://github.com/xrf9268-hue/aiops-platform/issues/784 (P2/P3, docs-only)
方案以 issue 正文为准。证据已于 2026-06-13 现场复核全部成立。

## Verified evidence (2026-06-13 复核)

1. `docs/runbooks/binary-deployment.md` ~L24: "runs the agent, enforces
   policy, and stops" — 但 worker 侧 policy gate 已在 #561/#574 移除
   （DEVIATIONS D33；`policy.mode` 现在只是 prompt-directive 选择器）。
2. `docs/runbooks/local-dev.md` ~L24: 同款 "enforces policy, and stops"。
   讽刺的是同文件 L30-31 自己写着 "If a doc ... is stale — file an issue"。
3. `docs/adr/0002-ready-gated-binary-self-hosting.md`：全仓 .md 零入链
   （grep 复核，排除自身与 .trellis）。
4. `docs/runbooks/gitea-bot-and-branch-protection.md`：同样零入链；
   是 Gitea token scopes 的唯一文档。

## Verdict（原则 7：link，不删）

- ADR 0002（2026-06-05，Accepted）内容仍现行：`aiops:ready` 约定被
  batch-issue-processing.md 引用、external worker mode 是当前 dogfood 策略。
  → 链入 README "Architecture notes"（紧邻 ADR 0001）。
- Gitea bot runbook 是 token-scope 唯一来源，明显现行。
  → 链入 README "Architecture notes" 的 runbook 列表；同时在 README
  "Gitea issue-state labels" 节尾加一句指引（natural discovery path）。

## Changes

1. binary-deployment.md L24-25:"prepares a git workspace, runs the agent,
   and stops. Push and draft-PR creation happen agent-side."（去掉
   enforces policy；可提 policy.mode 仅为 prompt directive）。
2. local-dev.md L23-27: 同类改写，保持该文件原有句式。
3. README "Architecture notes":加 ADR 0002 + Gitea bot runbook 两行。
4. README "Gitea issue-state labels" 节尾一句链接 gitea-bot runbook。

## Acceptance criteria（照 issue）

- [ ] 两处 "enforces policy" 改为 post-D33 现实。
- [ ] ADR 0002 与 Gitea bot runbook 从 natural index 可达（README）。

## Gate

docs-only；within budget；PR body 带 `Closes #784`。
与 #783 串行（共享 README.md，soft overlap）；#783 合并后 rebase。
