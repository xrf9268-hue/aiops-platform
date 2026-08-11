package gitea

import (
	"testing"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

func TestBaseURLFromTrackerConfigUsesEndpointBeforeFallback(t *testing.T) {
	got := BaseURLFromTrackerConfig(workflow.TrackerConfig{Provider: map[string]any{"base_url": " https://gitea-endpoint.example.test/ "}}, "https://gitea-env.example.test/")
	if got != "https://gitea-endpoint.example.test" {
		t.Fatalf("BaseURLFromTrackerConfig = %q, want tracker.provider.base_url without trailing slash", got)
	}
}

func TestBaseURLFromTrackerConfigIgnoresProjectSlugAsFallback(t *testing.T) {
	got := BaseURLFromTrackerConfig(workflow.TrackerConfig{Provider: map[string]any{"future_scope": "unchanged"}}, "https://gitea-env.example.test/")
	if got != "https://gitea-env.example.test" {
		t.Fatalf("BaseURLFromTrackerConfig = %q, want fallback when tracker.provider.base_url is absent", got)
	}
}

func TestBaseURLFromTrackerConfigUsesFallbackWhenTrackerURLIsEmpty(t *testing.T) {
	got := BaseURLFromTrackerConfig(workflow.TrackerConfig{}, " https://gitea-env.example.test/ ")
	if got != "https://gitea-env.example.test" {
		t.Fatalf("BaseURLFromTrackerConfig = %q, want fallback without trailing slash", got)
	}
}
