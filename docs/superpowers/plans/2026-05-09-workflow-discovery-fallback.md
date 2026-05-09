# Workflow Discovery and Fallback Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make worker behavior deterministic about WHERE `WORKFLOW.md` is loaded from and WHAT was used on a given run, by introducing a `Resolve(workdir)` function with documented precedence (root → `.aiops/` → `.github/`), a new `workflow_resolved` task event, and a `worker --print-config <workdir>` debug subcommand.

**Architecture:** New `internal/workflow/resolver.go` carries `Source` enum, `Resolution` value type, and `Resolve()`. The worker swaps `LoadOptional` for `Resolve` and emits `workflow_resolved` before `runner_start`. A new `cmd/worker/print_config.go` adds a `--print-config` subcommand that masks `tracker.api_key` and summarizes (not prints) the prompt body.

**Tech Stack:** Go 1.22 (standard library only — `os`, `path/filepath`, `strings`, `encoding/json`, `testing`); existing `gopkg.in/yaml.v3` used transitively through `workflow.Load`.

**Spec:** [`docs/superpowers/specs/2026-05-09-workflow-discovery-fallback-design.md`](../specs/2026-05-09-workflow-discovery-fallback-design.md)

---

## File Structure

**Create:**
- `internal/workflow/resolver.go` — `Source` constants, `Resolution` struct, `Resolve()` function, `hasFrontMatterAt` helper
- `internal/workflow/resolver_test.go` — discovery test matrix
- `cmd/worker/print_config.go` — `printConfig` entry, `maskSecrets` helper, `summarizePrompt` helper
- `cmd/worker/print_config_test.go` — print-config tests including secret masking and prompt-body canary

**Modify:**
- `internal/workflow/loader.go` — add deprecation comment on `LoadOptional`
- `internal/task/task.go` — add `EventWorkflowResolved` constant
- `cmd/worker/main.go` — `runTask` switches to `Resolve`, emits `workflow_resolved`; `runRunnerWithTimeout` accepts and emits `workflow_source`; `main()` dispatches `--print-config`
- `cmd/worker/main_test.go` — add `resolveWorkflow` helper test, `runner_start` payload assertion
- `README.md` — new "WORKFLOW.md discovery" subsection

**Touch nothing:** `cmd/linear-poller/main.go` (still uses `Load(path)` with explicit path), `cmd/api/*`, `internal/runner/*`, `internal/queue/*`.

---

## Task 1: `Resolve()` returns defaults when no WORKFLOW.md exists

**Files:**
- Create: `internal/workflow/resolver.go`
- Test: `internal/workflow/resolver_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/workflow/resolver_test.go`:

```go
package workflow

import (
	"testing"
)

// TestResolve_NoFileReturnsDefaults pins the contract from spec section
// "Discovery Contract": when no WORKFLOW.md exists in any of the search
// locations, Resolve must succeed with Source=default and the schema
// defaults applied (so a fresh repo can run with the mock runner).
func TestResolve_NoFileReturnsDefaults(t *testing.T) {
	dir := t.TempDir()
	wf, res, err := Resolve(dir)
	if err != nil {
		t.Fatalf("Resolve: unexpected error: %v", err)
	}
	if res.Source != SourceDefault {
		t.Fatalf("Source = %q, want %q", res.Source, SourceDefault)
	}
	if res.Path != "" {
		t.Fatalf("Path = %q, want empty (no file)", res.Path)
	}
	if len(res.ShadowedBy) != 0 {
		t.Fatalf("ShadowedBy = %v, want empty", res.ShadowedBy)
	}
	if wf == nil || wf.Config.Agent.Default != "mock" {
		t.Fatalf("default Agent.Default = %q, want %q", wf.Config.Agent.Default, "mock")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/workflow/ -run TestResolve_NoFileReturnsDefaults`

Expected: FAIL with "undefined: Resolve" / "undefined: SourceDefault".

- [ ] **Step 3: Write minimal implementation**

Create `internal/workflow/resolver.go`:

```go
package workflow

// Source describes where the effective workflow came from on a given
// load. SourceFile means a WORKFLOW.md with valid YAML front matter was
// found and parsed; SourcePromptOnly means a file existed but had no
// front matter (the body became the prompt template, config came from
// schema defaults); SourceDefault means no file was found at all.
type Source string

const (
	SourceFile       Source = "file"
	SourcePromptOnly Source = "prompt_only"
	SourceDefault    Source = "default"
)

// Resolution carries the runtime fact of how a workflow was loaded for
// a single task. It is intentionally separate from Workflow because
// "where did this come from on this invocation" is not a property of
// the configuration itself; it is per-load metadata that the worker
// uses to populate the workflow_resolved event.
type Resolution struct {
	Source     Source
	Path       string   // repo-relative; "" when Source == SourceDefault
	ShadowedBy []string // other repo-relative paths that exist but lost precedence
}

// Resolve discovers WORKFLOW.md inside workdir, applying the documented
// precedence (root > .aiops/ > .github/), and returns the loaded
// Workflow alongside a Resolution describing the source. When no file
// is found, the schema defaults are returned with Source=default.
func Resolve(workdir string) (*Workflow, *Resolution, error) {
	cfg := DefaultConfig()
	expandConfig(&cfg)
	wf := &Workflow{Config: cfg, PromptTemplate: DefaultPrompt()}
	return wf, &Resolution{Source: SourceDefault}, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/workflow/ -run TestResolve_NoFileReturnsDefaults`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/workflow/resolver.go internal/workflow/resolver_test.go
git commit -m "feat(workflow): add Resolve scaffold returning defaults when no file (#10)"
```

---

## Task 2: `Resolve()` finds root `WORKFLOW.md` and classifies as `file`

**Files:**
- Modify: `internal/workflow/resolver.go`
- Test: `internal/workflow/resolver_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/workflow/resolver_test.go`:

```go
import (
	"os"
	"path/filepath"
)

// TestResolve_FindsRootWorkflowFile covers the most common case: a
// WORKFLOW.md at the repo root with valid YAML front matter resolves
// as Source=file with the relative path "WORKFLOW.md".
func TestResolve_FindsRootWorkflowFile(t *testing.T) {
	dir := t.TempDir()
	body := "---\nrepo:\n  owner: o\n  name: r\n  clone_url: git@example.com:o/r.git\n---\nprompt body\n"
	if err := os.WriteFile(filepath.Join(dir, "WORKFLOW.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	wf, res, err := Resolve(dir)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Source != SourceFile {
		t.Fatalf("Source = %q, want %q", res.Source, SourceFile)
	}
	if res.Path != "WORKFLOW.md" {
		t.Fatalf("Path = %q, want %q", res.Path, "WORKFLOW.md")
	}
	if wf.Config.Repo.CloneURL != "git@example.com:o/r.git" {
		t.Fatalf("CloneURL = %q, not loaded from front matter", wf.Config.Repo.CloneURL)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/workflow/ -run TestResolve_FindsRootWorkflowFile`

Expected: FAIL — "Source = \"default\", want \"file\"".

- [ ] **Step 3: Replace `Resolve` with discovery logic**

Edit `internal/workflow/resolver.go` — replace the body of `Resolve` (keep imports/types):

```go
import (
	"os"
	"path/filepath"
	"strings"
)

var resolveCandidates = []string{
	"WORKFLOW.md",
	".aiops/WORKFLOW.md",
	".github/WORKFLOW.md",
}

func Resolve(workdir string) (*Workflow, *Resolution, error) {
	var found string
	for _, rel := range resolveCandidates {
		abs := filepath.Join(workdir, rel)
		info, err := os.Stat(abs)
		if err == nil && !info.IsDir() {
			if found == "" {
				found = rel
			}
			continue
		}
		if err != nil && !os.IsNotExist(err) {
			return nil, nil, err
		}
	}
	if found == "" {
		cfg := DefaultConfig()
		expandConfig(&cfg)
		wf := &Workflow{Config: cfg, PromptTemplate: DefaultPrompt()}
		return wf, &Resolution{Source: SourceDefault}, nil
	}
	abs := filepath.Join(workdir, found)
	wf, err := Load(abs)
	if err != nil {
		return nil, nil, err
	}
	return wf, &Resolution{Source: SourceFile, Path: found}, nil
}

// hasFrontMatterAt returns true when path begins with a YAML front
// matter fence (`---\n` or `---\r\n`) followed somewhere by a closing
// fence. Used by Resolve to distinguish prompt-only files from full
// workflow files without threading a flag out of Load.
func hasFrontMatterAt(path string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	s := string(b)
	if !strings.HasPrefix(s, "---\n") && !strings.HasPrefix(s, "---\r\n") {
		return false
	}
	trimmed := strings.TrimPrefix(strings.TrimPrefix(s, "---\r\n"), "---\n")
	return strings.Contains(trimmed, "\n---")
}
```

`hasFrontMatterAt` is unused in this task but added now so Task 3's diff stays small. The body mirrors the un-exported `splitFrontMatter` in `loader.go` deliberately — duplicating ~5 lines is cheaper than exporting an internal helper that promises a stable contract.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/workflow/`

Expected: all `TestResolve_*` pass; pre-existing `TestLoad_*` and `TestLoadOptional*` continue to pass.

- [ ] **Step 5: Commit**

```bash
git add internal/workflow/resolver.go internal/workflow/resolver_test.go
git commit -m "feat(workflow): Resolve discovers root WORKFLOW.md (#10)"
```

---

## Task 3: `Resolve()` classifies prompt-only files as `prompt_only`

**Files:**
- Modify: `internal/workflow/resolver.go`
- Test: `internal/workflow/resolver_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/workflow/resolver_test.go`:

```go
// TestResolve_PromptOnlyFile pins the spec contract that a WORKFLOW.md
// without a YAML front matter block resolves as Source=prompt_only:
// the body becomes the prompt template, but config falls through to
// schema defaults. This is consistent with TestLoad_AcceptsPromptOnlyFile.
func TestResolve_PromptOnlyFile(t *testing.T) {
	dir := t.TempDir()
	body := "just a prompt template, no front matter\n"
	if err := os.WriteFile(filepath.Join(dir, "WORKFLOW.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	wf, res, err := Resolve(dir)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Source != SourcePromptOnly {
		t.Fatalf("Source = %q, want %q", res.Source, SourcePromptOnly)
	}
	if res.Path != "WORKFLOW.md" {
		t.Fatalf("Path = %q, want %q", res.Path, "WORKFLOW.md")
	}
	if wf.PromptTemplate != "just a prompt template, no front matter" {
		t.Fatalf("PromptTemplate = %q", wf.PromptTemplate)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/workflow/ -run TestResolve_PromptOnlyFile`

Expected: FAIL — "Source = \"file\", want \"prompt_only\"".

- [ ] **Step 3: Branch on front-matter presence**

Edit `internal/workflow/resolver.go`. Replace the final `return` in `Resolve` (the one producing `SourceFile`) with:

```go
src := SourceFile
if !hasFrontMatterAt(abs) {
	src = SourcePromptOnly
}
return wf, &Resolution{Source: src, Path: found}, nil
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/workflow/`

Expected: all pass.

- [ ] **Step 5: Commit**

```bash
git add internal/workflow/resolver.go internal/workflow/resolver_test.go
git commit -m "feat(workflow): classify prompt-only files in Resolve (#10)"
```

---

## Task 4: `Resolve()` searches `.aiops/` and `.github/` in priority order

**Files:**
- Test: `internal/workflow/resolver_test.go`

This task is test-only — Task 2 already wired the candidate list in priority order. We pin the precedence with explicit cases.

- [ ] **Step 1: Add the table-driven test**

Append to `internal/workflow/resolver_test.go`:

```go
// TestResolve_AlternateLocations covers the .aiops/ and .github/
// fallback locations declared in the discovery contract. Each case
// puts WORKFLOW.md in exactly one place; precedence (when multiple
// exist) is covered by TestResolve_ShadowedBy.
func TestResolve_AlternateLocations(t *testing.T) {
	body := "---\nrepo:\n  owner: o\n  name: r\n  clone_url: git@example.com:o/r.git\n---\nprompt\n"
	cases := []struct {
		name string
		rel  string
	}{
		{"aiops_dir", ".aiops/WORKFLOW.md"},
		{"github_dir", ".github/WORKFLOW.md"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			abs := filepath.Join(dir, tc.rel)
			if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			_, res, err := Resolve(dir)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if res.Source != SourceFile {
				t.Fatalf("Source = %q, want %q", res.Source, SourceFile)
			}
			if res.Path != tc.rel {
				t.Fatalf("Path = %q, want %q", res.Path, tc.rel)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it passes immediately**

Run: `go test ./internal/workflow/ -run TestResolve_AlternateLocations`

Expected: PASS — Task 2's `resolveCandidates` list already covers these.

- [ ] **Step 3: Commit**

```bash
git add internal/workflow/resolver_test.go
git commit -m "test(workflow): pin .aiops/ and .github/ discovery (#10)"
```

---

## Task 5: `Resolve()` reports `ShadowedBy` for lower-priority files

**Files:**
- Modify: `internal/workflow/resolver.go`
- Test: `internal/workflow/resolver_test.go`

Task 2's discovery loop selects the first hit but does not track the others. This task adds shadow tracking.

- [ ] **Step 1: Add shadow tracking to `Resolve`**

Edit `internal/workflow/resolver.go`. In `Resolve`, declare `shadows` alongside `found` and append to it on subsequent hits:

```go
var found string
var shadows []string
for _, rel := range resolveCandidates {
	abs := filepath.Join(workdir, rel)
	info, err := os.Stat(abs)
	if err == nil && !info.IsDir() {
		if found == "" {
			found = rel
		} else {
			shadows = append(shadows, rel)
		}
		continue
	}
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, err
	}
}
```

Update the file-found return to thread `shadows`:

```go
// in the "file found" branch (Source set above):
return wf, &Resolution{Source: src, Path: found, ShadowedBy: shadows}, nil
```

The `SourceDefault` branch is unchanged — when no file is found, there is nothing to shadow.

- [ ] **Step 2: Write the test**

Append to `internal/workflow/resolver_test.go`:

```go
// TestResolve_ShadowedBy covers the precedence contract: when multiple
// WORKFLOW.md files exist, the first in declaration order wins and the
// remaining ones are recorded in ShadowedBy in declaration order. The
// shadow list is data only — Resolve does not warn or error.
func TestResolve_ShadowedBy(t *testing.T) {
	dir := t.TempDir()
	body := "---\nrepo:\n  owner: o\n  name: r\n  clone_url: git@example.com:o/r.git\n---\nprompt\n"
	for _, rel := range []string{"WORKFLOW.md", ".aiops/WORKFLOW.md", ".github/WORKFLOW.md"} {
		abs := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	_, res, err := Resolve(dir)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Path != "WORKFLOW.md" {
		t.Fatalf("Path = %q, want root", res.Path)
	}
	want := []string{".aiops/WORKFLOW.md", ".github/WORKFLOW.md"}
	if len(res.ShadowedBy) != len(want) {
		t.Fatalf("ShadowedBy = %v, want %v", res.ShadowedBy, want)
	}
	for i := range want {
		if res.ShadowedBy[i] != want[i] {
			t.Fatalf("ShadowedBy[%d] = %q, want %q", i, res.ShadowedBy[i], want[i])
		}
	}
}
```

- [ ] **Step 3: Run tests**

Run: `go test ./internal/workflow/`

Expected: all pass. If `TestResolve_ShadowedBy` fails, recheck Step 1's `else` branch.

- [ ] **Step 4: Commit**

```bash
git add internal/workflow/resolver.go internal/workflow/resolver_test.go
git commit -m "feat(workflow): record shadowed_by in Resolve (#10)"
```

---

## Task 6: `Resolve()` propagates schema validation errors

**Files:**
- Test: `internal/workflow/resolver_test.go`

- [ ] **Step 1: Write the test**

Append to `internal/workflow/resolver_test.go`:

```go
// TestResolve_PropagatesSchemaErrors guards the contract that Resolve
// does NOT silently fall back to defaults when a file is found but
// fails schema validation. Falling back would mask real configuration
// mistakes and reduce the entire validation effort from #48/#49 to
// theatre. The error must name the offending field and the file path.
func TestResolve_PropagatesSchemaErrors(t *testing.T) {
	dir := t.TempDir()
	body := "---\nrepo:\n  owner: o\n  name: r\n---\nprompt\n" // no clone_url
	if err := os.WriteFile(filepath.Join(dir, "WORKFLOW.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, _, err := Resolve(dir)
	if err == nil {
		t.Fatalf("Resolve: expected error for missing clone_url, got nil")
	}
	for _, want := range []string{"repo.clone_url", "WORKFLOW.md"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q: want substring %q", err.Error(), want)
		}
	}
}
```

Add `"strings"` to the test file's imports if not already present.

- [ ] **Step 2: Run test**

Run: `go test ./internal/workflow/ -run TestResolve_PropagatesSchemaErrors`

Expected: PASS — Task 2 calls `Load(abs)` which already validates; the error propagates through `Resolve` unchanged.

- [ ] **Step 3: Commit**

```bash
git add internal/workflow/resolver_test.go
git commit -m "test(workflow): pin Resolve does not mask schema errors (#10)"
```

---

## Task 7: Mark `LoadOptional` deprecated

**Files:**
- Modify: `internal/workflow/loader.go`

- [ ] **Step 1: Add deprecation comment**

Edit `internal/workflow/loader.go`. Above the existing `func LoadOptional(path string) (*Workflow, error) {` line, replace any current comment with:

```go
// LoadOptional loads a workflow from an explicit path, returning schema
// defaults when the file does not exist. New worker code should use
// Resolve(workdir), which handles repo-relative discovery and returns
// resolution metadata.
//
// Deprecated: use Resolve(workdir) for repo-relative discovery. Retained
// for callers that pass an explicit path (e.g. cmd/linear-poller has a
// related but separate loader contract).
func LoadOptional(path string) (*Workflow, error) {
```

- [ ] **Step 2: Verify the package still builds**

Run: `go build ./...`

Expected: clean build. The `// Deprecated:` line is a Go doc convention; `go vet` / staticcheck may surface it but `go build` accepts it.

- [ ] **Step 3: Commit**

```bash
git add internal/workflow/loader.go
git commit -m "docs(workflow): deprecate LoadOptional in favor of Resolve (#10)"
```

---

## Task 8: Add `EventWorkflowResolved` constant

**Files:**
- Modify: `internal/task/task.go`

- [ ] **Step 1: Add the constant**

Edit `internal/task/task.go`. The existing `const` block lives around lines 22-34 (between `EventEnqueued` and `EventFailedAttempt`). Add `EventWorkflowResolved` between `EventClaimed` and `EventRunnerStart`:

```go
const (
	EventEnqueued         = "enqueued"
	EventClaimed          = "claimed"
	EventWorkflowResolved = "workflow_resolved"
	EventRunnerStart      = "runner_start"
	EventRunnerEnd        = "runner_end"
	// ... rest unchanged ...
)
```

The position before `EventRunnerStart` reflects the actual emit order in `runTask`: workflow resolution happens after claim and before any runner work.

- [ ] **Step 2: Verify**

Run: `go build ./...`

Expected: clean build.

- [ ] **Step 3: Commit**

```bash
git add internal/task/task.go
git commit -m "feat(task): add EventWorkflowResolved constant (#10)"
```

---

## Task 9: Worker uses `Resolve` and emits `workflow_resolved`

**Files:**
- Modify: `cmd/worker/main.go`
- Modify: `cmd/worker/main_test.go`

This task introduces a small `resolveWorkflow` helper inside the worker package so the emit logic can be unit-tested without `workspace.PrepareGitWorkspace` (which would clone real repositories). The helper takes an already-prepared workdir.

- [ ] **Step 1: Write the failing test**

Append to `cmd/worker/main_test.go`:

```go
// TestResolveWorkflow_EmitsResolvedEvent verifies the worker emits a
// workflow_resolved event whose payload carries Source, Path, and the
// effective config quick-look fields (agent_default, policy_mode,
// tracker_kind). These four fields are what the spec promises for the
// post-hoc inspection contract.
func TestResolveWorkflow_EmitsResolvedEvent(t *testing.T) {
	dir := t.TempDir()
	body := "---\nrepo:\n  owner: o\n  name: r\n  clone_url: git@example.com:o/r.git\nagent:\n  default: codex\npolicy:\n  mode: draft_pr\ntracker:\n  kind: linear\n---\nprompt\n"
	if err := os.WriteFile(filepath.Join(dir, "WORKFLOW.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	ev := &fakeEmitter{}
	wf, src, err := resolveWorkflow(context.Background(), ev, "tsk_1", dir)
	if err != nil {
		t.Fatalf("resolveWorkflow: %v", err)
	}
	if src != "file" {
		t.Fatalf("workflow_source = %q, want %q", src, "file")
	}
	if wf.Config.Agent.Default != "codex" {
		t.Fatalf("agent.default not loaded: %q", wf.Config.Agent.Default)
	}
	got := ev.byKind(task.EventWorkflowResolved)
	if len(got) != 1 {
		t.Fatalf("workflow_resolved events = %d, want 1", len(got))
	}
	payload, _ := got[0].Payload.(map[string]any)
	for _, key := range []string{"source", "path", "agent_default", "policy_mode", "tracker_kind"} {
		if _, ok := payload[key]; !ok {
			t.Fatalf("payload missing key %q: %#v", key, payload)
		}
	}
	if payload["source"] != "file" {
		t.Fatalf("payload.source = %v, want \"file\"", payload["source"])
	}
	if payload["path"] != "WORKFLOW.md" {
		t.Fatalf("payload.path = %v, want \"WORKFLOW.md\"", payload["path"])
	}
	if payload["agent_default"] != "codex" {
		t.Fatalf("payload.agent_default = %v, want \"codex\"", payload["agent_default"])
	}
	if _, present := payload["shadowed_by"]; present {
		t.Fatalf("payload should omit shadowed_by when empty: %#v", payload)
	}
}

// TestResolveWorkflow_DefaultSourceOmitsPath checks that when no
// WORKFLOW.md exists, the resolved event records source=default and
// does not emit an empty path key.
func TestResolveWorkflow_DefaultSourceOmitsPath(t *testing.T) {
	dir := t.TempDir()
	ev := &fakeEmitter{}
	_, src, err := resolveWorkflow(context.Background(), ev, "tsk_2", dir)
	if err != nil {
		t.Fatalf("resolveWorkflow: %v", err)
	}
	if src != "default" {
		t.Fatalf("workflow_source = %q, want %q", src, "default")
	}
	got := ev.byKind(task.EventWorkflowResolved)
	if len(got) != 1 {
		t.Fatalf("workflow_resolved events = %d, want 1", len(got))
	}
	payload, _ := got[0].Payload.(map[string]any)
	if _, present := payload["path"]; present {
		t.Fatalf("payload should omit path when source=default: %#v", payload)
	}
	if _, present := payload["shadowed_by"]; present {
		t.Fatalf("payload should omit shadowed_by when empty: %#v", payload)
	}
}
```

Imports already in `main_test.go` cover `context`, `os`, `path/filepath`, `testing`, `task`. If `path/filepath` is missing, add it.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./cmd/worker/ -run TestResolveWorkflow`

Expected: FAIL with "undefined: resolveWorkflow".

- [ ] **Step 3: Add the helper**

Edit `cmd/worker/main.go`. Add a new function near the top of the file (just below `eventEmitter` interface, before `func main()`):

```go
// resolveWorkflow performs WORKFLOW.md discovery for a prepared workdir
// and emits the workflow_resolved event before any runner work begins.
// Returning the workflow_source string lets callers stamp it onto the
// runner_start payload as a quick-look field; the full provenance lives
// on the workflow_resolved event itself.
func resolveWorkflow(ctx context.Context, ev eventEmitter, taskID, workdir string) (*workflow.Workflow, string, error) {
	wf, res, err := workflow.Resolve(workdir)
	if err != nil {
		return nil, "", err
	}
	payload := map[string]any{
		"source":        string(res.Source),
		"agent_default": wf.Config.Agent.Default,
		"policy_mode":   wf.Config.Policy.Mode,
		"tracker_kind":  wf.Config.Tracker.Kind,
	}
	if res.Path != "" {
		payload["path"] = res.Path
	}
	if len(res.ShadowedBy) > 0 {
		payload["shadowed_by"] = res.ShadowedBy
	}
	emit(ctx, ev, taskID, task.EventWorkflowResolved, "workflow resolved", payload)
	return wf, string(res.Source), nil
}
```

- [ ] **Step 4: Wire the helper into `runTask`**

In `cmd/worker/main.go`, replace the existing block at line 95 (current `wf, err := workflow.LoadOptional(workdir + "/WORKFLOW.md")`):

```go
wf, _, err := resolveWorkflow(ctx, ev, t.ID, workdir)
if err != nil {
	return workflow.Config{}, err
}
```

The discarded return is the workflow_source string — Task 10 wires it through to `runRunnerWithTimeout`. For now `runTask` keeps compiling.

- [ ] **Step 5: Run all worker tests**

Run: `go test ./cmd/worker/`

Expected: all `TestResolveWorkflow_*` pass; pre-existing tests continue to pass.

- [ ] **Step 6: Commit**

```bash
git add cmd/worker/main.go cmd/worker/main_test.go
git commit -m "feat(worker): emit workflow_resolved via Resolve helper (#10)"
```

---

## Task 10: `runner_start` payload includes `workflow_source`

**Files:**
- Modify: `cmd/worker/main.go`
- Modify: `cmd/worker/main_test.go`

- [ ] **Step 1: Write the failing test**

Append to `cmd/worker/main_test.go`:

```go
// TestRunRunnerWithTimeout_StampsWorkflowSource verifies the
// runner_start payload carries workflow_source as a quick-look field.
// The full provenance is on workflow_resolved; this stamp lets a
// timeline viewer color the runner stage by source without joining
// against the earlier event.
func TestRunRunnerWithTimeout_StampsWorkflowSource(t *testing.T) {
	ev := &fakeEmitter{}
	stub := stubRunner{}
	in := runner.RunInput{Task: task.Task{ID: "tsk_1", Model: "mock"}}
	if _, err := runRunnerWithTimeout(context.Background(), ev, stub, in, time.Second, "prompt_only"); err != nil {
		t.Fatalf("runRunnerWithTimeout: %v", err)
	}
	got := ev.byKind(task.EventRunnerStart)
	if len(got) != 1 {
		t.Fatalf("runner_start events = %d, want 1", len(got))
	}
	payload, _ := got[0].Payload.(map[string]any)
	if payload["workflow_source"] != "prompt_only" {
		t.Fatalf("workflow_source = %v, want %q", payload["workflow_source"], "prompt_only")
	}
}

type stubRunner struct{}

func (stubRunner) Run(_ context.Context, _ runner.RunInput) (runner.Result, error) {
	return runner.Result{Summary: "ok"}, nil
}
```

If `time` is not yet imported in `main_test.go`, add it.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/worker/ -run TestRunRunnerWithTimeout_StampsWorkflowSource`

Expected: FAIL — `runRunnerWithTimeout` currently takes 5 args, test passes 6.

- [ ] **Step 3: Extend `runRunnerWithTimeout` signature**

Edit `cmd/worker/main.go`. Change the function signature (currently at line 252):

```go
func runRunnerWithTimeout(ctx context.Context, ev eventEmitter, r runner.Runner, in runner.RunInput, timeout time.Duration, workflowSource string) (runner.Result, error) {
```

Inside the function, modify the runner_start emit (currently around line 257):

```go
emit(ctx, ev, in.Task.ID, task.EventRunnerStart, "runner started", map[string]any{
	"model":           in.Task.Model,
	"timeout_ms":      timeout.Milliseconds(),
	"workflow_source": workflowSource,
})
```

- [ ] **Step 4: Update the caller in `runTask`**

In `runTask`, two changes:

1. Capture the `workflow_source` from the `resolveWorkflow` helper introduced in Task 9 — replace `wf, _, err := resolveWorkflow(...)` with:

```go
wf, workflowSource, err := resolveWorkflow(ctx, ev, t.ID, workdir)
if err != nil {
	return workflow.Config{}, err
}
```

2. Pass it through to `runRunnerWithTimeout` (around line 136):

```go
if _, runErr := runRunnerWithTimeout(ctx, ev, r, runner.RunInput{Task: t, Workflow: *wf, Workdir: workdir, Prompt: prompt}, cfg.Agent.Timeout, workflowSource); runErr != nil {
```

- [ ] **Step 5: Run all tests**

Run: `go test ./cmd/worker/`

Expected: all pass.

- [ ] **Step 6: Commit**

```bash
git add cmd/worker/main.go cmd/worker/main_test.go
git commit -m "feat(worker): stamp workflow_source on runner_start payload (#10)"
```

---

## Task 11: `--print-config` skeleton (default source case)

**Files:**
- Create: `cmd/worker/print_config.go`
- Create: `cmd/worker/print_config_test.go`
- Modify: `cmd/worker/main.go`

- [ ] **Step 1: Write the failing test**

Create `cmd/worker/print_config_test.go`:

```go
package main

import (
	"bytes"
	"encoding/json"
	"testing"
)

// TestPrintConfig_DefaultSource verifies the simplest case: an empty
// workdir resolves to source=default, and the JSON output reports it
// without a path or shadowed_by field.
func TestPrintConfig_DefaultSource(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := printConfig(dir, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	var out struct {
		Resolution struct {
			Source     string   `json:"source"`
			Path       string   `json:"path,omitempty"`
			ShadowedBy []string `json:"shadowed_by,omitempty"`
		} `json:"resolution"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v\nstdout: %s", err, stdout.String())
	}
	if out.Resolution.Source != "default" {
		t.Fatalf("resolution.source = %q, want %q", out.Resolution.Source, "default")
	}
	if out.Resolution.Path != "" {
		t.Fatalf("resolution.path = %q, want empty", out.Resolution.Path)
	}
}
```

`t.TempDir()` is part of `testing` — already imported transitively.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/worker/ -run TestPrintConfig_DefaultSource`

Expected: FAIL — "undefined: printConfig".

- [ ] **Step 3: Create `cmd/worker/print_config.go`**

```go
package main

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// printConfigOutput is the JSON shape of `worker --print-config <dir>`.
// Stable: external tooling may consume it.
type printConfigOutput struct {
	Resolution     printConfigResolution `json:"resolution"`
	Config         workflow.Config       `json:"config"`
	PromptTemplate promptSummary         `json:"prompt_template"`
}

type printConfigResolution struct {
	Source     string   `json:"source"`
	Path       string   `json:"path,omitempty"`
	ShadowedBy []string `json:"shadowed_by,omitempty"`
}

// promptSummary is intentionally not the full prompt body. See spec
// section "Why prompt body is summarized, not printed" for the rationale.
type promptSummary struct {
	Length    int    `json:"length"`
	FirstLine string `json:"first_line"`
}

// printConfig writes the effective workflow for workdir as JSON to
// stdout. Returns the process exit code (0 on success, 1 on schema
// validation error). Used both by main()'s --print-config dispatch and
// by tests; stdout/stderr are explicit io.Writer parameters so tests
// can capture the output without subprocessing.
func printConfig(workdir string, stdout, stderr io.Writer) int {
	wf, res, err := workflow.Resolve(workdir)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 1
	}
	out := printConfigOutput{
		Resolution: printConfigResolution{
			Source:     string(res.Source),
			Path:       res.Path,
			ShadowedBy: res.ShadowedBy,
		},
		Config:         wf.Config,
		PromptTemplate: promptSummary{}, // populated in Task 12
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 1
	}
	return 0
}
```

- [ ] **Step 4: Wire `main()` dispatch**

In `cmd/worker/main.go`, at the top of `func main()` (before `ctx := context.Background()`):

```go
func main() {
	if len(os.Args) >= 3 && os.Args[1] == "--print-config" {
		os.Exit(printConfig(os.Args[2], os.Stdout, os.Stderr))
	}
	ctx := context.Background()
	// ... rest unchanged
}
```

- [ ] **Step 5: Run tests**

Run: `go test ./cmd/worker/ -run TestPrintConfig_DefaultSource`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add cmd/worker/print_config.go cmd/worker/print_config_test.go cmd/worker/main.go
git commit -m "feat(worker): add --print-config subcommand skeleton (#10)"
```

---

## Task 12: `--print-config` populates config and summarizes prompt body

**Files:**
- Modify: `cmd/worker/print_config.go`
- Modify: `cmd/worker/print_config_test.go`

- [ ] **Step 1: Write the failing test (canary included)**

Append to `cmd/worker/print_config_test.go`. Merge `os`, `path/filepath`, `strings` into the existing `import` block (do **not** add a second `import (…)` group — Go's tooling complains):

```go
// TestPrintConfig_FileSourceWithPromptCanary covers two contracts at once:
//
//  1. Source=file populates config from the front matter (here: a
//     non-default agent.default and tracker.kind).
//  2. The prompt body is summarized rather than echoed. We embed a
//     recognizable canary string in the body and assert it never reaches
//     stdout. This is the spec's safety contract — see "Why prompt body
//     is summarized, not printed".
func TestPrintConfig_FileSourceWithPromptCanary(t *testing.T) {
	dir := t.TempDir()
	canary := "SHOULD_NOT_LEAK_xyz"
	body := "---\nrepo:\n  owner: o\n  name: r\n  clone_url: git@example.com:o/r.git\nagent:\n  default: codex\ntracker:\n  kind: linear\n---\nFirst line of prompt template.\nSecond line includes canary " + canary + " in the middle.\nMore body...\n"
	if err := os.WriteFile(filepath.Join(dir, "WORKFLOW.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	var stdout, stderr bytes.Buffer
	if code := printConfig(dir, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}

	if strings.Contains(stdout.String(), canary) {
		t.Fatalf("canary %q leaked into stdout:\n%s", canary, stdout.String())
	}

	var out struct {
		Resolution struct {
			Source string `json:"source"`
		} `json:"resolution"`
		Config struct {
			Agent struct {
				Default string `json:"default"`
			} `json:"agent"`
		} `json:"config"`
		PromptTemplate struct {
			Length    int    `json:"length"`
			FirstLine string `json:"first_line"`
		} `json:"prompt_template"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v\nstdout: %s", err, stdout.String())
	}
	if out.Resolution.Source != "file" {
		t.Fatalf("source = %q, want %q", out.Resolution.Source, "file")
	}
	if out.Config.Agent.Default != "codex" {
		t.Fatalf("agent.default = %q, want %q", out.Config.Agent.Default, "codex")
	}
	if out.PromptTemplate.FirstLine != "First line of prompt template." {
		t.Fatalf("first_line = %q", out.PromptTemplate.FirstLine)
	}
	if out.PromptTemplate.Length <= 0 {
		t.Fatalf("length = %d, want > 0", out.PromptTemplate.Length)
	}
}
```

After this edit the import block at the top of `cmd/worker/print_config_test.go` should be:

```go
import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/worker/ -run TestPrintConfig_FileSourceWithPromptCanary`

Expected: FAIL — `first_line` is empty (Task 11's stub).

- [ ] **Step 3: Implement `summarizePrompt`**

Edit `cmd/worker/print_config.go`. Add a helper function:

```go
const promptFirstLineMaxBytes = 200

// summarizePrompt produces the bounded prompt summary published by
// --print-config. Length is the byte length of the trimmed body so a
// reader can sanity-check completeness; FirstLine is truncated to keep
// debug output cheap to paste even when an author writes a long single
// line. The full body is never echoed (see spec safety contract).
func summarizePrompt(body string) promptSummary {
	trimmed := strings.TrimSpace(body)
	first := trimmed
	if i := strings.IndexByte(first, '\n'); i >= 0 {
		first = first[:i]
	}
	if len(first) > promptFirstLineMaxBytes {
		first = first[:promptFirstLineMaxBytes]
	}
	return promptSummary{
		Length:    len(trimmed),
		FirstLine: first,
	}
}
```

Add `"strings"` to the imports.

Then in `printConfig`, populate `out.PromptTemplate`:

```go
out := printConfigOutput{
	Resolution: printConfigResolution{
		Source:     string(res.Source),
		Path:       res.Path,
		ShadowedBy: res.ShadowedBy,
	},
	Config:         wf.Config,
	PromptTemplate: summarizePrompt(wf.PromptTemplate),
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./cmd/worker/`

Expected: all pass.

- [ ] **Step 5: Commit**

```bash
git add cmd/worker/print_config.go cmd/worker/print_config_test.go
git commit -m "feat(worker): summarize prompt body in --print-config (#10)"
```

---

## Task 13: `--print-config` masks `tracker.api_key`

**Files:**
- Modify: `cmd/worker/print_config.go`
- Modify: `cmd/worker/print_config_test.go`

- [ ] **Step 1: Write the failing test**

Append to `cmd/worker/print_config_test.go`:

```go
// TestPrintConfig_MasksTrackerAPIKey verifies the secret-masking
// contract from the spec. Even when --print-config is used legitimately
// for debugging, the API key must never reach stdout. We test with the
// env-var indirection style that examples/WORKFLOW.md uses, since that
// is the realistic source of a non-empty key.
func TestPrintConfig_MasksTrackerAPIKey(t *testing.T) {
	t.Setenv("AIOPS_TEST_LINEAR_KEY", "lin_super_secret_value")
	dir := t.TempDir()
	body := "---\nrepo:\n  owner: o\n  name: r\n  clone_url: git@example.com:o/r.git\ntracker:\n  kind: linear\n  api_key: $AIOPS_TEST_LINEAR_KEY\n---\nprompt\n"
	if err := os.WriteFile(filepath.Join(dir, "WORKFLOW.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	var stdout, stderr bytes.Buffer
	if code := printConfig(dir, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	got := stdout.String()
	if strings.Contains(got, "lin_super_secret_value") {
		t.Fatalf("api_key value leaked into stdout:\n%s", got)
	}
	if !strings.Contains(got, `"api_key": "***"`) {
		t.Fatalf("api_key not masked; stdout:\n%s", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/worker/ -run TestPrintConfig_MasksTrackerAPIKey`

Expected: FAIL — `api_key` value appears in stdout because `wf.Config` is encoded as-is.

- [ ] **Step 3: Add masking helper**

Edit `cmd/worker/print_config.go`. Add:

```go
const maskedSecret = "***"

// maskSecrets rewrites secret-bearing fields on a Config to a fixed
// placeholder before serialization. The function takes its argument by
// value; the workflow.Config used by the running worker is never
// touched. Currently only Tracker.APIKey is masked — extend this list
// when new secret-bearing fields are added to the schema.
func maskSecrets(cfg workflow.Config) workflow.Config {
	if cfg.Tracker.APIKey != "" {
		cfg.Tracker.APIKey = maskedSecret
	}
	return cfg
}
```

In `printConfig`, replace `Config: wf.Config` with:

```go
Config: maskSecrets(wf.Config),
```

- [ ] **Step 4: Run tests**

Run: `go test ./cmd/worker/`

Expected: all pass.

- [ ] **Step 5: Commit**

```bash
git add cmd/worker/print_config.go cmd/worker/print_config_test.go
git commit -m "feat(worker): mask tracker.api_key in --print-config output (#10)"
```

---

## Task 14: `--print-config` schema errors → stderr, exit 1

**Files:**
- Modify: `cmd/worker/print_config_test.go`

The `printConfig` function from Task 11 already returns 1 and writes to stderr when `Resolve` fails. Pin the contract.

- [ ] **Step 1: Write the test**

Append to `cmd/worker/print_config_test.go`:

```go
// TestPrintConfig_SchemaErrorReturnsExitOne pins the contract that
// schema validation failures produce a non-zero exit and route the
// human-readable error to stderr. Stdout must remain empty so a script
// piping the JSON elsewhere does not feed it a malformed document.
func TestPrintConfig_SchemaErrorReturnsExitOne(t *testing.T) {
	dir := t.TempDir()
	body := "---\nrepo:\n  owner: o\n  name: r\n---\nprompt\n" // no clone_url
	if err := os.WriteFile(filepath.Join(dir, "WORKFLOW.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	var stdout, stderr bytes.Buffer
	code := printConfig(dir, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout not empty on error: %s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "repo.clone_url") {
		t.Fatalf("stderr missing field name: %s", stderr.String())
	}
}
```

- [ ] **Step 2: Run test**

Run: `go test ./cmd/worker/ -run TestPrintConfig_SchemaErrorReturnsExitOne`

Expected: PASS — Task 11's implementation already exits 1 and writes to stderr.

- [ ] **Step 3: Commit**

```bash
git add cmd/worker/print_config_test.go
git commit -m "test(worker): pin --print-config error semantics (#10)"
```

---

## Task 15: README discovery section

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Locate the insertion point**

Run: `grep -n "internal/workflow" /Users/yvan/developer/aiops-platform/README.md`

Expected line: `internal/workflow: loads repo-owned WORKFLOW.md configuration and prompt body.`

This is around line 31, inside the project layout list. The new section goes immediately after the layout list ends (the README has a blank line before the next H2; insert there).

- [ ] **Step 2: Write the section**

Add this content to `README.md`, placed before the existing `## Two-track workflow` heading or wherever the project layout block ends:

```markdown
## WORKFLOW.md discovery

The worker looks for `WORKFLOW.md` in three locations and uses the first one it finds:

1. `<repo>/WORKFLOW.md`
2. `<repo>/.aiops/WORKFLOW.md`
3. `<repo>/.github/WORKFLOW.md`

When multiple files exist, lower-priority files are recorded as `shadowed_by` on the `workflow_resolved` event but are otherwise ignored. The worker does not warn or fail.

If none of the three exist, the worker proceeds with built-in defaults:

| Setting | Default |
|---------|---------|
| `agent.default` | `mock` |
| `agent.timeout` | `30m` |
| `agent.max_concurrent_agents` | `1` |
| `pr.draft` | `false` |
| `pr.labels` | `[ai-generated, needs-review]` |
| `policy.mode` | `draft_pr` |
| `policy.max_changed_files` | `12` |
| `policy.max_changed_loc` | `300` |
| `verify.commands` | none |

A `WORKFLOW.md` with no YAML front matter (just a prompt body) is supported: the body becomes the prompt template, all other settings fall through to the defaults above. The `workflow_resolved` event records this as `source: prompt_only` so an operator can tell apart "ran with full Symphony config" from "ran with body-only template".

To inspect the effective configuration for a workdir without consuming a task:

```bash
worker --print-config /path/to/repo/clone
```

The output is JSON. `tracker.api_key` is masked as `***`; the prompt template is summarized (length + first line) rather than printed verbatim — `cat <resolution.path>` to see the full body.

For post-hoc inspection, the `workflow_resolved` task event records the source, path, and shadowed list of every run.
```

- [ ] **Step 3: Verify the README still reads coherently**

Run: `grep -n "## " /Users/yvan/developer/aiops-platform/README.md`

Expected: section ordering still flows naturally (project layout → WORKFLOW.md discovery → Two-track workflow / etc.).

- [ ] **Step 4: Commit**

```bash
git add README.md
git commit -m "docs: document WORKFLOW.md discovery contract (#10)"
```

---

## Final verification

- [ ] **Step 1: Run the full test suite**

Run: `go test ./...`

Expected: all packages pass.

- [ ] **Step 2: Run `go vet`**

Run: `go vet ./...`

Expected: clean.

- [ ] **Step 3: Smoke-test `--print-config` against the in-tree example**

Run:
```bash
go build -o /tmp/aiops-worker ./cmd/worker
mkdir -p /tmp/aiops-pc-smoke
cp examples/WORKFLOW.md /tmp/aiops-pc-smoke/WORKFLOW.md
/tmp/aiops-worker --print-config /tmp/aiops-pc-smoke
```

Expected: JSON document with `resolution.source = "file"`, `resolution.path = "WORKFLOW.md"`, no leakage of any value matching the contents of `$LINEAR_API_KEY`.

- [ ] **Step 4: Confirm acceptance criteria from issue #10**

Walk the four lines of issue #10 against the merged code:
- [ ] Worker loads repo `WORKFLOW.md` when present — Tasks 2, 4
- [ ] Worker uses safe defaults when absent — Task 1
- [ ] Task event records which workflow file/default was used — Tasks 8, 9, 10
- [ ] README documents where `WORKFLOW.md` should live — Task 15

- [ ] **Step 5: Open PR**

```bash
git push -u origin <branch>
gh pr create --title "feat: workflow discovery and fallback (#10)" --body "$(cat <<'EOF'
## Summary
- Adds workflow.Resolve(workdir) with documented precedence (root > .aiops/ > .github/) and source classification (file / prompt_only / default).
- New workflow_resolved task event captures source, path, shadowed_by, and key effective config fields.
- New worker --print-config <workdir> debug subcommand; masks tracker.api_key and summarizes the prompt body to avoid leaks via shell history / support bundles.
- README documents the discovery contract and the default values an absent WORKFLOW.md falls back to.

Closes #10. Spec at docs/superpowers/specs/2026-05-09-workflow-discovery-fallback-design.md.

## Test plan
- [ ] go test ./... passes
- [ ] worker --print-config against examples/WORKFLOW.md does not leak tracker.api_key value
- [ ] worker --print-config against an empty workdir reports source=default
EOF
)"
```
