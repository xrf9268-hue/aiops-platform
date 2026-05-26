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
