package scripts_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

func TestTraceEvidenceManifestRunbookDocumentsCaptureOmissionAndConsumers(t *testing.T) {
	root := repoRoot(t)
	readme, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	if !strings.Contains(string(readme), "docs/runbooks/trace-evidence-manifest.md") {
		t.Fatalf("README does not link the trace evidence manifest runbook")
	}

	path := filepath.Join(root, "docs", "runbooks", "trace-evidence-manifest.md")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	text := strings.Join(strings.Fields(string(body)), " ")
	for _, want := range []string{
		"python3 scripts/trace-evidence-manifest.py",
		"trace-evidence-manifest/v2",
		"--evidence-manifest",
		"a trace database, queue, metrics store",
		"restart recovery stays tracker/filesystem-driven",
		"What is captured",
		"What is omitted",
		"payload=[redacted-payload]",
		"MaskCloneURL",
		"[redacted-token]",
		"not a new evidence source",
		"64 KiB per run",
		"256 KiB",
		"dropped_events",
		"Retention and bounds",
		"never become restart/recovery state",
		"How it is consumed",
		"Milestone 6",
		"#953",
		"Milestone 7",
		"#951",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("manifest runbook missing %q\n%s", want, body)
		}
	}
}

func TestTraceEvidenceManifestGroupsRunsAndRedactsOpaquePayload(t *testing.T) {
	root := repoRoot(t)
	secretURL := "https://user:secret@example.test/org/repo.git"
	body := strings.Join([]string{
		`2026/06/18 09:00:00 event=runner_timeout task_id=run-1 issue_id=issue-1 issue_identifier=GH-1 session_id=session-1 msg="timeout" payload=map[elapsed_ms:60000 output_bytes:70000 output_head:` + secretURL + `]`,
		`2026/06/18 09:02:00 event=runner_end task_id=run-1 issue_id=issue-1 msg="runner failed" payload=map[ok:false error:boom]`,
		`2026/06/18 09:01:00 event=turn_input_required task_id=run-2 issue_id=issue-2 session_id=session-2 msg="input" payload=map[method:approval]`,
	}, "\n") + "\n"
	raw := manifestRawBytes(t, root, body)
	manifest := parseManifest(t, raw)

	if manifest.SchemaVersion != "trace-evidence-manifest/v2" {
		t.Fatalf("schema_version = %q; want trace-evidence-manifest/v2", manifest.SchemaVersion)
	}
	if manifest.Bounds.MaxRunEvidenceBytes != 64*1024 || manifest.Bounds.MaxClusterBytes != 256*1024 {
		t.Fatalf("bounds = %#v; want 65536/262144", manifest.Bounds)
	}
	if len(manifest.Runs) != 2 {
		t.Fatalf("runs = %d; want 2 (grouped by run id)\n%s", len(manifest.Runs), raw)
	}
	run1 := manifestRun(t, manifest, "run-1")
	if len(run1.Events) != 2 {
		t.Fatalf("run-1 events = %d; want 2 (timeout + runner failure)\n%s", len(run1.Events), raw)
	}
	// Affected ids are partitioned by failure class: the timeout event's ids land
	// under runner-timeout, the runner_end event's under runner-failure.
	rtAffected := classAffected(t, run1, "runner-timeout")
	if !contains(rtAffected.Issues, "issue-1") || !contains(rtAffected.Sessions, "session-1") {
		t.Fatalf("run-1 runner-timeout affected = %#v; want issue-1/session-1", rtAffected)
	}
	if !contains(classAffected(t, run1, "runner-failure").Issues, "issue-1") {
		t.Fatalf("run-1 runner-failure affected = %#v; want issue-1", run1.AffectedByClass["runner-failure"])
	}
	if run1.Events[0].Metadata["output_bytes"] != "70000" || run1.Events[0].Metadata["timestamp"] != "2026/06/18 09:00:00" {
		t.Fatalf("run-1 first event metadata = %#v; want metadata-first scalars before the opaque key", run1.Events[0].Metadata)
	}
	// Each event records its resolved failure class so consumers re-cluster
	// without re-parsing the redacted excerpt.
	if run1.Events[0].Class != "runner-timeout" || run1.Events[1].Class != "runner-failure" {
		t.Fatalf("run-1 event classes = %q/%q; want runner-timeout/runner-failure", run1.Events[0].Class, run1.Events[1].Class)
	}
	// The second run is grouped separately with its own event and affected ids.
	run2 := manifestRun(t, manifest, "run-2")
	if len(run2.Events) != 1 || run2.Events[0].Class != "input-required" {
		t.Fatalf("run-2 = %#v; want one input-required event", run2.Events)
	}
	run2Affected := classAffected(t, run2, "input-required")
	if !contains(run2Affected.Issues, "issue-2") || !contains(run2Affected.Sessions, "session-2") {
		t.Fatalf("run-2 input-required affected = %#v; want issue-2/session-2", run2Affected)
	}
	// Mutation-style redaction check: dropping the reused mask()/opaque omission
	// would surface the clone-URL userinfo and the raw payload here.
	if strings.Contains(string(raw), "secret") {
		t.Fatalf("manifest leaked opaque clone-URL userinfo:\n%s", raw)
	}
	for _, event := range run1.Events {
		if !strings.Contains(event.Excerpt, "payload=[redacted-payload]") {
			t.Fatalf("event excerpt did not redact the payload region: %q", event.Excerpt)
		}
	}
}

func TestTraceEvidenceManifestMasksCloneURLAndTokensInPrefix(t *testing.T) {
	root := repoRoot(t)
	token := "ghp_" + strings.Repeat("a", 36)
	secretPR := "https://user:secret@example.test/org/repo/pull/1"
	body := `2026/06/18 09:00:00 event=runner_timeout task_id=run-1 issue_id=issue-1 session_id=` + token + ` pr_url=` + secretPR + ` msg="timeout"` + "\n"
	raw := manifestRawBytes(t, root, body)
	maskedPR := workflow.MaskCloneURL(secretPR)

	if strings.Contains(string(raw), token) || strings.Contains(string(raw), "user:secret") {
		t.Fatalf("manifest leaked token or clone-URL userinfo from the prefix:\n%s", raw)
	}
	if !strings.Contains(string(raw), "[redacted-token]") || !strings.Contains(string(raw), maskedPR) {
		t.Fatalf("manifest did not mask token/clone-URL with the report's conventions:\n%s", raw)
	}
}

func TestTraceEvidenceManifestMasksTokenInsideOpaquePayload(t *testing.T) {
	root := repoRoot(t)
	token := "ghp_" + strings.Repeat("a", 36)
	body := `2026/06/18 09:00:00 event=runner_end task_id=run-1 issue_id=issue-1 msg="runner failed" payload=map[output_head:` + token + `]` + "\n"
	raw := manifestRawBytes(t, root, body)

	if strings.Contains(string(raw), token) {
		t.Fatalf("manifest leaked token-like value from opaque payload:\n%s", raw)
	}
	if !strings.Contains(string(raw), "payload=[redacted-payload]") {
		t.Fatalf("manifest did not redact the opaque payload region:\n%s", raw)
	}
}

func TestTraceEvidenceManifestBoundsPerRunEvidenceBytes(t *testing.T) {
	root := repoRoot(t)
	const total = 40
	lines := make([]string, 0, total)
	for idx := 0; idx < total; idx++ {
		lines = append(lines, `2026/06/18 09:00:00 event=runner_timeout task_id=run-1 issue_id=issue-1 msg="`+strings.Repeat("x", 5000)+`"`)
	}
	manifest := runTraceEvidenceManifest(t, root, strings.Join(lines, "\n")+"\n")

	run := manifestRun(t, manifest, "run-1")
	if run.DroppedEvents <= 0 {
		t.Fatalf("dropped_events = %d; want > 0 once the 64 KiB per-run cap is reached", run.DroppedEvents)
	}
	if run.DroppedEvents != total-len(run.Events) {
		t.Fatalf("dropped_events = %d; want total-retained = %d", run.DroppedEvents, total-len(run.Events))
	}
	encoded, err := json.Marshal(run.Events)
	if err != nil {
		t.Fatalf("marshal run events: %v", err)
	}
	if len(encoded) > 64*1024+2*1024 {
		t.Fatalf("per-run evidence bytes = %d; want bounded near 65536", len(encoded))
	}
	// The byte cap drops whole events but never the per-class affected-id summary.
	rtAffected := classAffected(t, run, "runner-timeout")
	if !contains(rtAffected.Issues, "issue-1") {
		t.Fatalf("byte cap dropped the affected id summary: %#v", rtAffected)
	}
}

func TestTraceEvidenceManifestBoundsRunGroupByBytesIncludingAffectedIDs(t *testing.T) {
	root := repoRoot(t)
	// One run id, but thousands of distinct issue/session ids: the affected id
	// arrays — not just events — must be trimmed so the whole run group honors the
	// 256 KiB cluster-equivalent cap, matching the report's enforce_cluster_bound.
	const total = 12000
	var body strings.Builder
	for idx := 0; idx < total; idx++ {
		body.WriteString(`2026/06/18 09:00:00 event=runner_timeout task_id=run-1 issue_id=issue-`)
		body.WriteString(strconv.Itoa(idx))
		body.WriteString(` session_id=session-`)
		body.WriteString(strconv.Itoa(idx))
		body.WriteString(` msg="x"` + "\n")
	}
	manifest := runTraceEvidenceManifest(t, root, body.String())

	if len(manifest.Runs) != 1 {
		t.Fatalf("runs = %d; want 1", len(manifest.Runs))
	}
	run := manifest.Runs[0]
	encoded, err := json.Marshal(run)
	if err != nil {
		t.Fatalf("marshal run group: %v", err)
	}
	if len(encoded) > 256*1024 {
		t.Fatalf("run group bytes = %d; want <= 262144 (affected-id arrays must be capped too)", len(encoded))
	}
	rtAffected := classAffected(t, run, "runner-timeout")
	if rtAffected.Omitted["issues"] == 0 {
		t.Fatalf("affected id explosion did not record omitted counts: %#v", rtAffected.Omitted)
	}
	if len(rtAffected.Issues) == 0 {
		t.Fatalf("run group lost every concrete affected issue id: omitted=%#v", rtAffected.Omitted)
	}
	// Accounting must be complete: kept + omitted equals every distinct issue id.
	if got := len(rtAffected.Issues) + rtAffected.Omitted["issues"]; got != total {
		t.Fatalf("issues accounted for = %d; want %d (kept=%d omitted=%d)", got, total, len(rtAffected.Issues), rtAffected.Omitted["issues"])
	}
}

func TestTraceEvidenceManifestRoundTripsOversizedRunnerEndPrefix(t *testing.T) {
	root := repoRoot(t)
	// A runner_end failure whose prefix exceeds the excerpt byte cap before
	// "runner failed": classifying from the truncated excerpt would lose it, so
	// the manifest's stored class must carry the failure class through.
	body := `2026/06/18 09:00:00 event=runner_end task_id=run-1 issue_id=issue-1 session_id=` +
		strings.Repeat("S", 5000) + ` msg="runner failed" payload=map[ok:false]` + "\n"
	raw := manifestRawBytes(t, root, body)
	run := manifestRun(t, parseManifest(t, raw), "run-1")
	if len(run.Events) != 1 || run.Events[0].Class != "runner-failure" {
		t.Fatalf("oversized runner_end class = %#v; want one runner-failure event", run.Events)
	}

	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(manifestPath, raw, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	reportPath := filepath.Join(dir, "report.json")
	cmd := exec.Command("python3", filepath.Join(root, "scripts", "trace-harness-report.py"),
		"--evidence-manifest", manifestPath, "--json-out", reportPath)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("report consuming manifest failed: %v\n%s", err, out)
	}
	reportRaw, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	report := parseTraceReport(t, reportRaw)
	if !contains(clusterIDs(report), "runner-failure") {
		t.Fatalf("oversized runner_end failure was dropped on the manifest round trip: %v", clusterIDs(report))
	}
}

func TestTraceEvidenceManifestRoundTripDoesNotFabricateRunWithoutTaskID(t *testing.T) {
	root := repoRoot(t)
	// An event with issue_id but no task_id: the raw parser leaves affected.runs
	// empty, so the manifest's issue-id group key must not be promoted to a run.
	body := `2026/06/18 09:00:00 event=runner_timeout issue_id=issue-1 msg="timeout"` + "\n"
	cluster := findCluster(t, reportFromManifest(t, root, body), "runner-timeout")
	if len(cluster.Affected.Runs) != 0 {
		t.Fatalf("manifest fabricated affected runs from the issue-id group key: %#v", cluster.Affected.Runs)
	}
	if !contains(cluster.Affected.Issues, "issue-1") {
		t.Fatalf("manifest dropped the affected issue id: %#v", cluster.Affected.Issues)
	}
}

func TestTraceEvidenceManifestRoundTripRecoversDroppedEventClassPerClass(t *testing.T) {
	root := repoRoot(t)
	// Many runner_timeout events fill the manifest's 64 KiB event cap, then a large
	// turn_input_required event for the same run is dropped whole — leaving the
	// input-required class with no retained evidence. Its session id must still be
	// recovered under an input-required cluster (not the retained runner-timeout
	// one), matching the raw --worker-log path with no cross-class mis-attribution.
	var body strings.Builder
	for i := 0; i < 60; i++ {
		body.WriteString(`2026/06/18 09:00:00 event=runner_timeout task_id=run-1 issue_id=issue-1 session_id=session-rt msg="`)
		body.WriteString(strings.Repeat("x", 1500))
		body.WriteString(`"` + "\n")
	}
	body.WriteString(`2026/06/18 09:00:01 event=turn_input_required task_id=run-1 issue_id=issue-1 session_id=session-ir msg="`)
	body.WriteString(strings.Repeat("y", 5000))
	body.WriteString(`"` + "\n")

	// The class is dropped whole at the event cap, so it survives only in the
	// per-class summary — never in a retained event for that class.
	run := manifestRun(t, runTraceEvidenceManifest(t, root, body.String()), "run-1")
	if run.DroppedEvents == 0 {
		t.Fatalf("setup invalid: want a dropped event from the 64 KiB cap, got dropped=%d", run.DroppedEvents)
	}
	if got := classAffected(t, run, "input-required").Sessions; !contains(got, "session-ir") {
		t.Fatalf("dropped class summary lost session-ir: %#v", got)
	}

	report := reportFromManifest(t, root, body.String())
	timeout := findCluster(t, report, "runner-timeout")
	if contains(timeout.Affected.Sessions, "session-ir") {
		t.Fatalf("dropped input-required session was mis-attributed to runner-timeout: %#v", timeout.Affected.Sessions)
	}
	// Faithful per-class recovery: an evidence-less input-required cluster carries
	// the dropped session id under its own class.
	inputRequired := findCluster(t, report, "input-required")
	if !contains(inputRequired.Affected.Sessions, "session-ir") {
		t.Fatalf("dropped class session-ir was not recovered under input-required: %#v", inputRequired.Affected.Sessions)
	}
	if len(inputRequired.Evidence) != 0 {
		t.Fatalf("input-required cluster should be evidence-less (its events were all dropped): %#v", inputRequired.Evidence)
	}

	// Affected ids round-trip to exactly what the raw --worker-log path reports.
	raw := runTraceHarnessReport(t, root, body.String())
	assertSamePerClassAffected(t, report, raw)
}

func assertSamePerClassAffected(t *testing.T, got, want traceReport) {
	t.Helper()
	wantByID := map[string]traceCluster{}
	for _, cluster := range want.Clusters {
		wantByID[cluster.ID] = cluster
	}
	for _, cluster := range got.Clusters {
		other, ok := wantByID[cluster.ID]
		if !ok {
			t.Fatalf("manifest report has cluster %q absent from the raw path", cluster.ID)
		}
		if a, b := sortedStrings(cluster.Affected.Sessions), sortedStrings(other.Affected.Sessions); !equalStrings(a, b) {
			t.Fatalf("cluster %q sessions = %v; raw path = %v", cluster.ID, a, b)
		}
		if a, b := sortedStrings(cluster.Affected.Issues), sortedStrings(other.Affected.Issues); !equalStrings(a, b) {
			t.Fatalf("cluster %q issues = %v; raw path = %v", cluster.ID, a, b)
		}
	}
	if len(got.Clusters) != len(want.Clusters) {
		t.Fatalf("manifest produced %d clusters; raw path produced %d", len(got.Clusters), len(want.Clusters))
	}
}

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestTraceHarnessReportDedupesIdenticalManifestInputs(t *testing.T) {
	root := repoRoot(t)
	// Many distinct ids push the manifest's per-class summary over the 256 KiB cap,
	// so it records omitted>0. Folding the SAME manifest twice must not double that
	// omitted count: the kept ids dedup, but the count's dropped values are gone, so
	// summing it twice would inflate the recovered count vs the raw path (#961).
	var body strings.Builder
	for i := 0; i < 12000; i++ {
		body.WriteString(`2026/06/18 09:00:00 event=runner_timeout task_id=run-1 issue_id=issue-`)
		body.WriteString(strconv.Itoa(i))
		body.WriteString(` session_id=session-`)
		body.WriteString(strconv.Itoa(i))
		body.WriteString(` msg="x"` + "\n")
	}
	manPath := filepath.Join(t.TempDir(), "m.json")
	if err := os.WriteFile(manPath, manifestRawBytes(t, root, body.String()), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	once := runTraceReportArgs(t, root, "--evidence-manifest", manPath)
	twice := runTraceReportArgs(t, root, "--evidence-manifest", manPath, "--evidence-manifest", manPath)

	if got := inputsLen(t, twice); got != 1 {
		t.Fatalf("identical manifest inputs = %d; want 1 (deduped by digest)", got)
	}
	o1 := findCluster(t, parseTraceReport(t, once), "runner-timeout").Affected.Omitted["issues"]
	o2 := findCluster(t, parseTraceReport(t, twice), "runner-timeout").Affected.Omitted["issues"]
	if o1 == 0 {
		t.Fatalf("setup invalid: want omitted issues > 0, got 0")
	}
	if o2 != o1 {
		t.Fatalf("folding the same manifest twice changed omitted: once=%d twice=%d", o1, o2)
	}
}

func TestTraceHarnessReportDedupesIdenticalWorkerLogInputs(t *testing.T) {
	root := repoRoot(t)
	logPath := filepath.Join(t.TempDir(), "worker.log")
	body := `2026/06/18 09:00:00 event=runner_timeout task_id=run-1 issue_id=issue-1 session_id=session-1 msg="timeout"` + "\n"
	if err := os.WriteFile(logPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}

	once := parseTraceReport(t, runTraceReportArgs(t, root, "--worker-log", logPath))
	twiceRaw := runTraceReportArgs(t, root, "--worker-log", logPath, "--worker-log", logPath)

	if got := inputsLen(t, twiceRaw); got != 1 {
		t.Fatalf("identical worker-log inputs = %d; want 1 (deduped by digest)", got)
	}
	// A byte-identical log folded twice must not double the cluster's evidence.
	e1 := len(findCluster(t, once, "runner-timeout").Evidence)
	e2 := len(findCluster(t, parseTraceReport(t, twiceRaw), "runner-timeout").Evidence)
	if e1 == 0 || e2 != e1 {
		t.Fatalf("identical worker-log folded twice changed evidence count: once=%d twice=%d", e1, e2)
	}
}

func TestTraceEvidenceManifestDedupesIdenticalWorkerLogInputs(t *testing.T) {
	root := repoRoot(t)
	logPath := filepath.Join(t.TempDir(), "worker.log")
	body := `2026/06/18 09:00:00 event=runner_timeout task_id=run-1 issue_id=issue-1 session_id=session-1 msg="timeout"` + "\n"
	if err := os.WriteFile(logPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}

	once := manifestRun(t, parseManifest(t, runTraceEvidenceManifestRawPath(t, root, logPath)), "run-1")
	jsonPath := filepath.Join(t.TempDir(), "m.json")
	cmd := exec.Command("python3", filepath.Join(root, "scripts", "trace-evidence-manifest.py"),
		"--worker-log", logPath, "--worker-log", logPath, "--json-out", jsonPath)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("manifest with duplicate log failed: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	twice := parseManifest(t, raw)
	if len(twice.Inputs) != 1 {
		t.Fatalf("identical worker-log inputs = %d; want 1 (deduped by digest)", len(twice.Inputs))
	}
	if got := manifestRun(t, twice, "run-1"); len(got.Events) != len(once.Events) {
		t.Fatalf("same log twice doubled events: once=%d twice=%d", len(once.Events), len(got.Events))
	}
}

func runTraceReportArgs(t *testing.T, root string, args ...string) []byte {
	t.Helper()
	out := filepath.Join(t.TempDir(), "report.json")
	cmdArgs := append([]string{filepath.Join(root, "scripts", "trace-harness-report.py")}, args...)
	cmdArgs = append(cmdArgs, "--json-out", out)
	cmd := exec.Command("python3", cmdArgs...)
	cmd.Dir = root
	if o, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("trace-harness-report failed: %v\n%s", err, o)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	return raw
}

func inputsLen(t *testing.T, raw []byte) int {
	t.Helper()
	var v struct {
		Inputs []json.RawMessage `json:"inputs"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("unmarshal inputs: %v\n%s", err, raw)
	}
	return len(v.Inputs)
}

func reportFromManifest(t *testing.T, root, body string) traceReport {
	t.Helper()
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(manifestPath, manifestRawBytes(t, root, body), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	reportPath := filepath.Join(dir, "report.json")
	cmd := exec.Command("python3", filepath.Join(root, "scripts", "trace-harness-report.py"),
		"--evidence-manifest", manifestPath, "--json-out", reportPath)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("report consuming manifest failed: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	return parseTraceReport(t, raw)
}

func clusterIDs(report traceReport) []string {
	ids := make([]string, 0, len(report.Clusters))
	for _, cluster := range report.Clusters {
		ids = append(ids, cluster.ID)
	}
	return ids
}

func TestTraceEvidenceManifestReportsInputDigest(t *testing.T) {
	root := repoRoot(t)
	body := []byte(`2026/06/18 09:00:00 event=runner_timeout task_id=run-1 issue_id=issue-1 msg="timeout"` + "\n")
	manifest := manifestFromBytes(t, root, body)

	if len(manifest.Inputs) != 1 {
		t.Fatalf("inputs = %d; want 1", len(manifest.Inputs))
	}
	want := sha256.Sum256(body)
	in := manifest.Inputs[0]
	if in.Type != "worker_log" || in.Bytes != len(body) || in.Sha256 != hex.EncodeToString(want[:]) {
		t.Fatalf("input ref = %#v; want worker_log {%d,%s}", in, len(body), hex.EncodeToString(want[:]))
	}
}

func TestTraceEvidenceManifestRoundTripsThroughReport(t *testing.T) {
	root := repoRoot(t)
	secretURL := "https://user:secret@example.test/org/repo.git"
	body := strings.Join([]string{
		`2026/06/18 09:00:00 event=runner_timeout task_id=run-1 issue_id=issue-1 session_id=session-1 msg="timeout" payload=map[output_head:` + secretURL + `]`,
		`2026/06/18 09:02:00 event=runner_end task_id=run-2 issue_id=issue-2 msg="runner failed" payload=map[ok:false]`,
		`2026/06/18 09:01:00 event=turn_input_required task_id=run-3 issue_id=issue-3 msg="input" payload=map[method:approval]`,
	}, "\n") + "\n"

	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(manifestPath, manifestRawBytes(t, root, body), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	reportPath := filepath.Join(dir, "report.json")
	cmd := exec.Command("python3", filepath.Join(root, "scripts", "trace-harness-report.py"),
		"--evidence-manifest", manifestPath, "--json-out", reportPath)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("report consuming manifest failed: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	report := parseTraceReport(t, raw)

	ids := make([]string, 0, len(report.Clusters))
	for _, cluster := range report.Clusters {
		ids = append(ids, cluster.ID)
	}
	for _, want := range []string{"runner-timeout", "runner-failure", "input-required"} {
		if !contains(ids, want) {
			t.Fatalf("manifest-derived report missing %q cluster; got %v\n%s", want, ids, raw)
		}
	}
	if !contains(findCluster(t, report, "runner-timeout").Affected.Sessions, "session-1") {
		t.Fatalf("affected ids did not survive the manifest round-trip: %#v", report.Clusters)
	}
	if strings.Contains(string(raw), "secret") {
		t.Fatalf("report leaked opaque payload from manifest input:\n%s", raw)
	}
	var inputs struct {
		Inputs []struct {
			Type string `json:"type"`
		} `json:"inputs"`
	}
	if err := json.Unmarshal(raw, &inputs); err != nil {
		t.Fatalf("unmarshal report inputs: %v", err)
	}
	if len(inputs.Inputs) != 1 || inputs.Inputs[0].Type != "evidence_manifest" {
		t.Fatalf("report inputs = %#v; want one evidence_manifest input", inputs.Inputs)
	}
}

func TestTraceHarnessReportRejectsNonManifestJSONOnEvidenceManifest(t *testing.T) {
	root := repoRoot(t)
	dir := t.TempDir()
	// A trace-harness-report/v3 output has clusters, not runs: feeding it to
	// --evidence-manifest must fail loudly, not silently produce an empty report.
	logPath := filepath.Join(dir, "worker.log")
	if err := os.WriteFile(logPath, []byte(`2026/06/18 09:00:00 event=runner_timeout task_id=run-1 issue_id=issue-1 msg="timeout"`+"\n"), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}
	reportPath := filepath.Join(dir, "report-v3.json")
	build := exec.Command("python3", filepath.Join(root, "scripts", "trace-harness-report.py"),
		"--worker-log", logPath, "--json-out", reportPath)
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build v3 report: %v\n%s", err, out)
	}

	misuse := exec.Command("python3", filepath.Join(root, "scripts", "trace-harness-report.py"),
		"--evidence-manifest", reportPath)
	misuse.Dir = root
	out, err := misuse.CombinedOutput()
	if err == nil {
		t.Fatalf("report accepted a v3 report as an evidence manifest:\n%s", out)
	}
	if !strings.Contains(string(out), "not a trace-evidence-manifest/v2 evidence manifest") {
		t.Fatalf("misuse error did not name the schema mismatch:\n%s", out)
	}
}

func TestTraceEvidenceManifestRequiresWorkerLog(t *testing.T) {
	root := repoRoot(t)
	cmd := exec.Command("python3", filepath.Join(root, "scripts", "trace-evidence-manifest.py"))
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("manifest unexpectedly succeeded with no --worker-log:\n%s", out)
	}
	if !strings.Contains(string(out), "at least one --worker-log is required") {
		t.Fatalf("manifest error did not name the missing input:\n%s", out)
	}
}

func runTraceEvidenceManifest(t *testing.T, root, body string) traceManifest {
	t.Helper()
	return manifestFromBytes(t, root, []byte(body))
}

func manifestRawBytes(t *testing.T, root, body string) []byte {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "worker.log")
	if err := os.WriteFile(logPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}
	return runTraceEvidenceManifestRawPath(t, root, logPath)
}

func manifestFromBytes(t *testing.T, root string, body []byte) traceManifest {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "worker.log")
	if err := os.WriteFile(logPath, body, 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}
	return parseManifest(t, runTraceEvidenceManifestRawPath(t, root, logPath))
}

func runTraceEvidenceManifestRawPath(t *testing.T, root, logPath string) []byte {
	t.Helper()
	jsonPath := filepath.Join(t.TempDir(), "manifest.json")
	cmd := exec.Command("python3", filepath.Join(root, "scripts", "trace-evidence-manifest.py"),
		"--worker-log", logPath, "--json-out", jsonPath)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("trace-evidence-manifest failed: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	return raw
}

func parseManifest(t *testing.T, raw []byte) traceManifest {
	t.Helper()
	var manifest traceManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("unmarshal manifest: %v\n%s", err, raw)
	}
	return manifest
}

func manifestRun(t *testing.T, manifest traceManifest, run string) traceManifestRun {
	t.Helper()
	for _, candidate := range manifest.Runs {
		if candidate.Run == run {
			return candidate
		}
	}
	t.Fatalf("missing run %q in %#v", run, manifest.Runs)
	return traceManifestRun{}
}

type traceManifest struct {
	SchemaVersion string `json:"schema_version"`
	Bounds        struct {
		MaxRunEvidenceBytes int `json:"max_run_evidence_bytes"`
		MaxClusterBytes     int `json:"max_cluster_bytes"`
	} `json:"bounds"`
	Inputs []struct {
		Type   string `json:"type"`
		Path   string `json:"path"`
		Bytes  int    `json:"bytes"`
		Sha256 string `json:"sha256"`
	} `json:"inputs"`
	Runs []traceManifestRun `json:"runs"`
}

type traceAffectedSummary struct {
	Issues           []string       `json:"issues"`
	IssueIdentifiers []string       `json:"issue_identifiers"`
	PullRequests     []string       `json:"pull_requests"`
	Runs             []string       `json:"runs"`
	Sessions         []string       `json:"sessions"`
	Omitted          map[string]int `json:"omitted"`
}

type traceManifestRun struct {
	Run string `json:"run"`
	// Affected ids partitioned by resolved failure class (#958), so a dropped
	// event's ids are recovered under its own class on the report round trip.
	AffectedByClass map[string]traceAffectedSummary `json:"affected_by_class"`
	Events          []struct {
		Source   string            `json:"source"`
		Ref      string            `json:"ref"`
		Kind     string            `json:"kind"`
		Class    string            `json:"class"`
		Excerpt  string            `json:"excerpt"`
		Metadata map[string]string `json:"metadata"`
		Affected struct {
			Issues   []string `json:"issues"`
			Sessions []string `json:"sessions"`
		} `json:"affected"`
	} `json:"events"`
	DroppedEvents int `json:"dropped_events"`
}

func classAffected(t *testing.T, run traceManifestRun, classID string) traceAffectedSummary {
	t.Helper()
	affected, ok := run.AffectedByClass[classID]
	if !ok {
		t.Fatalf("missing affected_by_class[%q] in %#v", classID, run.AffectedByClass)
	}
	return affected
}
