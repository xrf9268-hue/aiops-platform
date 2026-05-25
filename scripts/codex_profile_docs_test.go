package scripts

import (
	"os"
	"strings"
	"testing"
)

func TestCodexSafeProfileDocsUseWorkspaceWriteSandbox(t *testing.T) {
	for _, tc := range []struct {
		path string
		want string
	}{
		{
			path: "../examples/WORKFLOW.md",
			want: "safe   (default) - codex exec --sandbox workspace-write --skip-git-repo-check",
		},
		{
			path: "../docs/runbooks/personal-daily-workflow.md",
			want: "safe` (default): builds `codex exec --sandbox workspace-write --skip-git-repo-check",
		},
		{
			path: "../internal/workflow/loader.go",
			want: `"safe" injects --sandbox
// workspace-write + --skip-git-repo-check`,
		},
	} {
		t.Run(tc.path, func(t *testing.T) {
			body, err := os.ReadFile(tc.path)
			if err != nil {
				t.Fatalf("ReadFile(%s): %v", tc.path, err)
			}
			text := string(body)
			if !strings.Contains(text, tc.want) {
				t.Fatalf("%s missing current safe-profile spelling %q", tc.path, tc.want)
			}
			if strings.Contains(text, "safe profile") && strings.Contains(text, "--full-auto") {
				t.Fatalf("%s describes safe profile with deprecated --full-auto", tc.path)
			}
			for _, old := range []string{
				"safe   (default) - codex exec --full-auto",
				"safe` (default): builds `codex exec --full-auto",
				`"safe" injects --full-auto`,
			} {
				if strings.Contains(text, old) {
					t.Fatalf("%s still contains stale safe-profile spelling %q", tc.path, old)
				}
			}
		})
	}
}
