package workflow

import "testing"

// TestExpandRepoConfigDefaultsBranchToMain pins the repo.default_branch
// defaulting that runs during config expansion.
func TestExpandRepoConfigDefaultsBranchToMain(t *testing.T) {
	repo := RepoConfig{}
	if err := expandRepoConfig("repo.clone_url", &repo); err != nil {
		t.Fatalf("expandRepoConfig(empty) error = %v; want nil", err)
	}
	if repo.DefaultBranch != "main" {
		t.Errorf("expandRepoConfig(empty).DefaultBranch = %q; want %q", repo.DefaultBranch, "main")
	}
}

// TestDefaultAgentMaxContinuationTurnsNilConfigSafe pins the nil-config guard:
// the helper must be a no-op (no panic) when handed a nil config.
func TestDefaultAgentMaxContinuationTurnsNilConfigSafe(t *testing.T) {
	defaultAgentMaxContinuationTurns(nil, false, nil)
}
