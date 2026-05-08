package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/xrf9268-hue/aiops-platform/internal/policy"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// initRepo creates a temp git repo seeded with a single committed file so
// `git diff HEAD` has a valid base.
func initRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	for _, args := range [][]string{
		{"git", "init", "-q", "-b", "main"},
		{"git", "config", "user.email", "test@example.com"},
		{"git", "config", "user.name", "test"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"git", "add", "."},
		{"git", "commit", "-q", "-m", "seed"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	return dir
}

func TestDiffstatCountsModifiedAndUntracked(t *testing.T) {
	dir := initRepo(t)
	// modify seed
	if err := os.WriteFile(filepath.Join(dir, "seed.txt"), []byte("seed\nmore\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// add untracked deny-pathish file
	if err := os.MkdirAll(filepath.Join(dir, "infra"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "infra", "main.tf"), []byte("a\nb\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	d, err := Diffstat(context.Background(), dir)
	if err != nil {
		t.Fatalf("Diffstat error: %v", err)
	}
	if len(d.Files) != 2 {
		t.Fatalf("expected 2 files, got %v", d.Files)
	}
	if d.Lines < 4 { // 1 added line on seed + 3 added lines on infra/main.tf
		t.Fatalf("expected >=4 lines, got %d", d.Lines)
	}
}

func TestEnforcePolicyDenyBlocksAndReturnsStructuredError(t *testing.T) {
	dir := initRepo(t)
	if err := os.MkdirAll(filepath.Join(dir, "infra"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "infra", "main.tf"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := workflow.Config{Policy: workflow.PolicyConfig{
		DenyPaths:       []string{"infra/**"},
		MaxChangedFiles: 100,
		MaxChangedLines: 100,
	}}
	err := EnforcePolicy(context.Background(), dir, cfg)
	if err == nil {
		t.Fatal("expected policy error")
	}
	var pe *PolicyError
	if !errors.As(err, &pe) {
		t.Fatalf("expected *PolicyError, got %T: %v", err, err)
	}
	if len(pe.Violations) != 1 || pe.Violations[0].Kind != policy.KindDenyPath {
		t.Fatalf("unexpected violations: %+v", pe.Violations)
	}
}

func TestEnforcePolicyMaxLinesUsesLegacyLOC(t *testing.T) {
	dir := initRepo(t)
	// 10 added lines
	if err := os.WriteFile(filepath.Join(dir, "seed.txt"), []byte("a\nb\nc\nd\ne\nf\ng\nh\ni\nj\nk\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := workflow.Config{Policy: workflow.PolicyConfig{MaxChangedLOC: 3}}
	err := EnforcePolicy(context.Background(), dir, cfg)
	var pe *PolicyError
	if !errors.As(err, &pe) {
		t.Fatalf("expected PolicyError, got %v", err)
	}
	found := false
	for _, v := range pe.Violations {
		if v.Kind == policy.KindMaxChangedLines {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected max_changed_lines violation, got %+v", pe.Violations)
	}
}

// TestParseNumstatZ_VanillaRenameAndBrace covers the three numstat -z
// shapes we care about for path-policy enforcement:
//
//  1. Normal modify/add: "added\tdeleted\tpath\0"
//  2. Cross-directory rename (would render as "old => new" without -z):
//     "added\tdeleted\t\0old\0new\0"
//  3. Brace rename with shared prefix (would render as "{a => b}/c" without -z):
//     "added\tdeleted\t\0src/sub/file.go\0infra/sub/file.go\0"
//
// In all rename cases we must report the *new* path so that policy globs
// like "infra/**" match.
func TestParseNumstatZ_VanillaRenameAndBrace(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		wantFiles []string
		wantLines int
	}{
		{
			name:      "vanilla modify",
			raw:       "1\t0\tseed.txt\x00",
			wantFiles: []string{"seed.txt"},
			wantLines: 1,
		},
		{
			name:      "cross-directory rename (=> form)",
			raw:       "0\t0\t\x00src/old.go\x00infra/new.go\x00",
			wantFiles: []string{"infra/new.go"},
			wantLines: 0,
		},
		{
			name:      "brace rename (shared prefix/suffix)",
			raw:       "0\t0\t\x00src/sub/file.go\x00infra/sub/file.go\x00",
			wantFiles: []string{"infra/sub/file.go"},
			wantLines: 0,
		},
		{
			name:      "mixed: modify + rename + add",
			raw:       "2\t1\tseed.txt\x000\t0\t\x00src/a.go\x00infra/a.go\x003\t0\tnew.md\x00",
			wantFiles: []string{"seed.txt", "infra/a.go", "new.md"},
			wantLines: 6,
		},
		{
			name:      "binary file (numstat reports - -)",
			raw:       "-\t-\timg.png\x00",
			wantFiles: []string{"img.png"},
			wantLines: 0,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseNumstatZ([]byte(tc.raw))
			if err != nil {
				t.Fatalf("parseNumstatZ error: %v", err)
			}
			if !reflect.DeepEqual(got.Files, tc.wantFiles) {
				t.Fatalf("files mismatch:\n got=%q\nwant=%q", got.Files, tc.wantFiles)
			}
			if got.Lines != tc.wantLines {
				t.Fatalf("lines mismatch: got=%d want=%d", got.Lines, tc.wantLines)
			}
		})
	}
}

// TestEnforcePolicyDeniesCrossDirectoryRename is the regression test for
// the P1 review thread: before normalizing rename paths, a `git mv
// src/foo.go infra/foo.tf` would emit `src/foo.go => infra/foo.tf` in
// numstat output, which does not match the `infra/**` glob, so the deny
// rule was silently bypassed. With -z parsing the destination path is
// matched directly.
func TestEnforcePolicyDeniesCrossDirectoryRename(t *testing.T) {
	dir := initRepo(t)
	// Seed a file inside src/ and commit it so we have something to rename.
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "main.tf"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"git", "add", "."},
		{"git", "commit", "-q", "-m", "add src/main.tf"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	// Rename across directories into a denied path.
	if err := os.MkdirAll(filepath.Join(dir, "infra"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "mv", "src/main.tf", "infra/main.tf")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git mv: %v\n%s", err, out)
	}

	cfg := workflow.Config{Policy: workflow.PolicyConfig{
		DenyPaths:       []string{"infra/**"},
		MaxChangedFiles: 100,
		MaxChangedLines: 100,
	}}
	err := EnforcePolicy(context.Background(), dir, cfg)
	if err == nil {
		t.Fatal("expected policy error for cross-directory rename into infra/")
	}
	var pe *PolicyError
	if !errors.As(err, &pe) {
		t.Fatalf("expected *PolicyError, got %T: %v", err, err)
	}
	foundDeny := false
	for _, v := range pe.Violations {
		if v.Kind == policy.KindDenyPath {
			foundDeny = true
		}
	}
	if !foundDeny {
		t.Fatalf("expected deny_path violation, got %+v", pe.Violations)
	}

	// Sanity: ChangedFiles should report the destination path, not the
	// `src/main.tf => infra/main.tf` rename form.
	files, err := ChangedFiles(context.Background(), dir)
	if err != nil {
		t.Fatalf("ChangedFiles: %v", err)
	}
	for _, f := range files {
		if f != "infra/main.tf" {
			t.Fatalf("expected normalized destination path infra/main.tf, got %q (full=%v)", f, files)
		}
	}
}

func TestEnforcePolicyCleanWorkdir(t *testing.T) {
	dir := initRepo(t)
	cfg := workflow.Config{Policy: workflow.PolicyConfig{
		DenyPaths:       []string{"infra/**"},
		MaxChangedFiles: 5,
		MaxChangedLines: 50,
	}}
	if err := EnforcePolicy(context.Background(), dir, cfg); err != nil {
		t.Fatalf("expected no policy error on clean workdir, got %v", err)
	}
}

// TestCheckSummaryStatuses pins the four states the worker gate inspects:
//
//  1. Missing — file does not exist on disk.
//  2. Empty   — file exists but is whitespace-only.
//  3. Placeholder — short file with TODO/TBD-style markers (rejected so
//     runners cannot trivially satisfy the gate with a stub).
//  4. OK — substantive content.
func TestCheckSummaryStatuses(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		write      bool
		wantStatus SummaryStatus
		wantBody   string
	}{
		{name: "missing", write: false, wantStatus: SummaryMissing},
		{name: "empty", write: true, body: "", wantStatus: SummaryEmpty},
		{name: "whitespace", write: true, body: "   \n\t\n", wantStatus: SummaryEmpty},
		{name: "todo placeholder", write: true, body: "TODO\n", wantStatus: SummaryPlaceholder, wantBody: "TODO"},
		{name: "tbd placeholder", write: true, body: "<TBD>\n", wantStatus: SummaryPlaceholder, wantBody: "<TBD>"},
		{
			name:       "real",
			write:      true,
			body:       "# Summary\n\nFixed off-by-one in foo(); verified with go test ./...\n",
			wantStatus: SummaryOK,
		},
		{
			// Long summary that contains the substring TODO is accepted because
			// length > placeholder threshold; we trust real summaries to
			// reference TODOs without being one.
			name:       "long body mentioning todo is ok",
			write:      true,
			body:       "# Summary\n\nFixed bug. Remaining TODO: investigate flake in worker_test.\n",
			wantStatus: SummaryOK,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.write {
				if err := os.MkdirAll(filepath.Join(dir, ".aiops"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, SummaryPath), []byte(tc.body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			body, status, err := CheckSummary(dir)
			if err != nil {
				t.Fatalf("CheckSummary: %v", err)
			}
			if status != tc.wantStatus {
				t.Fatalf("status = %s, want %s (body=%q)", status, tc.wantStatus, body)
			}
			if tc.wantBody != "" && body != tc.wantBody {
				t.Fatalf("body = %q, want %q", body, tc.wantBody)
			}
		})
	}
}

// TestResetRunSummary verifies the helper that the worker calls before
// starting the runner so the post-runner gate cannot pass on a stale summary
// committed to the base branch or left over from a previous attempt.
func TestResetRunSummary(t *testing.T) {
	t.Run("removes existing file", func(t *testing.T) {
		dir := t.TempDir()
		// Seed a summary that looks substantive enough to satisfy the gate so
		// the test fails loudly if ResetRunSummary becomes a no-op.
		stale := "# Stale summary\n\nThis came from a previous run and should not gate the next PR.\n"
		if err := WriteSummary(dir, stale); err != nil {
			t.Fatalf("seed stale summary: %v", err)
		}
		if _, status, _ := CheckSummary(dir); status != SummaryOK {
			t.Fatalf("precondition: stale summary should look OK, got %s", status)
		}
		if err := ResetRunSummary(dir); err != nil {
			t.Fatalf("ResetRunSummary: %v", err)
		}
		_, status, err := CheckSummary(dir)
		if err != nil {
			t.Fatalf("CheckSummary post-reset: %v", err)
		}
		if status != SummaryMissing {
			t.Fatalf("status after reset = %s, want %s", status, SummaryMissing)
		}
		if _, statErr := os.Stat(filepath.Join(dir, SummaryPath)); !os.IsNotExist(statErr) {
			t.Fatalf("file should be gone, got stat err = %v", statErr)
		}
	})

	t.Run("missing file is not an error", func(t *testing.T) {
		dir := t.TempDir()
		if err := ResetRunSummary(dir); err != nil {
			t.Fatalf("ResetRunSummary on empty dir: %v", err)
		}
		if err := ResetRunSummary(dir); err != nil {
			t.Fatalf("ResetRunSummary should be idempotent, got %v", err)
		}
	})

	t.Run("does not touch sibling artifacts", func(t *testing.T) {
		// CHANGED_FILES.txt and VERIFICATION.txt are produced by the worker
		// itself (which overwrites them each run); the runner-contract file
		// is RUN_SUMMARY.md alone. Confirm the reset is narrowly scoped.
		dir := t.TempDir()
		if err := WriteSummary(dir, "stale summary contents long enough to look real"); err != nil {
			t.Fatalf("seed summary: %v", err)
		}
		if err := WriteChangedFiles(dir, []string{"a.go", "b.go"}); err != nil {
			t.Fatalf("seed changed files: %v", err)
		}
		if err := ResetRunSummary(dir); err != nil {
			t.Fatalf("ResetRunSummary: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, ".aiops", "CHANGED_FILES.txt")); err != nil {
			t.Fatalf("CHANGED_FILES.txt should still exist after reset, got %v", err)
		}
	})
}

// TestWriteAndReadSummaryRoundTrip ensures WriteSummary persists the body
// and ReadSummary returns the trimmed contents (used by the worker's gate
// helper path).
func TestWriteAndReadSummaryRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := WriteSummary(dir, "# Summary\n\nDid the thing.\n"); err != nil {
		t.Fatalf("WriteSummary: %v", err)
	}
	got, err := ReadSummary(dir)
	if err != nil {
		t.Fatalf("ReadSummary: %v", err)
	}
	if got != "# Summary\n\nDid the thing." {
		t.Fatalf("ReadSummary = %q", got)
	}
}

func TestRunVerifyCapturesOutputAndStopsOnFailure(t *testing.T) {
	dir := t.TempDir()
	cfg := workflow.Config{Verify: workflow.VerifyConfig{Commands: []string{
		"echo hello-world",
		"   ", // empty command should be skipped without recording a result
		"sh -c 'echo to-stderr 1>&2; exit 7'",
		"echo unreachable",
	}}}

	results, err := RunVerify(context.Background(), dir, cfg)
	if err == nil {
		t.Fatalf("RunVerify should report failure when a command exits non-zero")
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 verify results (skip empty + stop after failure), got %d", len(results))
	}
	if !strings.Contains(results[0].Output, "hello-world") {
		t.Fatalf("first result missing stdout: %q", results[0].Output)
	}
	if results[0].ExitCode != 0 || results[0].Err != nil {
		t.Fatalf("first result should be success, got %+v", results[0])
	}
	if results[1].ExitCode != 7 {
		t.Fatalf("second result exit code = %d, want 7", results[1].ExitCode)
	}
	if !strings.Contains(results[1].Output, "to-stderr") {
		t.Fatalf("second result should capture stderr: %q", results[1].Output)
	}
	if results[1].Err == nil {
		t.Fatalf("failed verify result should retain underlying error")
	}
}

func TestWriteArtifacts(t *testing.T) {
	dir := t.TempDir()

	if err := WriteSummary(dir, "summary body"); err != nil {
		t.Fatalf("WriteSummary error: %v", err)
	}
	if err := WriteChangedFiles(dir, []string{"a.go", "b.go"}); err != nil {
		t.Fatalf("WriteChangedFiles error: %v", err)
	}
	if err := WriteVerification(dir, []VerifyResult{{
		Command:  "go test ./...",
		ExitCode: 0,
		Output:   "ok\n",
	}}); err != nil {
		t.Fatalf("WriteVerification error: %v", err)
	}

	for name, want := range map[string]string{
		"RUN_SUMMARY.md":    "summary body",
		"CHANGED_FILES.txt": "a.go\nb.go\n",
		"VERIFICATION.txt":  "go test ./...",
	} {
		got, err := os.ReadFile(filepath.Join(dir, ".aiops", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if !strings.Contains(string(got), want) {
			t.Fatalf("%s = %q, want substring %q", name, got, want)
		}
	}
}

func TestRunVerifyCapsLargeOutputAndMarksTruncated(t *testing.T) {
	dir := t.TempDir()
	// Emit ~2 MiB so we exceed VerifyOutputCap (1 MiB) and trigger truncation.
	cfg := workflow.Config{Verify: workflow.VerifyConfig{Commands: []string{
		"yes 0123456789abcdef0123456789abcdef | head -c 2097152",
	}}}

	results, err := RunVerify(context.Background(), dir, cfg)
	if err != nil {
		t.Fatalf("RunVerify error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 verify result, got %d", len(results))
	}
	r := results[0]
	if !r.Truncated {
		t.Fatalf("expected Truncated=true for 2 MiB output, got false")
	}
	if got := len(r.Output); got > VerifyOutputCap {
		t.Fatalf("captured output should be <= cap, got %d > %d", got, VerifyOutputCap)
	}
	if got := len(r.Output); got < VerifyOutputCap-1024 {
		t.Fatalf("captured output unexpectedly small: %d bytes", got)
	}

	// Persist + ensure the artifact mentions the truncation.
	if err := WriteVerification(dir, results); err != nil {
		t.Fatalf("WriteVerification: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, ".aiops", "VERIFICATION.txt"))
	if err != nil {
		t.Fatalf("read VERIFICATION.txt: %v", err)
	}
	wantMarker := fmt.Sprintf("...output truncated at %d bytes", VerifyOutputCap)
	if !strings.Contains(string(body), wantMarker) {
		t.Fatalf("VERIFICATION.txt missing truncation marker %q", wantMarker)
	}
}

func TestCappedBufferDropsBeyondCap(t *testing.T) {
	buf := &cappedBuffer{Cap: 10}
	n, err := buf.Write([]byte("hello "))
	if err != nil || n != 6 {
		t.Fatalf("first write n=%d err=%v", n, err)
	}
	n, err = buf.Write([]byte("world!!"))
	if err != nil || n != 7 {
		t.Fatalf("second write should report full length, n=%d err=%v", n, err)
	}
	if got := buf.String(); got != "hello worl" {
		t.Fatalf("buffered content = %q, want %q", got, "hello worl")
	}
	if !buf.Truncated() {
		t.Fatalf("buffer should report Truncated=true after exceeding cap")
	}
	if buf.Dropped() != 3 {
		t.Fatalf("dropped bytes = %d, want 3", buf.Dropped())
	}
}

func TestAllChangedFilesIncludesArtifactsWhenSnapshotAfterWrite(t *testing.T) {
	// Regression: success-path metadata under-reported pushed contents because
	// AllChangedFiles ran before .aiops artifacts were written. Ordering the
	// snapshot after WriteChangedFiles/WriteSummary should make those files
	// appear in the result.
	dir := t.TempDir()
	mustGit(t, dir, "init", "-q")
	mustGit(t, dir, "config", "user.email", "test@example.com")
	mustGit(t, dir, "config", "user.name", "test")
	mustGit(t, dir, "commit", "--allow-empty", "-q", "-m", "init")

	if err := os.WriteFile(filepath.Join(dir, "src.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("write src.go: %v", err)
	}
	if err := WriteChangedFiles(dir, nil); err != nil {
		t.Fatalf("seed CHANGED_FILES.txt: %v", err)
	}
	if err := WriteSummary(dir, ""); err != nil {
		t.Fatalf("seed RUN_SUMMARY.md: %v", err)
	}

	files, err := AllChangedFiles(context.Background(), dir)
	if err != nil {
		t.Fatalf("AllChangedFiles: %v", err)
	}
	got := strings.Join(files, ",")
	for _, want := range []string{"src.go", ".aiops/CHANGED_FILES.txt", ".aiops/RUN_SUMMARY.md"} {
		if !strings.Contains(got, want) {
			t.Fatalf("AllChangedFiles missing %q in %q", want, got)
		}
	}
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}
