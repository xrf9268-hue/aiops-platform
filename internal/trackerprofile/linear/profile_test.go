package linear

import (
	"reflect"
	"strings"
	"testing"
)

func TestResolveBuildsLinearEffectiveProfile(t *testing.T) {
	lookup := mapLookup(map[string]string{"LINEAR_SECRET": "linear-token"})
	profile, err := Resolve(map[string]any{
		"api_key":      "$LINEAR_SECRET",
		"project_slug": "delivery",
		"team_key":     "ENG",
		"future":       map[string]any{"value": "$LEAVE_OPAQUE"},
	}, lookup)
	if err != nil {
		t.Fatalf("Resolve(linear provider): %v", err)
	}
	if profile.Endpoint != DefaultEndpoint || profile.PaginationMaxPages != DefaultPaginationPages {
		t.Fatalf("linear defaults = endpoint %q pages %d; want %q and %d", profile.Endpoint, profile.PaginationMaxPages, DefaultEndpoint, DefaultPaginationPages)
	}
	if got := profile.Provider()["endpoint"]; got != DefaultEndpoint {
		t.Fatalf("effective provider endpoint = %#v; want %q", got, DefaultEndpoint)
	}
	if profile.APIKey != "linear-token" || profile.ProjectSlug != "delivery" || profile.TeamKey != "ENG" {
		t.Fatalf("linear profile = %+v; want resolved auth and scope", profile)
	}
	wantEnvs := []string{"LINEAR_API_KEY", "LINEAR_TOKEN", "LINEAR_SECRET"}
	if got := profile.SecretEnvironmentNames(); !reflect.DeepEqual(got, wantEnvs) {
		t.Fatalf("SecretEnvironmentNames = %#v; want %#v", got, wantEnvs)
	}
	future := profile.Provider()["future"].(map[string]any)
	if got := future["value"]; got != "$LEAVE_OPAQUE" {
		t.Fatalf("unknown provider value = %#v; want opaque value unchanged", got)
	}
}

func TestResolveLinearValidationErrorsAreStable(t *testing.T) {
	tests := []struct {
		name string
		raw  map[string]any
		want string
	}{
		{"missing secret", map[string]any{"project_slug": "delivery"}, "missing_tracker_secret: tracker.provider.api_key is required"},
		{"missing scope", map[string]any{"api_key": "token"}, "tracker.provider.project_slug is required when tracker.kind is linear"},
		{"invalid endpoint", map[string]any{"api_key": "token", "project_slug": "delivery", "endpoint": 7}, "tracker.provider.endpoint must be a string"},
		{"invalid pagination", map[string]any{"api_key": "token", "project_slug": "delivery", "pagination_max_pages": -1}, "tracker.provider.pagination_max_pages must be zero for the adapter default or a positive integer"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Resolve(tc.raw, mapLookup(nil))
			if err == nil || err.Error() != tc.want {
				t.Fatalf("Resolve(%s) error = %v; want %q", tc.name, err, tc.want)
			}
		})
	}
}

func TestResolveLinearEmptyReferencedSecretDoesNotLeak(t *testing.T) {
	_, err := Resolve(map[string]any{"api_key": "$EMPTY_SECRET", "project_slug": "delivery"}, mapLookup(map[string]string{"EMPTY_SECRET": ""}))
	if err == nil || !strings.Contains(err.Error(), "missing_tracker_secret") || strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("Resolve(empty secret) error = %v; want classified non-leaking error", err)
	}
}

func mapLookup(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}
