package workspace

import (
	"context"
	"errors"
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
