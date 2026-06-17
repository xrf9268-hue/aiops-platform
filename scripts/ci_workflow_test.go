package scripts

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestCITrivyImageScanBlocksHighAndCriticalVulnerabilities(t *testing.T) {
	body, err := os.ReadFile("../.github/workflows/ci.yml")
	if err != nil {
		t.Fatalf("ReadFile(ci.yml): %v", err)
	}

	text := string(body)
	if strings.Contains(text, "Report-only for now") {
		t.Fatal("CI Trivy scan still documents report-only mode")
	}

	step := regexp.MustCompile(`(?s)- name: Scan image for vulnerabilities \(Trivy\).*?(?:\n\n|$)`).FindString(text)
	if step == "" {
		t.Fatal("CI workflow missing Trivy image scan step")
	}
	for _, want := range []string{
		"severity: CRITICAL,HIGH",
		"ignore-unfixed: true",
		"vuln-type: os,library",
		`exit-code: "1"`,
	} {
		if !strings.Contains(step, want) {
			t.Fatalf("Trivy image scan step missing %q:\n%s", want, step)
		}
	}
}

func TestCIGolangCILintHasBlockingCorrectnessGate(t *testing.T) {
	body, err := os.ReadFile("../.github/workflows/ci.yml")
	if err != nil {
		t.Fatalf("ReadFile(ci.yml): %v", err)
	}

	text := string(body)
	blockingStep := regexp.MustCompile(`(?s)- name: Run golangci-lint blocking gate.*?(?:\n\n|$)`).FindString(text)
	if blockingStep == "" {
		t.Fatal("CI workflow missing blocking golangci-lint gate")
	}
	for _, want := range []string{
		"golangci/golangci-lint-action@82606bf257cbaff209d206a39f5134f0cfbfd2ee",
		"version: v2.12.2",
		// funlen+gocognit are part of the single blocking gate (#504); the
		// baseline is grandfathered in-line via //nolint, not report-only.
		"args: --config=.golangci.yml --enable-only=contextcheck,errcheck,errorlint,funlen,gocognit,gocritic,govet,ineffassign,revive,staticcheck,unparam,unused",
	} {
		if !strings.Contains(blockingStep, want) {
			t.Fatalf("blocking golangci-lint step missing %q:\n%s", want, blockingStep)
		}
	}
	if strings.Contains(blockingStep, "--issues-exit-code=0") {
		t.Fatalf("blocking golangci-lint step must not be report-only:\n%s", blockingStep)
	}

	// The report-only / only-new-issues baseline step must be gone: funlen and
	// gocognit are now blocking (baseline grandfathered via //nolint, #521).
	if strings.Contains(text, "--issues-exit-code=0") {
		t.Errorf("ci.yml still has a report-only golangci-lint step (--issues-exit-code=0); funlen/gocognit must block")
	}
	if strings.Contains(text, "only-new-issues") {
		t.Errorf("ci.yml still references only-new-issues; the gate is now a full blocking gate with an in-line //nolint baseline")
	}
}

func TestCIEnforcesProductionGoFileSizeBudgetUncached(t *testing.T) {
	body, err := os.ReadFile("../.github/workflows/ci.yml")
	if err != nil {
		t.Fatalf("ReadFile(ci.yml): %v", err)
	}

	text := string(body)
	step := regexp.MustCompile(`(?s)- name: Check production Go file size budget.*?(?:\n\n|$)`).FindString(text)
	if step == "" {
		t.Fatal("CI workflow missing production Go file size budget step")
	}
	want := "go test -run '^TestProductionGoFilesStayWithinSizeBudget$' -count=1 ./scripts"
	if !strings.Contains(step, want) {
		t.Fatalf("production Go file size budget step missing %q:\n%s", want, step)
	}
	sizeStepIndex := strings.Index(text, "- name: Check production Go file size budget")
	runTestsIndex := strings.Index(text, "- name: Run tests")
	if sizeStepIndex == -1 || runTestsIndex == -1 || sizeStepIndex > runTestsIndex {
		t.Fatalf("production Go file size budget step index = %d; want before Run tests index %d", sizeStepIndex, runTestsIndex)
	}
}

func TestFileSizeBudgetDocsDescribeRepoPolicy(t *testing.T) {
	docs := []string{
		"../AGENTS.md",
		"../docs/runbooks/ci.md",
	}
	for _, doc := range docs {
		body, err := os.ReadFile(doc)
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", doc, err)
		}
		text := strings.ToLower(strings.Join(strings.Fields(string(body)), " "))
		for _, want := range []string{
			"repo-specific maintainability budget",
			"not an official Go file-length limit",
			"raw physical lines",
			"non-test, non-generated Go files",
			"baseline/ratchet",
			"Decompose oversized files by responsibility",
		} {
			if !strings.Contains(text, strings.ToLower(want)) {
				t.Fatalf("%s missing file-size budget policy text %q", doc, want)
			}
		}
	}
}

func TestCIDashboardBuildFeedsGoEmbedWithoutCommittedDist(t *testing.T) {
	body, err := os.ReadFile("../.github/workflows/ci.yml")
	if err != nil {
		t.Fatalf("ReadFile(ci.yml): %v", err)
	}

	text := string(body)
	for _, want := range []string{
		"- name: Install dashboard dependencies",
		"- name: Test dashboard",
		"- name: Build dashboard",
		"- name: Verify dashboard dist is generated for embed",
		"go test -race -covermode=atomic ./...",
		"go build -trimpath -ldflags=\"-s -w\" -o dist/worker ./cmd/worker",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("CI workflow missing %q", want)
		}
	}
	if strings.Contains(text, "Verify dashboard dist is committed") {
		t.Fatal("CI workflow still requires committing dashboard dist")
	}
	if strings.Contains(text, "git diff --exit-code -- cmd/worker/dashboard/dist") {
		t.Fatal("CI workflow still diffs generated dashboard dist against git")
	}
}

func TestReleasePleaseAlwaysUpdatesPendingReleasePRs(t *testing.T) {
	configBody, err := os.ReadFile("../release-please-config.json")
	if err != nil {
		t.Fatalf("ReadFile(release-please-config.json): %v", err)
	}
	var config map[string]any
	if err := json.Unmarshal(configBody, &config); err != nil {
		t.Fatalf("json.Unmarshal(release-please-config.json): %v", err)
	}
	if got, ok := config["always-update"].(bool); !ok || !got {
		t.Fatalf(`release-please-config.json "always-update" = %v (%T); want true`, config["always-update"], config["always-update"])
	}

	runbookBody, err := os.ReadFile("../docs/runbooks/ci.md")
	if err != nil {
		t.Fatalf("ReadFile(ci.md): %v", err)
	}
	runbook := strings.Join(strings.Fields(string(runbookBody)), " ")
	for _, want := range []string{
		`"always-update": true`,
		"non-releasable",
		"strict required status checks",
	} {
		if !strings.Contains(runbook, want) {
			t.Fatalf("docs/runbooks/ci.md missing %q", want)
		}
	}
}
