package envpolicy

import (
	"strings"
	"testing"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

func TestBuildSanitizedEnvDeniesTrackerCredentialsAndDeduplicates(t *testing.T) {
	cfg := workflow.Config{
		Tracker: workflow.TrackerConfig{APIKey: "configured-tracker-secret"},
	}
	lookupValues := map[string]string{
		"PATH":                       "/worker/path",
		"HOME":                       "/home/agent",
		"AIOPS_ALLOWED":              "allowed-value",
		"AIOPS_DUPLICATE":            "duplicate-value",
		"GITHUB_TOKEN":               "github-secret",
		"AIOPS_CONFIGURED_TRACKER":   "configured-tracker-secret",
		"AIOPS_UNRELATED_TRACKERISH": "not-the-configured-secret",
	}

	env := BuildSanitizedEnv(
		[]string{"PATH", "HOME"},
		[]string{
			"AIOPS_ALLOWED",
			"GITHUB_TOKEN",
			"AIOPS_CONFIGURED_TRACKER",
			"AIOPS_UNRELATED_TRACKERISH",
			"AIOPS_DUPLICATE",
			"AIOPS_DUPLICATE",
			"BAD=NAME",
			"",
		},
		cfg,
		func(name string) (string, bool) {
			value, ok := lookupValues[name]
			return value, ok
		},
		func() string { return "/login/path" },
	)

	values, counts := envPolicyEnvByName(env)
	for _, tc := range []struct {
		name      string
		wantValue string
	}{
		{name: "PATH", wantValue: "/login/path"},
		{name: "HOME", wantValue: "/home/agent"},
		{name: "AIOPS_ALLOWED", wantValue: "allowed-value"},
		{name: "AIOPS_DUPLICATE", wantValue: "duplicate-value"},
		{name: "AIOPS_UNRELATED_TRACKERISH", wantValue: "not-the-configured-secret"},
	} {
		if values[tc.name] != tc.wantValue {
			t.Fatalf("%s = %q, want %q in env %#v", tc.name, values[tc.name], tc.wantValue, env)
		}
		if counts[tc.name] != 1 {
			t.Fatalf("%s appeared %d times, want 1 in env %#v", tc.name, counts[tc.name], env)
		}
	}
	for _, denied := range []string{"GITHUB_TOKEN", "AIOPS_CONFIGURED_TRACKER", "BAD"} {
		if counts[denied] != 0 {
			t.Fatalf("denied env %s appeared %d times in env %#v", denied, counts[denied], env)
		}
	}
}

func TestBuildSanitizedEnvFallsBackToLookupPATH(t *testing.T) {
	env := BuildSanitizedEnv(
		[]string{"PATH"},
		nil,
		workflow.Config{},
		func(name string) (string, bool) {
			if name == "PATH" {
				return "/worker/path", true
			}
			return "", false
		},
		func() string { return "" },
	)

	values, counts := envPolicyEnvByName(env)
	if values["PATH"] != "/worker/path" {
		t.Fatalf("PATH = %q, want worker PATH fallback in env %#v", values["PATH"], env)
	}
	if counts["PATH"] != 1 {
		t.Fatalf("PATH appeared %d times, want 1 in env %#v", counts["PATH"], env)
	}
}

func envPolicyEnvByName(env []string) (map[string]string, map[string]int) {
	values := make(map[string]string, len(env))
	counts := make(map[string]int, len(env))
	for _, pair := range env {
		name, value, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		values[name] = value
		counts[name]++
	}
	return values, counts
}
