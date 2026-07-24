package scripts

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"regexp"
	"testing"
)

const (
	symphonySpecCommit       = "653f8b3cc476db03420479ba6f95b2ed7281c401"
	symphonySpecCommitDate   = "2026-07-24"
	symphonySpecMirroredDate = "2026-07-24"
	symphonySpecSHA256       = "29d6b45a85453e045883c064c0e08595f9d4a33f9a2527f649bc1363b74e0176"
)

func TestSymphonySpecMirrorMatchesPinnedUpstream(t *testing.T) {
	root := gitRepoRoot(t)
	mirror := readRepoFile(t, root, "docs/research/SPEC.md")
	header, body, ok := bytes.Cut(mirror, []byte("-->\n\n"))
	if !ok {
		t.Fatal("docs/research/SPEC.md is missing its provenance header terminator")
	}
	if got, want := headerValue(header, "Upstream commit: "), symphonySpecCommit+" ("+symphonySpecCommitDate+")"; got != want {
		t.Errorf("SPEC mirror upstream commit = %q; want %q", got, want)
	}
	if got, want := headerValue(header, "Body SHA-256: "), symphonySpecSHA256; got != want {
		t.Errorf("SPEC mirror body SHA-256 header = %q; want %q", got, want)
	}
	for prefix, want := range map[string]string{
		"Source: ":   "https://github.com/openai/symphony/blob/" + symphonySpecCommit + "/SPEC.md",
		"Mirrored: ": symphonySpecMirroredDate,
		"License: ":  "Apache License 2.0 — https://github.com/openai/symphony/blob/" + symphonySpecCommit + "/LICENSE",
	} {
		if got := headerValue(header, prefix); got != want {
			t.Errorf("SPEC mirror %s header = %q; want %q", prefix, got, want)
		}
	}
	guardedClone := "  if ! git -C /tmp/symphony-upstream rev-parse --git-dir >/dev/null 2>&1; then\n" +
		"    git clone https://github.com/openai/symphony.git /tmp/symphony-upstream\n" +
		"  fi"
	if !bytes.Contains(header, []byte(guardedClone)) {
		t.Errorf("SPEC mirror header is missing guarded clone block %q", guardedClone)
	}
	for _, command := range []string{
		"  symphony_sha=" + symphonySpecCommit,
		`  git -C /tmp/symphony-upstream fetch origin "${symphony_sha}"`,
		`  git -C /tmp/symphony-upstream show "${symphony_sha}:SPEC.md" > /tmp/symphony-SPEC.md`,
		"  shasum -a 256 /tmp/symphony-SPEC.md",
	} {
		if !bytes.Contains(header, []byte(command)) {
			t.Errorf("SPEC mirror header is missing reproducible command %q", command)
		}
	}
	normalizedHeader := bytes.Join(bytes.Fields(header), []byte(" "))
	for _, updateTarget := range []string{"scripts/spec_mirror_docs_test.go", "pinned authority links"} {
		if !bytes.Contains(normalizedHeader, []byte(updateTarget)) {
			t.Errorf("SPEC mirror header does not name refresh update target %q", updateTarget)
		}
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(body)); got != symphonySpecSHA256 {
		t.Errorf("SPEC mirror body SHA-256 = %s; want %s", got, symphonySpecSHA256)
	}
}

func TestCurrentAuthorityDocsDoNotCallSymphonyUnmaintained(t *testing.T) {
	root := gitRepoRoot(t)
	for _, path := range []string{"AGENTS.md", "DECISION.md", "DEVIATIONS.md", "README.md", "docs/research/SPEC.md"} {
		body := repoAuthoredAuthorityText(t, root, path)
		normalized := bytes.ToLower(bytes.Join(bytes.Fields(body), []byte(" ")))
		if bytes.Contains(normalized, []byte("unmaintained demo")) {
			t.Errorf("%s still calls current upstream an unmaintained demo", path)
		}
	}
	decision := bytes.Join(bytes.Fields(readRepoFile(t, root, "DECISION.md")), []byte(" "))
	if current := "Upstream resumed development"; !bytes.Contains(decision, []byte(current)) {
		t.Errorf("DECISION.md is missing current upstream-status correction %q", current)
	}
	for _, stale := range []string{
		"OpenAI has stated the Elixir repo is a reference implementation and will not be maintained as a product.",
		"There are no future upstream changes to inherit by merge.",
		"Both paths (Go port and Elixir fork) become \"we own this code\" from day one.",
	} {
		if bytes.Contains(decision, []byte(stale)) {
			t.Errorf("DECISION.md still presents historical upstream premise as current fact %q", stale)
		}
	}
}

func TestCurrentSymphonyAuthorityLinksStayPinned(t *testing.T) {
	root := gitRepoRoot(t)
	for _, path := range []string{"AGENTS.md", "DECISION.md", "DEVIATIONS.md", "README.md", "docs/research/SPEC.md"} {
		body := repoAuthoredAuthorityText(t, root, path)
		assertAllSymphonyAuthorityLinksPinned(t, path, body)
	}
}

func TestCurrentDeviationDocsDoNotRouteNewFindingsToHistoricalLedger(t *testing.T) {
	root := gitRepoRoot(t)
	for path, contract := range map[string]struct {
		current string
		stale   string
	}{
		"DEVIATIONS.md": {
			current: "The current Symphony 0.0.2 alignment ledger is [#1137]",
		},
		"AGENTS.md": {
			current: "active alignment ledger named in `DEVIATIONS.md`",
			stale:   "The umbrella tracker is [#67]",
		},
		".claude/skills/handle-issue/SKILL.md": {
			current: "挂到 `DEVIATIONS.md` 指向的当前 alignment ledger",
			stale:   "伞 issue #67",
		},
		"docs/engineering-rules-rationale.md": {
			current: "Route new findings through the active ledger named in `DEVIATIONS.md`",
		},
	} {
		body := readRepoFile(t, root, path)
		normalized := bytes.Join(bytes.Fields(body), []byte(" "))
		if !bytes.Contains(normalized, []byte(contract.current)) {
			t.Errorf("%s is missing current deviation-ledger contract %q", path, contract.current)
		}
		if contract.stale != "" && bytes.Contains(normalized, []byte(contract.stale)) {
			t.Errorf("%s still routes new deviations to historical ledger #67 via %q", path, contract.stale)
		}
	}
}

func repoAuthoredAuthorityText(t *testing.T, root, path string) []byte {
	t.Helper()
	body := readRepoFile(t, root, path)
	if path != "docs/research/SPEC.md" {
		return body
	}
	header, _, ok := bytes.Cut(body, []byte("-->\n\n"))
	if !ok {
		t.Fatal("docs/research/SPEC.md is missing its provenance header terminator")
	}
	return header
}

func assertAllSymphonyAuthorityLinksPinned(t *testing.T, path string, body []byte) {
	t.Helper()
	githubLinks := regexp.MustCompile(
		`https://github\.com/openai/symphony/(blob|tree)/([^/)[:space:]]+)`,
	).FindAllSubmatch(body, -1)
	rawLinks := regexp.MustCompile(
		`https://raw\.githubusercontent\.com/openai/symphony/([^/)[:space:]]+)`,
	).FindAllSubmatch(body, -1)
	if len(githubLinks)+len(rawLinks) == 0 {
		t.Errorf("%s does not contain a pinned Symphony authority link", path)
	}
	for _, match := range githubLinks {
		assertSymphonyAuthorityRef(t, path, string(match[0]), string(match[2]))
	}
	for _, match := range rawLinks {
		assertSymphonyAuthorityRef(t, path, string(match[0]), string(match[1]))
	}
}

func assertSymphonyAuthorityRef(t *testing.T, path, link, got string) {
	t.Helper()
	if got != symphonySpecCommit {
		t.Errorf("%s Symphony authority link %q uses ref %q; want %q", path, link, got, symphonySpecCommit)
	}
}

func headerValue(header []byte, prefix string) string {
	for _, line := range bytes.Split(header, []byte("\n")) {
		if value, ok := bytes.CutPrefix(line, []byte(prefix)); ok {
			return string(value)
		}
	}
	return ""
}
