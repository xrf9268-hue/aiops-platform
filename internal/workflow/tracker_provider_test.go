package workflow

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestDefaultConfigTrackerProviderIsEmptyObject(t *testing.T) {
	raw, err := json.Marshal(DefaultConfig().Tracker)
	if err != nil {
		t.Fatalf("Marshal(DefaultConfig().Tracker): %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal(default tracker JSON): %v", err)
	}
	provider, ok := got["provider"].(map[string]any)
	if !ok || len(provider) != 0 {
		t.Fatalf("default tracker.provider = %#v; want empty object", got["provider"])
	}
}

func TestLoadLinearProviderResolvesDocumentedKeysAndPreservesUnknown(t *testing.T) {
	t.Setenv("AIOPS_TEST_LINEAR_PROVIDER_KEY", "linear-provider-secret")
	t.Setenv("AIOPS_TEST_UNKNOWN_PROVIDER_VALUE", "must-stay-unresolved")
	path := writeTempWorkflow(t, `---
repo:
  clone_url: git@example.com:o/r.git
tracker:
  kind: linear
  provider:
    endpoint: https://linear.example.test/graphql
    api_key: $AIOPS_TEST_LINEAR_PROVIDER_KEY
    team_key: ENG
    project_slug: platform
    pagination_max_pages: 17
    future:
      env_like: $AIOPS_TEST_UNKNOWN_PROVIDER_VALUE
      flags: [one, two]
---
Prompt body
`)

	wf, err := Load(path)
	if err != nil {
		t.Fatalf("Load(provider workflow): %v", err)
	}
	provider := trackerProviderJSON(t, wf.Config.Tracker)
	wantUnknown := map[string]any{
		"env_like": "$AIOPS_TEST_UNKNOWN_PROVIDER_VALUE",
		"flags":    []any{"one", "two"},
	}
	if !reflect.DeepEqual(provider["future"], wantUnknown) {
		t.Fatalf("tracker.provider.future = %#v; want %#v", provider["future"], wantUnknown)
	}
	if got := provider["api_key"]; got != "linear-provider-secret" {
		t.Fatalf("tracker.provider.api_key = %#v; want resolved secret", got)
	}
	if got := provider["pagination_max_pages"]; got != float64(17) {
		t.Fatalf("tracker.provider.pagination_max_pages = %#v; want 17", got)
	}
	for _, removed := range []string{"api_key", "endpoint", "team_key", "project_slug", "pagination_max_pages"} {
		raw, err := json.Marshal(wf.Config.Tracker)
		if err != nil {
			t.Fatalf("Marshal(Tracker): %v", err)
		}
		var trackerMap map[string]any
		if err := json.Unmarshal(raw, &trackerMap); err != nil {
			t.Fatalf("Unmarshal(Tracker): %v", err)
		}
		if _, present := trackerMap[removed]; present {
			t.Fatalf("tracker JSON still contains removed flat key %q: %s", removed, raw)
		}
	}
	secretNames, ok := any(wf.Config.Tracker).(interface{ SecretEnvironmentNames() []string })
	if !ok {
		t.Fatal("TrackerConfig does not expose adapter-declared SecretEnvironmentNames")
	}
	if got := secretNames.SecretEnvironmentNames(); !reflect.DeepEqual(got, []string{"LINEAR_API_KEY", "LINEAR_TOKEN", "AIOPS_TEST_LINEAR_PROVIDER_KEY"}) {
		t.Fatalf("SecretEnvironmentNames = %#v; want declared aliases plus exact reference", got)
	}
}

func TestLoadRejectsRemovedFlatTrackerProviderFields(t *testing.T) {
	for _, key := range []string{"api_key", "endpoint", "team_key", "project_slug", "pagination_max_pages"} {
		t.Run(key, func(t *testing.T) {
			path := writeTempWorkflow(t, `---
repo:
  clone_url: git@example.com:o/r.git
tracker:
  kind: linear
  provider:
    api_key: token
    project_slug: platform
  `+key+`: legacy-value
---
Prompt body
`)
			_, err := Load(path)
			if err == nil {
				t.Fatalf("Load(flat tracker.%s) error = nil; want removed-field rejection", key)
			}
			for _, want := range []string{"tracker." + key, "tracker.provider"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("Load(flat tracker.%s) error = %q; want substring %q", key, err, want)
				}
			}
		})
	}
}

func TestLoadRejectsEmptyProviderSecretWithoutLeakingValue(t *testing.T) {
	t.Setenv("AIOPS_TEST_EMPTY_PROVIDER_SECRET", "")
	path := writeTempWorkflow(t, `---
repo:
  clone_url: git@example.com:o/r.git
tracker:
  kind: linear
  provider:
    api_key: $AIOPS_TEST_EMPTY_PROVIDER_SECRET
    project_slug: platform
---
Prompt body
`)

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load(empty provider secret) error = nil; want validation error")
	}
	for _, want := range []string{"missing_tracker_secret", "tracker.provider.api_key", "$AIOPS_TEST_EMPTY_PROVIDER_SECRET"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Load(empty provider secret) error = %q; want substring %q", err, want)
		}
	}
}

func trackerProviderJSON(t *testing.T, tracker TrackerConfig) map[string]any {
	t.Helper()
	raw, err := json.Marshal(tracker)
	if err != nil {
		t.Fatalf("Marshal(Tracker): %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal(Tracker): %v", err)
	}
	provider, ok := got["provider"].(map[string]any)
	if !ok {
		t.Fatalf("tracker.provider = %#v; want object; tracker JSON=%s", got["provider"], raw)
	}
	return provider
}
