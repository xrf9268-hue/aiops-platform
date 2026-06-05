package scripts

import (
	"os"
	"strings"
	"testing"
)

func TestGitHubLocalWorkflowRunsUncachedFileSizeGate(t *testing.T) {
	body, err := os.ReadFile("../examples/github-local-WORKFLOW.md")
	if err != nil {
		t.Fatalf("ReadFile(github-local-WORKFLOW.md): %v", err)
	}
	text := string(body)
	fileSizeGate := "go test -run '^TestProductionGoFilesStayWithinSizeBudget$' -count=1 ./scripts"
	raceGate := "go test -race -covermode=atomic ./..."
	if !strings.Contains(text, fileSizeGate) {
		t.Fatalf("github-local-WORKFLOW.md missing %q", fileSizeGate)
	}
	if fileSizeIndex, raceIndex := strings.Index(text, fileSizeGate), strings.Index(text, raceGate); fileSizeIndex == -1 || raceIndex == -1 || fileSizeIndex > raceIndex {
		t.Fatalf("github-local-WORKFLOW.md file-size gate index = %d; want before race gate index %d", fileSizeIndex, raceIndex)
	}
}
