package worker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
	"github.com/xrf9268-hue/aiops-platform/internal/workspace"
)

// secretScanKinds extracts the ordered list of event kinds the fake
// emitter has recorded so far. Hoisted out of each subtest to keep
// individual cases focused on their assertions.
func secretScanKinds(ev *fakeEmitter) []string {
	out := make([]string, 0, len(ev.events))
	for _, e := range ev.events {
		out = append(out, e.Kind)
	}
	return out
}

// payloadString JSON-marshals a recorded payload so substring assertions
// (e.g. "leak found in foo.go" or a command argument) remain stable
// regardless of whether the helper passes a map[string]any, a []byte, or
// any other shape into AddEventWithPayload.
func payloadString(t *testing.T, payload any) string {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("payload marshal: %v", err)
	}
	return string(b)
}

func enabledSecretScanCfg() workflow.Config {
	return workflow.Config{Verify: workflow.VerifyConfig{
		SecretScan: workflow.SecretScanConfig{
			Enabled: true,
			Command: []string{"gitleaks", "detect", "--source", "."},
		},
	}}
}

func TestRunSecretScanWith_DisabledIsNoop(t *testing.T) {
	ev := &fakeEmitter{}
	called := false
	stub := func(_ context.Context, _ string, _ workflow.SecretScanConfig) workspace.SecretScanResult {
		called = true
		return workspace.SecretScanResult{}
	}
	cfg := workflow.Config{} // SecretScan zero value: disabled
	if err := runSecretScanWith(context.Background(), ev, "tsk", "/tmp", cfg, stub); err != nil {
		t.Fatalf("disabled scan must not error, got %v", err)
	}
	if called {
		t.Fatal("scanner must not run when disabled")
	}
	if len(ev.events) != 0 {
		t.Fatalf("disabled scan must emit no events, got %v", secretScanKinds(ev))
	}
}

func TestRunSecretScanWith_CleanEmitsStartAndClean(t *testing.T) {
	ev := &fakeEmitter{}
	stub := func(_ context.Context, _ string, _ workflow.SecretScanConfig) workspace.SecretScanResult {
		return workspace.SecretScanResult{Status: workspace.SecretScanClean, ExitCode: 0, DurationMs: 12}
	}
	if err := runSecretScanWith(context.Background(), ev, "tsk", "/tmp", enabledSecretScanCfg(), stub); err != nil {
		t.Fatalf("clean scan must not error, got %v", err)
	}
	got := secretScanKinds(ev)
	want := []string{"secret_scan_start", "secret_scan_clean"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("event kinds: got=%v want=%v", got, want)
	}
}

func TestRunSecretScanWith_ViolationBlocksPushAndEmitsViolationEvent(t *testing.T) {
	ev := &fakeEmitter{}
	stub := func(_ context.Context, _ string, _ workflow.SecretScanConfig) workspace.SecretScanResult {
		return workspace.SecretScanResult{
			Status:     workspace.SecretScanViolation,
			ExitCode:   1,
			DurationMs: 99,
			Stdout:     "leak found in file foo.go\n",
		}
	}
	err := runSecretScanWith(context.Background(), ev, "tsk", "/tmp", enabledSecretScanCfg(), stub)
	if err == nil {
		t.Fatal("violation with default fail_on_finding must abort push")
	}
	if !strings.Contains(err.Error(), "secret scan reported findings") {
		t.Fatalf("unexpected error message: %v", err)
	}
	kinds := secretScanKinds(ev)
	if len(kinds) != 2 || kinds[0] != "secret_scan_start" || kinds[1] != "secret_scan_violation" {
		t.Fatalf("expected start+violation events, got %v", kinds)
	}
	// Sanity: the violation event payload includes the captured stdout so
	// operators can see what the scanner reported without re-running it.
	if payload := payloadString(t, ev.events[1].Payload); !strings.Contains(payload, "leak found in file foo.go") {
		t.Fatalf("violation payload should include scanner stdout: %s", payload)
	}
}

func TestRunSecretScanWith_ViolationWarnOnlyAllowsPush(t *testing.T) {
	ev := &fakeEmitter{}
	cfg := enabledSecretScanCfg()
	off := false
	cfg.Verify.SecretScan.FailOnFinding = &off
	stub := func(_ context.Context, _ string, _ workflow.SecretScanConfig) workspace.SecretScanResult {
		return workspace.SecretScanResult{Status: workspace.SecretScanViolation, ExitCode: 1}
	}
	if err := runSecretScanWith(context.Background(), ev, "tsk", "/tmp", cfg, stub); err != nil {
		t.Fatalf("warn-only must not block push, got %v", err)
	}
	kinds := secretScanKinds(ev)
	if len(kinds) != 2 || kinds[1] != "secret_scan_violation" {
		t.Fatalf("expected violation event in warn-only mode, got %v", kinds)
	}
}

func TestRunSecretScanWith_ExecErrorBlocksAndEmitsErrorEvent(t *testing.T) {
	ev := &fakeEmitter{}
	stub := func(_ context.Context, _ string, _ workflow.SecretScanConfig) workspace.SecretScanResult {
		return workspace.SecretScanResult{
			Status: workspace.SecretScanError,
			Err:    errors.New("exec: gitleaks: not found"),
		}
	}
	err := runSecretScanWith(context.Background(), ev, "tsk", "/tmp", enabledSecretScanCfg(), stub)
	if err == nil {
		t.Fatal("exec error must block push")
	}
	if !strings.Contains(err.Error(), "secret scan failed to execute") {
		t.Fatalf("unexpected error message: %v", err)
	}
	kinds := secretScanKinds(ev)
	if len(kinds) != 2 || kinds[1] != "secret_scan_error" {
		t.Fatalf("expected secret_scan_error event, got %v", kinds)
	}
}

func TestRunSecretScanWith_StartPayloadCarriesCommand(t *testing.T) {
	ev := &fakeEmitter{}
	stub := func(_ context.Context, _ string, _ workflow.SecretScanConfig) workspace.SecretScanResult {
		return workspace.SecretScanResult{Status: workspace.SecretScanClean}
	}
	cfg := enabledSecretScanCfg()
	if err := runSecretScanWith(context.Background(), ev, "tsk", "/tmp", cfg, stub); err != nil {
		t.Fatalf("clean scan must not error, got %v", err)
	}
	if len(ev.events) < 1 {
		t.Fatal("expected at least one event")
	}
	startPayload := payloadString(t, ev.events[0].Payload)
	for _, want := range cfg.Verify.SecretScan.Command {
		if !strings.Contains(startPayload, want) {
			t.Fatalf("start payload missing arg %q: %s", want, startPayload)
		}
	}
}
