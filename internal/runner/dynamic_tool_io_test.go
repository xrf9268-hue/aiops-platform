package runner

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// decodeToolResult unwraps a dynamic-tool result envelope into its success flag
// and inner output payload so tests can assert on the agent-visible body.
func decodeToolResult(t *testing.T, result string) (success bool, output string) {
	t.Helper()
	var payload struct {
		Success bool   `json:"success"`
		Output  string `json:"output"`
	}
	if err := json.Unmarshal([]byte(result), &payload); err != nil {
		t.Fatalf("decode tool result %q: %v", result, err)
	}
	return payload.Success, payload.Output
}

// hangingServer never responds, so the only thing that can end a request is the
// client's own per-request deadline — which is exactly what the timeout tests
// assert. The handler parks on a stop channel (not r.Context().Done()) because
// server-side request-context cancellation does not reliably propagate on a
// client POST disconnect; closing stop before srv.Close() guarantees Cleanup
// cannot deadlock on a parked handler.
func hangingServer(t *testing.T) *httptest.Server {
	t.Helper()
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		<-stop
	}))
	t.Cleanup(func() {
		close(stop)
		srv.Close()
	})
	return srv
}

func TestLinearGraphQLRequestTimesOut(t *testing.T) {
	srv := hangingServer(t)
	proxy := linearGraphQLProxy{
		apiKey:      "secret-linear-key",
		baseURL:     srv.URL,
		http:        srv.Client(),
		httpTimeout: 20 * time.Millisecond,
	}

	start := time.Now()
	result, err := proxy.call(context.Background(), ToolCall{Query: "query { viewer { id } }"})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("call(hung server) returned Go error: %v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("call(hung server) took %v; want it bounded by the 20ms per-request timeout", elapsed)
	}
	success, output := decodeToolResult(t, result)
	if success {
		t.Fatalf("call(hung server) success = true; want a transport-timeout failure (output=%q)", output)
	}
	if !strings.Contains(output, "transport") {
		t.Errorf("call(hung server) output = %q; want it to report the transport failure", output)
	}
}

func TestGiteaIssueLabelsRequestTimesOut(t *testing.T) {
	srv := hangingServer(t)
	proxy := giteaIssueLabelsProxy{
		token:       "secret-gitea-token",
		baseURL:     srv.URL,
		owner:       "owner",
		repo:        "repo",
		http:        srv.Client(),
		httpTimeout: 20 * time.Millisecond,
	}

	start := time.Now()
	result, err := proxy.call(context.Background(), ToolCall{IssueNumber: 7, Labels: []string{"aiops/todo"}})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("call(hung server) returned Go error: %v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("call(hung server) took %v; want it bounded by the 20ms per-request timeout", elapsed)
	}
	success, output := decodeToolResult(t, result)
	if success {
		t.Fatalf("call(hung server) success = true; want a transport-timeout failure (output=%q)", output)
	}
	if !strings.Contains(output, "transport") {
		t.Errorf("call(hung server) output = %q; want it to report the transport failure", output)
	}
}

func TestGiteaIssueLabelsRejectsOversizedResponse(t *testing.T) {
	oversized := strings.Repeat("a", maxLinearGraphQLResponseBytes+10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, oversized)
	}))
	defer srv.Close()

	proxy := giteaIssueLabelsProxy{token: "token", baseURL: srv.URL, owner: "owner", repo: "repo", http: srv.Client()}
	result, err := proxy.call(context.Background(), ToolCall{IssueNumber: 7, Labels: []string{"aiops/todo"}})
	if err != nil {
		t.Fatalf("call(oversized response) returned Go error: %v", err)
	}
	success, output := decodeToolResult(t, result)
	if success {
		t.Fatalf("call(oversized response) success = true; want a size-cap failure (output=%q)", output)
	}
	if !strings.Contains(output, "exceeded maximum size") {
		t.Errorf("call(oversized response) output = %q; want it to report the size-cap failure", output)
	}
	if strings.Contains(output, "aaaa") {
		t.Errorf("call(oversized response) leaked the oversized body into the tool result")
	}
}

func TestGiteaIssueLabelsTruncatesAndRedactsErrorBody(t *testing.T) {
	const token = "super-secret-gitea-token"
	filler := strings.Repeat("x", maxToolErrorBodyRunes+1000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "gitea error echoing token "+token+" then "+filler)
	}))
	defer srv.Close()

	proxy := giteaIssueLabelsProxy{token: token, baseURL: srv.URL, owner: "owner", repo: "repo", http: srv.Client()}
	result, err := proxy.call(context.Background(), ToolCall{IssueNumber: 7, Labels: []string{"aiops/todo"}})
	if err != nil {
		t.Fatalf("call(500) returned Go error: %v", err)
	}
	if strings.Contains(result, token) {
		t.Fatalf("call(500) leaked the gitea token into the tool result")
	}
	success, output := decodeToolResult(t, result)
	if success {
		t.Fatalf("call(500) success = true; want a failure")
	}
	if !strings.Contains(output, "[REDACTED]") {
		t.Errorf("call(500) output did not redact the token: %q", clip(output))
	}
	if !strings.Contains(output, "[truncated]") {
		t.Errorf("call(500) output was not truncated (len=%d): %q", len(output), clip(output))
	}
}

func TestLinearGraphQLTruncatesAndRedactsErrorBody(t *testing.T) {
	const token = "super-secret-linear-key"
	filler := strings.Repeat("x", maxToolErrorBodyRunes+1000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "upstream error token="+token+" then "+filler)
	}))
	defer srv.Close()

	proxy := linearGraphQLProxy{apiKey: token, baseURL: srv.URL, http: srv.Client()}
	result, err := proxy.call(context.Background(), ToolCall{Query: "query { viewer { id } }"})
	if err != nil {
		t.Fatalf("call(502) returned Go error: %v", err)
	}
	if strings.Contains(result, token) {
		t.Fatalf("call(502) leaked the linear token into the tool result")
	}
	success, output := decodeToolResult(t, result)
	if success {
		t.Fatalf("call(502) success = true; want a failure")
	}
	if !strings.Contains(output, "[REDACTED]") {
		t.Errorf("call(502) output did not redact the token: %q", clip(output))
	}
	if !strings.Contains(output, "[truncated]") {
		t.Errorf("call(502) output was not truncated (len=%d): %q", len(output), clip(output))
	}
}

func TestSanitizeToolErrorBody(t *testing.T) {
	// Every occurrence of a secret is replaced; the secret never survives.
	if got := sanitizeToolErrorBody([]byte("a SECRET b SECRET c"), "SECRET"); strings.Contains(got, "SECRET") || !strings.Contains(got, "[REDACTED]") {
		t.Errorf("sanitizeToolErrorBody(redaction) = %q; want SECRET replaced by [REDACTED]", got)
	}
	// Empty/whitespace secrets are ignored — no spurious substitution.
	if got := sanitizeToolErrorBody([]byte("hello world"), "", "   "); got != "hello world" {
		t.Errorf("sanitizeToolErrorBody(empty secrets) = %q; want %q", got, "hello world")
	}
	// Ordering is load-bearing: a secret straddling the truncation boundary must
	// be redacted in full BEFORE truncation. A truncate-first implementation
	// would retain the secret's head fragment ("SEC") in the kept prefix;
	// redact-first leaves only "[RE…". The body also exceeds the cap, so the
	// truncation marker is appended.
	straddle := strings.Repeat("y", maxToolErrorBodyRunes-3) + "SECRET" + strings.Repeat("z", 100)
	got := sanitizeToolErrorBody([]byte(straddle), "SECRET")
	if strings.Contains(got, "SEC") {
		t.Errorf("sanitizeToolErrorBody leaked a secret fragment across the truncation boundary (truncate-before-redact?): tail=%q", got[max(0, len(got)-40):])
	}
	if !strings.HasSuffix(got, "…[truncated]") {
		t.Errorf("sanitizeToolErrorBody did not append the truncation marker: tail=%q", got[max(0, len(got)-20):])
	}
	// A short body is returned verbatim (no marker).
	if got := sanitizeToolErrorBody([]byte("brief"), "nope"); got != "brief" {
		t.Errorf("sanitizeToolErrorBody(short) = %q; want %q", got, "brief")
	}
}

// The acceptance criterion is "no tracker token appears in ANY tool result",
// so redaction must cover the success and unparseable-2xx paths too, not only
// non-2xx — a fronting proxy can echo the Authorization value at any status.

func TestLinearGraphQLRedactsTokenInSuccessBody(t *testing.T) {
	const token = "super-secret-linear-key"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// A valid-JSON 200 body that echoes the request's Authorization value.
		_, _ = io.WriteString(w, `{"data":{"viewer":{"id":"`+token+`"}}}`)
	}))
	defer srv.Close()

	proxy := linearGraphQLProxy{apiKey: token, baseURL: srv.URL, http: srv.Client()}
	result, err := proxy.call(context.Background(), ToolCall{Query: "query { viewer { id } }"})
	if err != nil {
		t.Fatalf("call(success echo) returned Go error: %v", err)
	}
	if strings.Contains(result, token) {
		t.Fatalf("call(success echo) leaked the linear token into the tool result: %q", clip(result))
	}
	success, output := decodeToolResult(t, result)
	if !success {
		t.Fatalf("call(success echo) success = false; want success (output=%q)", clip(output))
	}
	if !strings.Contains(output, "[REDACTED]") {
		t.Errorf("call(success echo) output did not redact the echoed token: %q", clip(output))
	}
}

func TestLinearGraphQLRedactsAndTruncatesUnparseableBody(t *testing.T) {
	const token = "super-secret-linear-key"
	filler := strings.Repeat("x", maxToolErrorBodyRunes+1000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// 200 OK but NOT valid JSON (e.g. a proxy interstitial) echoing the token.
		_, _ = io.WriteString(w, "<html>proxy 200 reflecting token "+token+" "+filler+"</html>")
	}))
	defer srv.Close()

	proxy := linearGraphQLProxy{apiKey: token, baseURL: srv.URL, http: srv.Client()}
	result, err := proxy.call(context.Background(), ToolCall{Query: "query { viewer { id } }"})
	if err != nil {
		t.Fatalf("call(unparseable 200) returned Go error: %v", err)
	}
	if strings.Contains(result, token) {
		t.Fatalf("call(unparseable 200) leaked the linear token into the tool result: %q", clip(result))
	}
	success, output := decodeToolResult(t, result)
	if success {
		t.Fatalf("call(unparseable 200) success = true; want a not-valid-JSON failure")
	}
	if !strings.Contains(output, "[REDACTED]") {
		t.Errorf("call(unparseable 200) output did not redact the token: %q", clip(output))
	}
	if !strings.Contains(output, "[truncated]") {
		t.Errorf("call(unparseable 200) output was not truncated (len=%d): %q", len(output), clip(output))
	}
}

func TestGiteaIssueLabelsRedactsTokenInSuccessBody(t *testing.T) {
	const token = "super-secret-gitea-token"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_, _ = io.WriteString(w, `[{"id":5,"name":"bug"}]`)
		case http.MethodPost:
			// 2xx label-add body that also echoes the Authorization token.
			_, _ = io.WriteString(w, `{"labels":[{"id":1,"name":"aiops/todo"}],"echo":"`+token+`"}`)
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	}))
	defer srv.Close()

	proxy := giteaIssueLabelsProxy{token: token, baseURL: srv.URL, owner: "owner", repo: "repo", http: srv.Client()}
	result, err := proxy.call(context.Background(), ToolCall{IssueNumber: 7, Labels: []string{"aiops/todo"}})
	if err != nil {
		t.Fatalf("call(success echo) returned Go error: %v", err)
	}
	if strings.Contains(result, token) {
		t.Fatalf("call(success echo) leaked the gitea token into the tool result: %q", clip(result))
	}
	success, output := decodeToolResult(t, result)
	if !success {
		t.Fatalf("call(success echo) success = false; want success (output=%q)", clip(output))
	}
	if !strings.Contains(output, "[REDACTED]") {
		t.Errorf("call(success echo) output did not redact the echoed token: %q", clip(output))
	}
}

// clip bounds an error-message excerpt so a multi-kilobyte body does not flood
// test output.
func clip(s string) string {
	const n = 200
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
