package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/gitea"
	"github.com/xrf9268-hue/aiops-platform/internal/runner"
	"github.com/xrf9268-hue/aiops-platform/internal/task"
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
	emit(context.Background(), ev, "tsk_1", task.EventRunnerStart, "runner started", map[string]any{"model": "mock"})
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
	emit(context.Background(), nil, "tsk_1", task.EventRunnerStart, "ignored", nil)
}

func TestEmitLogsEmitterError(t *testing.T) {
	ev := &fakeEmitter{err: errors.New("db down")}
	emit(context.Background(), ev, "tsk_1", task.EventPush, "push", nil)
	if len(ev.events) != 1 {
		t.Fatalf("event should still be recorded by fake even when error returned")
	}
}

func TestErrSummaryTruncatesLongMessages(t *testing.T) {
	if errSummary(nil) != "" {
		t.Fatalf("nil error should map to empty string")
	}
	long := strings.Repeat("x", 600)
	got := errSummary(errors.New(long))
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
	got := summarizeVerifyResults(results)
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
	body := buildPRBody(t1, summary)
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
// at prBodySummaryCap and surfaces a truncation marker so reviewers know to
// open the artifact for the rest.
func TestBuildPRBodyTruncatesLongSummary(t *testing.T) {
	t1 := task.Task{ID: "tsk_big", WorkBranch: "ai/tsk_big"}
	// Use a sentinel char that does not appear elsewhere in the PR chrome
	// so we can count exactly how much of the user-provided summary made
	// it into the rendered body.
	const sentinel = "Z"
	long := strings.Repeat(sentinel, prBodySummaryCap+1024)
	body := buildPRBody(t1, long)
	if !strings.Contains(body, "_Summary truncated at") {
		t.Fatalf("expected truncation marker in PR body for oversized summary")
	}
	if got := strings.Count(body, sentinel); got > prBodySummaryCap {
		t.Fatalf("excerpt exceeded prBodySummaryCap: %d > %d", got, prBodySummaryCap)
	}
}

// TestAppendRunSummaryDirectiveAppendsOnce verifies the runner contract is
// only appended when the rendered prompt does not already mention it. This
// keeps workflow templates free to embed their own (more detailed) version
// without duplication.
func TestAppendRunSummaryDirectiveAppendsOnce(t *testing.T) {
	plain := "do the work"
	out := appendRunSummaryDirective(plain)
	if !strings.Contains(out, "RUN_SUMMARY.md") {
		t.Fatalf("directive not appended: %q", out)
	}
	if !strings.Contains(out, "**Required output:**") {
		t.Fatalf("expected Required output marker in appended directive")
	}

	// Idempotent: prompt that already mentions RUN_SUMMARY.md is left alone.
	already := "please write .aiops/RUN_SUMMARY.md when done"
	if got := appendRunSummaryDirective(already); got != already {
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
// runRunnerWithTimeout assertions resilient to the eventEmitter accepting
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
	_, err := runRunnerWithTimeout(context.Background(), ev, stubRunner{sleep: 5 * time.Second}, in, 30*time.Millisecond)
	if !runner.IsTimeout(err) {
		t.Fatalf("expected TimeoutError from runRunnerWithTimeout, got %v", err)
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
	if _, err := runRunnerWithTimeout(context.Background(), ev, stubRunner{}, in, time.Second); err != nil {
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
	_, err := runRunnerWithTimeout(context.Background(), ev, stubRunner{err: wantErr}, in, time.Second)
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
	if _, err := runRunnerWithTimeout(context.Background(), ev, stubRunner{sleep: 10 * time.Millisecond}, in, 0); err != nil {
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

	if err := createPRWith(context.Background(), ev, tk, cfg, "summary", client); err != nil {
		t.Fatalf("createPRWith: %v", err)
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

	if err := createPRWith(context.Background(), ev, tk, workflow.Config{}, "summary", client); err != nil {
		t.Fatalf("createPRWith: %v", err)
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

	if err := createPRWith(context.Background(), ev, tk, workflow.Config{}, "summary", client); err != nil {
		t.Fatalf("createPRWith: %v", err)
	}
	if len(client.createCalls) != 1 {
		t.Fatalf("expected fallback CreatePullRequest call, got %d", len(client.createCalls))
	}
}
