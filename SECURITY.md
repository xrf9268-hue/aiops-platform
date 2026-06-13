# Security Policy

## Supported versions

`aiops-platform` is pre-release and ships from a single line of development.
Only the **latest published release** (see
[Releases](https://github.com/xrf9268-hue/aiops-platform/releases)) receives
security fixes. There are no maintained back-release branches.

## Reporting a vulnerability

**Please do not open a public issue for a security vulnerability.**

Report privately through GitHub's
[Private vulnerability reporting](https://github.com/xrf9268-hue/aiops-platform/security/advisories/new):
on the repository's **Security** tab, choose **Report a vulnerability**. This
opens a private advisory visible only to the maintainers, so the fix can land
before any public disclosure.

Please include enough to reproduce: affected version/commit, configuration
(redact tokens and secrets), and the impact you observed. You will get an
acknowledgement of the report; the maintainers will coordinate a fix and a
coordinated-disclosure timeline with you.

## Scope

This tool runs coding agents against real repositories with real tracker and
git credentials, so its threat model — sandboxing, token isolation, the
scheduler/runner boundary, and the controls operators should apply before
connecting it to live repositories or trackers — is documented in
[`docs/security-posture.md`](docs/security-posture.md). Read it before a
first real run.
