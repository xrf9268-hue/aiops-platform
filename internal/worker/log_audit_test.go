package worker_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestNoStdLibLogPrintfMissingStructuredContext is the SPEC §13.1 lint-style
// guard: every `log.Printf` in internal/worker, internal/runner, and
// internal/orchestrator must either route through one of the structured
// helpers (LogIssueEventf / LogTaskIDEventf / LogReconcileEventf) or
// hard-code the canonical `event=` / `issue_id=` / `task_id=` keys inline.
//
// The check is intentionally text-based — it deliberately mis-trips a
// regression like `log.Printf("task %s: ...", t.ID, ...)` because that's
// exactly the prose pattern #240 retired. The audit walks .go files only;
// it ignores _test.go because tests are allowed to dump arbitrary log
// shapes for assertions.
func TestNoStdLibLogPrintfMissingStructuredContext(t *testing.T) {
	repoRoot := repoRootForAuditTest(t)
	roots := []string{
		filepath.Join(repoRoot, "internal", "worker"),
		filepath.Join(repoRoot, "internal", "runner"),
		filepath.Join(repoRoot, "internal", "orchestrator"),
	}
	printfRE := regexp.MustCompile(`\blog\.Printf\(`)
	structuredRE := regexp.MustCompile(`"event=|"task_id=|"issue_id=`)

	var offenders []string
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			// The helpers themselves wrap stdlib log.Printf — that's the
			// only sanctioned use, so skip the file that defines them.
			if filepath.Base(path) == "log.go" {
				return nil
			}
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			for lineIdx, line := range strings.Split(string(data), "\n") {
				if !printfRE.MatchString(line) {
					continue
				}
				if structuredRE.MatchString(line) {
					continue
				}
				offenders = append(offenders, path+":"+itoa(lineIdx+1)+": "+strings.TrimSpace(line))
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("SPEC §13.1: log.Printf without `event=` / `task_id=` / `issue_id=` (use LogIssueEventf / LogTaskIDEventf / LogReconcileEventf instead):\n  %s", strings.Join(offenders, "\n  "))
	}
}

func itoa(n int) string {
	// strconv.Itoa would do, but this keeps the import block lean.
	if n == 0 {
		return "0"
	}
	var b []byte
	if n < 0 {
		b = append(b, '-')
		n = -n
	}
	digits := make([]byte, 0, 10)
	for n > 0 {
		digits = append(digits, byte('0'+n%10))
		n /= 10
	}
	for i := len(digits) - 1; i >= 0; i-- {
		b = append(b, digits[i])
	}
	return string(b)
}

func repoRootForAuditTest(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not find go.mod walking up from %s", dir)
		}
		dir = parent
	}
}
