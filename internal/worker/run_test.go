package worker_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/gitea"
	"github.com/xrf9268-hue/aiops-platform/internal/runner"
	"github.com/xrf9268-hue/aiops-platform/internal/task"
	worker "github.com/xrf9268-hue/aiops-platform/internal/worker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
	"github.com/xrf9268-hue/aiops-platform/internal/workspace"
)

type recordedEvent struct {
	Kind    string
	Message string
	Payload any
}

type fakeEmitter struct {
	mu     sync.Mutex
	events []recordedEvent
	err    error
}

func (f *fakeEmitter) AddEvent(_ context.Context, _ string, kind, msg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, recordedEvent{Kind: kind, Message: msg})
	return f.err
}

func (f *fakeEmitter) AddEventWithPayload(_ context.Context, _ string, kind, msg string, payload any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, recordedEvent{Kind: kind, Message: msg, Payload: payload})
	return f.err
}

// byKind returns the recorded events whose Kind matches.
func (f *fakeEmitter) byKind(kind string) []recordedEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recordedEvent
	for _, e := range f.events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func TestEmitRecordsEventOnFakeEmitter(t *testing.T) {
	ev := &fakeEmitter{}
	worker.Emit(context.Background(), ev, "tsk_1", task.EventRunnerStart, "runner started", map[string]any{"model": "mock"})
	if len(ev.events) != 1 {
		t.Fatalf("events = %d, want 1", len(ev.events))
	}
	got := ev.events[0]
	if got.Kind != task.EventRunnerStart {
		t.Fatalf("kind = %q, want %q", got.Kind, task.EventRunnerStart)
	}
	// Payload must round-trip through JSON since the queue store stores it as jsonb.
	b, err := json.Marshal(got.Payload)
	if err != nil {
		t.Fatalf("payload marshal error: %v", err)
	}
	if !strings.Contains(string(b), `"model":"mock"`) {
		t.Fatalf("payload JSON = %s, want model=mock", b)
	}
}

func TestEmitNilEmitterIsNoop(t *testing.T) {
	// Should not panic when worker is started without an emitter (e.g. tests).
	worker.Emit(context.Background(), nil, "tsk_1", task.EventRunnerStart, "ignored", nil)
}

func TestEmitLogsEmitterError(t *testing.T) {
	ev := &fakeEmitter{err: errors.New("db down")}
	worker.Emit(context.Background(), ev, "tsk_1", task.EventPush, "push", nil)
	if len(ev.events) != 1 {
		t.Fatalf("event should still be recorded by fake even when error returned")
	}
}

func TestErrSummaryTruncatesLongMessages(t *testing.T) {
	if worker.ErrSummary(nil) != "" {
		t.Fatalf("nil error should map to empty string")
	}
	long := strings.Repeat("x", 600)
	got := worker.ErrSummary(errors.New(long))
	if len(got) > 600 {
		t.Fatalf("errSummary did not truncate: len=%d", len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("truncated message should end with ellipsis, got %q", got[len(got)-10:])
	}
}

func TestSummarizeVerifyResultsIncludesError(t *testing.T) {
	results := []workspace.VerifyResult{
		{Command: "go test", ExitCode: 0, Duration: 10 * time.Millisecond},
		{Command: "make lint", ExitCode: 2, Err: errors.New("lint failed"), Duration: 5 * time.Millisecond},
	}
	got := worker.SummarizeVerifyResults(results)
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
	if got[0]["command"] != "go test" || got[0]["exit_code"] != 0 {
		t.Fatalf("entry 0 = %+v", got[0])
	}
	if got[1]["error"] != "lint failed" {
		t.Fatalf("entry 1 should propagate error, got %+v", got[1])
	}
	// Round-trip JSON to ensure it is a valid jsonb payload.
	if _, err := json.Marshal(got); err != nil {
		t.Fatalf("verify summary JSON: %v", err)
	}
}

// TestBuildPRBodyEmbedsRunSummary verifies the PR body includes the runner-
// produced RUN_SUMMARY.md content, the source/task fields, and a link back
// to the artifact path so reviewers can find the full file even when the
// excerpt is truncated.
func TestBuildPRBodyEmbedsRunSummary(t *testing.T) {
	t1 := task.Task{
		ID:            "tsk_42",
		Title:         "fix bug",
		SourceType:    "github_issue",
		SourceEventID: "issue#7",
		WorkBranch:    "ai/tsk_42",
	}
	summary := "# Run summary\n\nFixed the off-by-one in foo().\nVerified with `go test ./...`.\n"
	body := worker.BuildPRBody(t1, summary, false)
	for _, want := range []string{
		"tsk_42",
		"github_issue / issue#7",
		"Run summary",
		"Fixed the off-by-one in foo().",
		"`.aiops/RUN_SUMMARY.md`",
		"`ai/tsk_42`",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("PR body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "_Summary truncated") {
		t.Fatalf("PR body should not advertise truncation for short summaries")
	}
}

// TestBuildPRBodyTruncatesLongSummary confirms the PR body excerpt is capped
// at PRBodySummaryCap and surfaces a truncation marker so reviewers know to
// open the artifact for the rest.
func TestBuildPRBodyTruncatesLongSummary(t *testing.T) {
	t1 := task.Task{ID: "tsk_big", WorkBranch: "ai/tsk_big"}
	// Use a sentinel char that does not appear elsewhere in the PR chrome
	// so we can count exactly how much of the user-provided summary made
	// it into the rendered body.
	const sentinel = "Z"
	long := strings.Repeat(sentinel, worker.PRBodySummaryCap+1024)
	body := worker.BuildPRBody(t1, long, false)
	if !strings.Contains(body, "_Summary truncated at") {
		t.Fatalf("expected truncation marker in PR body for oversized summary")
	}
	if got := strings.Count(body, sentinel); got > worker.PRBodySummaryCap {
		t.Fatalf("excerpt exceeded PRBodySummaryCap: %d > %d", got, worker.PRBodySummaryCap)
	}
}

// TestBuildPRBody_VerifyDegradedBanner pins the contract that a
// degraded run (verify.allow_failure took effect) prepends a clear
// warning banner to the PR body so a human reviewer cannot miss it.
func TestBuildPRBody_VerifyDegradedBanner(t *testing.T) {
	tk := task.Task{ID: "tsk_demo", SourceType: "linear", SourceEventID: "ABC-1", WorkBranch: "ai/tsk_demo"}
	body := worker.BuildPRBody(tk, "Did the thing.", true)

	if !strings.Contains(body, "Verification failed (investigation mode)") {
		t.Fatalf("expected investigation-mode banner; body:\n%s", body)
	}
	bannerIdx := strings.Index(body, "Verification failed (investigation mode)")
	taskHeaderIdx := strings.Index(body, "## AI Task")
	if bannerIdx < 0 || taskHeaderIdx < 0 || bannerIdx > taskHeaderIdx {
		t.Fatalf("banner must precede the AI Task heading; banner=%d task=%d body:\n%s", bannerIdx, taskHeaderIdx, body)
	}

	// And: a non-degraded body must not carry the banner.
	clean := worker.BuildPRBody(tk, "Did the thing.", false)
	if strings.Contains(clean, "Verification failed") {
		t.Fatalf("clean body should not mention verification failure; body:\n%s", clean)
	}
}

// TestAppendRunSummaryDirectiveAppendsOnce verifies the runner contract is
// only appended when the rendered prompt does not already mention it. This
// keeps workflow templates free to embed their own (more detailed) version
// without duplication.
func TestAppendRunSummaryDirectiveAppendsOnce(t *testing.T) {
	plain := "do the work"
	out := worker.AppendRunSummaryDirective(plain)
	if !strings.Contains(out, "RUN_SUMMARY.md") {
		t.Fatalf("directive not appended: %q", out)
	}
	if !strings.Contains(out, "**Required output:**") {
		t.Fatalf("expected Required output marker in appended directive")
	}

	// Idempotent: prompt that already mentions RUN_SUMMARY.md is left alone.
	already := "please write .aiops/RUN_SUMMARY.md when done"
	if got := worker.AppendRunSummaryDirective(already); got != already {
		t.Fatalf("expected no-op when prompt already references RUN_SUMMARY.md, got %q", got)
	}
}

// TestMockRunnerWritesRunSummary confirms the mock runner produces a
// RUN_SUMMARY.md that satisfies workspace.CheckSummary, so the worker gate
// passes end-to-end without touching codex/claude.
func TestMockRunnerWritesRunSummary(t *testing.T) {
	dir := t.TempDir()
	r := runner.MockRunner{}
	tk := task.Task{ID: "tsk_mock", Title: "mock task", Actor: "tester", Model: "mock"}
	if _, err := r.Run(context.Background(), runner.RunInput{Task: tk, Workflow: workflow.Workflow{}, Workdir: dir, Prompt: "p"}); err != nil {
		t.Fatalf("mock runner: %v", err)
	}
	body, status, err := workspace.CheckSummary(dir)
	if err != nil {
		t.Fatalf("CheckSummary: %v", err)
	}
	if status != workspace.SummaryOK {
		t.Fatalf("status = %s, want ok; body=%q", status, body)
	}
	if !strings.Contains(body, "tsk_mock") {
		t.Fatalf("mock summary should contain task id, got: %s", body)
	}
}

// TestCheckSummaryRejectsMissingEmptyAndPlaceholder asserts the gate
// distinguishes the three failure modes the worker exposes via the
// `summary_missing` / `failed_attempt` events.
func TestCheckSummaryRejectsMissingEmptyAndPlaceholder(t *testing.T) {
	cases := []struct {
		name    string
		write   func(dir string) error
		want    workspace.SummaryStatus
		wantErr bool
	}{
		{
			name:  "missing",
			write: func(dir string) error { return nil },
			want:  workspace.SummaryMissing,
		},
		{
			name: "empty",
			write: func(dir string) error {
				return writeAiopsFileForTest(dir, "")
			},
			want: workspace.SummaryEmpty,
		},
		{
			name: "whitespace only",
			write: func(dir string) error {
				return writeAiopsFileForTest(dir, "   \n\n\t\n")
			},
			want: workspace.SummaryEmpty,
		},
		{
			name: "TODO placeholder",
			write: func(dir string) error {
				return writeAiopsFileForTest(dir, "TODO\n")
			},
			want: workspace.SummaryPlaceholder,
		},
		{
			name: "real summary",
			write: func(dir string) error {
				return writeAiopsFileForTest(dir, "# Summary\n\nFixed bug, verified with go test ./...\n")
			},
			want: workspace.SummaryOK,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := tc.write(dir); err != nil {
				t.Fatalf("setup: %v", err)
			}
			_, status, err := workspace.CheckSummary(dir)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if status != tc.want {
				t.Fatalf("status=%s want=%s", status, tc.want)
			}
		})
	}
}

// writeAiopsFileForTest is a small helper to seed .aiops/RUN_SUMMARY.md on
// disk for gate tests. It avoids exporting a workspace test helper.
func writeAiopsFileForTest(dir, body string) error {
	aiops := filepath.Join(dir, ".aiops")
	if err := os.MkdirAll(aiops, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(aiops, "RUN_SUMMARY.md"), []byte(body), 0o644)
}

func TestEventKindConstantsAreSnakeCase(t *testing.T) {
	required := []string{
		task.EventRunnerStart,
		task.EventRunnerEnd,
		task.EventRunnerTimeout,
		task.EventVerifyStart,
		task.EventVerifyEnd,
		task.EventPush,
		task.EventPRCreated,
		task.EventPRReused,
		task.EventTrackerTransition,
		task.EventTrackerTransitionError,
		task.EventTrackerComment,
	}
	for _, kind := range required {
		if kind == "" {
			t.Fatalf("event kind constant is empty")
		}
		if strings.ToLower(kind) != kind {
			t.Fatalf("event kind %q must be lowercase snake_case", kind)
		}
	}
}

// stubRunner lets tests control the runner's outcome (sleep + final
// error) without invoking a subprocess.
type stubRunner struct {
	sleep time.Duration
	err   error
}

func (s stubRunner) Run(ctx context.Context, _ runner.RunInput) (runner.Result, error) {
	if s.sleep > 0 {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return runner.Result{}, &runner.TimeoutError{
					Timeout: 25 * time.Millisecond,
					Elapsed: 25 * time.Millisecond,
					Cause:   ctx.Err(),
				}
			}
			return runner.Result{}, ctx.Err()
		case <-time.After(s.sleep):
		}
	}
	return runner.Result{Summary: "ok"}, s.err
}

// payloadField round-trips a recorded event payload through JSON and
// returns the value at key. Centralising this here keeps the
// RunRunnerWithTimeout assertions resilient to the EventEmitter accepting
// `any` payload (map[string]any in this code path).
func payloadField(t *testing.T, p any, key string) any {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return m[key]
}

// TestRunRunnerWithTimeoutEmitsTimeoutEvent confirms that a runner
// killed by the per-task timeout produces exactly one runner_start +
// one runner_timeout event (and no runner_end), with payload fields the
// debug API can rely on.
func TestRunRunnerWithTimeoutEmitsTimeoutEvent(t *testing.T) {
	t.Parallel()
	ev := &fakeEmitter{}
	in := runner.RunInput{
		Task:     task.Task{ID: "tsk_to", Model: "mock"},
		Workflow: workflow.Workflow{},
		Workdir:  t.TempDir(),
	}
	_, err := worker.RunRunnerWithTimeout(context.Background(), ev, stubRunner{sleep: 5 * time.Second}, in, 30*time.Millisecond, "file")
	if !runner.IsTimeout(err) {
		t.Fatalf("expected TimeoutError from RunRunnerWithTimeout, got %v", err)
	}
	if got := len(ev.byKind(task.EventRunnerStart)); got != 1 {
		t.Fatalf("runner_start count: got=%d want=1", got)
	}
	if got := len(ev.byKind(task.EventRunnerTimeout)); got != 1 {
		t.Fatalf("runner_timeout count: got=%d want=1", got)
	}
	if got := len(ev.byKind(task.EventRunnerEnd)); got != 0 {
		t.Fatalf("runner_end must not fire on timeout, got=%d", got)
	}

	// Payload sanity: timeout_ms and elapsed_ms must be present.
	pe := ev.byKind(task.EventRunnerTimeout)[0]
	if got := payloadField(t, pe.Payload, "timeout_ms"); got == nil {
		t.Fatal("runner_timeout payload missing timeout_ms")
	}
	if got := payloadField(t, pe.Payload, "elapsed_ms"); got == nil {
		t.Fatal("runner_timeout payload missing elapsed_ms")
	}
}

// TestRunRunnerWithTimeoutHappyPath emits runner_start + runner_end
// (with ok=true) when the runner completes within budget.
func TestRunRunnerWithTimeoutHappyPath(t *testing.T) {
	t.Parallel()
	ev := &fakeEmitter{}
	in := runner.RunInput{
		Task:     task.Task{ID: "tsk_ok", Model: "mock"},
		Workflow: workflow.Workflow{},
		Workdir:  t.TempDir(),
	}
	if _, err := worker.RunRunnerWithTimeout(context.Background(), ev, stubRunner{}, in, time.Second, "file"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := len(ev.byKind(task.EventRunnerStart)); got != 1 {
		t.Fatalf("runner_start count: got=%d want=1", got)
	}
	if got := len(ev.byKind(task.EventRunnerEnd)); got != 1 {
		t.Fatalf("runner_end count: got=%d want=1", got)
	}
	if got := len(ev.byKind(task.EventRunnerTimeout)); got != 0 {
		t.Fatalf("runner_timeout must not fire on success, got=%d", got)
	}
	pe := ev.byKind(task.EventRunnerEnd)[0]
	ok, _ := payloadField(t, pe.Payload, "ok").(bool)
	if !ok {
		t.Fatalf("runner_end payload ok=true expected, got %v", payloadField(t, pe.Payload, "ok"))
	}
}

// TestRunRunnerWithTimeoutNonTimeoutError keeps verify-vs-timeout
// retry buckets disjoint: a generic runner error must surface as
// runner_end with ok=false (not runner_timeout).
func TestRunRunnerWithTimeoutNonTimeoutError(t *testing.T) {
	t.Parallel()
	ev := &fakeEmitter{}
	wantErr := errors.New("agent crashed")
	in := runner.RunInput{
		Task:     task.Task{ID: "tsk_err", Model: "mock"},
		Workflow: workflow.Workflow{},
		Workdir:  t.TempDir(),
	}
	_, err := worker.RunRunnerWithTimeout(context.Background(), ev, stubRunner{err: wantErr}, in, time.Second, "file")
	if !errors.Is(err, wantErr) {
		t.Fatalf("err propagation broken: got %v want %v", err, wantErr)
	}
	if runner.IsTimeout(err) {
		t.Fatal("non-timeout error must not be classified as timeout")
	}
	if got := len(ev.byKind(task.EventRunnerTimeout)); got != 0 {
		t.Fatalf("runner_timeout must not fire on plain error, got=%d", got)
	}
	if got := len(ev.byKind(task.EventRunnerEnd)); got != 1 {
		t.Fatalf("runner_end count: got=%d want=1", got)
	}
}

// TestRunRunnerWithTimeoutZeroBudgetUsesDefault ensures we never call
// context.WithTimeout(0), which would fire instantly. A zero budget
// should fall back to the schema default (30m).
func TestRunRunnerWithTimeoutZeroBudgetUsesDefault(t *testing.T) {
	t.Parallel()
	ev := &fakeEmitter{}
	in := runner.RunInput{
		Task:     task.Task{ID: "tsk_zero", Model: "mock"},
		Workflow: workflow.Workflow{},
		Workdir:  t.TempDir(),
	}
	if _, err := worker.RunRunnerWithTimeout(context.Background(), ev, stubRunner{sleep: 10 * time.Millisecond}, in, 0, "default"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	pe := ev.byKind(task.EventRunnerStart)[0]
	got, _ := payloadField(t, pe.Payload, "timeout_ms").(float64)
	want := float64((30 * time.Minute).Milliseconds())
	if got != want {
		t.Fatalf("expected default timeout %v ms, got %v", want, got)
	}
}

// fakePRClient lets PR-handoff tests script the responses from the gitea
// client without going through HTTP. It also records every call so tests can
// assert on whether CreatePullRequest was even attempted on the reuse path.
type fakePRClient struct {
	findResult *gitea.PullRequest
	findErr    error
	createPR   *gitea.PullRequest
	createErr  error

	mu          sync.Mutex
	findCalls   []gitea.FindOpenPullRequestInput
	createCalls []gitea.CreatePullRequestInput
}

func (f *fakePRClient) FindOpenPullRequest(_ context.Context, in gitea.FindOpenPullRequestInput) (*gitea.PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.findCalls = append(f.findCalls, in)
	return f.findResult, f.findErr
}

func (f *fakePRClient) CreatePullRequest(_ context.Context, in gitea.CreatePullRequestInput) (*gitea.PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalls = append(f.createCalls, in)
	if f.createErr != nil {
		return nil, f.createErr
	}
	if f.createPR != nil {
		return f.createPR, nil
	}
	return &gitea.PullRequest{Number: 1, HTMLURL: "http://gitea.local/o/r/pulls/1", Title: in.Title}, nil
}

func samplePRTask() task.Task {
	return task.Task{
		ID:            "tsk_42",
		Title:         "fix bug",
		SourceType:    "gitea_issue",
		SourceEventID: "evt-1",
		RepoOwner:     "o",
		RepoName:      "r",
		BaseBranch:    "main",
		WorkBranch:    "ai/tsk_42",
	}
}

// TestCreatePRWith_ReusesExistingPR is the core of issue #7: when a previous
// attempt already opened a PR for the work branch, the retry must reuse it
// rather than asking Gitea to create a duplicate (which would fail with 422
// and surface as a task failure). The test asserts both that CreatePullRequest
// is NOT called and that a pr_reused event is emitted with the existing PR's
// metadata so observers can attribute the handoff.
func TestCreatePRWith_ReusesExistingPR(t *testing.T) {
	ev := &fakeEmitter{}
	tk := samplePRTask()
	cfg := workflow.Config{}
	client := &fakePRClient{
		findResult: &gitea.PullRequest{Number: 17, HTMLURL: "http://gitea.local/o/r/pulls/17", Title: "chore(ai): fix bug"},
	}

	if err := worker.CreatePRWith(context.Background(), ev, tk, cfg, "summary", false, client); err != nil {
		t.Fatalf("CreatePRWith: %v", err)
	}

	if len(client.createCalls) != 0 {
		t.Fatalf("expected zero CreatePullRequest calls when reusing, got %d", len(client.createCalls))
	}
	if len(client.findCalls) != 1 || client.findCalls[0].Head != tk.WorkBranch {
		t.Fatalf("expected single FindOpenPullRequest with head=%q, got %#v", tk.WorkBranch, client.findCalls)
	}
	reused := ev.byKind(task.EventPRReused)
	if len(reused) != 1 {
		t.Fatalf("expected one pr_reused event, got %d (events=%#v)", len(reused), ev.events)
	}
	number, _ := payloadField(t, reused[0].Payload, "number").(float64)
	if int(number) != 17 {
		t.Fatalf("pr_reused number: got %v want 17", number)
	}
	if got := len(ev.byKind(task.EventPRCreated)); got != 0 {
		t.Fatalf("pr_created must not fire on reuse, got %d", got)
	}
}

// TestCreatePRWith_CreatesWhenNoneExists confirms the unchanged happy path:
// when the lookup returns no match, the worker still calls CreatePullRequest
// exactly once and emits pr_created with the new PR metadata.
func TestCreatePRWith_CreatesWhenNoneExists(t *testing.T) {
	ev := &fakeEmitter{}
	tk := samplePRTask()
	client := &fakePRClient{
		findResult: nil,
		createPR:   &gitea.PullRequest{Number: 99, HTMLURL: "http://gitea.local/o/r/pulls/99", Title: "chore(ai): fix bug"},
	}

	if err := worker.CreatePRWith(context.Background(), ev, tk, workflow.Config{}, "summary", false, client); err != nil {
		t.Fatalf("CreatePRWith: %v", err)
	}

	if len(client.createCalls) != 1 {
		t.Fatalf("expected exactly one CreatePullRequest call, got %d", len(client.createCalls))
	}
	if got := len(ev.byKind(task.EventPRReused)); got != 0 {
		t.Fatalf("pr_reused must not fire on create path, got %d", got)
	}
	created := ev.byKind(task.EventPRCreated)
	if len(created) != 1 {
		t.Fatalf("expected one pr_created event, got %d", len(created))
	}
	number, _ := payloadField(t, created[0].Payload, "number").(float64)
	if int(number) != 99 {
		t.Fatalf("pr_created number: got %v want 99", number)
	}
}

// TestCreatePRWith_ListErrorFallsThroughToCreate ensures that a transient
// failure in the list step does not block the worker: we still attempt to
// create. This is the safer fallback because if a PR really does already
// exist, Gitea will surface a 422 from CreatePullRequest, which then flows
// through the existing failure-attribution path; meanwhile a real
// "no PR exists yet" lookup that just happened to fail (network hiccup,
// 5xx) still completes the handoff.
func TestCreatePRWith_ListErrorFallsThroughToCreate(t *testing.T) {
	ev := &fakeEmitter{}
	tk := samplePRTask()
	client := &fakePRClient{
		findErr: errors.New("list pull requests failed: 500"),
	}

	if err := worker.CreatePRWith(context.Background(), ev, tk, workflow.Config{}, "summary", false, client); err != nil {
		t.Fatalf("CreatePRWith: %v", err)
	}
	if len(client.createCalls) != 1 {
		t.Fatalf("expected fallback CreatePullRequest call, got %d", len(client.createCalls))
	}
}

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
	wf, src, err := worker.ResolveWorkflow(context.Background(), ev, "tsk_1", dir)
	if err != nil {
		t.Fatalf("ResolveWorkflow: %v", err)
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

// TestResolveWorkflow_LogsResolutionLine pins the observability
// requirement from issue #69: every workflow resolution emits a single
// info-level log line that summarizes source, path, and the shadow set.
// The standalone log line lets an operator answer "which file is in
// effect?" by tailing worker logs, without parsing the structured event
// stream. When nothing is shadowed, the `shadowed=` segment is omitted
// so the common case stays terse.
func TestResolveWorkflow_LogsResolutionLine(t *testing.T) {
	dir := t.TempDir()
	body := "---\nrepo:\n  owner: o\n  name: r\n  clone_url: git@example.com:o/r.git\ntracker:\n  kind: linear\n---\nprompt\n"
	if err := os.WriteFile(filepath.Join(dir, "WORKFLOW.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write root: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".aiops"), 0o755); err != nil {
		t.Fatalf("mkdir .aiops: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".aiops", "WORKFLOW.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write .aiops: %v", err)
	}

	var buf bytes.Buffer
	origOut := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(origOut)
		log.SetFlags(origFlags)
	})

	if _, _, err := worker.ResolveWorkflow(context.Background(), &fakeEmitter{}, "tsk_log", dir); err != nil {
		t.Fatalf("ResolveWorkflow: %v", err)
	}

	got := buf.String()
	wantSubstrings := []string{
		"task tsk_log: workflow resolved",
		"source=file",
		"path=WORKFLOW.md",
		"shadowed=[.aiops/WORKFLOW.md]",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(got, want) {
			t.Fatalf("log line missing %q; got:\n%s", want, got)
		}
	}
}

// TestResolveWorkflow_LogsResolutionLineOmitsEmptyShadowed keeps the
// common no-shadow case readable: when only the canonical path exists,
// the line carries `source=` and `path=` but no `shadowed=` segment.
func TestResolveWorkflow_LogsResolutionLineOmitsEmptyShadowed(t *testing.T) {
	dir := t.TempDir()
	body := "---\nrepo:\n  owner: o\n  name: r\n  clone_url: git@example.com:o/r.git\ntracker:\n  kind: linear\n---\nprompt\n"
	if err := os.WriteFile(filepath.Join(dir, "WORKFLOW.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	var buf bytes.Buffer
	origOut := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(origOut)
		log.SetFlags(origFlags)
	})

	if _, _, err := worker.ResolveWorkflow(context.Background(), &fakeEmitter{}, "tsk_log2", dir); err != nil {
		t.Fatalf("ResolveWorkflow: %v", err)
	}

	got := buf.String()
	if !strings.Contains(got, "source=file") || !strings.Contains(got, "path=WORKFLOW.md") {
		t.Fatalf("missing source/path in log line:\n%s", got)
	}
	if strings.Contains(got, "shadowed=") {
		t.Fatalf("shadowed= must be omitted when empty:\n%s", got)
	}
}

// TestResolveWorkflow_DefaultSourceOmitsPath checks that when no
// WORKFLOW.md exists, the resolved event records source=default and
// does not emit an empty path key.
func TestResolveWorkflow_DefaultSourceOmitsPath(t *testing.T) {
	dir := t.TempDir()
	ev := &fakeEmitter{}
	_, src, err := worker.ResolveWorkflow(context.Background(), ev, "tsk_2", dir)
	if err != nil {
		t.Fatalf("ResolveWorkflow: %v", err)
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

// TestRunRunnerWithTimeout_StampsWorkflowSource verifies the
// runner_start payload carries workflow_source as a quick-look field.
// The full provenance is on workflow_resolved; this stamp lets a
// timeline viewer color the runner stage by source without joining
// against the earlier event.
func TestRunRunnerWithTimeout_StampsWorkflowSource(t *testing.T) {
	ev := &fakeEmitter{}
	in := runner.RunInput{Task: task.Task{ID: "tsk_1", Model: "mock"}}
	if _, err := worker.RunRunnerWithTimeout(context.Background(), ev, stubRunner{}, in, time.Second, "prompt_only"); err != nil {
		t.Fatalf("RunRunnerWithTimeout: %v", err)
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

// TestVerifyAllowFailure_OpensDegradedDraftPR pins the investigation-override
// contract: when verify fails AND verify.allow_failure=true, RunVerifyPhase
// returns (true, nil), the caller forces cfg.PR.Draft=true, and the eventual
// CreatePRWith call opens a draft PR. A verify_end event with
// status="failed_allowed" must be emitted.
func TestVerifyAllowFailure_OpensDegradedDraftPR(t *testing.T) {
	ev := &fakeEmitter{}
	dir := t.TempDir()

	cfg := workflow.Config{
		Verify: workflow.VerifyConfig{
			Commands:     []string{"sh -c 'exit 1'"},
			AllowFailure: true,
		},
		PR: workflow.PRConfig{Draft: false}, // prove override forces draft
	}

	degraded, err := worker.RunVerifyPhase(context.Background(), ev, "tsk_af", dir, cfg)
	if err != nil {
		t.Fatalf("RunVerifyPhase with allow_failure=true must not return error, got: %v", err)
	}
	if !degraded {
		t.Fatal("RunVerifyPhase must return degraded=true when verify fails with allow_failure=true")
	}

	// Confirm verify_end event with status=failed_allowed.
	ends := ev.byKind(task.EventVerifyEnd)
	if len(ends) != 1 {
		t.Fatalf("verify_end event count: got=%d want=1", len(ends))
	}
	status, _ := payloadField(t, ends[0].Payload, "status").(string)
	if status != "failed_allowed" {
		t.Fatalf("verify_end status: got=%q want=%q", status, "failed_allowed")
	}

	// Now exercise CreatePRWith with verifyDegraded=true and Draft forced to true.
	if degraded {
		cfg.PR.Draft = true
	}
	tk := samplePRTask()
	client := &fakePRClient{}
	if err := worker.CreatePRWith(context.Background(), ev, tk, cfg, "summary", true, client); err != nil {
		t.Fatalf("CreatePRWith: %v", err)
	}
	if len(client.createCalls) != 1 {
		t.Fatalf("expected one CreatePullRequest call, got %d", len(client.createCalls))
	}
	if !client.createCalls[0].Draft {
		t.Fatal("PR must be created as draft when verifyDegraded=true")
	}
}

// TestVerifyFails_BlocksPRWhenAllowFailureOff pins the default behavior:
// a failing verify command without allow_failure prevents PR creation.
// This locks the contract that the new collect-all semantics did not
// weaken safety.
func TestVerifyFails_BlocksPRWhenAllowFailureOff(t *testing.T) {
	ev := &fakeEmitter{}
	dir := t.TempDir()

	cfg := workflow.Config{
		Verify: workflow.VerifyConfig{
			Commands:     []string{"sh -c 'exit 1'"},
			AllowFailure: false,
		},
	}

	degraded, err := worker.RunVerifyPhase(context.Background(), ev, "tsk_naf", dir, cfg)
	if err == nil {
		t.Fatal("RunVerifyPhase must return error when verify fails and allow_failure=false")
	}
	if degraded {
		t.Fatal("RunVerifyPhase must return degraded=false when allow_failure=false")
	}

	// Confirm verify_end event with status=failed.
	ends := ev.byKind(task.EventVerifyEnd)
	if len(ends) != 1 {
		t.Fatalf("verify_end event count: got=%d want=1", len(ends))
	}
	status, _ := payloadField(t, ends[0].Payload, "status").(string)
	if status != "failed" {
		t.Fatalf("verify_end status: got=%q want=%q", status, "failed")
	}
}

// fakeOutputRunner returns a fixed Result with non-zero output fields so we
// can assert RunRunnerWithTimeout forwards them onto the runner_end payload.
type fakeOutputRunner struct{}

func (fakeOutputRunner) Run(_ context.Context, _ runner.RunInput) (runner.Result, error) {
	return runner.Result{
		Summary:       "fake done",
		OutputBytes:   42,
		OutputDropped: 7,
		OutputHead:    "head-canary",
		OutputTail:    "tail-canary",
	}, nil
}

// TestRunRunnerWithTimeout_EmitsOutputFieldsOnRunnerEnd verifies that when a
// runner returns non-zero output telemetry, the runner_end payload carries
// output_bytes, output_dropped, output_head, and output_tail.
func TestRunRunnerWithTimeout_EmitsOutputFieldsOnRunnerEnd(t *testing.T) {
	ev := &fakeEmitter{}
	in := runner.RunInput{Task: task.Task{ID: "tsk_payload", Model: "codex"}}
	if _, err := worker.RunRunnerWithTimeout(context.Background(), ev, fakeOutputRunner{}, in, 5*time.Second, "file"); err != nil {
		t.Fatalf("RunRunnerWithTimeout: %v", err)
	}
	ends := ev.byKind(task.EventRunnerEnd)
	if len(ends) == 0 {
		t.Fatal("no runner_end event recorded")
	}
	pe := ends[len(ends)-1]

	wantInt := map[string]float64{
		"output_bytes":   42,
		"output_dropped": 7,
	}
	for k, want := range wantInt {
		got := payloadField(t, pe.Payload, k)
		if got != want {
			t.Fatalf("payload[%q] = %v (%T), want %v", k, got, got, want)
		}
	}
	wantStr := map[string]string{
		"output_head": "head-canary",
		"output_tail": "tail-canary",
	}
	for k, want := range wantStr {
		got, _ := payloadField(t, pe.Payload, k).(string)
		if got != want {
			t.Fatalf("payload[%q] = %q, want %q", k, got, want)
		}
	}
}

// TestRunRunnerWithTimeout_OmitsOutputFieldsForMockRunner verifies that the
// MockRunner (which leaves all Output* fields zero) does not pollute the
// runner_end payload with output_* keys.
func TestRunRunnerWithTimeout_OmitsOutputFieldsForMockRunner(t *testing.T) {
	ev := &fakeEmitter{}
	in := runner.RunInput{Task: task.Task{ID: "tsk_mock_payload", Model: "mock"}, Workdir: t.TempDir()}
	if _, err := worker.RunRunnerWithTimeout(context.Background(), ev, runner.MockRunner{}, in, 5*time.Second, "file"); err != nil {
		t.Fatalf("RunRunnerWithTimeout: %v", err)
	}
	ends := ev.byKind(task.EventRunnerEnd)
	if len(ends) == 0 {
		t.Fatal("no runner_end event recorded")
	}
	pe := ends[len(ends)-1]
	for _, k := range []string{"output_bytes", "output_dropped", "output_head", "output_tail"} {
		if got := payloadField(t, pe.Payload, k); got != nil {
			t.Fatalf("payload should not contain %q for mock runner; got %v", k, got)
		}
	}
}

// TestVerifyAllowFailure_DoesNotMaskParentCancel pins that allow_failure
// only downgrades real verification failures, not context cancellation.
// Codex review on PR #55 caught that the original runVerifyPhase swallowed
// every non-nil verifyErr under allow_failure, including context.Canceled
// from a parent ctx, turning worker shutdown into a "failed_allowed" PR.
// The cancellation must still abort the task.
func TestVerifyAllowFailure_DoesNotMaskParentCancel(t *testing.T) {
	ev := &fakeEmitter{}
	dir := t.TempDir()

	cfg := workflow.Config{
		Verify: workflow.VerifyConfig{
			Commands:     []string{"sleep 5"},
			AllowFailure: true, // would otherwise downgrade
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	degraded, err := worker.RunVerifyPhase(ctx, ev, "tsk_cancel", dir, cfg)
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Fatalf("RunVerifyPhase did not honor parent cancel; took %v", elapsed)
	}
	if degraded {
		t.Fatalf("degraded must be false on parent cancel even when allow_failure=true")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want errors.Is(err, context.Canceled)", err)
	}

	// Event should report status=canceled, not failed_allowed.
	ends := ev.byKind(task.EventVerifyEnd)
	if len(ends) != 1 {
		t.Fatalf("verify_end event count: got=%d want=1", len(ends))
	}
	status, _ := payloadField(t, ends[0].Payload, "status").(string)
	if status != "canceled" {
		t.Fatalf("verify_end status: got=%q want=%q", status, "canceled")
	}
}
