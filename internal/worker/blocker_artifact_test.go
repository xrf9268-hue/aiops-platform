package worker

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadBlockerArtifactStrictSchema(t *testing.T) {
	workdir := t.TempDir()
	mustWriteBlockerArtifact(t, workdir, `{
		"version": 1,
		"kind": "external_dependency",
		"reason": "PR #455 still owns the same retry finalization path",
		"retry_after_seconds": 3600
	}`)

	got, err := ReadBlockerArtifact(workdir)
	if err != nil {
		t.Fatalf("ReadBlockerArtifact() error = %v; want nil", err)
	}
	if got.Kind != "external_dependency" || got.Reason == "" || got.RetryAfterSeconds != 3600 {
		t.Fatalf("ReadBlockerArtifact() = %+v; want parsed external dependency blocker", got)
	}
}

func TestReadBlockerArtifactMissing(t *testing.T) {
	_, err := ReadBlockerArtifact(t.TempDir())
	if !errors.Is(err, ErrBlockerArtifactMissing) {
		t.Fatalf("ReadBlockerArtifact() error = %v; want ErrBlockerArtifactMissing", err)
	}
}

func TestReadBlockerArtifactRejectsUnknownFields(t *testing.T) {
	workdir := t.TempDir()
	mustWriteBlockerArtifact(t, workdir, `{
		"version": 1,
		"kind": "external_dependency",
		"reason": "blocked",
		"retry_after_seconds": 3600,
		"legacy_reason": "do not accept aliases"
	}`)

	_, err := ReadBlockerArtifact(workdir)
	if err == nil {
		t.Fatal("ReadBlockerArtifact() error = nil; want unknown-field error")
	}
}

func TestReadBlockerArtifactRejectsUnknownVersion(t *testing.T) {
	workdir := t.TempDir()
	mustWriteBlockerArtifact(t, workdir, `{
		"version": 2,
		"kind": "external_dependency",
		"reason": "blocked",
		"retry_after_seconds": 3600
	}`)

	_, err := ReadBlockerArtifact(workdir)
	if err == nil {
		t.Fatal("ReadBlockerArtifact() error = nil; want version error")
	}
}

func TestConsumeBlockerArtifactRemovesFile(t *testing.T) {
	workdir := t.TempDir()
	mustWriteBlockerArtifact(t, workdir, `{
		"version": 1,
		"kind": "external_dependency",
		"reason": "blocked",
		"retry_after_seconds": 3600
	}`)

	if _, err := ConsumeBlockerArtifact(workdir); err != nil {
		t.Fatalf("ConsumeBlockerArtifact() error = %v; want nil", err)
	}
	if _, err := os.Stat(filepath.Join(workdir, BlockerArtifactPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("consumed blocker file stat error = %v; want not exist", err)
	}
}

func TestResetBlockerArtifactRemovesStaleFile(t *testing.T) {
	workdir := t.TempDir()
	mustWriteBlockerArtifact(t, workdir, `{
		"version": 1,
		"kind": "external_dependency",
		"reason": "blocked",
		"retry_after_seconds": 3600
	}`)

	if err := ResetBlockerArtifact(workdir); err != nil {
		t.Fatalf("ResetBlockerArtifact() error = %v; want nil", err)
	}
	if _, err := os.Stat(filepath.Join(workdir, BlockerArtifactPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reset blocker file stat error = %v; want not exist", err)
	}
	if err := ResetBlockerArtifact(workdir); err != nil {
		t.Fatalf("ResetBlockerArtifact() on missing file error = %v; want nil", err)
	}
}

func TestExternalBlockerErrorRetryAfter(t *testing.T) {
	err := &ExternalBlockerError{Artifact: BlockerArtifact{Reason: "blocked", RetryAfterSeconds: 90}}
	if got := err.RetryAfter(); got != 90*time.Second {
		t.Fatalf("RetryAfter() = %v; want %v", got, 90*time.Second)
	}
}

func mustWriteBlockerArtifact(t *testing.T, workdir, body string) {
	t.Helper()
	dir := filepath.Join(workdir, ".aiops")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", dir, err)
	}
	path := filepath.Join(workdir, BlockerArtifactPath)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}
