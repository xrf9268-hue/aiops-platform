package github

import (
	"reflect"
	"testing"
)

func TestResolveBuildsGitHubEffectiveProfile(t *testing.T) {
	lookup := githubLookup(map[string]string{
		"GITHUB_SECRET":       "github-token",
		"GITHUB_API_BASE_URL": "https://github-env.example.test/api/v3/",
	})
	profile, err := Resolve(map[string]any{
		"token":                "$GITHUB_SECRET",
		"repo":                 "provider/widgets",
		"pagination_max_pages": 23,
		"future_query":         map[string]any{"label": "$UNRESOLVED"},
	}, "fallback", "repo", lookup)
	if err != nil {
		t.Fatalf("Resolve(github provider): %v", err)
	}
	if profile.APIURL != "https://github-env.example.test/api/v3/" || profile.Owner != "provider" || profile.Repo != "widgets" {
		t.Fatalf("github endpoint/scope = %+v; want env fallback and provider repo", profile)
	}
	if profile.Token != "github-token" || profile.PaginationMaxPages != 23 {
		t.Fatalf("github auth/pages = token %q pages %d; want resolved token and 23", profile.Token, profile.PaginationMaxPages)
	}
	wantEnvs := []string{"GITHUB_TOKEN", "GH_TOKEN", "GITHUB_PAT", "GITHUB_SECRET"}
	if got := profile.SecretEnvironmentNames(); !reflect.DeepEqual(got, wantEnvs) {
		t.Fatalf("SecretEnvironmentNames = %#v; want %#v", got, wantEnvs)
	}
	future := profile.Provider()["future_query"].(map[string]any)
	if got := future["label"]; got != "$UNRESOLVED" {
		t.Fatalf("unknown provider value = %#v; want unchanged", got)
	}
}

func TestResolveGitHubDefaultsAndValidationErrors(t *testing.T) {
	profile, err := Resolve(map[string]any{"token": "token"}, "acme", "widgets", githubLookup(nil))
	if err != nil {
		t.Fatalf("Resolve(default github profile): %v", err)
	}
	if profile.APIURL != DefaultAPIURL || profile.Owner != "acme" || profile.Repo != "widgets" || profile.PaginationMaxPages != DefaultPaginationPages {
		t.Fatalf("github defaults = %+v; want documented defaults and repo fallback", profile)
	}
	if got := profile.Provider()["api_url"]; got != DefaultAPIURL {
		t.Fatalf("effective provider api_url = %#v; want default", got)
	}

	tests := []struct {
		name string
		raw  map[string]any
		want string
	}{
		{"missing secret", nil, "missing_tracker_secret: tracker.provider.token is required"},
		{"invalid api url", map[string]any{"token": "token", "api_url": 7}, "tracker.provider.api_url must be a string"},
		{"invalid repo", map[string]any{"token": "token", "repo": "one-part"}, "tracker.provider.repo must be in owner/name form"},
		{"missing repo", map[string]any{"token": "token"}, "tracker.provider.repo is required when repo.owner or repo.name is missing"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			owner, repo := "", ""
			if tc.name == "invalid api url" || tc.name == "invalid repo" {
				owner, repo = "acme", "widgets"
			}
			_, err := Resolve(tc.raw, owner, repo, githubLookup(nil))
			if err == nil || err.Error() != tc.want {
				t.Fatalf("Resolve(%s) error = %v; want %q", tc.name, err, tc.want)
			}
		})
	}
}

func githubLookup(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}
