package scripts_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
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
		"trace-evidence-manifest/v1",
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

	if manifest.SchemaVersion != "trace-evidence-manifest/v1" {
		t.Fatalf("schema_version = %q; want trace-evidence-manifest/v1", manifest.SchemaVersion)
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
	if !contains(run1.Affected.Issues, "issue-1") || !contains(run1.Affected.Sessions, "session-1") {
		t.Fatalf("run-1 affected = %#v; want issue-1/session-1", run1.Affected)
	}
	if run1.Events[0].Metadata["output_bytes"] != "70000" || run1.Events[0].Metadata["timestamp"] != "2026/06/18 09:00:00" {
		t.Fatalf("run-1 first event metadata = %#v; want metadata-first scalars before the opaque key", run1.Events[0].Metadata)
	}
	// Each event records its resolved failure class so consumers re-cluster
	// without re-parsing the redacted excerpt.
	if run1.Events[0].Class != "runner-timeout" || run1.Events[1].Class != "runner-failure" {
		t.Fatalf("run-1 event classes = %q/%q; want runner-timeout/runner-failure", run1.Events[0].Class, run1.Events[1].Class)
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
	// The byte cap drops whole events but never the affected-id summary.
	if !contains(run.Affected.Issues, "issue-1") {
		t.Fatalf("byte cap dropped the affected id summary: %#v", run.Affected)
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
	if run.Affected.Omitted["issues"] == 0 {
		t.Fatalf("affected id explosion did not record omitted counts: %#v", run.Affected.Omitted)
	}
	if len(run.Affected.Issues) == 0 {
		t.Fatalf("run group lost every concrete affected issue id: omitted=%#v", run.Affected.Omitted)
	}
	// Accounting must be complete: kept + omitted equals every distinct issue id.
	if got := len(run.Affected.Issues) + run.Affected.Omitted["issues"]; got != total {
		t.Fatalf("issues accounted for = %d; want %d (kept=%d omitted=%d)", got, total, len(run.Affected.Issues), run.Affected.Omitted["issues"])
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

type traceManifestRun struct {
	Run      string `json:"run"`
	Affected struct {
		Issues           []string       `json:"issues"`
		IssueIdentifiers []string       `json:"issue_identifiers"`
		PullRequests     []string       `json:"pull_requests"`
		Runs             []string       `json:"runs"`
		Sessions         []string       `json:"sessions"`
		Omitted          map[string]int `json:"omitted"`
	} `json:"affected"`
	Events []struct {
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
