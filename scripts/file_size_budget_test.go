package scripts

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const productionGoFileLineBudget = 800

var oversizedProductionGoFileBaseline = map[string]int{
	"internal/doctor/doctor.go":           1294,
	"internal/orchestrator/state.go":      1020,
	"cmd/tui/main.go":                     904,
	"internal/workflow/loader.go":         852,
	"internal/runner/codex_app_server.go": 840,
}

func TestProductionGoFilesStayWithinSizeBudget(t *testing.T) {
	root := gitRepoRoot(t)
	files := trackedGoFiles(t, root)
	seenBaseline := make(map[string]bool, len(oversizedProductionGoFileBaseline))

	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		body := readRepoFile(t, root, file)
		if isGeneratedGoFile(body) {
			continue
		}
		lines := countLines(body)
		baseline, hasBaseline := oversizedProductionGoFileBaseline[file]
		if hasBaseline {
			seenBaseline[file] = true
			if lines <= productionGoFileLineBudget {
				t.Errorf("ProductionGoFileLines(%q) = %d; want baseline entry removed because file is <= %d lines", file, lines, productionGoFileLineBudget)
			}
			if lines != baseline {
				t.Errorf("ProductionGoFileLines(%q) = %d; want recorded baseline %d", file, lines, baseline)
			}
			continue
		}
		if lines > productionGoFileLineBudget {
			t.Errorf("ProductionGoFileLines(%q) = %d; want <= %d or an explicit baseline", file, lines, productionGoFileLineBudget)
		}
	}

	for file := range oversizedProductionGoFileBaseline {
		if !seenBaseline[file] {
			t.Errorf("ProductionGoFileBaseline(%q) = missing from tracked non-generated production Go files; want baseline removed", file)
		}
	}
}

func gitRepoRoot(t *testing.T) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse --show-toplevel: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func trackedGoFiles(t *testing.T, root string) []string {
	t.Helper()
	cmd := exec.Command("git", "-C", root, "ls-files", "*.go")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-files *.go: %v", err)
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

func readRepoFile(t *testing.T, root, file string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, file))
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", file, err)
	}
	return body
}

func isGeneratedGoFile(body []byte) bool {
	header := body
	if len(header) > 2048 {
		header = header[:2048]
	}
	return bytes.Contains(header, []byte("Code generated")) &&
		bytes.Contains(header, []byte("DO NOT EDIT"))
}

func countLines(body []byte) int {
	if len(body) == 0 {
		return 0
	}
	lines := bytes.Count(body, []byte{'\n'})
	if body[len(body)-1] != '\n' {
		lines++
	}
	return lines
}
