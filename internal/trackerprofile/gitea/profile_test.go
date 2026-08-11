package gitea

import (
	"reflect"
	"testing"
)

func TestResolveBuildsGiteaEffectiveProfile(t *testing.T) {
	lookup := lookupValues(map[string]string{
		"GITEA_SECRET":   "gitea-token",
		"GITEA_BASE_URL": "https://gitea-env.example.test/",
	})
	profile, err := Resolve(map[string]any{
		"token":  "${GITEA_SECRET}",
		"repo":   "provider/widgets",
		"future": []any{"opaque", "$UNRESOLVED"},
	}, "fallback", "repo", lookup)
	if err != nil {
		t.Fatalf("Resolve(gitea provider): %v", err)
	}
	if profile.BaseURL != "https://gitea-env.example.test/" || profile.Owner != "provider" || profile.Repo != "widgets" {
		t.Fatalf("gitea endpoint/scope = %+v; want env fallback and provider repo", profile)
	}
	if profile.Token != "gitea-token" || profile.PaginationMaxPages != DefaultPaginationPages {
		t.Fatalf("gitea auth/pages = token %q pages %d; want resolved token and %d", profile.Token, profile.PaginationMaxPages, DefaultPaginationPages)
	}
	wantEnvs := []string{"GITEA_TOKEN", "GITEA_API_TOKEN", "GITEA_SECRET"}
	if got := profile.SecretEnvironmentNames(); !reflect.DeepEqual(got, wantEnvs) {
		t.Fatalf("SecretEnvironmentNames = %#v; want %#v", got, wantEnvs)
	}
	if got := profile.Provider()["future"]; !reflect.DeepEqual(got, []any{"opaque", "$UNRESOLVED"}) {
		t.Fatalf("unknown provider value = %#v; want unchanged", got)
	}
}

func TestResolveGiteaDefaultsAndValidationErrors(t *testing.T) {
	profile, err := Resolve(map[string]any{"token": "token"}, "acme", "widgets", lookupValues(nil))
	if err != nil {
		t.Fatalf("Resolve(default gitea profile): %v", err)
	}
	if profile.BaseURL != DefaultBaseURL || profile.Owner != "acme" || profile.Repo != "widgets" || profile.PaginationMaxPages != DefaultPaginationPages {
		t.Fatalf("gitea defaults = %+v; want documented defaults and repo fallback", profile)
	}
	if got := profile.Provider()["repo"]; got != "acme/widgets" {
		t.Fatalf("effective provider repo = %#v; want fallback scope", got)
	}

	tests := []struct {
		name string
		raw  map[string]any
		want string
	}{
		{"missing secret", nil, "missing_tracker_secret: tracker.provider.token is required"},
		{"invalid repo", map[string]any{"token": "token", "repo": "one-part"}, "tracker.provider.repo must be in owner/name form"},
		{"missing repo", map[string]any{"token": "token"}, "tracker.provider.repo is required when repo.owner or repo.name is missing"},
		{"invalid pagination", map[string]any{"token": "token", "pagination_max_pages": -1}, "tracker.provider.pagination_max_pages must be zero for the adapter default or a positive integer"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			owner, repo := "", ""
			if tc.name == "invalid repo" || tc.name == "invalid pagination" {
				owner, repo = "acme", "widgets"
			}
			_, err := Resolve(tc.raw, owner, repo, lookupValues(nil))
			if err == nil || err.Error() != tc.want {
				t.Fatalf("Resolve(%s) error = %v; want %q", tc.name, err, tc.want)
			}
		})
	}
}

func lookupValues(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}
