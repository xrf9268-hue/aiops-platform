package scripts

import (
	"os"
	"strings"
	"testing"
)

func TestDockerDogfoodGitHubIssueIntakeAvoidsProjectCardsQuery(t *testing.T) {
	body, err := os.ReadFile("../docs/runbooks/first-run-docker-linear-codex.md")
	if err != nil {
		t.Fatalf("ReadFile(first-run docker runbook) = %v; want nil", err)
	}
	text := string(body)
	section := dogfoodGitHubIssueSmokeSection(t, text)

	for _, forbidden := range []string{
		"gh issue view \"$gh_issue\" \\\n  --repo xrf9268-hue/aiops-platform \\\n  --comments",
		"--json number,title,state,labels,body,url,comments",
		"--json closed,closedAt,closedBy",
		"closedBy,number,state,url",
	} {
		if strings.Contains(section, forbidden) {
			t.Fatalf("GitHub issue intake section contains forbidden %q:\n%s", forbidden, section)
		}
	}

	for _, want := range []string{
		"--json number,title,state,labels,body,url",
		"gh api --paginate",
		"issues/${gh_issue}/comments?per_page=100",
		"repository.issue.projectCards",
		"not an authentication failure",
	} {
		if !strings.Contains(section, want) {
			t.Fatalf("GitHub issue intake section missing %q:\n%s", want, section)
		}
	}
}

func dogfoodGitHubIssueSmokeSection(t *testing.T, text string) string {
	t.Helper()
	start := strings.Index(text, "## 6. Run a GitHub issue-to-PR smoke")
	if start < 0 {
		t.Fatal("runbook missing GitHub issue-to-PR smoke section")
	}
	end := strings.Index(text[start+1:], "\n## ")
	if end < 0 {
		return text[start:]
	}
	return text[start : start+1+end]
}
