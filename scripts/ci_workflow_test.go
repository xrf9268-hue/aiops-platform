package scripts

import (
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
		"golangci/golangci-lint-action@db582008a42febd596419635a5abc9d9815daa9c",
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
