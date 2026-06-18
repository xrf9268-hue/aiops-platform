package scripts_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

func TestTraceHarnessReportRunbookDocumentsSupportedInputsAndBounds(t *testing.T) {
	root := repoRoot(t)
	readme, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	if !strings.Contains(string(readme), "docs/runbooks/trace-harness-report.md") {
		t.Fatalf("README does not link the trace harness report runbook")
	}

	path := filepath.Join(root, "docs", "runbooks", "trace-harness-report.md")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	text := string(body)
	normalizedText := strings.Join(strings.Fields(text), " ")
	for _, want := range []string{
		"python3 scripts/trace-harness-report.py",
		"--worker-log",
		"Supported inputs",
		"worker process logs",
		"64 KiB per run",
		"256 KiB per cluster",
		"does not mutate tracker state",
		"does not open PRs",
		"does not edit prompts",
		"does not create a worker phase",
		"MaskCloneURL",
		"before the first opaque payload key",
		"does not parse GraphQL",
		"hard boundary",
		"ambiguous multiline closures",
		"prefers omission over fabricating affected issues",
		"cannot prove",
		"known scalar metadata is byte-bounded per field and in aggregate",
		"Unsupported inputs",
		"workspace `.aiops` artifacts",
		"Examples",
	} {
		if !strings.Contains(normalizedText, want) {
			t.Fatalf("runbook missing %q\n%s", want, text)
		}
	}
}

func TestTraceHarnessReportScriptGroupsWorkerLogsAndRedactsCloneURLs(t *testing.T) {
	root := repoRoot(t)
	dir := t.TempDir()
	logPath := filepath.Join(dir, "worker.log")
	secretURL := "https://user:secret@example.test/org/repo.git"
	body := strings.Join([]string{
		`2026/06/18 09:00:00 event=runner_timeout task_id=issue-1 issue_id=issue-1 issue_identifier=GH-1 session_id=session-1 msg="timeout" payload=map[elapsed_ms:60000 output_bytes:70000 output_dropped:12 output_head:` + secretURL + `]`,
		`2026/06/18 09:01:00 event=turn_input_required task_id=issue-2 issue_id=issue-2 issue_identifier=GH-2 session_id=session-2 msg="input" payload=map[method:approval]`,
	}, "\n") + "\n"
	if err := os.WriteFile(logPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}
	jsonPath := filepath.Join(dir, "report.json")
	cmd := exec.Command("python3", filepath.Join(root, "scripts", "trace-harness-report.py"), "--worker-log", logPath, "--json-out", jsonPath)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("trace-harness-report failed: %v\n%s", err, out)
	}

	raw, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	var report struct {
		SchemaVersion string `json:"schema_version"`
		Clusters      []struct {
			ID       string `json:"id"`
			Affected struct {
				Issues   []string `json:"issues"`
				Sessions []string `json:"sessions"`
			} `json:"affected"`
			Evidence []struct {
				Excerpt  string            `json:"excerpt"`
				Metadata map[string]string `json:"metadata"`
			} `json:"evidence"`
			RedactionNote string `json:"redaction_note"`
		} `json:"clusters"`
	}
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatalf("unmarshal report: %v\n%s", err, raw)
	}
	if report.SchemaVersion != "trace-harness-report/v1" || len(report.Clusters) != 2 {
		t.Fatalf("unexpected report: %#v", report)
	}
	timeout := report.Clusters[1]
	if timeout.ID != "runner-timeout" || !contains(timeout.Affected.Issues, "issue-1") || !contains(timeout.Affected.Sessions, "session-1") {
		t.Fatalf("timeout cluster missing affected fields: %#v", timeout)
	}
	if timeout.Evidence[0].Metadata["timestamp"] != "2026/06/18 09:00:00" {
		t.Fatalf("timeout timestamp metadata = %#v", timeout.Evidence[0].Metadata)
	}
	if timeout.Evidence[0].Metadata["output_bytes"] != "70000" || timeout.Evidence[0].Metadata["output_dropped"] != "12" {
		t.Fatalf("timeout metadata = %#v", timeout.Evidence[0].Metadata)
	}
	if strings.Contains(string(raw), "secret") || !strings.Contains(timeout.Evidence[0].Excerpt, "payload=[redacted-payload]") {
		t.Fatalf("report leaked or retained opaque runner output:\n%s", raw)
	}
	for _, want := range []string{"output_head", "output_tail", "error", "arguments", "raw", "params"} {
		if !strings.Contains(timeout.RedactionNote, want) {
			t.Fatalf("redaction note missing omitted payload %q: %q", want, timeout.RedactionNote)
		}
	}
}

func TestTraceHarnessReportScriptUsesPayloadSessionID(t *testing.T) {
	root := repoRoot(t)
	body := `2026/06/18 09:01:00 event=turn_input_required task_id=issue-2 issue_id=issue-2 issue_identifier=GH-2 msg="input" payload=map[method:approval session_id:payload-session]` + "\n"
	report := runTraceHarnessReport(t, root, body)

	cluster := findCluster(t, report, "input-required")
	if !contains(cluster.Affected.Sessions, "payload-session") {
		t.Fatalf("payload session_id was not reported as affected: %#v", cluster.Affected.Sessions)
	}
}

func TestTraceHarnessReportScriptUsesPayloadIssueAndRunIDs(t *testing.T) {
	root := repoRoot(t)
	body := `2026/06/18 09:01:00 event=runner_timeout msg="timeout" payload=map[task_id:payload-run issue_id:payload-issue issue_identifier:PAY-1 timeout_ms:60000]` + "\n"
	report := runTraceHarnessReport(t, root, body)

	cluster := findCluster(t, report, "runner-timeout")
	if !contains(cluster.Affected.Issues, "payload-issue") {
		t.Fatalf("payload issue_id was not reported as affected: %#v", cluster.Affected.Issues)
	}
	if !contains(cluster.Affected.Runs, "payload-run") {
		t.Fatalf("payload task_id was not reported as affected run: %#v", cluster.Affected.Runs)
	}
	if !contains(cluster.Affected.IssueIdentifiers, "PAY-1") {
		t.Fatalf("payload issue_identifier was not reported as affected: %#v", cluster.Affected.IssueIdentifiers)
	}
}

func TestTraceHarnessReportScriptMasksMalformedCloneURLUserinfo(t *testing.T) {
	root := repoRoot(t)
	for _, secretURL := range []string{
		"https://user:bad token@example.test/org/repo.git",
		"https://:secret@example.test/org/repo.git",
	} {
		body := `2026/06/18 09:00:00 event=runner_timeout task_id=issue-1 issue_id=issue-1 msg="timeout" payload=map[output_head:` + secretURL + `]` + "\n"
		raw := runTraceHarnessReportRaw(t, root, body)
		if strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "bad token") || !strings.Contains(string(raw), "payload=[redacted-payload]") {
			t.Fatalf("report leaked or retained malformed userinfo from opaque output %q:\n%s", secretURL, raw)
		}
	}
}

func TestTraceHarnessReportScriptMasksMalformedCloneURLUserinfoWithBracket(t *testing.T) {
	root := repoRoot(t)
	secretURL := "https://user:tok]en@example.test/org/repo.git"
	body := `2026/06/18 09:00:00 event=runner_timeout task_id=issue-1 issue_id=issue-1 msg="timeout" payload=map[output_head:` + secretURL + `]` + "\n"
	raw := runTraceHarnessReportRaw(t, root, body)

	if strings.Contains(string(raw), "tok]en") || strings.Contains(string(raw), "user:") {
		t.Fatalf("report leaked malformed bracket userinfo:\n%s", raw)
	}
}

func TestTraceHarnessReportScriptMasksMalformedCloneURLUserinfoInMetadata(t *testing.T) {
	root := repoRoot(t)
	secretURL := "https://user:tok]en@example.test/org/repo.git"
	body := `2026/06/18 09:00:00 event=runner_end task_id=issue-1 issue_id=issue-1 msg="runner failed" payload=map[ok:false error:` + secretURL + `]` + "\n"
	raw := runTraceHarnessReportRaw(t, root, body)

	if strings.Contains(string(raw), "tok]en") || strings.Contains(string(raw), "user:") {
		t.Fatalf("report leaked malformed bracket userinfo in metadata:\n%s", raw)
	}
}

func TestTraceHarnessReportScriptMasksHTTPCloneURLUserinfo(t *testing.T) {
	root := repoRoot(t)
	secretURL := "http://user:secret@example.test/org/repo.git"
	body := `2026/06/18 09:00:00 event=runner_timeout task_id=issue-1 issue_id=issue-1 msg="timeout" payload=map[output_head:` + secretURL + `]` + "\n"
	raw := runTraceHarnessReportRaw(t, root, body)
	if strings.Contains(string(raw), "secret") || !strings.Contains(string(raw), "payload=[redacted-payload]") {
		t.Fatalf("report leaked or retained HTTP userinfo from opaque output:\n%s", raw)
	}
}

func TestTraceHarnessReportScriptMasksSSHCloneURLUserinfo(t *testing.T) {
	root := repoRoot(t)
	secretURL := "ssh://user:secret@example.test/org/repo.git"
	body := `2026/06/18 09:00:00 event=runner_timeout task_id=issue-1 issue_id=issue-1 msg="timeout" payload=map[output_head:` + secretURL + `]` + "\n"
	raw := runTraceHarnessReportRaw(t, root, body)
	if strings.Contains(string(raw), "secret") || !strings.Contains(string(raw), "payload=[redacted-payload]") {
		t.Fatalf("report leaked or retained SSH userinfo from opaque output:\n%s", raw)
	}
}

func TestTraceHarnessReportScriptMasksGenericSchemeCloneURLUserinfo(t *testing.T) {
	root := repoRoot(t)
	for _, secretURL := range []string{
		"git://user:secret@example.test/org/repo.git",
		"rsync://user:secret@example.test/org/repo.git",
		"file://user:secret@example.test/org/repo.git",
	} {
		body := `2026/06/18 09:00:00 event=runner_timeout task_id=issue-1 issue_id=issue-1 msg="timeout" payload=map[output_head:` + secretURL + `]` + "\n"
		raw := runTraceHarnessReportRaw(t, root, body)
		if strings.Contains(string(raw), "secret") || !strings.Contains(string(raw), "payload=[redacted-payload]") {
			t.Fatalf("report leaked or retained generic scheme userinfo from opaque output:\n%s", raw)
		}
	}
}

func TestTraceHarnessReportScriptMasksTokenLikeSecrets(t *testing.T) {
	root := repoRoot(t)
	for _, token := range []string{
		"ghp_" + strings.Repeat("a", 36),
		"lin_api_" + strings.Repeat("a", 40),
	} {
		body := `2026/06/18 09:00:00 event=runner_end task_id=issue-1 issue_id=issue-1 msg="runner failed" payload=map[ok:false error:` + token + ` output_head:` + token + `]` + "\n"
		raw := runTraceHarnessReportRaw(t, root, body)

		if strings.Contains(string(raw), token) || !strings.Contains(string(raw), "payload=[redacted-payload]") || !strings.Contains(string(raw), "[redacted-error]") {
			t.Fatalf("report leaked or retained token-like opaque payload %q:\n%s", token, raw)
		}
	}
}

func TestTraceHarnessReportScriptMasksPrefixAffectedFields(t *testing.T) {
	root := repoRoot(t)
	token := "ghp_" + strings.Repeat("a", 36)
	secretPR := "https://user:secret@example.test/org/repo/pull/1"
	body := `2026/06/18 09:00:00 event=runner_timeout task_id=issue-1 issue_id=issue-1 session_id=` + token + ` pr_url=` + secretPR + ` msg="timeout"` + "\n"
	raw := runTraceHarnessReportRaw(t, root, body)
	maskedPR := workflow.MaskCloneURL(secretPR)

	if strings.Contains(string(raw), token) || strings.Contains(string(raw), "user:secret") || !strings.Contains(string(raw), "[redacted-token]") || !strings.Contains(string(raw), maskedPR) {
		t.Fatalf("report leaked prefix affected fields:\n%s", raw)
	}
}

func TestTraceHarnessReportScriptMasksInputPathRefs(t *testing.T) {
	root := repoRoot(t)
	token := "ghp_" + strings.Repeat("a", 36)
	dir := filepath.Join(t.TempDir(), token)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("mkdir token path: %v", err)
	}
	logPath := filepath.Join(dir, "worker.log")
	body := []byte(`2026/06/18 09:00:00 event=runner_timeout task_id=issue-1 issue_id=issue-1 msg="timeout"` + "\n")
	if err := os.WriteFile(logPath, body, 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}
	raw := runTraceHarnessReportRawPath(t, root, logPath)

	if strings.Contains(string(raw), token) || !strings.Contains(string(raw), "[redacted-token]") {
		t.Fatalf("report leaked token-like input path:\n%s", raw)
	}
}

func TestTraceHarnessReportScriptMasksInputPathErrors(t *testing.T) {
	root := repoRoot(t)
	token := "ghp_" + strings.Repeat("a", 36)
	missing := filepath.Join(t.TempDir(), token, "missing.log")
	cmd := exec.Command("python3", filepath.Join(root, "scripts", "trace-harness-report.py"), "--worker-log", missing)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("trace-harness-report unexpectedly succeeded for missing log:\n%s", out)
	}
	if strings.Contains(string(out), token) || !strings.Contains(string(out), "[redacted-token]") {
		t.Fatalf("stderr leaked token-like input path:\n%s", out)
	}
}

func TestTraceHarnessReportScriptMasksOutputPathErrors(t *testing.T) {
	root := repoRoot(t)
	dir := t.TempDir()
	logPath := filepath.Join(dir, "worker.log")
	body := []byte(`2026/06/18 09:00:00 event=runner_timeout task_id=issue-1 issue_id=issue-1 msg="timeout"` + "\n")
	if err := os.WriteFile(logPath, body, 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}
	token := "ghp_" + strings.Repeat("a", 36)
	jsonPath := filepath.Join(dir, token, "report.json")
	cmd := exec.Command("python3", filepath.Join(root, "scripts", "trace-harness-report.py"), "--worker-log", logPath, "--json-out", jsonPath)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("trace-harness-report unexpectedly wrote missing output path:\n%s", out)
	}
	if strings.Contains(string(out), token) || !strings.Contains(string(out), "[redacted-token]") {
		t.Fatalf("stderr leaked token-like output path:\n%s", out)
	}
}

func TestTraceHarnessReportScriptBoundsExcerptsByBytes(t *testing.T) {
	root := repoRoot(t)
	body := `2026/06/18 09:00:00 event=runner_timeout task_id=issue-1 issue_id=issue-1 msg="` + strings.Repeat("界", 5000) + `"` + "\n"
	report := runTraceHarnessReport(t, root, body)

	cluster := findCluster(t, report, "runner-timeout")
	if got := len([]byte(cluster.Evidence[0].Excerpt)); got > 4*1024 {
		t.Fatalf("excerpt byte length = %d; want <= 4096", got)
	}
}

func TestTraceHarnessReportScriptKeepsAffectedIDsWhenEvidenceIsCapped(t *testing.T) {
	root := repoRoot(t)
	lines := make([]string, 0, 17)
	for idx := 1; idx <= 17; idx++ {
		lines = append(lines, `2026/06/18 09:00:00 event=runner_timeout task_id=run-1 issue_id=issue-`+string(rune('A'+idx-1))+` msg="`+strings.Repeat("x", 5000)+`"`)
	}
	report := runTraceHarnessReport(t, root, strings.Join(lines, "\n")+"\n")

	cluster := findCluster(t, report, "runner-timeout")
	if !contains(cluster.Affected.Issues, "issue-Q") {
		t.Fatalf("capped evidence dropped affected issue ids: %#v", cluster.Affected.Issues)
	}
}

func TestTraceHarnessReportScriptBoundsFullEvidenceEntriesByBytes(t *testing.T) {
	root := repoRoot(t)
	var body strings.Builder
	for idx := 0; idx < 4000; idx++ {
		fmt.Fprintf(&body, "2026/06/18 09:00:00 event=runner_timeout task_id=run-%d issue_id=issue-shared msg=\"x\"\n", idx%16)
	}
	report := runTraceHarnessReport(t, root, body.String())

	cluster := findCluster(t, report, "runner-timeout")
	raw, err := json.Marshal(cluster.Evidence)
	if err != nil {
		t.Fatalf("marshal evidence: %v", err)
	}
	if len(raw) > 256*1024 {
		t.Fatalf("cluster evidence bytes = %d; want <= 262144", len(raw))
	}
	if !contains(cluster.Affected.Issues, "issue-shared") {
		t.Fatalf("affected ids stopped when evidence cap was reached: %#v", cluster.Affected.Issues)
	}
}

func TestTraceHarnessReportScriptBoundsFullClusterByBytes(t *testing.T) {
	root := repoRoot(t)
	var body strings.Builder
	for idx := 0; idx < 12000; idx++ {
		fmt.Fprintf(&body, "2026/06/18 09:00:00 event=runner_timeout task_id=run-%d issue_id=issue-%d session_id=session-%d msg=\"x\"\n", idx%64, idx, idx)
	}
	rawReport := runTraceHarnessReportRaw(t, root, body.String())

	var raw struct {
		Clusters []json.RawMessage `json:"clusters"`
	}
	if err := json.Unmarshal(rawReport, &raw); err != nil {
		t.Fatalf("unmarshal raw report: %v\n%s", err, rawReport)
	}
	if len(raw.Clusters) != 1 {
		t.Fatalf("raw cluster count = %d; want 1\n%s", len(raw.Clusters), rawReport)
	}
	if len(raw.Clusters[0]) > 256*1024 {
		t.Fatalf("emitted cluster bytes = %d; want <= 262144", len(raw.Clusters[0]))
	}

	var report traceReport
	if err := json.Unmarshal(rawReport, &report); err != nil {
		t.Fatalf("unmarshal typed report: %v\n%s", err, rawReport)
	}
	cluster := findCluster(t, report, "runner-timeout")
	if len(cluster.Evidence) == 0 {
		t.Fatalf("cluster evidence count = 0; want at least 1; omitted=%#v issues=%d sessions=%d", cluster.Affected.Omitted, len(cluster.Affected.Issues), len(cluster.Affected.Sessions))
	}
	encoded, err := json.Marshal(cluster)
	if err != nil {
		t.Fatalf("marshal cluster: %v", err)
	}
	if len(encoded) > 256*1024 {
		t.Fatalf("typed cluster bytes = %d; want <= 262144", len(encoded))
	}
	if got := cluster.Affected.Omitted["issues"]; got == 0 {
		t.Fatalf("affected omitted counts were not reported: %#v", cluster.Affected.Omitted)
	}
}

func TestTraceHarnessReportScriptBoundsLargeScalarMetadata(t *testing.T) {
	root := repoRoot(t)
	body := `2026/06/18 09:00:00 event=runner_timeout task_id=issue-1 issue_id=issue-1 msg="timeout" payload=map[model:` + strings.Repeat("m", 300*1024) + ` timeout_ms:60000]` + "\n"
	rawReport := runTraceHarnessReportRaw(t, root, body)

	var raw struct {
		Clusters []json.RawMessage `json:"clusters"`
	}
	if err := json.Unmarshal(rawReport, &raw); err != nil {
		t.Fatalf("unmarshal raw report: %v\n%s", err, rawReport)
	}
	if len(raw.Clusters) != 1 || len(raw.Clusters[0]) > 256*1024 {
		t.Fatalf("raw clusters = %d, first bytes = %d; want one cluster <= 262144", len(raw.Clusters), len(raw.Clusters[0]))
	}

	report := runTraceHarnessReport(t, root, body)
	cluster := findCluster(t, report, "runner-timeout")
	if len(cluster.Evidence) == 0 {
		t.Fatalf("large scalar metadata dropped all evidence")
	}
	model := cluster.Evidence[0].Metadata["model"]
	if len([]byte(model)) > 4*1024 || !strings.Contains(model, "[truncated]") {
		t.Fatalf("model metadata was not byte-bounded: len=%d value suffix=%q", len([]byte(model)), model[max(0, len(model)-32):])
	}
}

func TestTraceHarnessReportScriptBoundsLargeScalarMetadataAcrossFields(t *testing.T) {
	root := repoRoot(t)
	large := strings.Repeat("m", 8*1024)
	keys := []string{
		"elapsed_ms",
		"timeout_ms",
		"duration_ms",
		"output_bytes",
		"output_dropped",
		"model",
		"method",
		"session_id",
		"pr",
		"pr_number",
		"pr_url",
		"pull_request",
		"pull_request_url",
		"exit_code",
		"error",
		"ok",
	}
	var payload strings.Builder
	for _, key := range keys {
		fmt.Fprintf(&payload, " %s:%s", key, large)
	}
	body := `2026/06/18 09:00:00 event=runner_timeout task_id=issue-1 issue_id=issue-1 msg="timeout" payload=map[` + strings.TrimSpace(payload.String()) + `]` + "\n"
	report := runTraceHarnessReport(t, root, body)

	cluster := findCluster(t, report, "runner-timeout")
	if len(cluster.Evidence) == 0 {
		t.Fatalf("large aggregate scalar metadata dropped all evidence")
	}
	rawMetadata, err := json.Marshal(cluster.Evidence[0].Metadata)
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	if len(rawMetadata) > 16*1024 {
		t.Fatalf("metadata bytes = %d; want <= 16384", len(rawMetadata))
	}
}

func TestTraceHarnessReportScriptReportsAffectedPullRequests(t *testing.T) {
	root := repoRoot(t)
	body := `2026/06/18 09:00:00 event=runner_timeout task_id=issue-1 issue_id=issue-1 msg="timeout" payload=map[pr_number:938]` + "\n"
	report := runTraceHarnessReport(t, root, body)

	cluster := findCluster(t, report, "runner-timeout")
	if !contains(cluster.Affected.PullRequests, "938") {
		t.Fatalf("pull request id was not reported as affected: %#v", cluster.Affected.PullRequests)
	}
}

func TestTraceHarnessReportScriptClassifiesMalformedProtocolSeparately(t *testing.T) {
	root := repoRoot(t)
	body := `2026/06/18 09:00:00 event=malformed task_id=issue-1 issue_id=issue-1 msg="bad protocol line"` + "\n"
	report := runTraceHarnessReport(t, root, body)

	if len(report.Clusters) != 1 || report.Clusters[0].ID != "malformed-protocol" {
		t.Fatalf("malformed event clusters = %#v; want malformed-protocol only", report.Clusters)
	}
}

func TestTraceHarnessReportScriptIgnoresPlainEventText(t *testing.T) {
	root := repoRoot(t)
	body := `agent output says event=runner_timeout task_id=fake issue_id=fake` + "\n"
	report := runTraceHarnessReport(t, root, body)

	if len(report.Clusters) != 0 {
		t.Fatalf("plain event-shaped text became report evidence: %#v", report.Clusters)
	}
}

func TestTraceHarnessReportScriptIgnoresTimestampedProseEventText(t *testing.T) {
	root := repoRoot(t)
	body := `2026/06/18 09:00:00 agent output says event=runner_timeout task_id=fake issue_id=fake` + "\n"
	report := runTraceHarnessReport(t, root, body)

	if len(report.Clusters) != 0 {
		t.Fatalf("timestamped prose became report evidence: %#v", report.Clusters)
	}
}

func TestTraceHarnessReportScriptIgnoresKeyShapedTextInsideMessage(t *testing.T) {
	root := repoRoot(t)
	body := `2026/06/18 09:00:00 event=runner_timeout task_id=issue-real issue_id=issue-real msg="agent says task_id=fake issue_id=fake" payload=map[timeout_ms:60000]` + "\n"
	report := runTraceHarnessReport(t, root, body)

	cluster := findCluster(t, report, "runner-timeout")
	if contains(cluster.Affected.Issues, "fake") || contains(cluster.Affected.Runs, "fake") {
		t.Fatalf("message text overrode trusted prefix fields: issues=%#v runs=%#v", cluster.Affected.Issues, cluster.Affected.Runs)
	}
	if !contains(cluster.Affected.Issues, "issue-real") || !contains(cluster.Affected.Runs, "issue-real") {
		t.Fatalf("trusted prefix fields missing: issues=%#v runs=%#v", cluster.Affected.Issues, cluster.Affected.Runs)
	}
}

func TestTraceHarnessReportScriptDoesNotSwallowPayloadlessEventAfterMultilineOutput(t *testing.T) {
	root := repoRoot(t)
	body := strings.Join([]string{
		`2026/06/18 09:00:00 event=runner_timeout task_id=issue-timeout issue_id=issue-timeout msg="timeout" payload=map[output_head:agent started`,
		`agent output finished]]`,
		`2026/06/18 09:00:01 event=malformed task_id=issue-malformed issue_id=issue-malformed msg="bad protocol line"`,
	}, "\n") + "\n"
	report := runTraceHarnessReport(t, root, body)

	timeout := findCluster(t, report, "runner-timeout")
	if !contains(timeout.Affected.Issues, "issue-timeout") || contains(timeout.Affected.Issues, "issue-malformed") {
		t.Fatalf("runner-timeout affected issues = %#v; want issue-timeout only", timeout.Affected.Issues)
	}
	malformed := findCluster(t, report, "malformed-protocol")
	if !contains(malformed.Affected.Issues, "issue-malformed") {
		t.Fatalf("payloadless malformed event after multiline output was swallowed: %#v", malformed.Affected.Issues)
	}
}

func TestTraceHarnessReportScriptDoesNotPromoteEventAfterKeyShapedOpaqueLine(t *testing.T) {
	root := repoRoot(t)
	for _, key := range []string{"arguments", "arguments_raw", "error", "output_head", "output_tail", "params", "raw"} {
		t.Run(key, func(t *testing.T) {
			body := strings.Join([]string{
				`2026/06/18 09:00:00 event=runner_timeout task_id=issue-real issue_id=issue-real msg="timeout" payload=map[elapsed_ms:70000 ` + key + `:first line`,
				`timeout_ms:123]`,
				`2026/06/18 09:00:01 event=runner_timeout task_id=fake issue_id=fake msg="from opaque"`,
				`tail done]]`,
			}, "\n") + "\n"
			report := runTraceHarnessReport(t, root, body)

			cluster := findCluster(t, report, "runner-timeout")
			if contains(cluster.Affected.Issues, "fake") || contains(cluster.Affected.Runs, "fake") {
				t.Fatalf("%s opaque payload promoted fake event: issues=%#v runs=%#v", key, cluster.Affected.Issues, cluster.Affected.Runs)
			}
			if got := cluster.Evidence[0].Metadata["elapsed_ms"]; got != "70000" {
				t.Fatalf("%s elapsed_ms before opaque text = %q; want 70000; metadata=%#v", key, got, cluster.Evidence[0].Metadata)
			}
		})
	}
}

func TestTraceHarnessReportScriptClassifiesFailedRunnerEndDespiteOutputOKText(t *testing.T) {
	root := repoRoot(t)
	body := `2026/06/18 09:00:00 event=runner_end task_id=issue-1 issue_id=issue-1 msg="runner failed" payload=map[ok:false error:boom output_head:agent-text-ok:true]` + "\n"
	report := runTraceHarnessReport(t, root, body)

	cluster := findCluster(t, report, "runner-failure")
	if !contains(cluster.Affected.Issues, "issue-1") {
		t.Fatalf("runner_end failure was not grouped: %#v", cluster.Affected.Issues)
	}
}

func TestTraceHarnessReportScriptRedactsGraphQLSelectionSetFromRunnerErrorExcerpt(t *testing.T) {
	root := repoRoot(t)
	body := `2026/06/18 09:00:00 event=runner_end task_id=issue-real issue_id=issue-real msg="runner failed" payload=map[ok:false error:GraphQL request: { issue(id:"secret-id") { id title } }]` + "\n"
	raw := runTraceHarnessReportRaw(t, root, body)
	report := parseTraceReport(t, raw)

	cluster := findCluster(t, report, "runner-failure")
	if got := cluster.Evidence[0].Metadata["ok"]; got != "false" {
		t.Fatalf("ok metadata after runner error GraphQL redaction = %q; want false; metadata=%#v", got, cluster.Evidence[0].Metadata)
	}
	text := string(raw)
	for _, leaked := range []string{"issue(id:", "secret-id", "{ id title }"} {
		if strings.Contains(text, leaked) {
			t.Fatalf("runner error selection-set GraphQL leaked %q in report:\n%s", leaked, raw)
		}
	}
	if !strings.Contains(text, "payload=[redacted-payload]") || cluster.Evidence[0].Metadata["error"] != "[redacted-error]" {
		t.Fatalf("runner error payload was not replaced with opaque redaction markers:\n%s", raw)
	}
}

func TestTraceHarnessReportScriptRedactsGraphQLFromRunnerErrorMetadata(t *testing.T) {
	root := repoRoot(t)
	body := `2026/06/18 09:00:00 event=runner_end task_id=issue-real issue_id=issue-real msg="runner failed" payload=map[ok:false error:Linear GraphQL failed: query SecretQuery($secret: String = "not-a-token-secret") { viewer { id } }]` + "\n"
	raw := runTraceHarnessReportRaw(t, root, body)
	report := parseTraceReport(t, raw)

	cluster := findCluster(t, report, "runner-failure")
	metadata := cluster.Evidence[0].Metadata
	if got := metadata["error"]; got != "[redacted-error]" {
		t.Fatalf("runner error metadata = %q; want opaque redacted marker; metadata=%#v", got, metadata)
	}
	text := string(raw)
	for _, leaked := range []string{"SecretQuery", "$secret", "not-a-token-secret", "viewer { id }"} {
		if strings.Contains(text, leaked) {
			t.Fatalf("runner error GraphQL metadata leaked %q in report:\n%s", leaked, raw)
		}
	}
}

func TestTraceHarnessReportScriptRedactsGraphQLBracketStringFromRunnerErrorMetadata(t *testing.T) {
	root := repoRoot(t)
	body := `2026/06/18 09:00:00 event=runner_end task_id=issue-real issue_id=issue-real msg="runner failed" payload=map[ok:false error:GraphQL request: query { issue(id:"]secret") { id title } }]` + "\n"
	raw := runTraceHarnessReportRaw(t, root, body)
	report := parseTraceReport(t, raw)

	cluster := findCluster(t, report, "runner-failure")
	if got := cluster.Evidence[0].Metadata["error"]; got != "[redacted-error]" {
		t.Fatalf("runner error bracket metadata = %q; want opaque redacted marker; metadata=%#v", got, cluster.Evidence[0].Metadata)
	}
	text := string(raw)
	for _, leaked := range []string{"]secret", "issue(id:", "{ id title }"} {
		if strings.Contains(text, leaked) {
			t.Fatalf("runner error bracket GraphQL metadata leaked %q in report:\n%s", leaked, raw)
		}
	}
}

func TestTraceHarnessReportScriptDoesNotPromoteGraphQLDefaultKeyShapeFromRunnerErrorMetadata(t *testing.T) {
	root := repoRoot(t)
	body := `2026/06/18 09:00:00 event=runner_end task_id=issue-real issue_id=issue-real msg="runner failed" payload=map[ok:false error:Linear GraphQL failed: query SecretQuery($secret: String = " session_id:fake-session") { viewer { id } }]` + "\n"
	raw := runTraceHarnessReportRaw(t, root, body)
	report := parseTraceReport(t, raw)

	cluster := findCluster(t, report, "runner-failure")
	if contains(cluster.Affected.Sessions, "fake-session") {
		t.Fatalf("GraphQL default session_id was promoted into affected sessions: %#v", cluster.Affected.Sessions)
	}
	metadata := cluster.Evidence[0].Metadata
	if got := metadata["error"]; got != "[redacted-error]" {
		t.Fatalf("runner error default metadata = %q; want opaque redacted marker; metadata=%#v", got, metadata)
	}
	if got := metadata["session_id"]; strings.Contains(got, "fake-session") {
		t.Fatalf("GraphQL default session_id was promoted into metadata: %#v", metadata)
	}
	if strings.Contains(string(raw), "fake-session") {
		t.Fatalf("GraphQL default key-shaped value leaked in report:\n%s", raw)
	}
}

func TestTraceHarnessReportScriptRedactsMultiWordRunnerErrorMetadata(t *testing.T) {
	root := repoRoot(t)
	body := `2026/06/18 09:00:00 event=runner_end task_id=issue-1 issue_id=issue-1 msg="runner failed" payload=map[ok:false error:agent crashed after checkout output_head:tail]` + "\n"
	report := runTraceHarnessReport(t, root, body)

	cluster := findCluster(t, report, "runner-failure")
	if got := cluster.Evidence[0].Metadata["error"]; got != "[redacted-error]" {
		t.Fatalf("multi-word runner error metadata = %q; want redacted marker; metadata=%#v", got, cluster.Evidence[0].Metadata)
	}
}

func TestTraceHarnessReportScriptDoesNotPromoteKeyShapedRunnerErrorText(t *testing.T) {
	root := repoRoot(t)
	body := `2026/06/18 09:00:00 event=runner_end task_id=issue-real issue_id=issue-real msg="runner failed" payload=map[ok:false error:agent crashed with session_id:fake-session pr_number:777 after checkout output_head:tail]` + "\n"
	report := runTraceHarnessReport(t, root, body)

	cluster := findCluster(t, report, "runner-failure")
	if contains(cluster.Affected.Sessions, "fake-session") {
		t.Fatalf("runner error text session_id was promoted: %#v", cluster.Affected.Sessions)
	}
	if contains(cluster.Affected.PullRequests, "777") {
		t.Fatalf("runner error text pr_number was promoted: %#v", cluster.Affected.PullRequests)
	}
	metadata := cluster.Evidence[0].Metadata
	for _, key := range []string{"session_id", "pr_number"} {
		if _, ok := metadata[key]; ok {
			t.Fatalf("runner error text %s was promoted into metadata: %#v", key, metadata)
		}
	}
	if got := metadata["error"]; got != "[redacted-error]" {
		t.Fatalf("runner error metadata = %q; want opaque redacted marker; metadata=%#v", got, metadata)
	}
}

func TestTraceHarnessReportScriptRedactsOrdinaryQueryWordRunnerErrorMetadata(t *testing.T) {
	root := repoRoot(t)
	body := `2026/06/18 09:00:00 event=runner_end task_id=issue-1 issue_id=issue-1 msg="runner failed" payload=map[ok:false error:database query failed after checkout output_head:tail]` + "\n"
	report := runTraceHarnessReport(t, root, body)

	cluster := findCluster(t, report, "runner-failure")
	if got := cluster.Evidence[0].Metadata["error"]; got != "[redacted-error]" {
		t.Fatalf("ordinary query-word runner error metadata = %q; want redacted marker; metadata=%#v", got, cluster.Evidence[0].Metadata)
	}
}

func TestTraceHarnessReportScriptRedactsOrdinaryQueryWordRunnerErrorBeforeFreeformBraces(t *testing.T) {
	root := repoRoot(t)
	body := `2026/06/18 09:00:00 event=runner_end task_id=issue-1 issue_id=issue-1 msg="runner failed" payload=map[ok:false error:database query failed after checkout output_head:agent {not graphql}]` + "\n"
	report := runTraceHarnessReport(t, root, body)

	cluster := findCluster(t, report, "runner-failure")
	if got := cluster.Evidence[0].Metadata["error"]; got != "[redacted-error]" {
		t.Fatalf("ordinary query-word runner error before freeform braces = %q; want redacted marker; metadata=%#v", got, cluster.Evidence[0].Metadata)
	}
}

func TestTraceHarnessReportScriptRedactsOrdinaryQueryWordRunnerErrorWithJSONBraces(t *testing.T) {
	root := repoRoot(t)
	body := `2026/06/18 09:00:00 event=runner_end task_id=issue-1 issue_id=issue-1 msg="runner failed" payload=map[ok:false error:database query failed: {"code":500} after checkout output_head:tail]` + "\n"
	report := runTraceHarnessReport(t, root, body)

	cluster := findCluster(t, report, "runner-failure")
	if got := cluster.Evidence[0].Metadata["error"]; got != "[redacted-error]" {
		t.Fatalf("ordinary query-word runner error with JSON braces = %q; want redacted marker; metadata=%#v", got, cluster.Evidence[0].Metadata)
	}
}

func TestTraceHarnessReportScriptIgnoresOutputFailureTextForSuccessfulRunnerEnd(t *testing.T) {
	root := repoRoot(t)
	body := `2026/06/18 09:00:00 event=runner_end task_id=issue-1 issue_id=issue-1 msg="runner completed" payload=map[ok:true output_head:agent-text-ok:false]` + "\n"
	report := runTraceHarnessReport(t, root, body)

	if len(report.Clusters) != 0 {
		t.Fatalf("successful runner_end should not be grouped as failure: %#v", report.Clusters)
	}
}

func TestTraceHarnessReportScriptTrustsRunnerEndOKMetadataOverFailureMessage(t *testing.T) {
	root := repoRoot(t)
	body := `2026/06/18 09:00:00 event=runner_end task_id=issue-1 issue_id=issue-1 msg="runner failed" payload=map[ok:true output_head:tail]` + "\n"
	report := runTraceHarnessReport(t, root, body)

	if len(report.Clusters) != 0 {
		t.Fatalf("runner_end ok:true should not be grouped as failure despite message text: %#v", report.Clusters)
	}
}

func TestTraceHarnessReportScriptDoesNotParseOutputTextAsPayloadFields(t *testing.T) {
	root := repoRoot(t)
	body := `2026/06/18 09:00:00 event=runner_end task_id=issue-1 issue_id=issue-1 msg="runner completed" payload=map[ok:true output_head:agent text ok:false runner failed]` + "\n"
	report := runTraceHarnessReport(t, root, body)

	if len(report.Clusters) != 0 {
		t.Fatalf("output text should not override runner_end payload fields: %#v", report.Clusters)
	}
}

func TestTraceHarnessReportScriptDoesNotPromoteOutputTextIdentifiers(t *testing.T) {
	root := repoRoot(t)
	body := `2026/06/18 09:00:00 event=runner_timeout task_id=issue-real issue_id=issue-real msg="timeout" payload=map[output_head:agent text session_id:fake-session pr_number:777 error:boom ok:false]` + "\n"
	report := runTraceHarnessReport(t, root, body)

	cluster := findCluster(t, report, "runner-timeout")
	if contains(cluster.Affected.Sessions, "fake-session") {
		t.Fatalf("output text session_id was promoted: %#v", cluster.Affected.Sessions)
	}
	if contains(cluster.Affected.PullRequests, "777") {
		t.Fatalf("output text pr_number was promoted: %#v", cluster.Affected.PullRequests)
	}
	metadata := cluster.Evidence[0].Metadata
	for _, key := range []string{"error", "ok"} {
		if _, ok := metadata[key]; ok {
			t.Fatalf("output text %s was promoted into metadata: %#v", key, metadata)
		}
	}
}

func TestTraceHarnessReportScriptStopsMetadataAtOutputText(t *testing.T) {
	root := repoRoot(t)
	body := `2026/06/18 09:00:00 event=runner_timeout task_id=issue-real issue_id=issue-real msg="timeout" payload=map[elapsed_ms:70000 output_head:agent [ text session_id:fake-session pr_number:777 error:boom ok:false timeout_ms:111 output_tail:tail text timeout_ms:999 timeout_ms:60000]` + "\n"
	report := runTraceHarnessReport(t, root, body)

	cluster := findCluster(t, report, "runner-timeout")
	if got := cluster.Evidence[0].Metadata["elapsed_ms"]; got != "70000" {
		t.Fatalf("elapsed_ms before output text = %q; want 70000; metadata=%#v", got, cluster.Evidence[0].Metadata)
	}
	if _, ok := cluster.Evidence[0].Metadata["timeout_ms"]; ok {
		t.Fatalf("timeout_ms after opaque output was promoted: %#v", cluster.Evidence[0].Metadata)
	}
	if _, ok := cluster.Evidence[0].Metadata["error"]; ok {
		t.Fatalf("output text error was promoted into metadata: %#v", cluster.Evidence[0].Metadata)
	}
	if _, ok := cluster.Evidence[0].Metadata["ok"]; ok {
		t.Fatalf("output text ok was promoted into metadata: %#v", cluster.Evidence[0].Metadata)
	}
	if contains(cluster.Affected.Sessions, "fake-session") || contains(cluster.Affected.PullRequests, "777") {
		t.Fatalf("output text identifiers were promoted: sessions=%#v prs=%#v", cluster.Affected.Sessions, cluster.Affected.PullRequests)
	}
}

func TestTraceHarnessReportScriptTreatsUnbalancedBracketScalarBeforeOpaqueAsOpaqueBoundary(t *testing.T) {
	root := repoRoot(t)
	body := `2026/06/18 09:00:00 event=runner_timeout task_id=issue-real issue_id=issue-real msg="timeout" payload=map[model:[codex output_head:SECRET timeout_ms:60000]` + "\n"
	report := runTraceHarnessReport(t, root, body)

	cluster := findCluster(t, report, "runner-timeout")
	if strings.Contains(mustJSON(t, report), "SECRET") {
		t.Fatalf("unbalanced safe scalar hid opaque output text: %#v", cluster)
	}
	if !strings.Contains(cluster.Evidence[0].Excerpt, "payload=[redacted-payload]") {
		t.Fatalf("unbalanced safe scalar excerpt was not redacted: %q", cluster.Evidence[0].Excerpt)
	}
	if _, ok := cluster.Evidence[0].Metadata["timeout_ms"]; ok {
		t.Fatalf("metadata after opaque output was promoted: %#v", cluster.Evidence[0].Metadata)
	}
}

func TestTraceHarnessReportScriptStopsMetadataAtMultilineOutputText(t *testing.T) {
	root := repoRoot(t)
	body := strings.Join([]string{
		`2026/06/18 09:00:00 event=runner_timeout task_id=issue-real issue_id=issue-real msg="timeout" payload=map[elapsed_ms:70000 output_head:agent text session_id:fake-session pr_number:777 ok:false`,
		`2026/06/18 09:00:01 output line with timeout_ms:111 but no event marker`,
		`output_tail:tail text timeout_ms:999 timeout_ms:60000]`,
	}, "\n") + "\n"
	report := runTraceHarnessReport(t, root, body)

	cluster := findCluster(t, report, "runner-timeout")
	if got := cluster.Evidence[0].Metadata["elapsed_ms"]; got != "70000" {
		t.Fatalf("elapsed_ms before multiline output text = %q; want 70000; metadata=%#v", got, cluster.Evidence[0].Metadata)
	}
	if _, ok := cluster.Evidence[0].Metadata["timeout_ms"]; ok {
		t.Fatalf("multiline timeout_ms after opaque output was promoted: %#v", cluster.Evidence[0].Metadata)
	}
	if contains(cluster.Affected.Sessions, "fake-session") || contains(cluster.Affected.PullRequests, "777") {
		t.Fatalf("multiline output text identifiers were promoted: sessions=%#v prs=%#v", cluster.Affected.Sessions, cluster.Affected.PullRequests)
	}
}

func TestTraceHarnessReportScriptKeepsEventShapedMultilineOutputInSameRecord(t *testing.T) {
	root := repoRoot(t)
	body := strings.Join([]string{
		`2026/06/18 09:00:00 event=runner_timeout task_id=issue-real issue_id=issue-real msg="timeout" payload=map[elapsed_ms:70000 output_head:agent text`,
		`2026/06/18 09:00:01 event=runner_timeout task_id=fake issue_id=fake msg="from output"`,
		`output_tail:tail text timeout_ms:60000]`,
	}, "\n") + "\n"
	report := runTraceHarnessReport(t, root, body)

	cluster := findCluster(t, report, "runner-timeout")
	if contains(cluster.Affected.Issues, "fake") || contains(cluster.Affected.Sessions, "fake") {
		t.Fatalf("event-shaped output line was promoted: issues=%#v sessions=%#v", cluster.Affected.Issues, cluster.Affected.Sessions)
	}
	if got := cluster.Evidence[0].Metadata["elapsed_ms"]; got != "70000" {
		t.Fatalf("event-shaped multiline elapsed_ms = %q; want 70000; metadata=%#v", got, cluster.Evidence[0].Metadata)
	}
}

func TestTraceHarnessReportScriptKeepsBracketedEventShapedOutputInSameRecord(t *testing.T) {
	root := repoRoot(t)
	body := strings.Join([]string{
		`2026/06/18 09:00:00 event=runner_timeout task_id=issue-real issue_id=issue-real msg="timeout" payload=map[elapsed_ms:70000 output_head:agent ] text`,
		`2026/06/18 09:00:01 event=runner_timeout task_id=fake issue_id=fake msg="from output"`,
		`output_tail:tail text timeout_ms:60000]`,
	}, "\n") + "\n"
	report := runTraceHarnessReport(t, root, body)

	cluster := findCluster(t, report, "runner-timeout")
	if contains(cluster.Affected.Issues, "fake") || contains(cluster.Affected.Runs, "fake") {
		t.Fatalf("bracketed event-shaped output line was promoted: issues=%#v runs=%#v", cluster.Affected.Issues, cluster.Affected.Runs)
	}
	if got := cluster.Evidence[0].Metadata["elapsed_ms"]; got != "70000" {
		t.Fatalf("bracketed event-shaped multiline elapsed_ms = %q; want 70000; metadata=%#v", got, cluster.Evidence[0].Metadata)
	}
}

func TestTraceHarnessReportScriptKeepsBracketTerminatedOutputLineInSameRecord(t *testing.T) {
	root := repoRoot(t)
	body := strings.Join([]string{
		`2026/06/18 09:00:00 event=runner_timeout task_id=issue-real issue_id=issue-real msg="timeout" payload=map[elapsed_ms:70000 output_head:agent text`,
		`stack frame [done]`,
		`2026/06/18 09:00:01 event=runner_timeout task_id=fake issue_id=fake msg="from output"`,
		`output_tail:tail text timeout_ms:60000]`,
	}, "\n") + "\n"
	report := runTraceHarnessReport(t, root, body)

	cluster := findCluster(t, report, "runner-timeout")
	if contains(cluster.Affected.Issues, "fake") || contains(cluster.Affected.Runs, "fake") {
		t.Fatalf("bracket-terminated output line promoted event-shaped output: issues=%#v runs=%#v", cluster.Affected.Issues, cluster.Affected.Runs)
	}
	if got := cluster.Evidence[0].Metadata["elapsed_ms"]; got != "70000" {
		t.Fatalf("bracket-terminated multiline elapsed_ms = %q; want 70000; metadata=%#v", got, cluster.Evidence[0].Metadata)
	}
}

func TestTraceHarnessReportScriptKeepsSingleLineBracketedOpaquePayloadOpen(t *testing.T) {
	root := repoRoot(t)
	body := strings.Join([]string{
		`2026/06/18 09:00:00 event=runner_timeout task_id=issue-real issue_id=issue-real msg="timeout" payload=map[elapsed_ms:70000 output_head:stack [done]`,
		`2026/06/18 09:00:01 event=runner_timeout task_id=fake issue_id=fake msg="from output"`,
		`output_tail:tail text timeout_ms:60000]`,
	}, "\n") + "\n"
	report := runTraceHarnessReport(t, root, body)

	cluster := findCluster(t, report, "runner-timeout")
	if contains(cluster.Affected.Issues, "fake") || contains(cluster.Affected.Runs, "fake") {
		t.Fatalf("single-line bracketed opaque payload promoted fake event: issues=%#v runs=%#v", cluster.Affected.Issues, cluster.Affected.Runs)
	}
	if got := cluster.Evidence[0].Metadata["elapsed_ms"]; got != "70000" {
		t.Fatalf("single-line bracketed opaque payload elapsed_ms = %q; want 70000; metadata=%#v", got, cluster.Evidence[0].Metadata)
	}
}

func TestTraceHarnessReportScriptKeepsBareBracketOutputLineInSameRecordWhenTailFollows(t *testing.T) {
	root := repoRoot(t)
	body := strings.Join([]string{
		`2026/06/18 09:00:00 event=runner_timeout task_id=issue-real issue_id=issue-real msg="timeout" payload=map[elapsed_ms:70000 output_head:agent text`,
		`stack frame done]`,
		`2026/06/18 09:00:01 event=runner_timeout task_id=fake issue_id=fake msg="from output"`,
		`output_tail:tail text timeout_ms:60000]`,
	}, "\n") + "\n"
	report := runTraceHarnessReport(t, root, body)

	cluster := findCluster(t, report, "runner-timeout")
	if contains(cluster.Affected.Issues, "fake") || contains(cluster.Affected.Runs, "fake") {
		t.Fatalf("bare bracket output line promoted event-shaped output: issues=%#v runs=%#v", cluster.Affected.Issues, cluster.Affected.Runs)
	}
	if got := cluster.Evidence[0].Metadata["elapsed_ms"]; got != "70000" {
		t.Fatalf("bare bracket output multiline elapsed_ms = %q; want 70000; metadata=%#v", got, cluster.Evidence[0].Metadata)
	}
}

func TestTraceHarnessReportScriptKeepsPayloadEventShapedOutputAfterBareBracketLineUntilTail(t *testing.T) {
	root := repoRoot(t)
	body := strings.Join([]string{
		`2026/06/18 09:00:00 event=runner_timeout task_id=issue-real issue_id=issue-real msg="timeout" payload=map[elapsed_ms:70000 output_head:agent text`,
		`stack frame done]`,
		`2026/06/18 09:00:01 event=runner_timeout task_id=fake issue_id=fake msg="from output" payload=map[timeout_ms:123]`,
		`output_tail:tail text timeout_ms:60000]`,
	}, "\n") + "\n"
	report := runTraceHarnessReport(t, root, body)

	cluster := findCluster(t, report, "runner-timeout")
	if contains(cluster.Affected.Issues, "fake") || contains(cluster.Affected.Runs, "fake") {
		t.Fatalf("payload-bearing event-shaped output line was promoted: issues=%#v runs=%#v", cluster.Affected.Issues, cluster.Affected.Runs)
	}
	if got := cluster.Evidence[0].Metadata["elapsed_ms"]; got != "70000" {
		t.Fatalf("payload-bearing event-shaped output elapsed_ms = %q; want 70000; metadata=%#v", got, cluster.Evidence[0].Metadata)
	}
}

func TestTraceHarnessReportScriptKeepsBareBracketOutputLineBeforePayloadlessEventText(t *testing.T) {
	root := repoRoot(t)
	body := strings.Join([]string{
		`2026/06/18 09:00:00 event=runner_timeout task_id=issue-real issue_id=issue-real msg="timeout" payload=map[elapsed_ms:70000 output_head:agent text`,
		`stack frame done]`,
		`2026/06/18 09:00:01 event=runner_timeout task_id=fake issue_id=fake msg="from output"`,
		`more output after event-shaped text`,
		`output_tail:tail text timeout_ms:60000]`,
	}, "\n") + "\n"
	report := runTraceHarnessReport(t, root, body)

	cluster := findCluster(t, report, "runner-timeout")
	if contains(cluster.Affected.Issues, "fake") || contains(cluster.Affected.Runs, "fake") {
		t.Fatalf("payloadless event-shaped output line was promoted: issues=%#v runs=%#v", cluster.Affected.Issues, cluster.Affected.Runs)
	}
	if got := cluster.Evidence[0].Metadata["elapsed_ms"]; got != "70000" {
		t.Fatalf("payloadless event-shaped output elapsed_ms = %q; want 70000; metadata=%#v", got, cluster.Evidence[0].Metadata)
	}
}

func TestTraceHarnessReportScriptKeepsOrdinaryOutputAfterBareBracketLineUntilTail(t *testing.T) {
	root := repoRoot(t)
	body := strings.Join([]string{
		`2026/06/18 09:00:00 event=runner_timeout task_id=issue-real issue_id=issue-real msg="timeout" payload=map[elapsed_ms:70000 output_head:first line`,
		`stack frame done]`,
		`ordinary output after bracket`,
		`output_tail:tail timeout_ms:60000]`,
	}, "\n") + "\n"
	report := runTraceHarnessReport(t, root, body)

	cluster := findCluster(t, report, "runner-timeout")
	if got := cluster.Evidence[0].Metadata["elapsed_ms"]; got != "70000" {
		t.Fatalf("ordinary output after bracket elapsed_ms = %q; want 70000; metadata=%#v", got, cluster.Evidence[0].Metadata)
	}
}

func TestTraceHarnessReportScriptDoesNotPromoteEventAfterKeyShapedOutputLine(t *testing.T) {
	root := repoRoot(t)
	body := strings.Join([]string{
		`2026/06/18 09:00:00 event=runner_timeout task_id=issue-real issue_id=issue-real msg="timeout" payload=map[elapsed_ms:70000 output_head:first line`,
		`timeout_ms:123]`,
		`2026/06/18 09:00:01 event=runner_timeout task_id=fake issue_id=fake msg="from output"`,
		`output_tail:tail text timeout_ms:60000]`,
	}, "\n") + "\n"
	report := runTraceHarnessReport(t, root, body)

	cluster := findCluster(t, report, "runner-timeout")
	if contains(cluster.Affected.Issues, "fake") || contains(cluster.Affected.Runs, "fake") {
		t.Fatalf("event after key-shaped output line was promoted: issues=%#v runs=%#v", cluster.Affected.Issues, cluster.Affected.Runs)
	}
	if got := cluster.Evidence[0].Metadata["elapsed_ms"]; got != "70000" {
		t.Fatalf("elapsed_ms before key-shaped output line = %q; want 70000; metadata=%#v", got, cluster.Evidence[0].Metadata)
	}
}

func TestTraceHarnessReportScriptDoesNotSwallowNextEventAfterBracketedOutputHeadCloses(t *testing.T) {
	root := repoRoot(t)
	body := strings.Join([]string{
		`2026/06/18 09:00:00 event=runner_end task_id=issue-ok issue_id=issue-ok msg="done" payload=map[ok:true output_head:first line`,
		`[INFO]]`,
		`2026/06/18 09:00:01 event=runner_timeout task_id=issue-timeout issue_id=issue-timeout msg="timeout" payload=map[timeout_ms:60000]`,
	}, "\n") + "\n"
	report := runTraceHarnessReport(t, root, body)

	cluster := findCluster(t, report, "runner-timeout")
	if !contains(cluster.Affected.Issues, "issue-timeout") || !contains(cluster.Affected.Runs, "issue-timeout") {
		t.Fatalf("timeout after bracketed output_head close was swallowed: issues=%#v runs=%#v", cluster.Affected.Issues, cluster.Affected.Runs)
	}
	if got := cluster.Evidence[0].Metadata["timeout_ms"]; got != "60000" {
		t.Fatalf("timeout after bracketed output_head close timeout_ms = %q; want 60000; metadata=%#v", got, cluster.Evidence[0].Metadata)
	}
}

func TestTraceHarnessReportScriptDoesNotSwallowNextEventAfterSpacedBracketOutputHeadCloses(t *testing.T) {
	root := repoRoot(t)
	body := strings.Join([]string{
		`2026/06/18 09:00:00 event=runner_end task_id=issue-ok issue_id=issue-ok msg="done" payload=map[ok:true output_head:first line`,
		`[INFO] ]`,
		`2026/06/18 09:00:01 event=runner_timeout task_id=issue-timeout issue_id=issue-timeout msg="timeout" payload=map[timeout_ms:60000]`,
	}, "\n") + "\n"
	report := runTraceHarnessReport(t, root, body)

	cluster := findCluster(t, report, "runner-timeout")
	if !contains(cluster.Affected.Issues, "issue-timeout") || !contains(cluster.Affected.Runs, "issue-timeout") {
		t.Fatalf("timeout after spaced bracket output_head close was swallowed: issues=%#v runs=%#v", cluster.Affected.Issues, cluster.Affected.Runs)
	}
	if got := cluster.Evidence[0].Metadata["timeout_ms"]; got != "60000" {
		t.Fatalf("timeout after spaced bracket output_head close timeout_ms = %q; want 60000; metadata=%#v", got, cluster.Evidence[0].Metadata)
	}
}

func TestTraceHarnessReportScriptDoesNotPromoteAmbiguousEventAfterOutputHeadOnlyMultiline(t *testing.T) {
	root := repoRoot(t)
	body := strings.Join([]string{
		`2026/06/18 09:00:00 event=runner_end task_id=issue-ok issue_id=issue-ok msg="done" payload=map[ok:true output_head:first line`,
		`last line]`,
		`2026/06/18 09:00:01 event=runner_timeout task_id=fake issue_id=fake msg="from opaque output" payload=map[timeout_ms:60000]`,
	}, "\n") + "\n"
	report := runTraceHarnessReport(t, root, body)

	if len(report.Clusters) != 0 {
		t.Fatalf("ambiguous event-shaped output after single-bracket close was promoted: %#v", report.Clusters)
	}
}

func TestTraceHarnessReportScriptDoesNotPromoteAmbiguousClosedNextEventBeforeOrdinaryLog(t *testing.T) {
	root := repoRoot(t)
	body := strings.Join([]string{
		`2026/06/18 09:00:00 event=runner_end task_id=issue-ok issue_id=issue-ok msg="done" payload=map[ok:true output_head:first line`,
		`last line]`,
		`2026/06/18 09:00:01 event=runner_timeout task_id=issue-timeout issue_id=issue-timeout msg="timeout" payload=map[timeout_ms:60000]`,
		`ordinary log after timeout`,
	}, "\n") + "\n"
	report := runTraceHarnessReport(t, root, body)

	if len(report.Clusters) != 0 {
		t.Fatalf("ambiguous closed event-shaped output before ordinary log was promoted: %#v", report.Clusters)
	}
}

func TestTraceHarnessReportScriptDoesNotPromoteAmbiguousEventAfterTimestampedNonEventLog(t *testing.T) {
	root := repoRoot(t)
	body := strings.Join([]string{
		`2026/06/18 09:00:00 event=runner_end task_id=issue-ok issue_id=issue-ok msg="done" payload=map[ok:true output_head:first line`,
		`last line]`,
		`2026/06/18 09:00:00 state HTTP server listening on http://127.0.0.1:8080`,
		`2026/06/18 09:00:01 event=runner_timeout task_id=issue-timeout issue_id=issue-timeout msg="timeout" payload=map[timeout_ms:60000]`,
	}, "\n") + "\n"
	report := runTraceHarnessReport(t, root, body)

	if len(report.Clusters) != 0 {
		t.Fatalf("ambiguous event-shaped output after timestamped non-event text was promoted: %#v", report.Clusters)
	}
}

func TestTraceHarnessReportScriptDoesNotPromoteAmbiguousMultilineEventAfterOutputHeadOnlyMultiline(t *testing.T) {
	root := repoRoot(t)
	body := strings.Join([]string{
		`2026/06/18 09:00:00 event=runner_end task_id=issue-ok issue_id=issue-ok msg="done" payload=map[ok:true output_head:first line`,
		`last line]`,
		`2026/06/18 09:00:01 event=runner_timeout task_id=issue-timeout issue_id=issue-timeout msg="timeout" payload=map[elapsed_ms:60000 output_head:timeout first line`,
		`output_tail:timeout tail timeout_ms:60000]`,
	}, "\n") + "\n"
	report := runTraceHarnessReport(t, root, body)

	if len(report.Clusters) != 0 {
		t.Fatalf("ambiguous multiline event-shaped output after single-bracket close was promoted: %#v", report.Clusters)
	}
}

func TestTraceHarnessReportScriptIgnoresContinuationTextAfterClosedPayload(t *testing.T) {
	root := repoRoot(t)
	body := strings.Join([]string{
		`2026/06/18 09:00:00 event=runner_timeout task_id=issue-real issue_id=issue-real msg="timeout" payload=map[elapsed_ms:70000 output_head:agent timeout_ms:60000]`,
		`unrelated diagnostic timeout_ms:111`,
	}, "\n") + "\n"
	report := runTraceHarnessReport(t, root, body)

	cluster := findCluster(t, report, "runner-timeout")
	if got := cluster.Evidence[0].Metadata["elapsed_ms"]; got != "70000" {
		t.Fatalf("elapsed_ms before closed payload continuation = %q; want 70000; metadata=%#v", got, cluster.Evidence[0].Metadata)
	}
	if _, ok := cluster.Evidence[0].Metadata["timeout_ms"]; ok {
		t.Fatalf("timeout_ms after opaque output was promoted: %#v", cluster.Evidence[0].Metadata)
	}
}

func TestTraceHarnessReportScriptDoesNotPromoteNestedPayloadIdentifiers(t *testing.T) {
	root := repoRoot(t)
	body := `2026/06/18 09:00:00 event=unsupported_tool_call task_id=issue-real issue_id=issue-real session_id=session-real msg="tool" payload=map[tool:missing arguments:map[output_head:agent-text session_id:fake-session pr_number:777] pr_number:938]` + "\n"
	report := runTraceHarnessReport(t, root, body)

	cluster := findCluster(t, report, "tool-unsupported")
	if contains(cluster.Affected.Sessions, "fake-session") || !contains(cluster.Affected.Sessions, "session-real") {
		t.Fatalf("nested payload session_id was promoted: %#v", cluster.Affected.Sessions)
	}
	if contains(cluster.Affected.PullRequests, "777") || contains(cluster.Affected.PullRequests, "938") {
		t.Fatalf("nested payload pr_number was promoted: %#v", cluster.Affected.PullRequests)
	}
}

func TestTraceHarnessReportScriptRedactsUnsupportedToolCallArgumentsFromExcerpt(t *testing.T) {
	root := repoRoot(t)
	body := `2026/06/18 09:00:00 event=unsupported_tool_call task_id=issue-real issue_id=issue-real msg="tool" payload=map[pr_number:938 tool:missing_graphql arguments:map[query:mutation{issueUpdate(input:{id:"secret"})} variables:map[token:abc]]]` + "\n"
	raw := runTraceHarnessReportRaw(t, root, body)
	report := parseTraceReport(t, raw)

	cluster := findCluster(t, report, "tool-unsupported")
	if !contains(cluster.Affected.PullRequests, "938") {
		t.Fatalf("top-level pr_number was lost while redacting arguments: %#v", cluster.Affected.PullRequests)
	}
	text := string(raw)
	for _, leaked := range []string{"mutation{issueUpdate", `id:\"secret\"`, "variables:map"} {
		if strings.Contains(text, leaked) {
			t.Fatalf("unsupported tool arguments leaked %q in report:\n%s", leaked, raw)
		}
	}
	if !strings.Contains(text, "payload=[redacted-payload]") {
		t.Fatalf("unsupported tool arguments were not replaced with a redaction marker:\n%s", raw)
	}
}

func TestTraceHarnessReportScriptRedactsUnsupportedToolCallArgumentsWithBracketText(t *testing.T) {
	root := repoRoot(t)
	body := `2026/06/18 09:00:00 event=unsupported_tool_call task_id=issue-real issue_id=issue-real msg="tool" payload=map[tool:missing_graphql arguments:map[query:query { issue(id: "]secret") { id } } variables:map[token:abc]] pr_number:938]` + "\n"
	raw := runTraceHarnessReportRaw(t, root, body)
	report := parseTraceReport(t, raw)

	cluster := findCluster(t, report, "tool-unsupported")
	if !contains(cluster.Affected.Issues, "issue-real") {
		t.Fatalf("issue id was lost while redacting bracketed arguments: %#v", cluster.Affected.Issues)
	}
	text := string(raw)
	for _, leaked := range []string{"issue(id:", "]secret", "variables:map"} {
		if strings.Contains(text, leaked) {
			t.Fatalf("unsupported tool arguments with bracket text leaked %q in report:\n%s", leaked, raw)
		}
	}
	if !strings.Contains(text, "payload=[redacted-payload]") {
		t.Fatalf("unsupported tool bracket arguments were not replaced with a redaction marker:\n%s", raw)
	}
}

func TestTraceHarnessReportScriptStopsAtBracketedOpaquePayload(t *testing.T) {
	root := repoRoot(t)
	body := `2026/06/18 09:00:00 event=unsupported_tool_call task_id=issue-real issue_id=issue-real msg="tool" payload=map[pr_number:938 arguments:map[text:hello] session_id:fake pr_number:777] tool:missing]` + "\n"
	raw := runTraceHarnessReportRaw(t, root, body)
	report := parseTraceReport(t, raw)

	cluster := findCluster(t, report, "tool-unsupported")
	if !contains(cluster.Affected.PullRequests, "938") {
		t.Fatalf("trusted pr_number before opaque boundary was lost: %#v", cluster.Affected.PullRequests)
	}
	if contains(cluster.Affected.Sessions, "fake") || contains(cluster.Affected.PullRequests, "777]") {
		t.Fatalf("bracketed opaque payload fields were promoted: sessions=%#v prs=%#v", cluster.Affected.Sessions, cluster.Affected.PullRequests)
	}
	text := string(raw)
	for _, leaked := range []string{"hello]", "session_id:fake", "777]"} {
		if strings.Contains(text, leaked) {
			t.Fatalf("bracketed opaque payload leaked %q in report:\n%s", leaked, raw)
		}
	}
	if !strings.Contains(text, "payload=[redacted-payload]") {
		t.Fatalf("bracketed opaque payload was not replaced with a redaction marker:\n%s", raw)
	}
}

func TestTraceHarnessReportScriptStopsAtScalarUnsupportedToolArguments(t *testing.T) {
	root := repoRoot(t)
	body := `2026/06/18 09:00:00 event=unsupported_tool_call task_id=issue-real issue_id=issue-real msg="tool" payload=map[arguments:not-an-object session_id:payload-session pr_number:938 tool:missing]` + "\n"
	raw := runTraceHarnessReportRaw(t, root, body)
	report := parseTraceReport(t, raw)

	cluster := findCluster(t, report, "tool-unsupported")
	if contains(cluster.Affected.Sessions, "payload-session") {
		t.Fatalf("scalar arguments trailing session_id was promoted: %#v", cluster.Affected.Sessions)
	}
	if contains(cluster.Affected.PullRequests, "938") {
		t.Fatalf("scalar arguments trailing pr_number was promoted: %#v", cluster.Affected.PullRequests)
	}
	text := string(raw)
	for _, leaked := range []string{"not-an-object", "payload-session", "938", "tool:missing"} {
		if strings.Contains(text, leaked) {
			t.Fatalf("scalar unsupported tool arguments leaked %q in report:\n%s", leaked, raw)
		}
	}
	if !strings.Contains(text, "payload=[redacted-payload]") {
		t.Fatalf("scalar unsupported tool arguments leaked in report:\n%s", raw)
	}
}

func TestTraceHarnessReportScriptDoesNotPromoteUnsupportedToolArgumentsToMetadata(t *testing.T) {
	root := repoRoot(t)
	body := `2026/06/18 09:00:00 event=unsupported_tool_call task_id=issue-real issue_id=issue-real msg="tool" payload=map[tool:missing_graphql arguments:map[query:query { issue(id: "] SECRET_METADATA") { id } } session_id:fake-session error:SECRET_METADATA] pr_number:938]` + "\n"
	raw := runTraceHarnessReportRaw(t, root, body)
	report := parseTraceReport(t, raw)

	cluster := findCluster(t, report, "tool-unsupported")
	if contains(cluster.Affected.Sessions, "fake-session") {
		t.Fatalf("unsupported tool argument session_id was promoted: %#v", cluster.Affected.Sessions)
	}
	metadata := cluster.Evidence[0].Metadata
	if metadata["session_id"] == "fake-session" || metadata["error"] == "SECRET_METADATA" {
		t.Fatalf("unsupported tool arguments were promoted into metadata: %#v", metadata)
	}
	text := string(raw)
	for _, leaked := range []string{"fake-session", "SECRET_METADATA"} {
		if strings.Contains(text, leaked) {
			t.Fatalf("unsupported tool arguments leaked %q in report:\n%s", leaked, raw)
		}
	}
	if !strings.Contains(text, "payload=[redacted-payload]") {
		t.Fatalf("unsupported tool arguments were not replaced with a redaction marker:\n%s", raw)
	}
}

func TestTraceHarnessReportScriptRedactsMalformedRawGraphQLPayloadFromExcerpt(t *testing.T) {
	root := repoRoot(t)
	body := `2026/06/18 09:00:00 event=malformed task_id=issue-real issue_id=issue-real msg="bad protocol" payload=map[raw:{"method":"item/tool/call","arguments":{"query":"mutation { issueUpdate(input:{id:\"secret\"}) { id } }"}}]` + "\n"
	raw := runTraceHarnessReportRaw(t, root, body)
	report := parseTraceReport(t, raw)

	cluster := findCluster(t, report, "malformed-protocol")
	if !contains(cluster.Affected.Issues, "issue-real") {
		t.Fatalf("issue id was lost while redacting malformed payload: %#v", cluster.Affected.Issues)
	}
	text := string(raw)
	for _, leaked := range []string{"issueUpdate", `id:\\\"secret\\\"`, "item/tool/call"} {
		if strings.Contains(text, leaked) {
			t.Fatalf("malformed raw GraphQL payload leaked %q in report:\n%s", leaked, raw)
		}
	}
	if !strings.Contains(text, "payload=[redacted-malformed-payload]") {
		t.Fatalf("malformed raw payload was not replaced with a redaction marker:\n%s", raw)
	}
}

func TestTraceHarnessReportScriptRedactsGraphQLFromRunnerOutputExcerpt(t *testing.T) {
	root := repoRoot(t)
	body := `2026/06/18 09:00:00 event=runner_timeout task_id=issue-real issue_id=issue-real msg="timeout" payload=map[output_head:mutation { issueUpdate(input:{id:"secret"}) { id } } timeout_ms:60000]` + "\n"
	raw := runTraceHarnessReportRaw(t, root, body)
	report := parseTraceReport(t, raw)

	findCluster(t, report, "runner-timeout")
	text := string(raw)
	for _, leaked := range []string{"issueUpdate", `id:\"secret\"`, "mutation {"} {
		if strings.Contains(text, leaked) {
			t.Fatalf("runner output GraphQL leaked %q in report:\n%s", leaked, raw)
		}
	}
	if !strings.Contains(text, "payload=[redacted-payload]") {
		t.Fatalf("runner output GraphQL was not replaced with a redaction marker:\n%s", raw)
	}
}

func TestTraceHarnessReportScriptRedactsNamedGraphQLFromRunnerOutputExcerpt(t *testing.T) {
	root := repoRoot(t)
	body := `2026/06/18 09:00:00 event=runner_timeout task_id=issue-real issue_id=issue-real msg="timeout" payload=map[output_head:mutation IssueUpdate($input: IssueUpdateInput!) { issueUpdate(input:$input) { id } } timeout_ms:60000]` + "\n"
	raw := runTraceHarnessReportRaw(t, root, body)
	report := parseTraceReport(t, raw)

	findCluster(t, report, "runner-timeout")
	text := string(raw)
	for _, leaked := range []string{"IssueUpdate", "issueUpdate", "$input"} {
		if strings.Contains(text, leaked) {
			t.Fatalf("named runner output GraphQL leaked %q in report:\n%s", leaked, raw)
		}
	}
	if !strings.Contains(text, "payload=[redacted-payload]") {
		t.Fatalf("named runner output GraphQL was not replaced with a redaction marker:\n%s", raw)
	}
}

func TestTraceHarnessReportScriptRedactsMinifiedScalarOnlyGraphQLFromRunnerOutputExcerpt(t *testing.T) {
	root := repoRoot(t)
	body := `2026/06/18 09:00:00 event=runner_timeout task_id=issue-real issue_id=issue-real msg="timeout" payload=map[output_head:query Secret($token:String="not-a-token-secret"){id} timeout_ms:60000]` + "\n"
	raw := runTraceHarnessReportRaw(t, root, body)
	report := parseTraceReport(t, raw)

	findCluster(t, report, "runner-timeout")
	text := string(raw)
	for _, leaked := range []string{"query Secret", "not-a-token-secret", "{id}"} {
		if strings.Contains(text, leaked) {
			t.Fatalf("minified scalar-only GraphQL leaked %q in report:\n%s", leaked, raw)
		}
	}
	if !strings.Contains(text, "payload=[redacted-payload]") {
		t.Fatalf("minified scalar-only GraphQL was not replaced with a redaction marker:\n%s", raw)
	}
}

func TestTraceHarnessReportScriptRedactsLongHeaderGraphQLFromRunnerOutputExcerpt(t *testing.T) {
	root := repoRoot(t)
	var vars []string
	for idx := 0; idx < 40; idx++ {
		vars = append(vars, fmt.Sprintf("$v%d: String", idx))
	}
	body := `2026/06/18 09:00:00 event=runner_timeout task_id=issue-real issue_id=issue-real msg="timeout" payload=map[output_head:mutation LongOperation(` + strings.Join(vars, ", ") + `) { issueUpdate(input:{id:"secret-long"}) { id } } timeout_ms:60000]` + "\n"
	raw := runTraceHarnessReportRaw(t, root, body)
	report := parseTraceReport(t, raw)

	findCluster(t, report, "runner-timeout")
	text := string(raw)
	for _, leaked := range []string{"LongOperation", "issueUpdate", "secret-long"} {
		if strings.Contains(text, leaked) {
			t.Fatalf("long-header runner output GraphQL leaked %q in report:\n%s", leaked, raw)
		}
	}
	if !strings.Contains(text, "payload=[redacted-payload]") {
		t.Fatalf("long-header runner output GraphQL was not replaced with a redaction marker:\n%s", raw)
	}
}

func TestTraceHarnessReportScriptRedactsGraphQLFragmentFromRunnerOutputExcerpt(t *testing.T) {
	root := repoRoot(t)
	body := `2026/06/18 09:00:00 event=runner_timeout task_id=issue-real issue_id=issue-real msg="timeout" payload=map[output_head:fragment SecretFields on Issue { id title privateNote } mutation M { issueUpdate(input:{id:"secret-frag"}) { id } } timeout_ms:60000]` + "\n"
	raw := runTraceHarnessReportRaw(t, root, body)
	report := parseTraceReport(t, raw)

	findCluster(t, report, "runner-timeout")
	text := string(raw)
	for _, leaked := range []string{"fragment SecretFields", "privateNote", "secret-frag"} {
		if strings.Contains(text, leaked) {
			t.Fatalf("fragment runner output GraphQL leaked %q in report:\n%s", leaked, raw)
		}
	}
	if !strings.Contains(text, "payload=[redacted-payload]") {
		t.Fatalf("fragment runner output GraphQL was not replaced with a redaction marker:\n%s", raw)
	}
}

func TestTraceHarnessReportScriptRedactsGraphQLVariablesFromRunnerOutputExcerpt(t *testing.T) {
	root := repoRoot(t)
	body := `2026/06/18 09:00:00 event=runner_timeout task_id=issue-real issue_id=issue-real msg="timeout" payload=map[output_head:mutation { issueUpdate(input:{id:"id-public"}) { id } } variables:{"private":"not-a-token-secret"} timeout_ms:60000]` + "\n"
	raw := runTraceHarnessReportRaw(t, root, body)
	report := parseTraceReport(t, raw)

	findCluster(t, report, "runner-timeout")
	text := string(raw)
	for _, leaked := range []string{"issueUpdate", "variables:", "not-a-token-secret"} {
		if strings.Contains(text, leaked) {
			t.Fatalf("runner output GraphQL variables leaked %q in report:\n%s", leaked, raw)
		}
	}
	if !strings.Contains(text, "payload=[redacted-payload]") {
		t.Fatalf("runner output GraphQL variables were not replaced with a redaction marker:\n%s", raw)
	}
}

func TestTraceHarnessReportScriptRedactsVariablesOnlyGraphQLRunnerOutputExcerpt(t *testing.T) {
	root := repoRoot(t)
	body := `2026/06/18 09:00:00 event=runner_timeout task_id=issue-real issue_id=issue-real msg="timeout" payload=map[output_head:variables:{"private":"not-a-token-secret"} timeout_ms:60000]` + "\n"
	raw := runTraceHarnessReportRaw(t, root, body)
	report := parseTraceReport(t, raw)

	findCluster(t, report, "runner-timeout")
	text := string(raw)
	for _, leaked := range []string{"variables:", "not-a-token-secret"} {
		if strings.Contains(text, leaked) {
			t.Fatalf("variables-only GraphQL leaked %q in report:\n%s", leaked, raw)
		}
	}
	if !strings.Contains(text, "payload=[redacted-payload]") {
		t.Fatalf("variables-only GraphQL was not replaced with a redaction marker:\n%s", raw)
	}
}

func TestTraceHarnessReportScriptRedactsJSONVariablesOnlyGraphQLRunnerOutputExcerpt(t *testing.T) {
	root := repoRoot(t)
	body := `2026/06/18 09:00:00 event=runner_timeout task_id=issue-real issue_id=issue-real msg="timeout" payload=map[output_head:{"arguments":{"variables":{"private":"not-a-token-secret"}}} timeout_ms:60000]` + "\n"
	raw := runTraceHarnessReportRaw(t, root, body)
	report := parseTraceReport(t, raw)

	findCluster(t, report, "runner-timeout")
	text := string(raw)
	for _, leaked := range []string{"variables", "not-a-token-secret"} {
		if strings.Contains(text, leaked) {
			t.Fatalf("JSON variables-only GraphQL leaked %q in report:\n%s", leaked, raw)
		}
	}
	if !strings.Contains(text, "payload=[redacted-payload]") {
		t.Fatalf("JSON variables-only GraphQL was not replaced with a redaction marker:\n%s", raw)
	}
}

func TestTraceHarnessReportScriptRedactsEscapedJSONVariablesOnlyGraphQLRunnerOutputExcerpt(t *testing.T) {
	root := repoRoot(t)
	body := `2026/06/18 09:00:00 event=runner_timeout task_id=issue-real issue_id=issue-real msg="timeout" payload=map[output_head:{\"arguments\":{\"variables\":{\"private\":\"not-a-token-secret\"}}} timeout_ms:60000]` + "\n"
	raw := runTraceHarnessReportRaw(t, root, body)
	report := parseTraceReport(t, raw)

	findCluster(t, report, "runner-timeout")
	text := string(raw)
	for _, leaked := range []string{`\"variables\"`, "not-a-token-secret"} {
		if strings.Contains(text, leaked) {
			t.Fatalf("escaped JSON variables-only GraphQL leaked %q in report:\n%s", leaked, raw)
		}
	}
	if !strings.Contains(text, "payload=[redacted-payload]") {
		t.Fatalf("escaped JSON variables-only GraphQL was not replaced with a redaction marker:\n%s", raw)
	}
}

func TestTraceHarnessReportScriptRedactsJSONVariablesBeforeQueryRunnerOutputExcerpt(t *testing.T) {
	root := repoRoot(t)
	body := `2026/06/18 09:00:00 event=runner_timeout task_id=issue-real issue_id=issue-real msg="timeout" payload=map[output_head:{"arguments":{"variables":{"private":"not-a-token-secret"},"query":"mutation { issueUpdate(input:{id:\"id-public\"}) { id } }"}} timeout_ms:60000]` + "\n"
	raw := runTraceHarnessReportRaw(t, root, body)
	report := parseTraceReport(t, raw)

	findCluster(t, report, "runner-timeout")
	text := string(raw)
	for _, leaked := range []string{"variables", "not-a-token-secret", "issueUpdate", "id-public"} {
		if strings.Contains(text, leaked) {
			t.Fatalf("JSON variables-before-query GraphQL leaked %q in report:\n%s", leaked, raw)
		}
	}
	if !strings.Contains(text, "payload=[redacted-payload]") {
		t.Fatalf("JSON variables-before-query GraphQL was not replaced with a redaction marker:\n%s", raw)
	}
}

func TestTraceHarnessReportScriptDoesNotStopGraphQLRedactionAtAliasNamedTimeoutMS(t *testing.T) {
	root := repoRoot(t)
	body := `2026/06/18 09:00:00 event=runner_timeout task_id=issue-real issue_id=issue-real msg="timeout" payload=map[output_head:mutation { timeout_ms:issue(id:"secret-id"){id}}]` + "\n"
	raw := runTraceHarnessReportRaw(t, root, body)
	report := parseTraceReport(t, raw)

	findCluster(t, report, "runner-timeout")
	text := string(raw)
	for _, leaked := range []string{"timeout_ms:issue", "issue(id:", "secret-id", "{id}"} {
		if strings.Contains(text, leaked) {
			t.Fatalf("GraphQL alias named timeout_ms leaked %q in report:\n%s", leaked, raw)
		}
	}
	if !strings.Contains(text, "payload=[redacted-payload]") {
		t.Fatalf("GraphQL alias named timeout_ms was not replaced with a redaction marker:\n%s", raw)
	}
}

func TestTraceHarnessReportScriptRedactsGraphQLStringBraceFromRunnerOutputExcerpt(t *testing.T) {
	root := repoRoot(t)
	body := `2026/06/18 09:00:00 event=runner_timeout task_id=issue-real issue_id=issue-real msg="timeout" payload=map[output_head:mutation { issue(id:"} SECRET_AFTER_BRACE") { id title } } timeout_ms:60000]` + "\n"
	raw := runTraceHarnessReportRaw(t, root, body)
	report := parseTraceReport(t, raw)

	findCluster(t, report, "runner-timeout")
	text := string(raw)
	for _, leaked := range []string{"SECRET_AFTER_BRACE", "{ id title }", "issue(id:"} {
		if strings.Contains(text, leaked) {
			t.Fatalf("runner output GraphQL string brace leaked %q in report:\n%s", leaked, raw)
		}
	}
	if !strings.Contains(text, "payload=[redacted-payload]") {
		t.Fatalf("runner output GraphQL string brace was not replaced with a redaction marker:\n%s", raw)
	}
}

func TestTraceHarnessReportScriptRedactsGraphQLAliasFromRunnerOutputExcerpt(t *testing.T) {
	root := repoRoot(t)
	body := `2026/06/18 09:00:00 event=runner_timeout task_id=issue-real issue_id=issue-real msg="timeout" payload=map[output_head:mutation { model: issue(id:"secret-id") { id title } } timeout_ms:60000]` + "\n"
	raw := runTraceHarnessReportRaw(t, root, body)
	report := parseTraceReport(t, raw)

	findCluster(t, report, "runner-timeout")
	text := string(raw)
	for _, leaked := range []string{"model: issue", "issue(id:", "secret-id"} {
		if strings.Contains(text, leaked) {
			t.Fatalf("alias runner output GraphQL leaked %q in report:\n%s", leaked, raw)
		}
	}
	if !strings.Contains(text, "payload=[redacted-payload]") {
		t.Fatalf("alias runner output GraphQL was not replaced with a redaction marker:\n%s", raw)
	}
}

func TestTraceHarnessReportScriptRedactsGraphQLSelectionSetFromRunnerOutputExcerpt(t *testing.T) {
	root := repoRoot(t)
	body := `2026/06/18 09:00:00 event=runner_timeout task_id=issue-real issue_id=issue-real msg="timeout" payload=map[output_head:{ issue(id:"secret-id") { id title } } timeout_ms:60000]` + "\n"
	raw := runTraceHarnessReportRaw(t, root, body)
	report := parseTraceReport(t, raw)

	findCluster(t, report, "runner-timeout")
	text := string(raw)
	for _, leaked := range []string{"issue(id:", "secret-id", "{ id title }"} {
		if strings.Contains(text, leaked) {
			t.Fatalf("selection-set runner output GraphQL leaked %q in report:\n%s", leaked, raw)
		}
	}
	if !strings.Contains(text, "payload=[redacted-payload]") {
		t.Fatalf("selection-set runner output GraphQL was not replaced with a redaction marker:\n%s", raw)
	}
}

func TestTraceHarnessReportScriptRedactsSpreadFirstGraphQLSelectionSet(t *testing.T) {
	root := repoRoot(t)
	body := `2026/06/18 09:00:00 event=runner_timeout task_id=issue-real issue_id=issue-real msg="timeout" payload=map[output_head:{ ...SecretFields } timeout_ms:60000]` + "\n"
	raw := runTraceHarnessReportRaw(t, root, body)
	report := parseTraceReport(t, raw)

	findCluster(t, report, "runner-timeout")
	text := string(raw)
	for _, leaked := range []string{"SecretFields", "{ ..."} {
		if strings.Contains(text, leaked) {
			t.Fatalf("spread-first selection-set GraphQL leaked %q in report:\n%s", leaked, raw)
		}
	}
	if !strings.Contains(text, "payload=[redacted-payload]") {
		t.Fatalf("spread-first selection-set GraphQL was not replaced with a redaction marker:\n%s", raw)
	}
}

func TestTraceHarnessReportScriptRedactsCommentFirstGraphQLSelectionSet(t *testing.T) {
	root := repoRoot(t)
	body := strings.Join([]string{
		`2026/06/18 09:00:00 event=runner_timeout task_id=issue-real issue_id=issue-real msg="timeout" payload=map[output_head:{ # leading comment`,
		`issue(id:"secret-id") { id title } } timeout_ms:60000]`,
	}, "\n") + "\n"
	raw := runTraceHarnessReportRaw(t, root, body)
	report := parseTraceReport(t, raw)

	findCluster(t, report, "runner-timeout")
	text := string(raw)
	for _, leaked := range []string{"leading comment", "issue(id:", "secret-id", "{ id title }"} {
		if strings.Contains(text, leaked) {
			t.Fatalf("comment-first selection-set GraphQL leaked %q in report:\n%s", leaked, raw)
		}
	}
	if !strings.Contains(text, "payload=[redacted-payload]") {
		t.Fatalf("comment-first selection-set GraphQL was not replaced with a redaction marker:\n%s", raw)
	}
}

func TestTraceHarnessReportScriptRedactsGraphQLCommentBracesFromRunnerOutputExcerpt(t *testing.T) {
	root := repoRoot(t)
	body := strings.Join([]string{
		`2026/06/18 09:00:00 event=runner_timeout task_id=issue-real issue_id=issue-real msg="timeout" payload=map[output_head:mutation { # } SECRET_COMMENT`,
		`issue(id:"secret-id") { id title } } timeout_ms:60000]`,
	}, "\n") + "\n"
	raw := runTraceHarnessReportRaw(t, root, body)
	report := parseTraceReport(t, raw)

	findCluster(t, report, "runner-timeout")
	text := string(raw)
	for _, leaked := range []string{"SECRET_COMMENT", "issue(id:", "secret-id", "{ id title }"} {
		if strings.Contains(text, leaked) {
			t.Fatalf("runner output GraphQL comment leaked %q in report:\n%s", leaked, raw)
		}
	}
	if !strings.Contains(text, "payload=[redacted-payload]") {
		t.Fatalf("runner output GraphQL comment was not replaced with a redaction marker:\n%s", raw)
	}
}

func TestTraceHarnessReportScriptRedactsProsePrefixedGraphQLSelectionSet(t *testing.T) {
	root := repoRoot(t)
	body := `2026/06/18 09:00:00 event=runner_timeout task_id=issue-real issue_id=issue-real msg="timeout" payload=map[output_head:GraphQL request: { issue(id:"secret-id") { id title } } timeout_ms:60000]` + "\n"
	raw := runTraceHarnessReportRaw(t, root, body)
	report := parseTraceReport(t, raw)

	findCluster(t, report, "runner-timeout")
	text := string(raw)
	for _, leaked := range []string{"issue(id:", "secret-id", "{ id title }"} {
		if strings.Contains(text, leaked) {
			t.Fatalf("prose-prefixed selection-set GraphQL leaked %q in report:\n%s", leaked, raw)
		}
	}
	if !strings.Contains(text, "payload=[redacted-payload]") {
		t.Fatalf("prose-prefixed selection-set GraphQL was not replaced with a redaction marker:\n%s", raw)
	}
}

func TestTraceHarnessReportScriptRedactsBracketedProseGraphQLSelectionSet(t *testing.T) {
	root := repoRoot(t)
	body := `2026/06/18 09:00:00 event=runner_timeout task_id=issue-real issue_id=issue-real msg="timeout" payload=map[output_head:[INFO] GraphQL request: { issue(id:"secret-id") { id title } } timeout_ms:60000]` + "\n"
	raw := runTraceHarnessReportRaw(t, root, body)
	report := parseTraceReport(t, raw)

	findCluster(t, report, "runner-timeout")
	text := string(raw)
	for _, leaked := range []string{"issue(id:", "secret-id", "{ id title }"} {
		if strings.Contains(text, leaked) {
			t.Fatalf("bracketed prose selection-set GraphQL leaked %q in report:\n%s", leaked, raw)
		}
	}
	if !strings.Contains(text, "payload=[redacted-payload]") {
		t.Fatalf("bracketed prose selection-set GraphQL was not replaced with a redaction marker:\n%s", raw)
	}
}

func TestTraceHarnessReportScriptKeepsTopLevelFieldsWhenOutputRepeatsKeys(t *testing.T) {
	root := repoRoot(t)
	body := `2026/06/18 09:00:00 event=runner_timeout task_id=issue-real issue_id=issue-real session_id=session-real msg="timeout" payload=map[output_head:event=runner_end task_id=issue-output issue_id=issue-output session_id=session-output]` + "\n"
	report := runTraceHarnessReport(t, root, body)

	cluster := findCluster(t, report, "runner-timeout")
	if !contains(cluster.Affected.Issues, "issue-real") || contains(cluster.Affected.Issues, "issue-output") {
		t.Fatalf("issue ids were parsed from output text: %#v", cluster.Affected.Issues)
	}
	if !contains(cluster.Affected.Sessions, "session-real") || contains(cluster.Affected.Sessions, "session-output") {
		t.Fatalf("session ids were parsed from output text: %#v", cluster.Affected.Sessions)
	}
}

func TestTraceHarnessReportScriptRedactsInvalidUTF8InsideOpaquePayload(t *testing.T) {
	root := repoRoot(t)
	raw := []byte("2026/06/18 09:00:00 event=runner_timeout task_id=issue-1 issue_id=issue-1 msg=\"timeout\" payload=map[output_head:\xff]\n")
	reportRaw := runTraceHarnessReportRawBytes(t, root, raw)

	if !strings.Contains(string(reportRaw), "runner-timeout") || !strings.Contains(string(reportRaw), "payload=[redacted-payload]") || strings.Contains(string(reportRaw), `\ufffd`) {
		t.Fatalf("invalid UTF-8 opaque payload was not redacted:\n%s", reportRaw)
	}
}

type traceReport struct {
	SchemaVersion string `json:"schema_version"`
	Clusters      []traceCluster
}

type traceCluster struct {
	ID       string `json:"id"`
	Affected struct {
		Issues           []string       `json:"issues"`
		IssueIdentifiers []string       `json:"issue_identifiers"`
		PullRequests     []string       `json:"pull_requests"`
		Runs             []string       `json:"runs"`
		Sessions         []string       `json:"sessions"`
		Omitted          map[string]int `json:"omitted"`
	} `json:"affected"`
	Evidence []struct {
		Excerpt  string            `json:"excerpt"`
		Metadata map[string]string `json:"metadata"`
	} `json:"evidence"`
}

func runTraceHarnessReport(t *testing.T, root, body string) traceReport {
	t.Helper()
	raw := runTraceHarnessReportRaw(t, root, body)
	return parseTraceReport(t, raw)
}

func parseTraceReport(t *testing.T, raw []byte) traceReport {
	t.Helper()
	var report traceReport
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatalf("unmarshal report: %v\n%s", err, raw)
	}
	return report
}

func runTraceHarnessReportRaw(t *testing.T, root, body string) []byte {
	t.Helper()
	return runTraceHarnessReportRawBytes(t, root, []byte(body))
}

func runTraceHarnessReportRawBytes(t *testing.T, root string, body []byte) []byte {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "worker.log")
	if err := os.WriteFile(logPath, body, 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}
	jsonPath := filepath.Join(dir, "report.json")
	cmd := exec.Command("python3", filepath.Join(root, "scripts", "trace-harness-report.py"), "--worker-log", logPath, "--json-out", jsonPath)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("trace-harness-report failed: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	return raw
}

func runTraceHarnessReportRawPath(t *testing.T, root, logPath string) []byte {
	t.Helper()
	jsonPath := filepath.Join(t.TempDir(), "report.json")
	cmd := exec.Command("python3", filepath.Join(root, "scripts", "trace-harness-report.py"), "--worker-log", logPath, "--json-out", jsonPath)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("trace-harness-report failed: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	return raw
}

func findCluster(t *testing.T, report traceReport, id string) traceCluster {
	t.Helper()
	for _, cluster := range report.Clusters {
		if cluster.ID == id {
			return cluster
		}
	}
	t.Fatalf("missing cluster %q in %#v", id, report.Clusters)
	return traceCluster{}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal JSON: %v", err)
	}
	return string(raw)
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
