package task

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTaskJSONUsesSnakeCaseFields(t *testing.T) {
	input := []byte(`{
		"id": "tsk_1",
		"status": "queued",
		"source_type": "manual",
		"source_event_id": "manual-1",
		"repo_owner": "octo",
		"repo_name": "demo",
		"clone_url": "git@example.com:octo/demo.git",
		"base_branch": "main",
		"work_branch": "ai/tsk_1",
		"title": "Manual task",
		"description": "Smoke test",
		"actor": "tester",
		"model": "mock",
		"priority": 50,
		"attempts": 1,
		"max_attempts": 3
	}`)

	var got Task
	if err := json.Unmarshal(input, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	if got.RepoOwner != "octo" {
		t.Fatalf("RepoOwner = %q, want octo", got.RepoOwner)
	}
	if got.CloneURL != "git@example.com:octo/demo.git" {
		t.Fatalf("CloneURL = %q, want clone URL", got.CloneURL)
	}
	if got.BaseBranch != "main" {
		t.Fatalf("BaseBranch = %q, want main", got.BaseBranch)
	}

	out, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	for _, field := range []string{`"repo_owner"`, `"clone_url"`, `"base_branch"`, `"max_attempts"`} {
		if !containsJSONField(out, field) {
			t.Fatalf("Marshal() output %s missing %s", out, field)
		}
	}
}

func containsJSONField(out []byte, field string) bool {
	return json.Valid(out) && strings.Contains(string(out), field)
}
