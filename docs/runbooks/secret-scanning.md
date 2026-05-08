# Secret scanning runbook

This runbook covers the optional pre-push secret scan hook the worker runs
between the `verify` phase and `git push`. Its goal is to catch obvious
credential leaks before an AI-generated branch reaches the remote.

The hook is **off by default** so existing workflows are unchanged.
Operators opt in by adding a `verify.secret_scan` block to `WORKFLOW.md`.

## How it fits in the worker flow

```
runner -> policy enforce -> verify commands -> secret scan -> git push -> PR
                                               ^^^^^^^^^^^
                                               this runbook
```

When the scan reports findings, the worker:

1. Emits a `secret_scan_violation` task event with the captured stdout,
   stderr, exit code, and duration in the payload.
2. Returns an error from `runTask`, which the queue records via the
   existing `failed_attempt` path.
3. **Skips `git push` and PR creation entirely.** No remote artifact is
   created until the leak is removed and the task is retried.

When the scanner cannot be executed at all (binary missing, permission
denied), the worker emits `secret_scan_error` and also blocks the push,
regardless of `fail_on_finding`. This is intentional: a misconfigured
scanner should fail closed, not silently allow pushes.

## Configuration schema

```yaml
verify:
  commands:
    - go test ./...
  secret_scan:
    enabled: true
    command:
      - gitleaks
      - detect
      - --source
      - .
      - --no-banner
    fail_on_finding: true   # optional, defaults to true
```

Fields:

- `enabled` (bool): toggles the hook. When false or omitted, the worker
  skips the scan and emits no events.
- `command` ([]string): argv to exec inside the workspace directory. The
  first element is the binary; remaining elements are passed verbatim.
  No shell is involved, so quoting/expansion does not happen — pass each
  flag as its own list item.
- `fail_on_finding` (bool, default `true`): when false, the worker still
  emits `secret_scan_violation` events but does **not** block the push.
  Useful while tuning rules to avoid false-positive churn. Operators
  should treat this as a temporary state, not a steady-state setting.

The scanner runs with the workspace directory as its working directory,
so relative paths like `--source .` resolve to the AI-modified tree.

## Recommended tools

The worker does not bundle a scanner; install one of the following on the
worker host (or in the worker container image) and reference its binary
in `verify.secret_scan.command`.

### gitleaks

[gitleaks](https://github.com/gitleaks/gitleaks) is fast, has good
defaults, and ships as a static binary.

Install:

```bash
# macOS
brew install gitleaks

# Linux (binary release)
curl -sSL https://github.com/gitleaks/gitleaks/releases/latest/download/gitleaks_linux_x64.tar.gz \
  | tar -xz -C /usr/local/bin gitleaks

# Verify
gitleaks version
```

Recommended `WORKFLOW.md` snippet:

```yaml
verify:
  secret_scan:
    enabled: true
    command:
      - gitleaks
      - detect
      - --source
      - .
      - --no-banner
      - --redact
```

Notes:

- `--no-banner` keeps stdout machine-readable; the worker captures up to
  1 MiB of output into the task event payload.
- `--redact` masks the matched secret in the report. Recommended so the
  task event payload itself does not become a secondary leak.
- A custom `gitleaks.toml` checked into the repo is honored automatically.

### trufflehog

[trufflehog](https://github.com/trufflesecurity/trufflehog) has a
verifier mode that calls real APIs to confirm a secret is live. The
filesystem subcommand is the one to use here:

```yaml
verify:
  secret_scan:
    enabled: true
    command:
      - trufflehog
      - filesystem
      - --no-update
      - --fail
      - .
```

Notes:

- `--no-update` prevents the binary from contacting the network for
  self-updates, which keeps the scan deterministic.
- `--fail` makes trufflehog exit non-zero on any verified finding. The
  worker treats any non-zero exit as a violation.

### detect-secrets

[detect-secrets](https://github.com/Yelp/detect-secrets) is the right
choice if your team already curates a baseline file:

```yaml
verify:
  secret_scan:
    enabled: true
    command:
      - detect-secrets-hook
      - --baseline
      - .secrets.baseline
```

The hook exits non-zero when new secrets appear that are not in the
baseline. Combine with periodic `detect-secrets scan --update` runs done
by humans, not the AI worker.

## CI integration

Run the same scanner in CI so that secrets which somehow slipped past
the worker (or were added to the base branch by humans) still fail
visibly. A minimal GitHub Actions step:

```yaml
- name: Secret scan
  run: |
    gitleaks detect --source . --no-banner --redact
```

Run this in the same job that runs `go test`, before any deploy steps.
The worker's pre-push hook is a fast feedback loop; CI is the safety
net.

## Handling false positives

False positives are inevitable. Resolve them in this order:

1. **Prefer rule-level allowlists in the scanner config.** Both gitleaks
   and trufflehog support per-repo allowlists checked into the repo
   (e.g. `gitleaks.toml`). This keeps the suppression next to the code
   and reviewable in PRs.
2. **For detect-secrets, update the baseline.** Run
   `detect-secrets scan --update .secrets.baseline` locally, review the
   diff, and commit it. Never let the worker commit baseline updates.
3. **Use `fail_on_finding: false` only as a stopgap.** It is fine for
   the first day or two while you tune rules, but a permanent
   `fail_on_finding: false` defeats the purpose of the hook. Track it as
   technical debt.
4. **Do not silence findings by editing the AI-generated branch.** If
   the scanner flags a real secret, rotate it, then remove it from the
   working tree. Re-running the task should produce a clean scan.

## Task events

Operators can inspect scan outcomes via the task events API
(`docs/runbooks/task-api.md`). Event kinds emitted by this hook:

| Event kind              | Meaning                                                   |
| ----------------------- | --------------------------------------------------------- |
| `secret_scan_start`     | Scanner is about to run; payload includes the argv.       |
| `secret_scan_clean`     | Scanner exited zero. No findings.                         |
| `secret_scan_violation` | Scanner exited non-zero. Push was blocked unless          |
|                         | `fail_on_finding: false`. Payload has stdout/stderr.      |
| `secret_scan_error`     | Scanner could not be executed (binary missing, etc).     |
|                         | Push is always blocked. Fix the worker host configuration.|

All payloads include `command`, `exit_code`, `duration_ms`, and the
captured `stdout`/`stderr` truncated to 1 MiB to bound event size.

## Operational tips

- **Pin the scanner version** in your worker image so behavior does not
  drift between deploys. A new gitleaks rule release can change which
  branches pass overnight.
- **Watch event durations.** A scan that spikes from 2s to 60s usually
  means the workspace is accumulating untracked artifacts (caches, build
  outputs). Add them to `.gitignore` rather than tuning the scanner.
- **Do not point the scanner at `.git/`.** Some scanners walk it by
  default and surface historical findings unrelated to the AI change.
  Use `--source .` (gitleaks) or scan only the diff if your scanner
  supports it.
- **Treat `secret_scan_error` as a paging-level event.** Unlike
  `secret_scan_violation` (real finding, AI branch is the problem),
  `secret_scan_error` means the worker host itself is broken.
