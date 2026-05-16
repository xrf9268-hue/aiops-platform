//go:build !windows

package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestConfigurePlatformKillDoesNotInvokeDefaultCancel(t *testing.T) {
	probe := filepath.Join(t.TempDir(), "default-cancel-called")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", "exit 0")
	cmd.Cancel = func() error {
		return os.WriteFile(probe, []byte("called"), 0o600)
	}
	configurePlatformKill(cmd)
	if cmd.Cancel == nil {
		t.Fatal("expected configurePlatformKill to install a cancel hook")
	}

	if err := cmd.Start(); err != nil {
		t.Fatalf("start command: %v", err)
	}
	if err := cmd.Cancel(); err != nil {
		t.Fatalf("cancel command: %v", err)
	}
	_ = cmd.Wait()

	if _, err := os.Stat(probe); err == nil {
		t.Fatal("configurePlatformKill must not invoke exec.CommandContext default Cancel because it bypasses killGrace with immediate Kill")
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat default cancel probe: %v", err)
	}
}

func TestConfigurePlatformKillRunsCleanupCancelWhenWaitDelayAlreadySet(t *testing.T) {
	probe := filepath.Join(t.TempDir(), "cleanup-called")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", "exit 0")
	cmd.WaitDelay = time.Nanosecond
	cmd.Cancel = func() error {
		return os.WriteFile(probe, []byte("called"), 0o600)
	}
	configurePlatformKill(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start command: %v", err)
	}
	if err := cmd.Cancel(); err != nil {
		t.Fatalf("cancel command: %v", err)
	}
	_ = cmd.Wait()

	if _, err := os.Stat(probe); err != nil {
		t.Fatalf("expected preinstalled cleanup Cancel to run when WaitDelay marks it as non-default cleanup, stat err = %v", err)
	}
}
