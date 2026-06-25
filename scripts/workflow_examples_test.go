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

func TestGitHubLocalWorkflowClaimsPRBeforeLongGates(t *testing.T) {
	body, err := os.ReadFile("../examples/github-local-WORKFLOW.md")
	if err != nil {
		t.Fatalf("ReadFile(github-local-WORKFLOW.md): %v", err)
	}
	text := string(body)
	claimGate := "After the first meaningful commit"
	verifyGate := "Run the configured verify commands"
	reviewGate := "run two independent local reviews"
	finalGate := "Before marking the PR ready or mergeable"
	reuseGate := "check for an existing open PR"

	for _, want := range []string{claimGate, verifyGate, reviewGate, finalGate, reuseGate, "Open or update a draft PR"} {
		if !strings.Contains(text, want) {
			t.Fatalf("github-local-WORKFLOW.md missing %q", want)
		}
	}
	claimIndex := strings.Index(text, claimGate)
	if verifyIndex := strings.Index(text, verifyGate); claimIndex > verifyIndex {
		t.Fatalf("github-local-WORKFLOW.md claim gate index = %d; want before verify gate index %d", claimIndex, verifyIndex)
	}
	if reviewIndex := strings.Index(text, reviewGate); claimIndex > reviewIndex {
		t.Fatalf("github-local-WORKFLOW.md claim gate index = %d; want before review gate index %d", claimIndex, reviewIndex)
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
