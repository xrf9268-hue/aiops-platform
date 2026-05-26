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
	blockingStep := regexp.MustCompile(`(?s)- name: Run golangci-lint blocking correctness gate.*?(?:\n\n|$)`).FindString(text)
	if blockingStep == "" {
		t.Fatal("CI workflow missing blocking golangci-lint correctness gate")
	}
	for _, want := range []string{
		"golangci/golangci-lint-action@db582008a42febd596419635a5abc9d9815daa9c",
		"version: v2.12.2",
		"args: --config=.golangci.yml --enable-only=errorlint,ineffassign,unused",
	} {
		if !strings.Contains(blockingStep, want) {
			t.Fatalf("blocking golangci-lint step missing %q:\n%s", want, blockingStep)
		}
	}
	if strings.Contains(blockingStep, "--issues-exit-code=0") {
		t.Fatalf("blocking golangci-lint step must not be report-only:\n%s", blockingStep)
	}

	reportStep := regexp.MustCompile(`(?s)- name: Run golangci-lint report-only baseline.*?(?:\n\n|$)`).FindString(text)
	if reportStep == "" {
		t.Fatal("CI workflow missing report-only golangci-lint baseline step")
	}
	if !strings.Contains(reportStep, "args: --config=.golangci.yml --issues-exit-code=0") {
		t.Fatalf("report-only golangci-lint step missing issues-exit-code=0:\n%s", reportStep)
	}
}
