package policy

import (
	"strings"
	"testing"
)

func TestMatch(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		want    bool
	}{
		// exact
		{"README.md", "README.md", true},
		{"README.md", "docs/README.md", false},

		// single star: bounded by "/"
		{"*.go", "main.go", true},
		{"*.go", "cmd/main.go", false},
		{"cmd/*.go", "cmd/main.go", true},
		{"cmd/*.go", "cmd/sub/main.go", false},
		{"foo*bar", "foozzbar", true},
		{"foo*bar", "foo/bar", false},

		// question mark
		{"?.md", "a.md", true},
		{"?.md", "ab.md", false},
		{"?.md", "/x.md", false},

		// double star: spans separators
		{"**", "anything/at/all.go", true},
		{"infra/**", "infra/", true},
		{"infra/**", "infra/db/migrate.sql", true},
		{"infra/**", "infrastructure/x.go", false},
		{"deploy/**", "deploy/k8s/values.yaml", true},
		{"db/migrations/**", "db/migrations/0001_init.sql", true},
		{"**/secrets.yaml", "config/prod/secrets.yaml", true},
		{"**/secrets.yaml", "secrets.yaml", true},
		{"a/**/b.go", "a/b.go", true},
		{"a/**/b.go", "a/x/b.go", true},
		{"a/**/b.go", "a/x/y/b.go", true},
		{"a/**/b.go", "a/b.go.bak", false},

		// trailing slash convenience: "foo/" == "foo/**"
		{"vendor/", "vendor/lib/x.go", true},
		{"vendor/", "vendor/", true},
		{"vendor/", "vendor", false},

		// edge cases
		{"", "anything", false},
		{"   ", "anything", false},
	}
	for _, tc := range cases {
		got := Match(tc.pattern, tc.path)
		if got != tc.want {
			t.Errorf("Match(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
		}
	}
}

func TestEvaluateDenyPaths(t *testing.T) {
	cfg := Config{
		DenyPaths: []string{"infra/**", "deploy/**", "**/secrets.yaml", "db/migrations/**"},
	}
	d := Diffstat{Files: []string{
		"cmd/worker/main.go",
		"infra/terraform/main.tf",
		"src/app/secrets.yaml",
		"db/migrations/0002_add.sql",
		"docs/readme.md",
	}}
	vs := Evaluate(d, cfg)
	if len(vs) != 3 {
		t.Fatalf("expected 3 deny violations, got %d: %v", len(vs), vs)
	}
	for _, v := range vs {
		if v.Kind != KindDenyPath {
			t.Errorf("expected KindDenyPath, got %s for %s", v.Kind, v.Path)
		}
		if v.Pattern == "" || v.Path == "" {
			t.Errorf("violation missing path/pattern: %+v", v)
		}
	}
}

func TestEvaluateAllowPathsRequiresMatch(t *testing.T) {
	cfg := Config{AllowPaths: []string{"src/**", "docs/**"}}
	d := Diffstat{Files: []string{"src/a.go", "docs/x.md", "Makefile"}}
	vs := Evaluate(d, cfg)
	if len(vs) != 1 {
		t.Fatalf("expected 1 not_allowed violation, got %d: %v", len(vs), vs)
	}
	if vs[0].Kind != KindNotAllowed || vs[0].Path != "Makefile" {
		t.Errorf("unexpected violation: %+v", vs[0])
	}
}

func TestEvaluateDenyTrumpsAllow(t *testing.T) {
	cfg := Config{
		AllowPaths: []string{"**"},
		DenyPaths:  []string{"infra/**"},
	}
	d := Diffstat{Files: []string{"infra/main.tf", "src/a.go"}}
	vs := Evaluate(d, cfg)
	if len(vs) != 1 || vs[0].Kind != KindDenyPath {
		t.Fatalf("expected single deny violation, got %v", vs)
	}
}

func TestEvaluateMaxChangedFiles(t *testing.T) {
	cfg := Config{MaxChangedFiles: 2}
	d := Diffstat{Files: []string{"a.go", "b.go", "c.go"}}
	vs := Evaluate(d, cfg)
	if len(vs) != 1 || vs[0].Kind != KindMaxChangedFiles {
		t.Fatalf("expected max_changed_files violation, got %v", vs)
	}
	if vs[0].Limit != 2 || vs[0].Actual != 3 {
		t.Errorf("violation limits wrong: %+v", vs[0])
	}
}

func TestEvaluateMaxChangedLines(t *testing.T) {
	cfg := Config{MaxChangedLines: 100}
	d := Diffstat{Files: []string{"a.go"}, Lines: 250}
	vs := Evaluate(d, cfg)
	if len(vs) != 1 || vs[0].Kind != KindMaxChangedLines {
		t.Fatalf("expected max_changed_lines violation, got %v", vs)
	}
	if vs[0].Limit != 100 || vs[0].Actual != 250 {
		t.Errorf("violation limits wrong: %+v", vs[0])
	}
}

func TestEvaluateZeroLimitsDisabled(t *testing.T) {
	cfg := Config{} // no limits, no deny, no allow
	d := Diffstat{Files: []string{"a.go", "b.go"}, Lines: 10000}
	if vs := Evaluate(d, cfg); len(vs) != 0 {
		t.Fatalf("expected no violations, got %v", vs)
	}
}

func TestEvaluateMultipleViolationsAccumulate(t *testing.T) {
	cfg := Config{
		DenyPaths:       []string{"secrets/**"},
		MaxChangedFiles: 1,
		MaxChangedLines: 5,
	}
	d := Diffstat{Files: []string{"secrets/key.pem", "src/a.go"}, Lines: 50}
	vs := Evaluate(d, cfg)
	if len(vs) != 3 {
		t.Fatalf("expected 3 violations, got %d: %v", len(vs), vs)
	}
	summary := Summarize(vs)
	for _, want := range []string{"deny pattern", "exceeds max"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary %q missing %q", summary, want)
		}
	}
}

func TestEvaluateSkipsBlankFiles(t *testing.T) {
	cfg := Config{DenyPaths: []string{"infra/**"}}
	d := Diffstat{Files: []string{"", "  ", "src/a.go"}}
	if vs := Evaluate(d, cfg); len(vs) != 0 {
		t.Fatalf("expected no violations for blank entries, got %v", vs)
	}
}
