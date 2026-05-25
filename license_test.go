package aiopsplatform_test

import (
	"os"
	"strings"
	"testing"
)

func TestRepositoryDeclaresApacheLicense(t *testing.T) {
	license, err := os.ReadFile("LICENSE")
	if err != nil {
		t.Fatalf("read LICENSE: %v", err)
	}
	licenseText := string(license)
	for _, want := range []string{
		"Apache License",
		"Version 2.0, January 2004",
		"Copyright 2026 xrf9268-hue",
	} {
		if !strings.Contains(licenseText, want) {
			t.Fatalf("LICENSE missing %q", want)
		}
	}

	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	readmeText := string(readme)
	for _, want := range []string{
		"## License",
		"Apache License 2.0",
		"(LICENSE)",
	} {
		if !strings.Contains(readmeText, want) {
			t.Fatalf("README.md missing license declaration %q", want)
		}
	}
}
