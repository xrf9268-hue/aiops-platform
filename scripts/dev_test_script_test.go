package scripts

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

func TestDevTestScriptIsLightweightLocalValidationEntrypoint(t *testing.T) {
	info, err := os.Stat("dev-test.sh")
	if err != nil {
		t.Fatalf("stat dev-test.sh: %v", err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatalf("dev-test.sh mode = %v, want executable bit", info.Mode())
	}

	if out, err := exec.Command("bash", "-n", "dev-test.sh").CombinedOutput(); err != nil {
		t.Fatalf("bash -n dev-test.sh: %v\n%s", err, out)
	}

	body, err := os.ReadFile("dev-test.sh")
	if err != nil {
		t.Fatalf("ReadFile(dev-test.sh): %v", err)
	}
	script := string(body)
	for _, want := range []string{
		`awk '/^go [0-9]/ {print $2; exit}' go.mod`,
		`go env GOVERSION`,
		`GOTOOLCHAIN=auto`,
		`git ls-files '*.go'`,
		`gofmt -l`,
		`go mod tidy`,
		`git diff --exit-code -- go.mod go.sum`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("dev-test.sh missing %q", want)
		}
	}
	if !regexp.MustCompile(`(?m)^go test \./\.\.\.$`).MatchString(script) {
		t.Fatal("dev-test.sh must run the full Go unit-test surface with `go test ./...`")
	}
	for _, forbidden := range []string{
		"docker ",
		"docker compose",
		"npm ",
		"LINEAR_API_KEY",
		"GITEA_TOKEN",
		"GITHUB_TOKEN",
	} {
		if strings.Contains(script, forbidden) {
			t.Fatalf("dev-test.sh default path must stay credential/Docker/dashboard-free; found %q", forbidden)
		}
	}
}

func TestLocalDevDocsMentionDevTestAndExactGoFloor(t *testing.T) {
	goMod, err := os.ReadFile("../go.mod")
	if err != nil {
		t.Fatalf("ReadFile(go.mod): %v", err)
	}
	match := regexp.MustCompile(`(?m)^go ([0-9]+\.[0-9]+\.[0-9]+)$`).FindSubmatch(goMod)
	if match == nil {
		t.Fatalf("go.mod missing exact patch go directive:\n%s", goMod)
	}
	goVersion := string(match[1])

	body, err := os.ReadFile("../docs/runbooks/local-dev.md")
	if err != nil {
		t.Fatalf("ReadFile(local-dev.md): %v", err)
	}
	text := string(body)
	for _, want := range []string{
		"Go " + goVersion,
		"./scripts/dev-test.sh",
		"GOTOOLCHAIN=auto",
		"offline",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("local-dev.md missing %q", want)
		}
	}
	if strings.Contains(text, "Go 1.25 or newer") {
		t.Fatal("local-dev.md still says only Go 1.25 or newer; use the exact pinned patch floor")
	}
}
