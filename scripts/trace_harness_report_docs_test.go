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
		"single line",
		"first opaque payload key",
		"as opaque and never parsed",
		"is not recovered",
		"emit a structured payload",
		"Known limitation",
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
	report := parseTraceReport(t, raw)
	if report.SchemaVersion != "trace-harness-report/v1" || len(report.Clusters) != 2 {
		t.Fatalf("unexpected report: %#v", report)
	}
	timeout := findCluster(t, report, "runner-timeout")
	if !contains(timeout.Affected.Issues, "issue-1") || !contains(timeout.Affected.Sessions, "session-1") {
		t.Fatalf("timeout cluster missing affected fields: %#v", timeout)
	}
	if timeout.Evidence[0].Metadata["timestamp"] != "2026/06/18 09:00:00" {
		t.Fatalf("timeout timestamp metadata = %#v", timeout.Evidence[0].Metadata)
	}
	if timeout.Evidence[0].Metadata["output_bytes"] != "70000" || timeout.Evidence[0].Metadata["output_dropped"] != "12" {
		t.Fatalf("timeout metadata = %#v; want output_bytes/output_dropped before opaque key", timeout.Evidence[0].Metadata)
	}
	if strings.Contains(string(raw), "secret") || !strings.Contains(timeout.Evidence[0].Excerpt, "payload=[redacted-payload]") {
		t.Fatalf("report leaked or retained opaque runner output:\n%s", raw)
	}
	note := clusterRedactionNote(t, raw, "runner-timeout")
	for _, want := range []string{"output_head", "output_tail", "error", "arguments", "raw", "params"} {
		if !strings.Contains(note, want) {
			t.Fatalf("redaction note missing omitted payload %q: %q", want, note)
		}
	}
}

func TestTraceHarnessReportScriptUsesPayloadSessionID(t *testing.T) {
	root := repoRoot(t)
	// session_id sorts before no opaque key here, so it is recoverable from the
	// single first line.
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

func TestTraceHarnessReportScriptClassifiesFailedRunnerEndFromMessage(t *testing.T) {
	root := repoRoot(t)
	body := `2026/06/18 09:00:00 event=runner_end task_id=issue-1 issue_id=issue-1 msg="runner failed" payload=map[ok:false error:boom]` + "\n"
	report := runTraceHarnessReport(t, root, body)

	cluster := findCluster(t, report, "runner-failure")
	if !contains(cluster.Affected.Issues, "issue-1") {
		t.Fatalf("failed runner_end was not classified: %#v", report.Clusters)
	}
}

func TestTraceHarnessReportScriptIgnoresSuccessfulRunnerEnd(t *testing.T) {
	root := repoRoot(t)
	// "runner failed" appears only in opaque output; the prefix msg is a success
	// message, so classification (prefix-only) must not surface a failure.
	body := `2026/06/18 09:00:00 event=runner_end task_id=issue-1 issue_id=issue-1 msg="runner completed" payload=map[ok:true output_head:runner failed downstream]` + "\n"
	report := runTraceHarnessReport(t, root, body)

	if len(report.Clusters) != 0 {
		t.Fatalf("successful runner_end with failure text in output was classified: %#v", report.Clusters)
	}
}

func TestTraceHarnessReportScriptMasksCloneURLUserinfoSchemes(t *testing.T) {
	root := repoRoot(t)
	for _, secretURL := range []string{
		"https://user:secret@example.test/org/repo.git",
		"http://user:secret@example.test/org/repo.git",
		"ssh://user:secret@example.test/org/repo.git",
		"git://user:secret@example.test/org/repo.git",
		"rsync://user:secret@example.test/org/repo.git",
		"file://user:secret@example.test/org/repo.git",
	} {
		body := `2026/06/18 09:00:00 event=runner_timeout task_id=issue-1 issue_id=issue-1 msg="timeout" payload=map[output_head:` + secretURL + `]` + "\n"
		raw := runTraceHarnessReportRaw(t, root, body)
		if strings.Contains(string(raw), "secret") || !strings.Contains(string(raw), "payload=[redacted-payload]") {
			t.Fatalf("report leaked or retained %q userinfo from opaque output:\n%s", secretURL, raw)
		}
	}
}

func TestTraceHarnessReportScriptMasksMalformedCloneURLUserinfo(t *testing.T) {
	root := repoRoot(t)
	for _, secretURL := range []string{
		"https://user:bad token@example.test/org/repo.git",
		"https://:secret@example.test/org/repo.git",
		"https://user:tok]en@example.test/org/repo.git",
	} {
		body := `2026/06/18 09:00:00 event=runner_timeout task_id=issue-1 issue_id=issue-1 msg="timeout" payload=map[output_head:` + secretURL + `]` + "\n"
		raw := runTraceHarnessReportRaw(t, root, body)
		if strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "bad token") || strings.Contains(string(raw), "tok]en") {
			t.Fatalf("report leaked malformed userinfo %q:\n%s", secretURL, raw)
		}
		if !strings.Contains(string(raw), "payload=[redacted-payload]") {
			t.Fatalf("report did not redact opaque payload for %q:\n%s", secretURL, raw)
		}
	}
}

func TestTraceHarnessReportScriptMasksTokenLikeSecrets(t *testing.T) {
	root := repoRoot(t)
	for _, token := range []string{
		"ghp_" + strings.Repeat("a", 36),
		"lin_api_" + strings.Repeat("a", 40),
	} {
		// The token sits inside opaque output, which is redacted wholesale; the
		// token regex is the backstop for any place a token still reaches text.
		body := `2026/06/18 09:00:00 event=runner_end task_id=issue-1 issue_id=issue-1 msg="runner failed ` + token + `" payload=map[output_head:` + token + `]` + "\n"
		raw := runTraceHarnessReportRaw(t, root, body)
		if strings.Contains(string(raw), token) || !strings.Contains(string(raw), "[redacted-token]") || !strings.Contains(string(raw), "payload=[redacted-payload]") {
			t.Fatalf("report leaked or retained token-like value %q:\n%s", token, raw)
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

func TestTraceHarnessReportScriptRedactsEveryOpaquePayloadFromExcerpt(t *testing.T) {
	root := repoRoot(t)
	for _, opaque := range []string{
		"output_head:secret-output",
		"output_tail:secret-output",
		"error:secret-error",
		"arguments:{\"q\":\"secret\"}",
		"arguments_raw:secret-args",
		"raw:secret-raw",
		"params:secret-params",
	} {
		body := `2026/06/18 09:00:00 event=runner_timeout task_id=issue-1 issue_id=issue-1 msg="timeout" payload=map[` + opaque + `]` + "\n"
		report := runTraceHarnessReport(t, root, body)
		cluster := findCluster(t, report, "runner-timeout")
		excerpt := cluster.Evidence[0].Excerpt
		if !strings.Contains(excerpt, "payload=[redacted-payload]") || strings.Contains(excerpt, "secret") {
			t.Fatalf("opaque payload %q leaked into excerpt %q", opaque, excerpt)
		}
	}
}

func TestTraceHarnessReportScriptStopsScalarMetadataAtFirstOpaqueKey(t *testing.T) {
	root := repoRoot(t)
	// timeout_ms appears only inside the opaque output_head value; the importer
	// must stop at output_head and not promote text after it.
	body := `2026/06/18 09:00:00 event=runner_timeout task_id=issue-1 issue_id=issue-1 msg="timeout" payload=map[exit_code:7 model:gpt-5 output_head:agent said timeout_ms:999]` + "\n"
	report := runTraceHarnessReport(t, root, body)

	meta := findCluster(t, report, "runner-timeout").Evidence[0].Metadata
	if meta["exit_code"] != "7" || meta["model"] != "gpt-5" {
		t.Fatalf("scalars before the opaque key were dropped: %#v", meta)
	}
	if got, ok := meta["timeout_ms"]; ok {
		t.Fatalf("Metadata[timeout_ms] = %q; want absent (it is buried inside opaque output)", got)
	}
}

func TestTraceHarnessReportScriptDoesNotExtractScalarsBuriedAfterOpaqueKey(t *testing.T) {
	root := repoRoot(t)
	// Go sorts `arguments` before `tool`, so a real unsupported_tool_call log
	// renders the opaque `arguments` first. `tool` is intentionally not
	// recovered; this is the documented cost of the opaque-boundary design.
	body := `2026/06/18 09:00:00 event=unsupported_tool_call task_id=issue-1 issue_id=issue-1 msg="unsupported" payload=map[arguments:{"q":"x"} tool:linear_graphql]` + "\n"
	report := runTraceHarnessReport(t, root, body)

	meta := findCluster(t, report, "tool-unsupported").Evidence[0].Metadata
	if got, ok := meta["tool"]; ok {
		t.Fatalf("Metadata[tool] = %q; want absent (it sorts after the opaque arguments key)", got)
	}
}

func TestTraceHarnessReportScriptDoesNotSmuggleFakeFieldsViaUnquotedToolValue(t *testing.T) {
	root := repoRoot(t)
	// An unsupported_tool_call with no thread context renders `tool` first (no
	// key sorts before it). `tool` is agent-controlled and Go renders it
	// unquoted, so a tool name carrying spaces and key-shaped text would, if
	// `tool` were a recognized safe scalar, resync the scan onto the embedded
	// `session_id:`/`pr_url:` fragments. The scan must instead stop at the
	// `tool` chunk, so no fake top-level scalar is promoted.
	body := `2026/06/18 09:00:00 event=unsupported_tool_call task_id=real-run issue_id=real-issue msg="unsupported" payload=map[tool:evil session_id:GHOST-SESSION pr_url:https://ghost/pull/1 turn_id:U]` + "\n"
	report := runTraceHarnessReport(t, root, body)

	cluster := findCluster(t, report, "tool-unsupported")
	if contains(cluster.Affected.Sessions, "GHOST-SESSION") || contains(cluster.Affected.PullRequests, "https://ghost/pull/1") {
		t.Fatalf("unquoted tool value smuggled fake top-level fields: %#v", cluster.Affected)
	}
	meta := cluster.Evidence[0].Metadata
	for _, key := range []string{"tool", "session_id", "pr_url"} {
		if got, ok := meta[key]; ok {
			t.Fatalf("Metadata[%s] = %q; want absent (scan must stop at the agent-controlled tool chunk)", key, got)
		}
	}
	if !contains(cluster.Affected.Runs, "real-run") || !contains(cluster.Affected.Issues, "real-issue") {
		t.Fatalf("prefix ids were dropped: %#v", cluster.Affected)
	}
}

func TestTraceHarnessReportScriptDoesNotPromoteIdentifiersFromOpaquePayload(t *testing.T) {
	root := repoRoot(t)
	body := `2026/06/18 09:00:00 event=runner_timeout task_id=run-real issue_id=issue-real msg="timeout" payload=map[output_head:issue_id:ghost-issue session_id:ghost-session]` + "\n"
	report := runTraceHarnessReport(t, root, body)

	cluster := findCluster(t, report, "runner-timeout")
	if contains(cluster.Affected.Issues, "ghost-issue") || contains(cluster.Affected.Sessions, "ghost-session") {
		t.Fatalf("identifiers inside opaque output were promoted: %#v", cluster.Affected)
	}
	if !contains(cluster.Affected.Issues, "issue-real") {
		t.Fatalf("prefix issue id was dropped: %#v", cluster.Affected.Issues)
	}
}

func TestTraceHarnessReportScriptRedactsInvalidUTF8InsideOpaquePayload(t *testing.T) {
	root := repoRoot(t)
	raw := []byte("2026/06/18 09:00:00 event=runner_timeout task_id=issue-1 issue_id=issue-1 msg=\"timeout\" payload=map[output_head:\xff]\n")
	reportRaw := runTraceHarnessReportRawBytes(t, root, raw)

	// The report is emitted with ensure_ascii, so a leaked U+FFFD replacement
	// char would appear as the escaped six-byte form, not as a literal rune.
	if !strings.Contains(string(reportRaw), "runner-timeout") || !strings.Contains(string(reportRaw), "payload=[redacted-payload]") || strings.Contains(string(reportRaw), "\\ufffd") {
		t.Fatalf("invalid UTF-8 opaque payload was not redacted:\n%s", reportRaw)
	}
}

func TestTraceHarnessReportScriptIgnoresPlainEventText(t *testing.T) {
	root := repoRoot(t)
	// No leading log timestamp, so this is not a worker record.
	body := `event=runner_timeout task_id=issue-1 issue_id=issue-1 msg="timeout"` + "\n"
	report := runTraceHarnessReport(t, root, body)

	if len(report.Clusters) != 0 {
		t.Fatalf("plain event-shaped text was promoted: %#v", report.Clusters)
	}
}

func TestTraceHarnessReportScriptIgnoresTimestampedProseEventText(t *testing.T) {
	root := repoRoot(t)
	// `event=` is not immediately after the timestamp, so the record-start
	// grammar does not match.
	body := `2026/06/18 09:00:00 note about event=runner_timeout in prose` + "\n"
	report := runTraceHarnessReport(t, root, body)

	if len(report.Clusters) != 0 {
		t.Fatalf("timestamped prose was promoted: %#v", report.Clusters)
	}
}

func TestTraceHarnessReportScriptIgnoresKeyShapedTextInsideMessage(t *testing.T) {
	root := repoRoot(t)
	// issue_id= inside the %q message body must not be parsed as a real field.
	body := `2026/06/18 09:00:00 event=runner_timeout task_id=issue-1 issue_id=issue-1 msg="see issue_id=ghost-issue for context"` + "\n"
	report := runTraceHarnessReport(t, root, body)

	cluster := findCluster(t, report, "runner-timeout")
	if contains(cluster.Affected.Issues, "ghost-issue") {
		t.Fatalf("key-shaped text inside msg was parsed as a field: %#v", cluster.Affected.Issues)
	}
}

func TestTraceHarnessReportScriptSkipsMultilineOpaqueContinuationLines(t *testing.T) {
	root := repoRoot(t)
	// A multi-line %v map: the opaque output spills onto continuation lines that
	// do not start a worker record, so they are skipped. The next real record is
	// still parsed normally.
	body := strings.Join([]string{
		`2026/06/18 09:00:00 event=runner_timeout task_id=issue-1 issue_id=issue-1 msg="timeout" payload=map[output_head:line one`,
		`line two with brackets [INFO] and a stray ]`,
		`timeout_ms:30000]`,
		`2026/06/18 09:05:00 event=turn_input_required task_id=issue-2 issue_id=issue-2 msg="input" payload=map[method:approval]`,
	}, "\n") + "\n"
	report := runTraceHarnessReport(t, root, body)

	if len(report.Clusters) != 2 {
		t.Fatalf("multiline continuation handling produced %d clusters; want 2: %#v", len(report.Clusters), report.Clusters)
	}
	timeout := findCluster(t, report, "runner-timeout")
	if got, ok := timeout.Evidence[0].Metadata["timeout_ms"]; ok {
		t.Fatalf("Metadata[timeout_ms] = %q; want absent (it is on a skipped continuation line)", got)
	}
	if !contains(findCluster(t, report, "input-required").Affected.Issues, "issue-2") {
		t.Fatalf("the record after multiline output was not parsed: %#v", report.Clusters)
	}
}

func TestTraceHarnessReportScriptDoesNotPromoteEmbeddedNonRecordOutput(t *testing.T) {
	root := repoRoot(t)
	// Embedded agent output that looks event-shaped but lacks the leading log
	// timestamp is a continuation line and must not become a record.
	body := strings.Join([]string{
		`2026/06/18 09:00:00 event=runner_timeout task_id=issue-1 issue_id=issue-1 msg="timeout" payload=map[output_head:agent log:`,
		`event=runner_end task_id=ghost issue_id=ghost-issue msg="runner failed"`,
		`]`,
	}, "\n") + "\n"
	report := runTraceHarnessReport(t, root, body)

	if len(report.Clusters) != 1 || report.Clusters[0].ID != "runner-timeout" {
		t.Fatalf("embedded non-record output was promoted: %#v", report.Clusters)
	}
	if contains(report.Clusters[0].Affected.Issues, "ghost-issue") {
		t.Fatalf("ghost identifier from embedded output leaked: %#v", report.Clusters[0].Affected.Issues)
	}
}

func TestTraceHarnessReportScriptSurfacesEmbeddedRecordShapedOutputAsKnownLimitation(t *testing.T) {
	root := repoRoot(t)
	// Documented limitation: Go's %v map rendering is unescaped, so opaque output
	// that reproduces the full record-start grammar (log timestamp + event=known)
	// is indistinguishable from a real record and is surfaced. The runbook
	// records this and recommends a structured payload as the harness fix; this
	// test pins the behavior so it is not "fixed" with another unsound heuristic.
	body := strings.Join([]string{
		`2026/06/18 09:00:00 event=runner_timeout task_id=issue-1 issue_id=issue-1 msg="timeout" payload=map[output_head:agent replayed a log line`,
		`2026/06/18 09:00:01 event=runner_end task_id=replayed issue_id=replayed-issue msg="runner failed"`,
		`]`,
	}, "\n") + "\n"
	report := runTraceHarnessReport(t, root, body)

	cluster := findCluster(t, report, "runner-failure")
	if !contains(cluster.Affected.Issues, "replayed-issue") {
		t.Fatalf("expected the known limitation to surface the replayed record: %#v", report.Clusters)
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
	// 4000 records cycle 16 distinct task_ids and one shared issue id; dedup must
	// collapse them, not append duplicates.
	if got := len(cluster.Affected.Runs); got != 16 {
		t.Fatalf("len(Affected.Runs) = %d; want 16 (deduped)", got)
	}
	if got := len(cluster.Affected.Issues); got != 1 {
		t.Fatalf("len(Affected.Issues) = %d; want 1 (deduped)", got)
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

	report := parseTraceReport(t, rawReport)
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

	report := parseTraceReport(t, rawReport)
	cluster := findCluster(t, report, "runner-timeout")
	if len(cluster.Evidence) == 0 {
		t.Fatalf("large scalar metadata dropped all evidence")
	}
	model := cluster.Evidence[0].Metadata["model"]
	if len([]byte(model)) > 4*1024 || !strings.Contains(model, "[truncated]") {
		t.Fatalf("model metadata was not byte-bounded: len=%d value=%q", len([]byte(model)), model)
	}
}

func TestTraceHarnessReportScriptBoundsLargeScalarMetadataAcrossFields(t *testing.T) {
	root := repoRoot(t)
	large := strings.Repeat("m", 8*1024)
	// All scalar keys, listed before any opaque key, so every one is extracted
	// and the aggregate must still be byte-bounded.
	keys := []string{
		"elapsed_ms", "timeout_ms", "duration_ms", "output_bytes", "output_dropped",
		"model", "method", "session_id", "pr", "pr_number", "pr_url",
		"pull_request", "pull_request_url", "exit_code",
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

func runTraceHarnessReport(t *testing.T, root, body string) traceReport {
	t.Helper()
	return parseTraceReport(t, runTraceHarnessReportRaw(t, root, body))
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
	return runTraceHarnessReportRawPath(t, root, logPath)
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

func clusterRedactionNote(t *testing.T, raw []byte, id string) string {
	t.Helper()
	var report struct {
		Clusters []struct {
			ID            string `json:"id"`
			RedactionNote string `json:"redaction_note"`
		} `json:"clusters"`
	}
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatalf("unmarshal report: %v\n%s", err, raw)
	}
	for _, cluster := range report.Clusters {
		if cluster.ID == id {
			return cluster.RedactionNote
		}
	}
	t.Fatalf("missing cluster %q for redaction note", id)
	return ""
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

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
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
