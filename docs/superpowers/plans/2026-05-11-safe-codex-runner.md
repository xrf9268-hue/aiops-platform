# Safe Codex Runner Profile Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the generic ShellRunner-for-codex with a profile-driven `CodexRunner` (safe / bypass / custom), capture codex stdout/stderr to a workspace artifact and the runner_end event payload, and pick up codex's `--output-last-message` artifact as `Result.Summary`. Closes issue #17.

**Architecture:** A new `CodexRunner` in `internal/runner` builds argv from `Workflow.Config.Codex.Profile`, runs `codex` via `exec.CommandContext` with PROMPT.md piped on stdin, and shares one capture path across all profiles. A small `cappedWriter` mirrors `workspace.cappedBuffer` semantics (1 MiB cap, drop counter). The worker's `RunRunnerWithTimeout` enriches `runner_end`/`runner_timeout` payloads with head/tail/bytes/dropped when the runner provides them. Schema gains one field: `codex.profile`.

**Tech Stack:** Go 1.25, `os/exec`, `syscall` (process group kill via existing `configurePlatformKill`), `gopkg.in/yaml.v3`, standard `testing` package. No new module deps.

**Reference spec:** `docs/superpowers/specs/2026-05-11-safe-codex-runner-design.md` — implementation details below repeat the relevant code rather than referring out.

---

## File Map

| File | Action | Responsibility |
|---|---|---|
| `internal/runner/capture.go` | Create | `cappedWriter` (1 MiB cap, drop counter) + `headTail` helper for event payload extraction. |
| `internal/runner/capture_test.go` | Create | Unit tests for cappedWriter and headTail. |
| `internal/runner/codex.go` | Create | `CodexRunner` struct, profile dispatch (safe/bypass/custom), argv assembly, stdin piping, capture, artifact write, last-message pickup, error classification. |
| `internal/runner/codex_test.go` | Create | Codex tests using a per-test PATH-stub `codex` binary. |
| `internal/runner/runner.go` | Modify | Extend `Result` with `OutputBytes/OutputDropped/OutputHead/OutputTail`. Re-route `New("codex")` to `CodexRunner`. |
| `internal/runner/runner_test.go` | Modify | Rebind the two ShellRunner kill/exit tests from `codex` to `claude` so they continue to exercise the shell path. |
| `internal/workflow/config.go` | Modify | Add `Profile string` to `CommandConfig`. Default `Codex.Profile = "safe"` in `DefaultConfig()`. |
| `internal/workflow/loader.go` | Modify | Add `supportedCodexProfiles`, validate `Codex.Profile`, reject `Claude.Profile`, normalize empty `Codex.Profile` to `safe` in `expandConfig`. |
| `internal/workflow/config_test.go` | Modify | Default-value, accept, reject, normalize, claude-rejection tests. |
| `internal/worker/runtask.go` | Modify | Enrich `runner_end` (success and failure) and `runner_timeout` event payloads with `output_head/output_tail/output_bytes/output_dropped` when `Result` populates them. |
| `internal/worker/run_test.go` | Modify | Assertion that the new payload keys appear when Result populated; mock path leaves them absent. |
| `examples/WORKFLOW.md` | Modify | Document `codex.profile` default + alternatives in the existing `codex:` block. |
| `docs/runbooks/personal-daily-workflow.md` | Modify | Profile semantics in the codex subsection + "Reading codex output after a run" pointer. |
| `docs/symphony-integration.md` | Modify | Wording fix on "advanced sandboxing" + Symphony app-server pointer. |
| `README.md` | Modify | One-line `codex.profile` mention near the existing codex.command reference (only if such a reference exists; skip otherwise). |

Total: 4 created, 11 modified.

---

## Pre-flight (Task 0)

- [ ] **Step 1: Read the spec end-to-end so the rationale is loaded**

```bash
cat docs/superpowers/specs/2026-05-11-safe-codex-runner-design.md
```

Expected: 14-section design including Profile Contract, API Surface, Data Flow, Error Handling, Testing.

- [ ] **Step 2: Verify clean tree on the implementation branch**

```bash
git status --short
```

Expected: empty output.

- [ ] **Step 3: Confirm Go toolchain matches go.mod**

```bash
go version && head -3 go.mod
```

Expected: `go version go1.25.x`; go.mod's `go` directive at 1.25.

---

## Task 1: `cappedWriter` capture helper

**Files:**
- Create: `internal/runner/capture.go`
- Create: `internal/runner/capture_test.go`

**Why first:** Leaf dependency. Codex tests will rely on it; getting the head/tail boundary semantics exactly right early prevents reworking codex tests.

- [ ] **Step 1: Write failing test for cap + drop counter**

Create `internal/runner/capture_test.go` with:

```go
package runner

import (
	"bytes"
	"strings"
	"testing"
)

func TestCappedWriter_KeepsUpToCapDropsRest(t *testing.T) {
	t.Parallel()
	w := &cappedWriter{Cap: 10}
	n, err := w.Write([]byte("0123456789ABCDE")) // 15 bytes
	if err != nil {
		t.Fatalf("Write returned err: %v", err)
	}
	if n != 15 {
		t.Fatalf("Write returned n=%d, want 15", n)
	}
	if got := w.Bytes(); !bytes.Equal(got, []byte("0123456789")) {
		t.Fatalf("Bytes()=%q, want %q", got, "0123456789")
	}
	if got, want := w.Dropped(), int64(5); got != want {
		t.Fatalf("Dropped()=%d, want %d", got, want)
	}
}

func TestCappedWriter_MultipleWritesAccumulate(t *testing.T) {
	t.Parallel()
	w := &cappedWriter{Cap: 10}
	w.Write([]byte("hello "))
	w.Write([]byte("world!!"))
	if got := w.Bytes(); string(got) != "hello worl" {
		t.Fatalf("Bytes()=%q, want %q", got, "hello worl")
	}
	if w.Dropped() != 3 {
		t.Fatalf("Dropped()=%d, want 3", w.Dropped())
	}
}

func TestCappedWriter_ZeroCapDropsEverything(t *testing.T) {
	t.Parallel()
	w := &cappedWriter{Cap: 0}
	w.Write([]byte("anything"))
	if len(w.Bytes()) != 0 {
		t.Fatalf("Bytes() not empty: %q", w.Bytes())
	}
	if w.Dropped() != 8 {
		t.Fatalf("Dropped()=%d, want 8", w.Dropped())
	}
}

func TestHeadTail_BelowCapReturnsHeadOnly(t *testing.T) {
	t.Parallel()
	body := []byte(strings.Repeat("x", 100))
	head, tail := headTail(body, 4096)
	if string(head) != string(body) {
		t.Fatalf("head should equal body when len < cap")
	}
	if tail != "" {
		t.Fatalf("tail should be empty when body fits in head; got %q", tail)
	}
}

func TestHeadTail_AboveCapSplits(t *testing.T) {
	t.Parallel()
	body := []byte(strings.Repeat("a", 4000) + strings.Repeat("b", 4000) + strings.Repeat("c", 4000))
	head, tail := headTail(body, 4096)
	if len(head) != 4096 {
		t.Fatalf("head len=%d, want 4096", len(head))
	}
	if len(tail) != 4096 {
		t.Fatalf("tail len=%d, want 4096", len(tail))
	}
	if !strings.HasPrefix(string(head), strings.Repeat("a", 4000)) {
		t.Fatalf("head should start with 'a's, got prefix %q", head[:8])
	}
	if !strings.HasSuffix(string(tail), strings.Repeat("c", 4000)) {
		t.Fatalf("tail should end with 'c's")
	}
}
```

- [ ] **Step 2: Verify tests fail (no symbol)**

Run: `go test ./internal/runner/... -run 'TestCappedWriter|TestHeadTail' -v 2>&1 | head -30`

Expected: compile error mentioning `cappedWriter` undefined.

- [ ] **Step 3: Implement `capture.go`**

Create `internal/runner/capture.go`:

```go
package runner

// CodexOutputCap is the upper bound on bytes the codex runner buffers in
// memory from a single run. Matches workspace.VerifyOutputCap so artifact
// ergonomics are uniform across the verify and runner phases.
const CodexOutputCap = 1 << 20 // 1 MiB

// CodexEventOutputCap bounds the head and tail slices embedded in the
// runner_end event payload. 4 KiB per side keeps the events table cheap
// to query while still surfacing enough context for triage.
const CodexEventOutputCap = 4 << 10 // 4 KiB

// cappedWriter is an io.Writer that buffers up to Cap bytes and silently
// drops the rest while remembering how many bytes were dropped. It is the
// runner-side twin of workspace.cappedBuffer; the duplication avoids an
// import cycle and keeps each package's IO contract local.
type cappedWriter struct {
	Cap     int
	buf     []byte
	dropped int64
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	if c.Cap <= 0 {
		c.dropped += int64(len(p))
		return len(p), nil
	}
	remaining := c.Cap - len(c.buf)
	if remaining <= 0 {
		c.dropped += int64(len(p))
		return len(p), nil
	}
	take := len(p)
	if take > remaining {
		take = remaining
	}
	c.buf = append(c.buf, p[:take]...)
	if take < len(p) {
		c.dropped += int64(len(p) - take)
	}
	return len(p), nil
}

// Bytes returns the buffered bytes (post-cap). Callers must not mutate.
func (c *cappedWriter) Bytes() []byte { return c.buf }

// Dropped reports how many bytes were dropped because Cap was reached.
func (c *cappedWriter) Dropped() int64 { return c.dropped }

// headTail returns the first headCap bytes and (when body is longer than
// headCap) the last headCap bytes. tail is empty when the entire body
// fits in head, so callers do not duplicate content. headCap is applied
// to each side independently — total payload size is bounded by 2*headCap.
func headTail(body []byte, headCap int) (head []byte, tail string) {
	if headCap <= 0 || len(body) == 0 {
		return nil, ""
	}
	if len(body) <= headCap {
		return body, ""
	}
	head = body[:headCap]
	tail = string(body[len(body)-headCap:])
	return head, tail
}
```

- [ ] **Step 4: Verify tests pass**

Run: `go test ./internal/runner/... -run 'TestCappedWriter|TestHeadTail' -v`

Expected: all five tests PASS.

- [ ] **Step 5: gofmt check**

Run: `gofmt -l internal/runner/capture.go internal/runner/capture_test.go`

Expected: empty output.

- [ ] **Step 6: Commit**

```bash
git add internal/runner/capture.go internal/runner/capture_test.go
git commit -m "feat(runner): add cappedWriter and headTail capture helpers (#17)"
```

---

## Task 2: `codex.profile` schema field + loader validation

**Files:**
- Modify: `internal/workflow/config.go`
- Modify: `internal/workflow/loader.go`
- Modify: `internal/workflow/config_test.go`

**Why second:** Independent of runner; safe to merge first if desired. Loader validation rejects bad input regardless of which runner consumes it.

- [ ] **Step 1: Read the existing config + loader to confirm shape**

Run: `sed -n '85,115p' internal/workflow/config.go` and `sed -n '50,90p' internal/workflow/loader.go`

Expected: confirms `CommandConfig` shape and `validateConfig` / `supportedAgentDefaults` patterns referenced below.

- [ ] **Step 2: Write failing tests first**

Edit `internal/workflow/config_test.go`. Append the following tests at the end of the file:

```go
func TestDefaultConfig_CodexProfileIsSafe(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()
	if cfg.Codex.Profile != "safe" {
		t.Fatalf("DefaultConfig().Codex.Profile = %q, want %q", cfg.Codex.Profile, "safe")
	}
}

func TestLoad_AcceptsSupportedCodexProfiles(t *testing.T) {
	t.Parallel()
	for _, profile := range []string{"safe", "bypass", "custom"} {
		profile := profile
		t.Run(profile, func(t *testing.T) {
			path := writeTempWorkflow(t, `---
repo:
  clone_url: file:///tmp/repo
tracker:
  kind: linear
agent:
  default: codex
codex:
  command: codex exec
  profile: `+profile+`
---
prompt body
`)
			wf, err := Load(path)
			if err != nil {
				t.Fatalf("Load(%s): %v", profile, err)
			}
			if wf.Config.Codex.Profile != profile {
				t.Fatalf("Codex.Profile = %q, want %q", wf.Config.Codex.Profile, profile)
			}
		})
	}
}

func TestLoad_RejectsUnknownCodexProfile(t *testing.T) {
	t.Parallel()
	path := writeTempWorkflow(t, `---
repo:
  clone_url: file:///tmp/repo
tracker:
  kind: linear
agent:
  default: codex
codex:
  command: codex exec
  profile: yolo
---
prompt
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load: expected error for codex.profile=yolo, got nil")
	}
	if !strings.Contains(err.Error(), "codex.profile") || !strings.Contains(err.Error(), "yolo") {
		t.Fatalf("error = %q; want it to mention codex.profile and yolo", err)
	}
}

func TestLoad_NormalizesEmptyCodexProfileToSafe(t *testing.T) {
	t.Parallel()
	path := writeTempWorkflow(t, `---
repo:
  clone_url: file:///tmp/repo
tracker:
  kind: linear
agent:
  default: codex
codex:
  command: codex exec
---
prompt
`)
	wf, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if wf.Config.Codex.Profile != "safe" {
		t.Fatalf("Codex.Profile = %q, want %q (normalization)", wf.Config.Codex.Profile, "safe")
	}
}

func TestLoad_RejectsClaudeProfile(t *testing.T) {
	t.Parallel()
	path := writeTempWorkflow(t, `---
repo:
  clone_url: file:///tmp/repo
tracker:
  kind: linear
agent:
  default: claude
claude:
  command: claude
  profile: safe
---
prompt
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load: expected error for claude.profile, got nil")
	}
	if !strings.Contains(err.Error(), "claude.profile") {
		t.Fatalf("error = %q; want it to mention claude.profile", err)
	}
}
```

If `writeTempWorkflow` does not yet exist in `config_test.go` (grep first), add this helper near the top:

```go
func writeTempWorkflow(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/WORKFLOW.md"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
```

Add `"os"` and `"strings"` to the imports if not already present.

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./internal/workflow/... -run 'TestDefaultConfig_CodexProfileIsSafe|TestLoad_AcceptsSupportedCodexProfiles|TestLoad_RejectsUnknownCodexProfile|TestLoad_NormalizesEmptyCodexProfileToSafe|TestLoad_RejectsClaudeProfile' -v 2>&1 | head -30`

Expected: failures — `Codex.Profile` field undefined OR all five fail because Profile field absent from struct, fields silently drop.

- [ ] **Step 4: Add `Profile` field to `CommandConfig`**

Edit `internal/workflow/config.go`. Replace the `CommandConfig` struct (around line 90):

```go
type CommandConfig struct {
	Command string `yaml:"command" json:"command"`
	// Profile is consulted only by the codex runner. Allowed values: "safe"
	// (default), "bypass", "custom". The field lives on the shared
	// CommandConfig type to avoid splitting CodexConfig out for one field;
	// loader validation rejects non-empty Profile on the Claude embed so a
	// copy-paste mistake fails loud at load time instead of silently doing
	// nothing.
	Profile string `yaml:"profile,omitempty" json:"profile,omitempty"`
}
```

In `DefaultConfig()` (same file, around line 198), update the Codex line:

```go
		Codex:  CommandConfig{Command: "codex exec", Profile: "safe"},
```

- [ ] **Step 5: Add validation and normalization in loader.go**

Edit `internal/workflow/loader.go`. Add this constant block near `supportedAgentDefaults` (around line 64):

```go
// supportedCodexProfiles enumerates the codex runner profile names the
// runner package knows how to dispatch. "safe" injects --full-auto +
// --skip-git-repo-check; "bypass" swaps in
// --dangerously-bypass-approvals-and-sandbox for already-isolated hosts;
// "custom" falls back to the operator-supplied codex.command via sh -lc.
var supportedCodexProfiles = map[string]struct{}{
	"safe":   {},
	"bypass": {},
	"custom": {},
}
```

In `validateConfig` (around line 76), append before the return:

```go
	if _, ok := supportedCodexProfiles[cfg.Codex.Profile]; !ok {
		return fmt.Errorf("%s: codex.profile %q is not supported (allowed: safe, bypass, custom)", path, cfg.Codex.Profile)
	}
	if strings.TrimSpace(cfg.Claude.Profile) != "" {
		return fmt.Errorf("%s: claude.profile is not supported (only codex has profiles)", path)
	}
```

In `expandConfig` (around line 146), add normalization for empty Codex.Profile:

```go
	if cfg.Codex.Profile == "" {
		cfg.Codex.Profile = "safe"
	}
```

- [ ] **Step 6: Run the new tests**

Run: `go test ./internal/workflow/... -run 'TestDefaultConfig_CodexProfileIsSafe|TestLoad_AcceptsSupportedCodexProfiles|TestLoad_RejectsUnknownCodexProfile|TestLoad_NormalizesEmptyCodexProfileToSafe|TestLoad_RejectsClaudeProfile' -v`

Expected: all five PASS.

- [ ] **Step 7: Run the full workflow package to catch regressions**

Run: `go test ./internal/workflow/...`

Expected: PASS. If any pre-existing test previously asserted `cfg.Codex` had no `Profile` field, fix it (most likely by accepting the new default `"safe"`).

- [ ] **Step 8: gofmt + go mod tidy verify**

Run: `gofmt -l internal/workflow/ && go mod tidy && git diff --exit-code -- go.mod go.sum`

Expected: empty + clean diff.

- [ ] **Step 9: Commit**

```bash
git add internal/workflow/config.go internal/workflow/loader.go internal/workflow/config_test.go
git commit -m "feat(workflow): add codex.profile schema field with safe/bypass/custom (#17)"
```

---

## Task 3: `CodexRunner` — safe profile end to end

**Files:**
- Create: `internal/runner/codex.go`
- Create: `internal/runner/codex_test.go`
- Modify: `internal/runner/runner.go` (extend `Result`, route `New("codex")` to `CodexRunner`)

**Why third:** Builds on Task 1 (cappedWriter) and Task 2 (Codex.Profile field). All other CodexRunner work (bypass, custom, error paths) extends what lands here.

- [ ] **Step 1: Extend `Result` in `runner.go`**

Edit `internal/runner/runner.go`. Replace `Result`:

```go
type Result struct {
	Summary       string
	OutputBytes   int64  // bytes the runner kept in its capture buffer
	OutputDropped int64  // bytes dropped because the buffer hit its cap
	OutputHead    string // up to CodexEventOutputCap bytes from the start of the captured output
	OutputTail    string // up to CodexEventOutputCap bytes from the end; empty when total <= head cap
}
```

Do NOT change `runner.New` yet — codex still routes to `ShellRunner` until later in this task.

- [ ] **Step 2: Build the test harness — PATH-stub `codex` helper**

Create `internal/runner/codex_test.go`:

```go
package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// codexStubScript writes a bash script named `codex` into a temp dir and
// prepends that dir to PATH for the duration of the test. The script body
// is supplied by the caller. The script always records its argv and stdin
// to predictable side files inside the same temp dir so tests can assert
// what codex was actually called with.
//
// Returns (binDir, scriptCommand) where binDir contains the recorded
// argv.txt / stdin.txt and the codex script itself.
func codexStubScript(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "codex")
	header := `#!/usr/bin/env bash
set -e
printf '%s\n' "$@" > "` + dir + `/argv.txt"
cat > "` + dir + `/stdin.txt"
`
	if err := os.WriteFile(scriptPath, []byte(header+body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

// codexWorkdir creates a per-test workdir with a populated .aiops/PROMPT.md.
func codexWorkdir(t *testing.T, prompt string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".aiops"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".aiops", "PROMPT.md"), []byte(prompt), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func codexInput(workdir string) RunInput {
	return RunInput{
		Task:    task.Task{ID: "tsk_codex_test", Model: "codex"},
		Workflow: workflow.Workflow{Config: workflow.Config{
			Codex: workflow.CommandConfig{Command: "codex exec", Profile: "safe"},
		}},
		Workdir: workdir,
		Prompt:  "ignored — runner reads .aiops/PROMPT.md",
	}
}

func TestCodexRunner_SafeProfileBuildsExpectedArgv(t *testing.T) {
	t.Parallel()
	binDir := codexStubScript(t, `exit 0`)
	wd := codexWorkdir(t, "hello prompt")

	in := codexInput(wd)
	if _, err := (CodexRunner{}).Run(context.Background(), in); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(binDir, "argv.txt"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"exec",
		"--full-auto",
		"--skip-git-repo-check",
		"--cd",
		wd,
		"-o",
		".aiops/CODEX_LAST_MESSAGE.md",
	}
	gotLines := strings.Split(strings.TrimRight(string(got), "\n"), "\n")
	if len(gotLines) != len(want) {
		t.Fatalf("argv lines = %d, want %d; got %q", len(gotLines), len(want), gotLines)
	}
	for i := range want {
		if gotLines[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q", i, gotLines[i], want[i])
		}
	}
}

func TestCodexRunner_PromptPipedToStdin(t *testing.T) {
	t.Parallel()
	binDir := codexStubScript(t, `exit 0`)
	wd := codexWorkdir(t, "stdin canary 42")

	if _, err := (CodexRunner{}).Run(context.Background(), codexInput(wd)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(binDir, "stdin.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "stdin canary 42" {
		t.Fatalf("stdin = %q, want %q", got, "stdin canary 42")
	}
}

func TestCodexRunner_CapturesOutputArtifact(t *testing.T) {
	t.Parallel()
	codexStubScript(t, `printf 'codex-canary-9134\n' ; printf 'err-line\n' >&2 ; exit 0`)
	wd := codexWorkdir(t, "x")

	res, err := (CodexRunner{}).Run(context.Background(), codexInput(wd))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	out, err := os.ReadFile(filepath.Join(wd, ".aiops/CODEX_OUTPUT.txt"))
	if err != nil {
		t.Fatalf("read CODEX_OUTPUT.txt: %v", err)
	}
	if !strings.Contains(string(out), "codex-canary-9134") {
		t.Fatalf("artifact missing stdout canary; got %q", out)
	}
	if !strings.Contains(string(out), "err-line") {
		t.Fatalf("artifact missing stderr line; got %q", out)
	}
	if res.OutputBytes <= 0 {
		t.Fatalf("Result.OutputBytes = %d, want > 0", res.OutputBytes)
	}
	if res.OutputDropped != 0 {
		t.Fatalf("Result.OutputDropped = %d, want 0 for small output", res.OutputDropped)
	}
	if !strings.Contains(res.OutputHead, "codex-canary-9134") {
		t.Fatalf("OutputHead missing canary; got %q", res.OutputHead)
	}
}

func TestCodexRunner_LastMessageBecomesSummary(t *testing.T) {
	t.Parallel()
	codexStubScript(t, `mkdir -p .aiops && printf 'codex completed task X\n' > .aiops/CODEX_LAST_MESSAGE.md ; exit 0`)
	wd := codexWorkdir(t, "x")

	res, err := (CodexRunner{}).Run(context.Background(), codexInput(wd))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Summary != "codex completed task X" {
		t.Fatalf("Summary = %q, want %q", res.Summary, "codex completed task X")
	}
}

func TestCodexRunner_MissingLastMessageFallsBackToDefaultSummary(t *testing.T) {
	t.Parallel()
	codexStubScript(t, `exit 0`)
	wd := codexWorkdir(t, "x")

	res, err := (CodexRunner{}).Run(context.Background(), codexInput(wd))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Summary != "codex completed" {
		t.Fatalf("Summary = %q, want %q", res.Summary, "codex completed")
	}
}

func TestCodexRunner_OutputExceedsCapTruncates(t *testing.T) {
	t.Parallel()
	// 1.5 MiB of stdout: comfortably above the 1 MiB cap.
	codexStubScript(t, `head -c 1572864 /dev/zero | tr '\0' 'a'`)
	wd := codexWorkdir(t, "x")

	res, err := (CodexRunner{}).Run(context.Background(), codexInput(wd))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.OutputDropped <= 0 {
		t.Fatalf("OutputDropped = %d, want > 0", res.OutputDropped)
	}
	if res.OutputBytes != int64(CodexOutputCap) {
		t.Fatalf("OutputBytes = %d, want %d (the cap)", res.OutputBytes, CodexOutputCap)
	}
	body, err := os.ReadFile(filepath.Join(wd, ".aiops/CODEX_OUTPUT.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(body), "\n") || !strings.Contains(string(body), "...output truncated at") {
		t.Fatalf("artifact missing truncation footer; tail=%q", body[max(0, len(body)-200):])
	}
}

func TestCodexRunner_MissingPromptReturnsWrappedError(t *testing.T) {
	t.Parallel()
	codexStubScript(t, `exit 0`)
	dir := t.TempDir() // no .aiops/PROMPT.md
	in := codexInput(dir)

	_, err := (CodexRunner{}).Run(context.Background(), in)
	if err == nil {
		t.Fatal("expected error for missing PROMPT.md, got nil")
	}
	if IsTimeout(err) {
		t.Fatalf("missing-prompt should not classify as timeout: %v", err)
	}
	if !strings.Contains(err.Error(), "PROMPT.md") {
		t.Fatalf("error %q should mention PROMPT.md", err)
	}
}

func TestCodexRunner_MissingBinaryReturnsClearError(t *testing.T) {
	t.Parallel()
	t.Setenv("PATH", "") // no codex anywhere
	wd := codexWorkdir(t, "x")
	_, err := (CodexRunner{}).Run(context.Background(), codexInput(wd))
	if err == nil {
		t.Fatal("expected error for missing codex binary")
	}
	if !strings.Contains(err.Error(), "codex binary not found") {
		t.Fatalf("error %q should mention 'codex binary not found'", err)
	}
}

func TestCodexRunner_NonZeroExitNotTimeout(t *testing.T) {
	t.Parallel()
	codexStubScript(t, `exit 3`)
	wd := codexWorkdir(t, "x")
	_, err := (CodexRunner{}).Run(context.Background(), codexInput(wd))
	if err == nil {
		t.Fatal("expected error from exit 3")
	}
	if IsTimeout(err) {
		t.Fatalf("non-zero exit must not classify as timeout: %v", err)
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("expected *exec.ExitError, got %T: %v", err, err)
	}
}

func TestCodexRunner_TimeoutKillsProcess(t *testing.T) {
	t.Parallel()
	codexStubScript(t, `sleep 30`)
	wd := codexWorkdir(t, "x")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := (CodexRunner{}).Run(ctx, codexInput(wd))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !IsTimeout(err) {
		t.Fatalf("expected *TimeoutError, got %T: %v", err, err)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("process not killed promptly: elapsed=%v", elapsed)
	}
}
```

- [ ] **Step 3: Run codex tests to verify they fail (no symbol)**

Run: `go test ./internal/runner/... -run 'TestCodexRunner_' -v 2>&1 | head -20`

Expected: compile error mentioning `CodexRunner` undefined.

- [ ] **Step 4: Implement `codex.go` with all required functionality**

Create `internal/runner/codex.go`:

```go
package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// CodexOutputPath is where the runner persists captured codex stdout+stderr,
// relative to the workdir.
const CodexOutputPath = ".aiops/CODEX_OUTPUT.txt"

// CodexLastMessagePath is where codex CLI writes its final message when
// invoked with -o; the runner ingests it as Result.Summary on success.
const CodexLastMessagePath = ".aiops/CODEX_LAST_MESSAGE.md"

// PromptPath is the workdir-relative location of the rendered prompt the
// worker writes before invoking the runner.
const PromptPath = ".aiops/PROMPT.md"

// CodexRunner is the profile-driven runner for the codex CLI. It replaces
// the generic ShellRunner for codex; claude continues to use ShellRunner.
//
// Profile dispatch lives entirely inside Run; the runner is stateless and
// safe to share across goroutines.
type CodexRunner struct{}

func (CodexRunner) Run(ctx context.Context, in RunInput) (Result, error) {
	promptAbs := filepath.Join(in.Workdir, PromptPath)
	if _, err := os.Stat(promptAbs); err != nil {
		return Result{}, fmt.Errorf("read %s: %w", PromptPath, err)
	}

	cmd, err := buildCodexCmd(ctx, in)
	if err != nil {
		return Result{}, err
	}
	cmd.Dir = in.Workdir
	configurePlatformKill(cmd)
	cmd.WaitDelay = killGrace

	stdin, err := os.Open(promptAbs)
	if err != nil {
		return Result{}, fmt.Errorf("open %s: %w", PromptPath, err)
	}
	defer stdin.Close()
	cmd.Stdin = stdin

	buf := &cappedWriter{Cap: CodexOutputCap}
	cmd.Stdout = buf
	cmd.Stderr = buf

	start := time.Now()
	runErr := cmd.Run()
	elapsed := time.Since(start)

	writeCodexArtifact(in.Workdir, buf)

	res := Result{
		Summary:       readCodexSummary(in.Workdir),
		OutputBytes:   int64(len(buf.Bytes())),
		OutputDropped: buf.Dropped(),
	}
	head, tail := headTail(buf.Bytes(), CodexEventOutputCap)
	if len(head) > 0 {
		res.OutputHead = string(head)
	}
	res.OutputTail = tail

	if runErr != nil {
		if cerr := ctx.Err(); errors.Is(cerr, context.DeadlineExceeded) {
			return res, &TimeoutError{
				Timeout: deadlineBudget(ctx, start),
				Elapsed: elapsed,
				Cause:   runErr,
			}
		}
		return res, runErr
	}
	return res, nil
}

// buildCodexCmd assembles the *exec.Cmd for the requested profile. PROMPT.md
// is always provided via stdin, never via shell redirection.
func buildCodexCmd(ctx context.Context, in RunInput) (*exec.Cmd, error) {
	profile := in.Workflow.Config.Codex.Profile
	if profile == "" {
		profile = "safe"
	}
	switch profile {
	case "safe":
		if _, err := exec.LookPath("codex"); err != nil {
			return nil, fmt.Errorf("codex binary not found in PATH; install codex CLI or set agent.default to claude/mock")
		}
		return exec.CommandContext(ctx,
			"codex", "exec",
			"--full-auto",
			"--skip-git-repo-check",
			"--cd", in.Workdir,
			"-o", CodexLastMessagePath,
		), nil
	case "bypass":
		if _, err := exec.LookPath("codex"); err != nil {
			return nil, fmt.Errorf("codex binary not found in PATH; install codex CLI or set agent.default to claude/mock")
		}
		return exec.CommandContext(ctx,
			"codex", "exec",
			"--dangerously-bypass-approvals-and-sandbox",
			"--skip-git-repo-check",
			"--cd", in.Workdir,
			"-o", CodexLastMessagePath,
		), nil
	case "custom":
		command := strings.TrimSpace(in.Workflow.Config.Codex.Command)
		if command == "" {
			return nil, fmt.Errorf("codex.profile=custom requires codex.command to be non-empty")
		}
		return exec.CommandContext(ctx, "sh", "-lc", command), nil
	default:
		return nil, fmt.Errorf("codex.profile %q is not supported", profile)
	}
}

// writeCodexArtifact persists the buffered output to .aiops/CODEX_OUTPUT.txt.
// On truncation, an explanatory footer line is appended. Errors are swallowed
// (the artifact is best-effort; the event payload still carries head/tail).
func writeCodexArtifact(workdir string, buf *cappedWriter) {
	dir := filepath.Join(workdir, ".aiops")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	body := append([]byte{}, buf.Bytes()...)
	if buf.Dropped() > 0 {
		footer := fmt.Sprintf("\n...output truncated at %d bytes\n", CodexOutputCap)
		body = append(body, []byte(footer)...)
	}
	_ = os.WriteFile(filepath.Join(workdir, CodexOutputPath), body, 0o644)
}

// readCodexSummary returns the trimmed contents of CODEX_LAST_MESSAGE.md or
// "codex completed" when the file is missing/empty/unreadable.
func readCodexSummary(workdir string) string {
	b, err := os.ReadFile(filepath.Join(workdir, CodexLastMessagePath))
	if err != nil {
		return "codex completed"
	}
	trimmed := strings.TrimSpace(string(b))
	if trimmed == "" {
		return "codex completed"
	}
	return trimmed
}
```

- [ ] **Step 5: Route `New("codex")` to `CodexRunner`**

Edit `internal/runner/runner.go`. In `New`, change the codex case:

```go
	case "codex":
		return CodexRunner{}, nil
```

- [ ] **Step 6: Run codex tests**

Run: `go test ./internal/runner/... -run 'TestCodexRunner_' -v -race`

Expected: all 11 tests PASS.

If `TestCodexRunner_OutputExceedsCapTruncates` fails on the `max` builtin (Go 1.21+ requires it explicitly), replace `body[max(0, len(body)-200):]` with `body[len(body)-min(200, len(body)):]` and re-run.

- [ ] **Step 7: Run the full runner package**

Run: `go test ./internal/runner/... -race`

Expected: PASS. The two ShellRunner tests bound to the `codex` name will FAIL because `New("codex")` no longer returns ShellRunner. That's expected — Task 4 fixes them.

- [ ] **Step 8: gofmt + go mod tidy verify**

Run: `gofmt -l internal/runner/ && go mod tidy && git diff --exit-code -- go.mod go.sum`

Expected: empty + clean.

- [ ] **Step 9: Commit**

```bash
git add internal/runner/codex.go internal/runner/codex_test.go internal/runner/runner.go
git commit -m "feat(runner): add CodexRunner with safe/bypass/custom profile dispatch (#17)"
```

---

## Task 4: Rebind ShellRunner tests to claude

**Files:**
- Modify: `internal/runner/runner_test.go`

**Why:** Task 3 changed `New("codex")` so the existing `TestShellRunnerKillsRunawayProcess` and `TestShellRunnerNonTimeoutErrorNotMisclassified` no longer exercise ShellRunner via `New`. They still construct `ShellRunner{Name: "codex"}` directly, which works mechanically but is confusing. Switching them to `claude` keeps them meaningful for the runner that still uses the shell path.

- [ ] **Step 1: Run the failing tests to confirm what breaks**

Run: `go test ./internal/runner/... -race 2>&1 | tail -30`

Expected: any test that called `runner.New("codex")` and asserted ShellRunner behavior fails. Capture which tests fail.

- [ ] **Step 2: Rebind the two existing tests**

Edit `internal/runner/runner_test.go`. In `TestShellRunnerKillsRunawayProcess` and `TestShellRunnerNonTimeoutErrorNotMisclassified`, change:

```go
	wf := workflow.Workflow{Config: workflow.Config{
		Codex: workflow.CommandConfig{Command: "sleep 30"},
	}}
	r := ShellRunner{Name: "codex"}
```

to:

```go
	wf := workflow.Workflow{Config: workflow.Config{
		Claude: workflow.CommandConfig{Command: "sleep 30"},
	}}
	r := ShellRunner{Name: "claude"}
```

(Same swap for the `"exit 3"` variant in the other test.)

- [ ] **Step 3: Run the runner package**

Run: `go test ./internal/runner/... -race`

Expected: all PASS.

- [ ] **Step 4: gofmt**

Run: `gofmt -l internal/runner/`

Expected: empty.

- [ ] **Step 5: Commit**

```bash
git add internal/runner/runner_test.go
git commit -m "test(runner): rebind ShellRunner kill/exit tests from codex to claude (#17)"
```

---

## Task 5: Custom-profile integration test

**Files:**
- Modify: `internal/runner/codex_test.go`

**Why:** Task 3 implemented the custom branch but didn't test it. A minimal end-to-end check confirms the documented "PROMPT.md goes to stdin, command runs via sh -lc" contract.

- [ ] **Step 1: Add the test**

Append to `internal/runner/codex_test.go`:

```go
func TestCodexRunner_CustomProfileUsesShellWithStdin(t *testing.T) {
	t.Parallel()
	wd := codexWorkdir(t, "custom-profile-canary")
	in := RunInput{
		Task: task.Task{ID: "tsk_custom", Model: "codex"},
		Workflow: workflow.Workflow{Config: workflow.Config{
			Codex: workflow.CommandConfig{Command: "cat", Profile: "custom"},
		}},
		Workdir: wd,
	}
	res, err := (CodexRunner{}).Run(context.Background(), in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(wd, ".aiops/CODEX_OUTPUT.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "custom-profile-canary") {
		t.Fatalf("artifact missing canary; got %q", body)
	}
	if res.OutputBytes <= 0 {
		t.Fatalf("OutputBytes = %d, want > 0", res.OutputBytes)
	}
	// Custom profile does NOT write CODEX_LAST_MESSAGE.md (no -o flag);
	// summary should fall back.
	if res.Summary != "codex completed" {
		t.Fatalf("Summary = %q, want default fallback", res.Summary)
	}
}

func TestCodexRunner_CustomProfileEmptyCommandRejected(t *testing.T) {
	t.Parallel()
	wd := codexWorkdir(t, "x")
	in := RunInput{
		Task: task.Task{ID: "tsk_custom_empty", Model: "codex"},
		Workflow: workflow.Workflow{Config: workflow.Config{
			Codex: workflow.CommandConfig{Profile: "custom"},
		}},
		Workdir: wd,
	}
	_, err := (CodexRunner{}).Run(context.Background(), in)
	if err == nil {
		t.Fatal("expected error for custom profile with empty command")
	}
	if !strings.Contains(err.Error(), "codex.command") {
		t.Fatalf("error %q should mention codex.command", err)
	}
}
```

- [ ] **Step 2: Run the new tests**

Run: `go test ./internal/runner/... -run 'TestCodexRunner_CustomProfile' -v -race`

Expected: both PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/runner/codex_test.go
git commit -m "test(runner): cover codex custom profile and empty-command rejection (#17)"
```

---

## Task 6: Worker `runner_end` payload enrichment

**Files:**
- Modify: `internal/worker/runtask.go`
- Modify: `internal/worker/run_test.go`

- [ ] **Step 1: Read the existing payload code**

Run: `sed -n '296,350p' internal/worker/runtask.go`

Expected: shows `RunRunnerWithTimeout` building the `runner_start`/`runner_end`/`runner_timeout` payloads.

- [ ] **Step 2: Find an existing test for runner_end payload to extend**

Run: `grep -n 'runner_end' internal/worker/run_test.go internal/worker/run_internal_test.go 2>/dev/null | head -20`

Expected: at least one test asserting the payload shape. Note its name; we'll add a new sibling test rather than modify it.

- [ ] **Step 3: Write a failing test asserting output_* fields appear**

Append to `internal/worker/run_test.go` (preserve imports):

```go
// fakeOutputRunner returns a fixed Result with non-zero output fields so we
// can assert RunRunnerWithTimeout forwards them onto the runner_end payload.
type fakeOutputRunner struct{}

func (fakeOutputRunner) Run(ctx context.Context, in runner.RunInput) (runner.Result, error) {
	return runner.Result{
		Summary:       "fake done",
		OutputBytes:   42,
		OutputDropped: 7,
		OutputHead:    "head-canary",
		OutputTail:    "tail-canary",
	}, nil
}

func TestRunRunnerWithTimeout_EmitsOutputFieldsOnRunnerEnd(t *testing.T) {
	ev := &captureEmitter{} // existing fake in this test file
	in := runner.RunInput{Task: task.Task{ID: "tsk_payload", Model: "codex"}}
	if _, err := worker.RunRunnerWithTimeout(context.Background(), ev, fakeOutputRunner{}, in, 5*time.Second, "file"); err != nil {
		t.Fatalf("RunRunnerWithTimeout: %v", err)
	}
	end := ev.findLast("runner_end")
	if end == nil {
		t.Fatal("no runner_end event recorded")
	}
	requireKey(t, end.payload, "output_bytes", float64(42))
	requireKey(t, end.payload, "output_dropped", float64(7))
	requireKey(t, end.payload, "output_head", "head-canary")
	requireKey(t, end.payload, "output_tail", "tail-canary")
}

func TestRunRunnerWithTimeout_OmitsOutputFieldsForMockRunner(t *testing.T) {
	ev := &captureEmitter{}
	in := runner.RunInput{Task: task.Task{ID: "tsk_mock_payload", Model: "mock"}, Workdir: t.TempDir()}
	if _, err := worker.RunRunnerWithTimeout(context.Background(), ev, runner.MockRunner{}, in, 5*time.Second, "file"); err != nil {
		t.Fatalf("RunRunnerWithTimeout: %v", err)
	}
	end := ev.findLast("runner_end")
	if end == nil {
		t.Fatal("no runner_end event recorded")
	}
	for _, k := range []string{"output_bytes", "output_dropped", "output_head", "output_tail"} {
		if _, ok := end.payload[k]; ok {
			t.Fatalf("payload should not contain %q for mock runner; got %v", k, end.payload)
		}
	}
}
```

If `captureEmitter`, `findLast`, or `requireKey` do not exist, look for the existing emitter fake and reuse its idioms — don't invent new helpers. Run `grep -n 'AddEventWithPayload' internal/worker/*_test.go` first to find the convention. If you must add helpers, put them next to the existing fake at the bottom of the file:

```go
type recordedEvent struct {
	kind    string
	msg     string
	payload map[string]any
}

type captureEmitter struct {
	mu     sync.Mutex
	events []recordedEvent
}

func (e *captureEmitter) AddEvent(_ context.Context, _ string, kind, msg string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, recordedEvent{kind: kind, msg: msg})
	return nil
}

func (e *captureEmitter) AddEventWithPayload(_ context.Context, _ string, kind, msg string, payload any) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	m, _ := payload.(map[string]any)
	e.events = append(e.events, recordedEvent{kind: kind, msg: msg, payload: m})
	return nil
}

func (e *captureEmitter) findLast(kind string) *recordedEvent {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := len(e.events) - 1; i >= 0; i-- {
		if e.events[i].kind == kind {
			ev := e.events[i]
			return &ev
		}
	}
	return nil
}

func requireKey(t *testing.T, payload map[string]any, key string, want any) {
	t.Helper()
	got, ok := payload[key]
	if !ok {
		t.Fatalf("payload missing key %q; got %v", key, payload)
	}
	if got != want {
		t.Fatalf("payload[%q] = %v (%T), want %v (%T)", key, got, got, want, want)
	}
}
```

(`requireKey`'s `float64` comparison: payloads going through JSON-style marshaling typically widen ints to float64. If the existing emitter does NOT marshal, swap `float64(42)` for `int64(42)` to match how `Result.OutputBytes` is stored.)

- [ ] **Step 4: Run tests to verify they fail**

Run: `go test ./internal/worker/... -run 'TestRunRunnerWithTimeout_(EmitsOutputFields|OmitsOutputFields)' -v 2>&1 | head -30`

Expected: failures because `runner_end` payload does not contain the new keys.

- [ ] **Step 5: Enrich the payload in `runtask.go`**

Edit `internal/worker/runtask.go`. In `RunRunnerWithTimeout`, replace the success-path `endPayload` block with:

```go
	endPayload := map[string]any{
		"model":       in.Task.Model,
		"duration_ms": elapsed.Milliseconds(),
		"ok":          true,
	}
	if res.Summary != "" {
		endPayload["summary"] = res.Summary
	}
	addOutputFields(endPayload, res)
	Emit(ctx, ev, in.Task.ID, task.EventRunnerEnd, "runner completed", endPayload)
	return res, nil
```

In the same function, replace the failure-path `runner_end` payload with:

```go
		failurePayload := map[string]any{
			"model":       in.Task.Model,
			"duration_ms": elapsed.Milliseconds(),
			"error":       ErrSummary(runErr),
			"ok":          false,
		}
		addOutputFields(failurePayload, res)
		Emit(ctx, ev, in.Task.ID, task.EventRunnerEnd, "runner failed", failurePayload)
		return runner.Result{}, runErr
```

And the timeout payload:

```go
			timeoutPayload := map[string]any{
				"model":      in.Task.Model,
				"timeout_ms": te.Timeout.Milliseconds(),
				"elapsed_ms": te.Elapsed.Milliseconds(),
			}
			addOutputFields(timeoutPayload, res)
			Emit(ctx, ev, in.Task.ID, task.EventRunnerTimeout, te.Error(), timeoutPayload)
			return runner.Result{}, runErr
```

Note the failure path currently returns `runner.Result{}` — change those returns to keep `res` so partial output is preserved when the timeout/exit path returns:

```go
		return res, runErr   // was: return runner.Result{}, runErr
```

(Apply to both the timeout branch and the generic-error branch.)

Add the helper at the bottom of the file:

```go
// addOutputFields merges runner Result output telemetry into a payload map
// when the runner reported any. Mock runs that leave Result.Output* zero
// add no keys, preserving payload diffs for tests written before
// codex/log capture landed.
func addOutputFields(payload map[string]any, res runner.Result) {
	if res.OutputBytes > 0 {
		payload["output_bytes"] = res.OutputBytes
	}
	if res.OutputDropped > 0 {
		payload["output_dropped"] = res.OutputDropped
	}
	if res.OutputHead != "" {
		payload["output_head"] = res.OutputHead
	}
	if res.OutputTail != "" {
		payload["output_tail"] = res.OutputTail
	}
}
```

- [ ] **Step 6: Run worker tests**

Run: `go test ./internal/worker/... -race`

Expected: all PASS, including the two new ones.

- [ ] **Step 7: gofmt**

Run: `gofmt -l internal/worker/`

Expected: empty.

- [ ] **Step 8: Commit**

```bash
git add internal/worker/runtask.go internal/worker/run_test.go
git commit -m "feat(worker): enrich runner_end payload with codex output head/tail/bytes/dropped (#17)"
```

---

## Task 7: Documentation updates

**Files:**
- Modify: `examples/WORKFLOW.md`
- Modify: `docs/runbooks/personal-daily-workflow.md`
- Modify: `docs/symphony-integration.md`
- Modify: `README.md` (only if it currently references `codex.command`)

- [ ] **Step 1: Update `examples/WORKFLOW.md`**

Edit the `codex:` block to read:

```yaml
codex:
  command: codex exec
  # profile selects how the runner invokes codex:
  #   safe   (default) - codex exec --full-auto --skip-git-repo-check ...
  #   bypass           - codex exec --dangerously-bypass-approvals-and-sandbox ...
  #                      (only when the worker host is already isolated, e.g. a
  #                      container; codex bypasses its own sandbox + approval gates)
  #   custom           - run the literal codex.command via sh -lc; PROMPT.md
  #                      is piped on stdin. Escape hatch for bespoke wrappers.
  profile: safe
```

- [ ] **Step 2: Update `docs/runbooks/personal-daily-workflow.md`**

In the `### codex` subsection (around line 87), append after the existing yaml block:

```markdown
**Profiles** select how the runner invokes codex:

- `safe` (default): builds `codex exec --full-auto --skip-git-repo-check --cd <workdir> -o .aiops/CODEX_LAST_MESSAGE.md` from argv (no shell). PROMPT.md is piped on stdin. `--full-auto` is codex's documented shorthand for `--sandbox workspace-write --ask-for-approval=never`.
- `bypass`: same shape but with `--dangerously-bypass-approvals-and-sandbox`. Use only when the worker host is already isolated (container, dedicated VM); the flag turns codex's own sandbox off.
- `custom`: runs the literal `codex.command` via `sh -lc` with PROMPT.md on stdin. Note the change from earlier versions: the runner no longer appends `< .aiops/PROMPT.md` to the command — your command must consume stdin (which `codex exec` does by default when no positional prompt is given).

### Reading codex output after a run

Each codex run writes `.aiops/CODEX_OUTPUT.txt` (combined stdout+stderr, capped at 1 MiB with a truncation footer when the cap fires) and reads `.aiops/CODEX_LAST_MESSAGE.md` (codex's own `-o` artifact) as the run summary. The `runner_end` task event payload also carries `output_head` (first 4 KiB), `output_tail` (last 4 KiB if non-overlapping), `output_bytes`, and `output_dropped` for at-a-glance triage from `/v1/tasks/<id>/events` without cloning the work branch.
```

- [ ] **Step 3: Update `docs/symphony-integration.md`**

In the "Not yet implemented" list, replace `- advanced sandboxing` with:

```markdown
- OS-level sandboxing (sandbox-exec, firejail, container isolation). Codex CLI's own sandbox is wired via `codex.profile`.
```

Optionally add at the bottom of the file:

```markdown
## Pointers

- Symphony's richer codex integration uses the long-running `codex app-server` JSON-RPC protocol (`elixir/lib/symphony_elixir/codex/app_server.ex`) and exposes per-turn sandbox overrides via `Codex.changeset` (`elixir/lib/symphony_elixir/config/schema.ex`). This platform's M4 stays on one-shot `codex exec`; an app-server-style integration is a candidate for M5+.
```

- [ ] **Step 4: Update README.md if applicable**

Run: `grep -n 'codex.command' README.md || echo 'no reference; skipping'`

If the file references `codex.command`, append a sentence to that paragraph: "Set `codex.profile: safe` (default), `bypass`, or `custom` to control how the runner invokes codex; see `docs/runbooks/personal-daily-workflow.md` for the trade-offs." Otherwise skip this step.

- [ ] **Step 5: Verify docs render and gofmt is still clean**

Run: `gofmt -l . | head` (should not list anything inside `internal/`).

- [ ] **Step 6: Commit**

```bash
git add examples/WORKFLOW.md docs/runbooks/personal-daily-workflow.md docs/symphony-integration.md
# include README.md only if step 4 modified it
git commit -m "docs: document codex.profile and CODEX_OUTPUT.txt artifact (#17)"
```

---

## Task 8: Final integration verification

**Files:** none modified; verification only.

- [ ] **Step 1: Run the CI gate end to end**

Run:

```bash
gofmt -l $(git ls-files '*.go') ; echo "---" ; go mod tidy && git diff --exit-code -- go.mod go.sum && echo "go mod tidy clean"
```

Expected: `gofmt` output empty; `go mod tidy clean` printed.

- [ ] **Step 2: Run the full test suite with race**

Run: `go test -race -covermode=atomic ./...`

Expected: PASS across all packages.

- [ ] **Step 3: Verify all binaries build**

Run: `go build ./cmd/trigger-api ./cmd/worker ./cmd/linear-poller`

Expected: clean exit, no output.

- [ ] **Step 4: Quick `--print-config` smoke for codex.profile surfacing**

Run:

```bash
mkdir -p /tmp/codex-profile-smoke && cat > /tmp/codex-profile-smoke/WORKFLOW.md <<'YAML'
---
repo:
  clone_url: file:///tmp/repo
tracker:
  kind: linear
agent:
  default: codex
codex:
  command: codex exec
  profile: bypass
---
prompt
YAML
go run ./cmd/worker --print-config /tmp/codex-profile-smoke
```

Expected: JSON output where `config.codex.profile` is `"bypass"`. (If `--print-config` doesn't include the new field, that's fine — it's not in scope to extend the redactor here unless the existing serializer drops `omitempty` empty fields.)

- [ ] **Step 5: Sanity check git log**

Run: `git log --oneline | head -10`

Expected: 6-7 new commits on top of the spec commit (`d40281c`), each scoped to one task.

- [ ] **Step 6: Verify spec acceptance criteria all map to landed code**

Open `docs/superpowers/specs/2026-05-11-safe-codex-runner-design.md` "Acceptance Criteria Mapping" table and confirm each row points to code that now exists. If any row is unimplemented, file a follow-up TODO and discuss with the requester before declaring the issue done.

- [ ] **Step 7: Push, open PR**

Push the branch and open a PR titled `feat: implement safe codex runner profile (#17)`. Body bullets:

- Adds `codex.profile` schema field (`safe` default, `bypass`, `custom`) with loader validation.
- New `internal/runner/codex.go` `CodexRunner` replaces ShellRunner for codex; argv-form invocation with PROMPT.md on stdin.
- Captures stdout/stderr to `.aiops/CODEX_OUTPUT.txt` (1 MiB cap with truncation footer) and surfaces head/tail/bytes/dropped on `runner_end` / `runner_timeout` event payloads.
- Picks up `.aiops/CODEX_LAST_MESSAGE.md` as `Result.Summary` on success.
- Behavior change for `profile=custom`: PROMPT.md goes via stdin instead of `< .aiops/PROMPT.md` shell redirect — runbook update calls this out.
- Closes #17.

---

## Self-Review Notes

- **Spec coverage:** every acceptance row in the spec maps to one of Tasks 2 (schema), 3 (safe profile + log capture), 5 (custom profile), 6 (event payload), 7 (docs). Task 1 implements the shared cappedWriter; Task 4 keeps existing tests meaningful.
- **No placeholders:** every code step contains the actual code. Test code includes assertions, not "add tests for the above". Commands include expected output.
- **Type consistency:** `CodexRunner`, `cappedWriter`, `Result.OutputBytes/OutputDropped/OutputHead/OutputTail`, `addOutputFields`, `CodexOutputCap`, `CodexEventOutputCap`, `CodexOutputPath`, `CodexLastMessagePath`, `PromptPath` are used identically across tasks.
- **Decomposition:** capture helper / schema / runner / wiring / payload / docs are landed in separate commits so a reviewer can read them in order without holding the whole feature in their head.
