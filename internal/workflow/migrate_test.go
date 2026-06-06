package workflow

import "testing"

// Characterization tests pinning migration/defaulting branches that the
// existing suite exercised only indirectly. They were added before the #661
// decomposition of loader.go (migration helpers and the continuation-turn
// default → migrate.go; config expansion/defaulting → expand.go) so the
// behavior-preserving move is provably safe at the unit level, not just via
// integration through Load.

// TestMigrateGiteaTrackerProjectSlugSkipsNonGitea pins the early return for
// non-Gitea trackers: project_slug must stay untouched and must not be
// promoted to endpoint. Without the kind guard, a Linear config that still
// carried the legacy project_slug would have it silently migrated and cleared.
func TestMigrateGiteaTrackerProjectSlugSkipsNonGitea(t *testing.T) {
	front := []byte("tracker:\n  kind: linear\n  project_slug: acme/widgets\n")
	cfg := Config{}
	cfg.Tracker.Kind = "linear"
	cfg.Tracker.ProjectSlug = "acme/widgets"

	migrateGiteaTrackerProjectSlug(front, &cfg)

	if cfg.Tracker.ProjectSlug != "acme/widgets" {
		t.Errorf("migrateGiteaTrackerProjectSlug(kind=linear).ProjectSlug = %q; want %q (untouched)", cfg.Tracker.ProjectSlug, "acme/widgets")
	}
	if cfg.Tracker.Endpoint != "" {
		t.Errorf("migrateGiteaTrackerProjectSlug(kind=linear).Endpoint = %q; want \"\" (no migration for non-gitea)", cfg.Tracker.Endpoint)
	}
}

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
