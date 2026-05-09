# Verifier Phase Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Harden the verifier phase (issue #18) so it runs all configured commands and collects their results, supports a phase-level timeout, and offers an opt-in "investigation" override that opens a degraded draft PR when verification fails.

**Architecture:** The verify phase already exists in `cmd/worker/main.go` between policy enforcement and the RUN_SUMMARY gate. This plan does not relocate the phase; it changes the semantics of `workspace.RunVerify` (collect-all instead of short-circuit, plus phase-level deadline) and threads a new `verify.allow_failure` configuration through `runTask` so a failing verify with the override opens a draft PR with a banner annotation. The schema gains two new fields (`verify.timeout`, `verify.allow_failure`); existing `verify.commands` and `verify.secret_scan` are unchanged.

**Tech Stack:** Go 1.22, gopkg.in/yaml.v3, existing internal packages (`internal/workflow`, `internal/workspace`, `internal/task`, `internal/gitea`).

---

## Context

The current state (read before starting):
- `internal/workflow/config.go` — `VerifyConfig{Commands, SecretScan}`. No timeout, no allow-failure flag.
- `internal/workspace/manager.go:151` — `RunVerify` short-circuits on first failing command. Returns `(results, error)` where error is the first failure.
- `cmd/worker/main.go:180` — `runTask` emits `verify_start`/`verify_end`, calls `WriteVerification`, and returns the verify error immediately on any failure (preventing PR creation).
- `cmd/worker/main.go:533` — `buildPRBody` builds the PR body. No verify-state input today.
- `internal/gitea/client.go:18` — `CreatePullRequestInput` has `Draft bool`. No `Labels` field. Adding labels would need a separate Gitea endpoint call; **out of scope**.

## Design decisions locked

(Confirmed by user; see conversation transcript 2026-05-09.)

1. **Collect-all** — `RunVerify` runs every non-empty command and returns one result per command, even after failures. Surfacing all failures at once shortens the AI agent's rework loop.
2. **Phase-level timeout** — A single `verify.timeout` caps the entire phase, not per-command. Default `0` = unbounded so existing repos see no behavior change.
3. **Global investigation override only** — Issue AC requires "Optional config allows PR creation with failed verification for investigation." A single boolean `verify.allow_failure` covers this. Per-command `continue_on_error` was discussed in brainstorming (Q2) but is **deferred**: it requires schema polymorphism (`commands` becoming string-or-object) and is not required by the issue AC. Add it in a separate PR if real demand emerges.
4. **Events are the source of truth** — `VERIFICATION.txt` already serves as the human-readable artifact derived from the same `[]VerifyResult` slice the events summarize. No new artifact file. The PR body gains a banner when running in degraded mode.
5. **Degraded PR semantics** — When verify fails AND `allow_failure: true`, the worker:
   - Emits `verify_end` with `status: failed_allowed` (distinct from `failed`).
   - Forces the PR to draft regardless of `pr.draft`.
   - Prepends a `> ⚠ Verification failed (investigation mode)` banner to the PR body.
   - Continues through the secret scan, summary gate, push, and PR creation.

## Out of scope

- Per-command `continue_on_error` (see decision 3).
- Adding labels to the Gitea PR (`internal/gitea` would need a separate /labels call).
- Parallel command execution (sequential is fine; verify commands are typically fast).
- Cancelling the runner phase early when verify is known to be required (orthogonal).

## File structure

| File | Change |
|------|--------|
| `internal/workflow/config.go` | Add `Timeout` and `AllowFailure` to `VerifyConfig`. |
| `internal/workflow/loader.go` | No change needed — `expandConfig` doesn't seed verify defaults; `0` timeout is intentional. |
| `internal/workflow/loader_test.go` | Add round-trip test for new fields. |
| `internal/workspace/manager.go` | Rewrite `RunVerify` for collect-all + phase deadline; result errors stay on individual `VerifyResult.Err`; aggregate error returned only when ≥1 command failed (or deadline hit before completion). |
| `internal/workspace/manager_test.go` | Update existing `TestRunVerifyCapturesOutputAndStopsOnFailure` → `TestRunVerifyCollectsAllFailures`. Add `TestRunVerify_PhaseTimeout`. |
| `cmd/worker/main.go` | In `runTask`: branch verify-failed-but-allowed path; force PR draft; pass new flag to `createPR` → `buildPRBody`. Update `summarizeVerifyResults` to include a `failed` boolean per command. |
| `cmd/worker/main_test.go` | Add `TestRunTask_VerifyFailedAllowsDegradedPR` and `TestRunTask_VerifyFailedBlocksPRWhenAllowFailureOff`. |
| `README.md` | Add a paragraph under the workflow config section documenting `verify.timeout` and `verify.allow_failure`. |

---

## Task 1: Schema additions for `verify.timeout` and `verify.allow_failure`

**Files:**
- Modify: `internal/workflow/config.go`
- Test: `internal/workflow/loader_test.go`

- [ ] **Step 1: Add the failing round-trip test**

Append to `internal/workflow/loader_test.go`:

```go
func TestLoad_VerifyTimeoutAndAllowFailureRoundTrip(t *testing.T) {
	dir := t.TempDir()
	body := "---\n" +
		"repo:\n  owner: o\n  name: r\n  clone_url: git@example.com:o/r.git\n" +
		"verify:\n  timeout: 5m\n  allow_failure: true\n  commands:\n    - go test ./...\n" +
		"---\nprompt\n"
	p := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	wf, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := wf.Config.Verify.Timeout, 5*time.Minute; got != want {
		t.Fatalf("Verify.Timeout = %v, want %v", got, want)
	}
	if !wf.Config.Verify.AllowFailure {
		t.Fatalf("Verify.AllowFailure = false, want true")
	}
}
```

If `time` is not yet imported in this test file, add `"time"` to the import block. Check before editing:

```bash
grep -n '"time"' internal/workflow/loader_test.go
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/workflow/... -run TestLoad_VerifyTimeoutAndAllowFailureRoundTrip -count=1 -v`
Expected: FAIL — compile error referencing `Timeout` / `AllowFailure` not defined on `VerifyConfig`.

- [ ] **Step 3: Add the schema fields**

In `internal/workflow/config.go`, modify `VerifyConfig`:

```go
type VerifyConfig struct {
	Commands   []string         `yaml:"commands" json:"commands"`
	SecretScan SecretScanConfig `yaml:"secret_scan" json:"secret_scan"`
	// Timeout caps the entire verify phase. Zero (the default) means
	// unbounded so repos that have not opted in keep their previous
	// behavior. When exceeded, the in-flight command is killed via
	// context cancellation and remaining commands are skipped; the
	// task fails through the normal verify path unless AllowFailure
	// is set.
	Timeout time.Duration `yaml:"timeout" json:"timeout"`
	// AllowFailure, when true, lets the worker open a draft PR even
	// after verify reports failures, so the human can inspect what
	// the agent produced and what the verifier saw. The PR body is
	// annotated with a "verification failed (investigation mode)"
	// banner. Default false: failed verification blocks PR creation.
	AllowFailure bool `yaml:"allow_failure" json:"allow_failure"`
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/workflow/... -count=1`
Expected: PASS (all tests, including the new round-trip).

- [ ] **Step 5: Commit**

```bash
git add internal/workflow/config.go internal/workflow/loader_test.go
git commit -m "feat: add verify.timeout and verify.allow_failure schema fields

Refs #18. Adds two opt-in VerifyConfig fields:
- Timeout caps the entire verify phase (0 = unbounded, preserving
  current behavior for existing repos).
- AllowFailure lets the worker open a degraded draft PR when verify
  fails, so a human can investigate what the agent produced.

Wiring in RunVerify and the worker pipeline lands in the next commits."
```

---

## Task 2: `RunVerify` collects all results + applies phase timeout

**Files:**
- Modify: `internal/workspace/manager.go:151-184`
- Test: `internal/workspace/manager_test.go:417-448` (rewrite) and append a new test

- [ ] **Step 1: Rewrite the failing test (collect-all)**

Replace the body of `TestRunVerifyCapturesOutputAndStopsOnFailure` with a new test that asserts collect-all. Rename it for clarity:

```go
// TestRunVerifyCollectsAllFailures pins the post-#18 contract: RunVerify
// runs every non-empty command and records a result for each, even after
// a non-zero exit. The aggregate error is non-nil iff at least one
// command failed; per-command failure detail lives on VerifyResult.Err.
func TestRunVerifyCollectsAllFailures(t *testing.T) {
	dir := t.TempDir()
	cfg := workflow.Config{Verify: workflow.VerifyConfig{Commands: []string{
		"echo hello-world",
		"   ", // empty command should be skipped without recording a result
		"sh -c 'echo to-stderr 1>&2; exit 7'",
		"sh -c 'echo still-running; exit 0'",
		"sh -c 'exit 3'",
	}}}

	results, err := RunVerify(context.Background(), dir, cfg)
	if err == nil {
		t.Fatalf("RunVerify should report an aggregate error when any command fails")
	}
	if got, want := len(results), 4; got != want {
		t.Fatalf("expected %d verify results (skipping the empty entry), got %d", want, got)
	}
	if results[0].ExitCode != 0 || results[0].Err != nil {
		t.Fatalf("result[0] should be success: %+v", results[0])
	}
	if !strings.Contains(results[0].Output, "hello-world") {
		t.Fatalf("result[0] missing stdout: %q", results[0].Output)
	}
	if results[1].ExitCode != 7 || results[1].Err == nil {
		t.Fatalf("result[1] should record exit=7 and Err: %+v", results[1])
	}
	if !strings.Contains(results[1].Output, "to-stderr") {
		t.Fatalf("result[1] should capture stderr: %q", results[1].Output)
	}
	if results[2].ExitCode != 0 || results[2].Err != nil {
		t.Fatalf("result[2] should be success despite earlier failure: %+v", results[2])
	}
	if !strings.Contains(results[2].Output, "still-running") {
		t.Fatalf("result[2] missing stdout: %q", results[2].Output)
	}
	if results[3].ExitCode != 3 || results[3].Err == nil {
		t.Fatalf("result[3] should record exit=3 and Err: %+v", results[3])
	}
}
```

- [ ] **Step 2: Add the failing phase-timeout test**

Append to `internal/workspace/manager_test.go`:

```go
// TestRunVerify_PhaseTimeout pins the verify-phase deadline behavior:
// when Verify.Timeout elapses, the in-flight command is killed via
// context cancellation, remaining commands are not started, and the
// aggregate error is non-nil. Already-completed results are preserved.
func TestRunVerify_PhaseTimeout(t *testing.T) {
	dir := t.TempDir()
	cfg := workflow.Config{Verify: workflow.VerifyConfig{
		Timeout: 200 * time.Millisecond,
		Commands: []string{
			"echo first-finished",
			"sleep 5",         // killed by phase deadline
			"echo unreachable", // skipped
		},
	}}
	start := time.Now()
	results, err := RunVerify(context.Background(), dir, cfg)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("RunVerify should report an aggregate error on timeout")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("phase took %v, expected ~200ms (timeout not enforced)", elapsed)
	}
	if len(results) < 2 {
		t.Fatalf("expected at least 2 results (first finished + sleep killed), got %d", len(results))
	}
	if results[0].ExitCode != 0 || results[0].Err != nil {
		t.Fatalf("result[0] should be success: %+v", results[0])
	}
	if results[1].Err == nil {
		t.Fatalf("result[1] (sleep) should record an error from context cancel")
	}
	for _, r := range results[2:] {
		if r.Command == "echo unreachable" {
			t.Fatalf("third command must not have been executed after deadline")
		}
	}
}
```

If `time` is not yet imported in this test file, add it.

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./internal/workspace/... -run 'TestRunVerifyCollectsAllFailures|TestRunVerify_PhaseTimeout' -count=1 -v`
Expected: FAIL.

- [ ] **Step 4: Rewrite `RunVerify`**

In `internal/workspace/manager.go`, replace the existing `RunVerify` (lines 151-184) with:

```go
// RunVerify executes the workflow verify commands in order and returns
// one VerifyResult per non-empty command. Unlike the original
// short-circuit semantics, it does not stop on the first failing
// command: the AI workflow is more efficient when a single rework cycle
// can address every reported failure. Per-command failure detail stays
// on VerifyResult.Err and ExitCode.
//
// When wf.Verify.Timeout > 0 the entire phase runs under a derived
// deadline. If the deadline elapses, the in-flight command is killed
// via context cancellation and remaining commands are skipped (no
// result is recorded for the skipped tail). The returned aggregate
// error is non-nil iff at least one command failed or the phase
// deadline was exceeded; callers inspect individual results to see
// which.
func RunVerify(ctx context.Context, workdir string, wf workflow.Config) ([]VerifyResult, error) {
	runCtx := ctx
	if wf.Verify.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, wf.Verify.Timeout)
		defer cancel()
	}

	var (
		results  []VerifyResult
		failures int
	)
	for _, command := range wf.Verify.Commands {
		if strings.TrimSpace(command) == "" {
			continue
		}
		// Stop launching new commands once the phase deadline has
		// elapsed; the in-flight command (if any) was already killed
		// by runCtx.Done().
		if runCtx.Err() != nil {
			break
		}
		start := time.Now()
		buf := &cappedBuffer{Cap: VerifyOutputCap}
		cmd := exec.CommandContext(runCtx, "sh", "-lc", command)
		cmd.Dir = workdir
		cmd.Stdout = buf
		cmd.Stderr = buf
		runErr := cmd.Run()
		res := VerifyResult{
			Command:   command,
			Output:    buf.String(),
			Truncated: buf.Truncated(),
			Duration:  time.Since(start),
			ExitCode:  cmd.ProcessState.ExitCode(),
		}
		if runErr != nil {
			res.Err = runErr
			failures++
		}
		results = append(results, res)
	}

	if runCtx.Err() == context.DeadlineExceeded {
		return results, fmt.Errorf("verify phase exceeded timeout %s after %d command(s)", wf.Verify.Timeout, len(results))
	}
	if failures > 0 {
		return results, fmt.Errorf("verify: %d of %d command(s) failed", failures, len(results))
	}
	return results, nil
}
```

- [ ] **Step 5: Run all workspace tests**

Run: `go test ./internal/workspace/... -count=1`
Expected: PASS. The existing `TestRunVerifyCapsLargeOutputAndMarksTruncated` and other artifact tests should remain green.

- [ ] **Step 6: Commit**

```bash
git add internal/workspace/manager.go internal/workspace/manager_test.go
git commit -m "feat: collect all verify results and honor phase timeout

Refs #18. RunVerify no longer short-circuits on the first failing
command — it runs every non-empty command and records a result per
command. The aggregate error stays non-nil iff at least one command
failed, but callers now have full diagnostic detail in one cycle
instead of fix-retry-fix-retry.

When verify.timeout > 0 the phase runs under a derived deadline.
The in-flight command is killed via context cancellation and the
remaining commands are skipped. Default 0 (unbounded) preserves
backward compatibility.

The existing stop-on-failure test is replaced with a collect-all
assertion plus a phase-timeout test."
```

---

## Task 3: Worker `runTask` honors `allow_failure` and emits a distinct event

**Files:**
- Modify: `cmd/worker/main.go:180-197` (verify block) and `cmd/worker/main.go:507-521` (`summarizeVerifyResults`)
- Modify: `cmd/worker/main.go:276` (`runTask`'s tail-call to `createPR`) and `cmd/worker/main.go:427-466` (`createPR`/`createPRWith`)
- Test: `cmd/worker/main_test.go`

- [ ] **Step 1: Locate the worker test scaffolding**

Read `cmd/worker/main_test.go` to find the existing fake PR client and event recorder used by current tests:

```bash
grep -n "func Test\|fakePR\|recordingEmitter\|runTask" cmd/worker/main_test.go | head -40
```

If a usable fake (with `CreatePullRequest` capturing `Draft` and `Body`) already exists, reuse it. Otherwise, the new test below defines one inline.

- [ ] **Step 2: Add the failing tests**

Append to `cmd/worker/main_test.go`. The two tests pin the contract independently. The exact harness is determined by what already exists in the file — adapt names and helper signatures to match. The shape below assumes a helper that drives `runTask` against an in-process fake; if no such helper exists, reach into the worker through its constituent functions (`runVerifyPhase` and `createPRWith`) and exercise them directly.

```go
// TestVerifyAllowFailure_OpensDegradedDraftPR pins the investigation
// override: when verify fails AND verify.allow_failure is true, the
// worker forces a draft PR, the body carries the failure banner, and
// a verify_end event with status="failed_allowed" is emitted.
func TestVerifyAllowFailure_OpensDegradedDraftPR(t *testing.T) {
	// Arrange: workflow with a deliberately failing verify command and
	//          allow_failure=true. pr.draft=false to prove the override
	//          forces draft regardless.
	cfg := workflow.Config{
		Verify: workflow.VerifyConfig{
			Commands:     []string{"sh -c 'exit 1'"},
			AllowFailure: true,
		},
		PR: workflow.PRConfig{Draft: false},
	}

	// Run only the post-runner verify+PR slice — see helper.
	got := runWorkerVerifyAndPR(t, cfg /* helper-supplied task, fake client, recorder */)

	if !got.PRCreated {
		t.Fatalf("expected PR to be created in allow_failure path")
	}
	if !got.PRDraftRequested {
		t.Fatalf("PR must be draft when verify failed under allow_failure (got Draft=false)")
	}
	if !strings.Contains(got.PRBody, "Verification failed (investigation mode)") {
		t.Fatalf("PR body missing degraded banner; body:\n%s", got.PRBody)
	}
	if !got.HasEventOfStatus("verify_end", "failed_allowed") {
		t.Fatalf("expected verify_end event with status=failed_allowed; events: %v", got.Events)
	}
}

// TestVerifyFails_BlocksPRWhenAllowFailureOff pins the default
// behavior: a failing verify command without allow_failure prevents
// PR creation. This locks the contract that the new collect-all
// semantics did not weaken safety.
func TestVerifyFails_BlocksPRWhenAllowFailureOff(t *testing.T) {
	cfg := workflow.Config{
		Verify: workflow.VerifyConfig{
			Commands:     []string{"sh -c 'exit 1'"},
			AllowFailure: false,
		},
	}
	got := runWorkerVerifyAndPR(t, cfg)
	if got.PRCreated {
		t.Fatalf("PR must not be created when verify fails and allow_failure is off")
	}
	if !got.HasEventOfStatus("verify_end", "failed") {
		t.Fatalf("expected verify_end event with status=failed; events: %v", got.Events)
	}
}
```

If the worker test file has no existing helper for this slice, write a minimal one in the same test file. The helper should:
- Create a temp dir as the workdir.
- Call the verify section directly (extracting helper if needed; see Step 4).
- Stub `prClient` with a fake that records the `CreatePullRequestInput`.
- Capture emitted events via the existing emitter test double.

If extracting a helper makes the change cleaner, do it as part of Step 4.

- [ ] **Step 3: Run new tests to verify they fail**

Run: `go test ./cmd/worker/... -run 'TestVerifyAllowFailure_OpensDegradedDraftPR|TestVerifyFails_BlocksPRWhenAllowFailureOff' -count=1 -v`
Expected: FAIL — either compile errors (helper not yet defined) or assertion failures (status=failed_allowed not emitted).

- [ ] **Step 4: Refactor `runTask`'s verify block + thread the degraded flag**

Extract the verify section into a helper to keep `runTask` tight. In `cmd/worker/main.go`, after Task 1's import additions are in place, replace lines 180-197 with:

```go
verifyDegraded, err := runVerifyPhase(ctx, ev, t.ID, workdir, cfg)
if err != nil {
	return cfg, err
}
```

…and define `runVerifyPhase` near the other phase helpers (e.g., just below `runRunnerWithTimeout`):

```go
// runVerifyPhase runs the configured verify commands, persists the
// VERIFICATION.txt artifact, emits the verify_start/verify_end events,
// and returns whether the run is in degraded mode. Degraded mode means
// at least one command failed (or the phase deadline elapsed) and the
// operator has opted into verify.allow_failure: the caller continues
// to PR creation but must mark the PR draft and annotate the body.
//
// Returns (degraded, err). When err is non-nil, verify failed AND
// allow_failure was off; the caller propagates the error and skips PR
// creation. When degraded=true, err is nil but downstream stages must
// signal the verify failure to the human.
func runVerifyPhase(ctx context.Context, ev eventEmitter, taskID, workdir string, cfg workflow.Config) (bool, error) {
	emit(ctx, ev, taskID, task.EventVerifyStart, "verify started", map[string]any{
		"commands":      cfg.Verify.Commands,
		"timeout_ms":    cfg.Verify.Timeout.Milliseconds(),
		"allow_failure": cfg.Verify.AllowFailure,
	})
	start := time.Now()
	results, verifyErr := workspace.RunVerify(ctx, workdir, cfg)
	if writeErr := workspace.WriteVerification(workdir, results); writeErr != nil {
		log.Printf("task %s: write verification artifact: %v", taskID, writeErr)
	}
	payload := map[string]any{
		"duration_ms":   time.Since(start).Milliseconds(),
		"commands":      summarizeVerifyResults(results),
		"failed_count":  countVerifyFailures(results),
		"allow_failure": cfg.Verify.AllowFailure,
	}
	if verifyErr == nil {
		payload["status"] = "ok"
		emit(ctx, ev, taskID, task.EventVerifyEnd, "verify completed", payload)
		return false, nil
	}
	payload["error"] = errSummary(verifyErr)
	if cfg.Verify.AllowFailure {
		payload["status"] = "failed_allowed"
		emit(ctx, ev, taskID, task.EventVerifyEnd, "verify failed (investigation mode)", payload)
		return true, nil
	}
	payload["status"] = "failed"
	emit(ctx, ev, taskID, task.EventVerifyEnd, "verify failed", payload)
	writeFailureArtifacts(ctx, workdir, results, "verify failed: "+errSummary(verifyErr))
	return false, verifyErr
}

func countVerifyFailures(results []workspace.VerifyResult) int {
	n := 0
	for _, r := range results {
		if r.Err != nil || r.ExitCode != 0 {
			n++
		}
	}
	return n
}
```

Then thread the degraded flag from `runTask` through to the PR creation site:

In `runTask`, replace the existing tail call

```go
return cfg, createPR(ctx, ev, t, cfg, summary)
```

with

```go
if verifyDegraded {
	cfg.PR.Draft = true
}
return cfg, createPR(ctx, ev, t, cfg, summary, verifyDegraded)
```

Update `createPR` and `createPRWith` signatures to accept the new flag and pass it to `buildPRBody` (Task 4 implements the body change). Until Task 4 lands, `buildPRBody` can ignore the parameter — keep it on the signature so this commit compiles.

```go
func createPR(ctx context.Context, ev eventEmitter, t task.Task, cfg workflow.Config, summary string, verifyDegraded bool) error {
	client := gitea.Client{BaseURL: os.Getenv("GITEA_BASE_URL"), Token: os.Getenv("GITEA_TOKEN")}
	return createPRWith(ctx, ev, t, cfg, summary, verifyDegraded, client)
}

func createPRWith(ctx context.Context, ev eventEmitter, t task.Task, cfg workflow.Config, summary string, verifyDegraded bool, client prClient) error {
	// ... existing body ...
	body := buildPRBody(t, summary, verifyDegraded)
	// ... existing CreatePullRequest call (keep Draft: cfg.PR.Draft) ...
}
```

Update `buildPRBody`'s signature to take the new `verifyDegraded bool` argument; for this task leave the body unchanged (just accept and ignore the flag). Task 4 implements the banner.

```go
func buildPRBody(t task.Task, summary string, verifyDegraded bool) string {
	_ = verifyDegraded // wired up in Task 4
	// existing body unchanged
}
```

- [ ] **Step 4b: Update existing test call sites for the new signatures**

Adding `verifyDegraded` to `createPRWith` and `buildPRBody` breaks compilation of existing tests. Update each call site to pass `false`:

In `cmd/worker/main_test.go`:
- `TestBuildPRBodyEmbedsRunSummary` (~line 132): `buildPRBody(tk, summary)` → `buildPRBody(tk, summary, false)`.
- `TestBuildPRBodyTruncatesLongSummary` (~line 162): same change.
- `TestCreatePRWith_ReusesExistingPR` (~line 523): `createPRWith(ctx, ev, tk, cfg, "summary", client)` → `createPRWith(ctx, ev, tk, cfg, "summary", false, client)`.
- `TestCreatePRWith_CreatesWhenNoneExists` (~line 557): same change.
- `TestCreatePRWith_ListErrorFallsThroughToCreate` (~line 592): same change.

If any other call sites in non-test code reference `createPR` or `buildPRBody`, update those too. Verify with:

```bash
grep -n "createPR(\|createPRWith(\|buildPRBody(" cmd/worker/*.go
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./cmd/worker/... -count=1`
Expected: PASS — including the two new tests. The banner-body assertion from `TestVerifyAllowFailure_OpensDegradedDraftPR` should be deferred to Task 4 (mark it with `// banner asserted in Task 4` and skip that single check here). The cleanest split:
- This task asserts: PRCreated, PRDraftRequested, status=failed_allowed event.
- Task 4 asserts: PR body contains the banner.

Verify all other worker tests (PR reuse, secret scan, summary gate, build PR body) still pass.

- [ ] **Step 6: Commit**

```bash
git add cmd/worker/main.go cmd/worker/main_test.go
git commit -m "feat: thread verify.allow_failure through the worker pipeline

Refs #18. Extract the verify section of runTask into runVerifyPhase
and add the investigation-override path: when verify fails AND
verify.allow_failure is true, the worker emits verify_end with
status=failed_allowed, forces the PR to draft regardless of the
configured pr.draft, and continues through summary/scan/push/PR.

The PR body banner is added in the next commit; this commit threads
the verifyDegraded flag through createPR / createPRWith / buildPRBody
so Task 4 only has to render."
```

---

## Task 4: PR body annotates verify-degraded state

**Files:**
- Modify: `cmd/worker/main.go:533-556` (`buildPRBody`)
- Test: `cmd/worker/main_test.go`

- [ ] **Step 1: Add the failing test**

Append to `cmd/worker/main_test.go`:

```go
// TestBuildPRBody_VerifyDegradedBanner pins the contract that a
// degraded run (verify.allow_failure took effect) prepends a clear
// warning banner to the PR body so a human reviewer cannot miss it.
func TestBuildPRBody_VerifyDegradedBanner(t *testing.T) {
	tk := task.Task{ID: "tsk_demo", SourceType: "linear", SourceEventID: "ABC-1", WorkBranch: "ai/tsk_demo"}
	body := buildPRBody(tk, "Did the thing.", true)

	if !strings.Contains(body, "Verification failed (investigation mode)") {
		t.Fatalf("expected investigation-mode banner; body:\n%s", body)
	}
	bannerIdx := strings.Index(body, "Verification failed (investigation mode)")
	taskHeaderIdx := strings.Index(body, "## AI Task")
	if bannerIdx < 0 || taskHeaderIdx < 0 || bannerIdx > taskHeaderIdx {
		t.Fatalf("banner must precede the AI Task heading; banner=%d task=%d body:\n%s", bannerIdx, taskHeaderIdx, body)
	}

	// And: a non-degraded body must not carry the banner.
	clean := buildPRBody(tk, "Did the thing.", false)
	if strings.Contains(clean, "Verification failed") {
		t.Fatalf("clean body should not mention verification failure; body:\n%s", clean)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/worker/... -run TestBuildPRBody_VerifyDegradedBanner -count=1 -v`
Expected: FAIL — banner not yet rendered.

- [ ] **Step 3: Render the banner**

In `cmd/worker/main.go`, modify `buildPRBody`:

```go
func buildPRBody(t task.Task, summary string, verifyDegraded bool) string {
	excerpt, truncated := truncateForPR(summary, prBodySummaryCap)
	var b strings.Builder
	if verifyDegraded {
		b.WriteString("> ⚠️ **Verification failed (investigation mode).** ")
		b.WriteString("This PR was opened despite a failing verify phase because ")
		b.WriteString("`verify.allow_failure` is enabled. Inspect ")
		b.WriteString("`.aiops/VERIFICATION.txt` before merging.\n\n")
	}
	fmt.Fprintf(&b, "## AI Task\n\nTask ID: `%s`\n\n", t.ID)
	// ... rest of the existing body unchanged ...
}
```

(Preserve the existing `## Source`, `## Run summary`, `## Verification`, `## Risk` sections after the banner.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/worker/... -count=1`
Expected: PASS for all worker tests including the new banner test and the previously deferred banner assertion in `TestVerifyAllowFailure_OpensDegradedDraftPR` (re-enable any banner assertion you commented out in Task 3).

- [ ] **Step 5: Commit**

```bash
git add cmd/worker/main.go cmd/worker/main_test.go
git commit -m "feat: annotate degraded PR body when verify failed under allow_failure

Refs #18. Prepends a blockquote banner to the PR body when verify
ran in investigation mode. The banner sits above the existing AI
Task / Source / Run summary sections so a human reviewer cannot
miss it, and points to .aiops/VERIFICATION.txt for the failure
detail."
```

---

## Task 5: Document the new fields

**Files:**
- Modify: `README.md` (workflow config section)

- [ ] **Step 1: Locate the workflow config section**

Run: `grep -n "verify\|VerifyConfig\|WORKFLOW.md" README.md | head -20`. Identify the section that documents `verify.commands` (or the section listing top-level workflow keys). If no such section exists, document under the "Configuration" or "Workflow" heading — the goal is one paragraph any operator can find.

- [ ] **Step 2: Add documentation**

Add this paragraph (adapt heading depth to match the existing README structure):

```markdown
### `verify.timeout` and `verify.allow_failure`

`verify.timeout` (Go duration string, e.g. `5m`) caps the entire verify phase.
The default `0` means unbounded, preserving the previous behavior. When the
deadline elapses, the in-flight command is killed via context cancellation and
the remaining commands are skipped; the task fails through the normal verify
path unless `verify.allow_failure` is set.

`verify.allow_failure: true` opts the worker into "investigation mode": when
verify fails the worker still opens a draft PR (regardless of `pr.draft`),
emits a `verify_end` event with `status: failed_allowed`, and prepends a
warning banner to the PR body pointing to `.aiops/VERIFICATION.txt`. Use this
when you want to inspect what the agent produced even though the checks
flagged it. Default is `false`; failed verification blocks PR creation.
```

- [ ] **Step 3: Commit**

```bash
git add README.md
git commit -m "docs: document verify.timeout and verify.allow_failure

Refs #18. Adds a short README section pointing operators at the two
new VerifyConfig fields and the investigation-mode behavior."
```

---

## Final verification

- [ ] **Run full test suite**

```bash
go build ./...
go vet ./...
go test ./... -count=1
```

Expected: all green.

- [ ] **Smoke-check `--print-config` output**

```bash
go run ./cmd/worker --print-config /tmp 2>&1 | grep -A2 '"verify"'
```

Expected: the JSON shape includes `"timeout"` and `"allow_failure"` under `"verify"` (alongside `"commands"` and `"secret_scan"`).

- [ ] **Open PR**

```bash
git push -u origin feat/verifier-phase
gh pr create --title "feat: verifier phase honors timeout and allow_failure (#18)" --body "$(cat <<'EOF'
## Summary
- Closes #18. RunVerify now collects all results instead of short-circuiting; surfacing every failure in one cycle is more efficient for the AI rework loop.
- New `verify.timeout` caps the phase (default 0 = unbounded, backward-compatible).
- New `verify.allow_failure` opts the worker into investigation mode: failed verify still opens a draft PR with a banner annotation; emits `verify_end` with `status=failed_allowed` so observers can distinguish the bypass.

## Out of scope
- Per-command `continue_on_error` (would need schema polymorphism; not required by issue AC).
- Adding labels to the Gitea PR (would need a new client endpoint).

## Test plan
- [x] `go test ./... -count=1`
- [x] `go vet ./...`
- [x] New unit tests cover collect-all, phase timeout, allow_failure path, default block-on-failure, and PR body banner.
EOF
)"
```

---

## Self-review notes

- Spec coverage:
  - "Worker runs configured verify commands after runner" ✓ (already true, preserved).
  - "Verification output is captured" ✓ (VERIFICATION.txt unchanged + collect-all gives more output).
  - "Failure prevents PR creation by default" ✓ (Task 3 default path).
  - "Optional config allows PR creation with failed verification for investigation" ✓ (Task 3 + Task 4).
- Type consistency: `verifyDegraded bool` is the single threaded flag. `cfg.Verify.AllowFailure` is the schema source. `cfg.PR.Draft` mutated locally in `runTask` only.
- Placeholders: none (all code blocks are concrete; the `runWorkerVerifyAndPR` helper in Task 3 Step 2 is the one piece that requires inspecting the existing test file before locking the exact form — that is intentional, not a TBD).
