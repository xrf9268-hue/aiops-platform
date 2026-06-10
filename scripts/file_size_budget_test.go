package scripts

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const productionGoFileLineBudget = 800
const gitProbeTimeout = 5 * time.Second

var oversizedProductionGoFileBaseline = map[string]int{
	"internal/doctor/doctor.go": 1294,
	// state.go's baseline rose from 1020 under #667, which adds the
	// operator-terminal-stop latch cap. Per #667 (and the #661 burn-down note it
	// cites), this cap must land *before* #661 decomposes state.go by
	// responsibility, which then drops it well under budget and removes this baseline.
	// #683 extracts the StateView projection types into views.go (a first
	// responsibility split), dropping the baseline from 1062 to 1002.
	"internal/orchestrator/state.go": 1002,
	"cmd/tui/main.go":                904,
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
			} else if lines != baseline {
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
	out := gitOutput(t, "rev-parse", "--show-toplevel")
	return strings.TrimSpace(string(out))
}

func trackedGoFiles(t *testing.T, root string) []string {
	t.Helper()
	out := gitOutput(t, "-C", root, "ls-files", "*.go")
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

func gitOutput(t *testing.T, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), gitProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", args...).Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return out
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
