package workspace

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// fakeExitError satisfies errors.As(*exec.ExitError) by using a real
// exec.ExitError obtained from running `false`. Building one by hand is
// awkward because os.ProcessState is unexported; running the binary is
// the cleanest cross-platform way.
func fakeExitError(t *testing.T) *exec.ExitError {
	t.Helper()
	cmd := exec.Command("false")
	err := cmd.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("could not synthesize *exec.ExitError, got %T: %v", err, err)
	}
	return ee
}

func TestRunSecretScan_DisabledIsSkipped(t *testing.T) {
	res := RunSecretScan(context.Background(), t.TempDir(), workflow.SecretScanConfig{
		Enabled: false,
		Command: []string{"echo", "hi"},
	})
	if res.Status != SecretScanSkipped {
		t.Fatalf("expected skipped, got %q", res.Status)
	}
	if res.ShouldBlockPush(workflow.SecretScanConfig{}) {
		t.Fatal("skipped result must not block push")
	}
}

func TestRunSecretScan_EnabledNoCommandIsSkipped(t *testing.T) {
	res := RunSecretScan(context.Background(), t.TempDir(), workflow.SecretScanConfig{
		Enabled: true,
		Command: nil,
	})
	if res.Status != SecretScanSkipped {
		t.Fatalf("expected skipped when command empty, got %q", res.Status)
	}
}

func TestRunSecretScan_CleanExit(t *testing.T) {
	cfg := workflow.SecretScanConfig{Enabled: true, Command: []string{"my-scanner", "detect"}}
	called := false
	stub := func(ctx context.Context, dir string, argv []string) ([]byte, []byte, int, error) {
		called = true
		if !reflect.DeepEqual(argv, cfg.Command) {
			t.Fatalf("argv passthrough mismatch: got=%v want=%v", argv, cfg.Command)
		}
		return []byte("no leaks\n"), nil, 0, nil
	}
	res := runSecretScanWith(context.Background(), t.TempDir(), cfg, stub)
	if !called {
		t.Fatal("runner was not invoked")
	}
	if res.Status != SecretScanClean {
		t.Fatalf("expected clean, got %q", res.Status)
	}
	if res.ExitCode != 0 {
		t.Fatalf("expected exit 0, got %d", res.ExitCode)
	}
	if res.Stdout != "no leaks\n" {
		t.Fatalf("expected stdout passthrough, got %q", res.Stdout)
	}
	if res.ShouldBlockPush(cfg) {
		t.Fatal("clean scan must not block push")
	}
}

func TestRunSecretScan_NonZeroIsViolation(t *testing.T) {
	cfg := workflow.SecretScanConfig{Enabled: true, Command: []string{"gitleaks", "detect"}}
	stub := func(ctx context.Context, dir string, argv []string) ([]byte, []byte, int, error) {
		return []byte("leak found in foo.go\n"), []byte("WRN ...\n"), 1, fakeExitError(t)
	}
	res := runSecretScanWith(context.Background(), t.TempDir(), cfg, stub)
	if res.Status != SecretScanViolation {
		t.Fatalf("expected violation, got %q", res.Status)
	}
	if res.ExitCode != 1 {
		t.Fatalf("expected exit code 1, got %d", res.ExitCode)
	}
	if !strings.Contains(res.Stdout, "leak found") {
		t.Fatalf("stdout not preserved: %q", res.Stdout)
	}
	if !res.ShouldBlockPush(cfg) {
		t.Fatal("violation with default fail_on_finding must block push")
	}
}

func TestRunSecretScan_ViolationWarnOnlyDoesNotBlock(t *testing.T) {
	warn := false
	cfg := workflow.SecretScanConfig{
		Enabled:       true,
		Command:       []string{"gitleaks"},
		FailOnFinding: &warn,
	}
	stub := func(ctx context.Context, dir string, argv []string) ([]byte, []byte, int, error) {
		return nil, nil, 1, fakeExitError(t)
	}
	res := runSecretScanWith(context.Background(), t.TempDir(), cfg, stub)
	if res.Status != SecretScanViolation {
		t.Fatalf("expected violation, got %q", res.Status)
	}
	if res.ShouldBlockPush(cfg) {
		t.Fatal("warn-only mode must not block push on violation")
	}
}

func TestRunSecretScan_ExecErrorBlocksPush(t *testing.T) {
	cfg := workflow.SecretScanConfig{Enabled: true, Command: []string{"gitleaks-not-installed"}}
	stub := func(ctx context.Context, dir string, argv []string) ([]byte, []byte, int, error) {
		return nil, nil, 0, errors.New(`exec: "gitleaks-not-installed": executable file not found in $PATH`)
	}
	res := runSecretScanWith(context.Background(), t.TempDir(), cfg, stub)
	if res.Status != SecretScanError {
		t.Fatalf("expected error status, got %q", res.Status)
	}
	if res.Err == nil {
		t.Fatal("expected Err populated for exec error")
	}
	// Error status always blocks regardless of FailOnFinding.
	off := false
	cfg.FailOnFinding = &off
	if !res.ShouldBlockPush(cfg) {
		t.Fatal("exec error must block push even when fail_on_finding=false")
	}
}

func TestRunSecretScan_RealBinaryCleanAndDirty(t *testing.T) {
	// Use built-in shell utilities so this works on any CI box without
	// installing gitleaks. `true` exits 0 (clean); `false` exits 1 (finding).
	if _, err := exec.LookPath("true"); err != nil {
		t.Skip("/usr/bin/true not available")
	}
	clean := RunSecretScan(context.Background(), t.TempDir(), workflow.SecretScanConfig{
		Enabled: true,
		Command: []string{"true"},
	})
	if clean.Status != SecretScanClean {
		t.Fatalf("expected clean from `true`, got %+v", clean)
	}

	if _, err := exec.LookPath("false"); err != nil {
		t.Skip("/usr/bin/false not available")
	}
	dirty := RunSecretScan(context.Background(), t.TempDir(), workflow.SecretScanConfig{
		Enabled: true,
		Command: []string{"false"},
	})
	if dirty.Status != SecretScanViolation {
		t.Fatalf("expected violation from `false`, got %+v", dirty)
	}
	if dirty.ExitCode == 0 {
		t.Fatalf("expected non-zero exit from `false`, got %d", dirty.ExitCode)
	}
}

func TestRunSecretScan_RealBinaryMissingIsExecError(t *testing.T) {
	res := RunSecretScan(context.Background(), t.TempDir(), workflow.SecretScanConfig{
		Enabled: true,
		Command: []string{"definitely-not-a-real-binary-aiops-platform"},
	})
	if res.Status != SecretScanError {
		t.Fatalf("expected error status, got %+v", res)
	}
}

func TestSecretScanConfig_ShouldFailOnFindingDefaultsTrue(t *testing.T) {
	cfg := workflow.SecretScanConfig{}
	if !cfg.ShouldFailOnFinding() {
		t.Fatal("default should be true (block on finding)")
	}
	off := false
	cfg.FailOnFinding = &off
	if cfg.ShouldFailOnFinding() {
		t.Fatal("explicit false must be honored")
	}
	on := true
	cfg.FailOnFinding = &on
	if !cfg.ShouldFailOnFinding() {
		t.Fatal("explicit true must be honored")
	}
}

func TestTruncate(t *testing.T) {
	if truncate([]byte("hello"), 10) != "hello" {
		t.Fatal("under-cap should be unchanged")
	}
	if truncate([]byte("hello"), 3) != "hel" {
		t.Fatal("over-cap should be truncated")
	}
}
