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

func TestWebTodoWorkflowsDocumentReworkConvergence(t *testing.T) {
	for _, tc := range []struct {
		path string
		want []string
	}{
		{
			path: "../examples/maker-WORKFLOW.md",
			want: []string{
				"Rework convergence rules",
				"Do not repost the same PR URL for an unchanged head",
				"source-substring checks or `index.html` markup checks are not enough",
				"`Rework response:` section",
				"`Blocked rework:` comment",
			},
		},
		{
			path: "../examples/reviewer-automerge-WORKFLOW.md",
			want: []string{
				"latest PR head SHA you previously reviewed",
				"Static source-string or markup-presence tests only cover",
				"static assets",
				"one concrete mutation",
				"`Operator attention:`",
			},
		},
		{
			path: "../docs/runbooks/unattended-maker-reviewer-automerge.md",
			want: []string{
				"## Rework convergence",
				"observed #964 failure class",
				"`Rework response:`",
				"`Operator attention:`",
				"without adding a new tracker state or worker phase outside the SPEC boundary",
			},
		},
	} {
		body, err := os.ReadFile(tc.path)
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", tc.path, err)
		}
		for _, want := range tc.want {
			if !strings.Contains(string(body), want) {
				t.Fatalf("%s missing %q", tc.path, want)
			}
		}
	}
}
