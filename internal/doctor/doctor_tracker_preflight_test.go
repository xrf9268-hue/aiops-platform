package doctor

// Tests for the Gitea/GitHub tracker credential preflight (#781): the
// empty-key FAIL, the mock-mode skip, and the real-mode listing that drives
// the worker's own tracker clients — so the requests asserted here are the
// production poll queries themselves, not hand-built doctor probes (the
// PR #801 drift class).

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// writeTrackerPreflightWorkflow writes a minimal non-Linear workflow whose
// tracker section is fully caller-controlled (no implicit project_slug, so
// the env-fallback base-URL tests can exercise the worker's resolution chain).
func writeTrackerPreflightWorkflow(t *testing.T, kind, extraTracker string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	body := `---
repo:
  owner: o
  name: r
  clone_url: https://example.invalid/o/r.git
tracker:
  kind: ` + kind + extraTracker + `
agent:
  default: mock
---
prompt
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	return path
}

// newTrackerStub serves status for every request, asserts the exact
// Authorization header the worker's client sends, and records each request's
// path+query for endpoint-shape assertions.
func newTrackerStub(t *testing.T, wantAuth string, status int) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var requests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != wantAuth {
			t.Errorf("Authorization = %q; want %q", got, wantAuth)
		}
		mu.Lock()
		requests = append(requests, r.URL.RequestURI())
		mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(requests)
	}
}

// newPullsRejectingStub serves 200 `[]` for every request except paths
// containing /pulls, which get 403 — modeling a token that can read issues
// but not pull requests (a fine-grained PAT with Issues:read only).
func newPullsRejectingStub(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var requests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.URL.RequestURI())
		mu.Unlock()
		if strings.Contains(r.URL.Path, "/pulls") {
			w.WriteHeader(http.StatusForbidden)
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(requests)
	}
}

// assertRequestsHitWorkerListingSurface asserts every recorded request stays
// on the worker's repo listing surface and no path carries a doubled slash
// (the trailing-slash base-URL regression). The exact query shape belongs to
// the production clients, so this deliberately pins only the prefix.
func assertRequestsHitWorkerListingSurface(t *testing.T, requests []string, wantPrefix string) {
	t.Helper()
	if len(requests) == 0 {
		t.Fatalf("recorded requests = %v; want at least one request on the worker's listing surface %q", requests, wantPrefix)
	}
	for _, request := range requests {
		if !strings.HasPrefix(request, wantPrefix) {
			t.Errorf("recorded request = %q; want prefix %q (the worker's listing surface)", request, wantPrefix)
		}
		if strings.Contains(request, "//") {
			t.Errorf("recorded request = %q; want no %q from base-URL joining", request, "//")
		}
	}
}

func countRequestsContaining(requests []string, fragment string) int {
	count := 0
	for _, request := range requests {
		if strings.Contains(request, fragment) {
			count++
		}
	}
	return count
}

func TestBuildReportFailsWhenGiteaTrackerAPIKeyMissing(t *testing.T) {
	report := BuildReport(context.Background(), Options{
		WorkflowPath: writeTrackerPreflightWorkflow(t, "gitea", ""),
		Mode:         "mock",
		Runner:       passingRunner,
	})

	check := findCheck(t, report, "Gitea API key")
	if check.Status != Fail {
		t.Fatalf("Gitea API key status = %s; want %s", check.Status, Fail)
	}
	if !strings.Contains(check.Fix, "tracker.api_key: $GITEA_TOKEN") {
		t.Fatalf("Gitea API key fix = %q; want the tracker.api_key: $GITEA_TOKEN remediation", check.Fix)
	}
}

func TestBuildReportMockModeSkipsGiteaTrackerLiveAuth(t *testing.T) {
	report := BuildReport(context.Background(), Options{
		WorkflowPath: writeTrackerPreflightWorkflow(t, "gitea", "\n  api_key: gitea-key"),
		Mode:         "mock",
		Runner:       passingRunner,
	})

	check := findCheck(t, report, "Gitea API key")
	if check.Status != Pass || !strings.Contains(check.Detail, "live auth skipped in mock mode") {
		t.Fatalf("Gitea API key check = %+v; want PASS with the mock-mode skip detail", check)
	}
	if checkExists(report, "Gitea auth") {
		t.Fatalf("Gitea auth check present in mock mode; want the live listing skipped in %+v", report.Checks)
	}
}

func TestBuildReportRealModeAuthenticatesGiteaTracker(t *testing.T) {
	installFakeGitOnly(t)
	srv, requests := newTrackerStub(t, "token gitea-key", http.StatusOK)
	report := BuildReport(context.Background(), Options{
		WorkflowPath: writeTrackerPreflightWorkflow(t, "gitea", "\n  api_key: gitea-key\n  endpoint: "+srv.URL),
		Mode:         "real",
		Runner:       passingRunner,
	})

	if check := findCheck(t, report, "Gitea auth"); check.Status != Pass {
		t.Fatalf("Gitea auth check = %+v; want PASS", check)
	}
	assertRequestsHitWorkerListingSurface(t, requests(), "/api/v1/repos/o/r/")
}

func TestBuildReportRealModeGiteaTrackerUsesGiteaBaseURLFallback(t *testing.T) {
	installFakeGitOnly(t)
	srv, requests := newTrackerStub(t, "token gitea-key", http.StatusOK)
	t.Setenv("GITEA_BASE_URL", srv.URL)
	report := BuildReport(context.Background(), Options{
		WorkflowPath: writeTrackerPreflightWorkflow(t, "gitea", "\n  api_key: gitea-key"),
		Mode:         "real",
		Runner:       passingRunner,
	})

	if check := findCheck(t, report, "Gitea auth"); check.Status != Pass {
		t.Fatalf("Gitea auth check with GITEA_BASE_URL fallback = %+v; want PASS", check)
	}
	assertRequestsHitWorkerListingSurface(t, requests(), "/api/v1/repos/o/r/")
}

func TestBuildReportRealModeFailsGiteaTrackerOnUnauthorized(t *testing.T) {
	installFakeGitOnly(t)
	srv, _ := newTrackerStub(t, "token bad-key", http.StatusUnauthorized)
	report := BuildReport(context.Background(), Options{
		WorkflowPath: writeTrackerPreflightWorkflow(t, "gitea", "\n  api_key: bad-key\n  endpoint: "+srv.URL),
		Mode:         "real",
		Runner:       passingRunner,
	})

	check := findCheck(t, report, "Gitea auth")
	if check.Status != Fail {
		t.Fatalf("Gitea auth status on 401 = %s; want %s", check.Status, Fail)
	}
	if !strings.Contains(check.Detail, "401") {
		t.Fatalf("Gitea auth detail = %q; want the client's 401 status surfaced", check.Detail)
	}
}

func TestBuildReportFailsWhenGitHubTrackerAPIKeyMissing(t *testing.T) {
	report := BuildReport(context.Background(), Options{
		WorkflowPath: writeTrackerPreflightWorkflow(t, "github", ""),
		Mode:         "mock",
		Runner:       passingRunner,
	})

	check := findCheck(t, report, "GitHub tracker API key")
	if check.Status != Fail {
		t.Fatalf("GitHub tracker API key status = %s; want %s", check.Status, Fail)
	}
	if !strings.Contains(check.Fix, "tracker.api_key: $GITHUB_TOKEN") {
		t.Fatalf("GitHub tracker API key fix = %q; want the tracker.api_key: $GITHUB_TOKEN remediation", check.Fix)
	}
}

func TestBuildReportMockModeSkipsGitHubTrackerLiveAuth(t *testing.T) {
	report := BuildReport(context.Background(), Options{
		WorkflowPath: writeTrackerPreflightWorkflow(t, "github", "\n  api_key: gh-key"),
		Mode:         "mock",
		Runner:       passingRunner,
	})

	check := findCheck(t, report, "GitHub tracker API key")
	if check.Status != Pass || !strings.Contains(check.Detail, "live auth skipped in mock mode") {
		t.Fatalf("GitHub tracker API key check = %+v; want PASS with the mock-mode skip detail", check)
	}
	if checkExists(report, "GitHub tracker auth") {
		t.Fatalf("GitHub tracker auth check present in mock mode; want the live listing skipped in %+v", report.Checks)
	}
}

func TestBuildReportRealModeAuthenticatesGitHubTracker(t *testing.T) {
	installFakeGitOnly(t)
	srv, requests := newTrackerStub(t, "Bearer gh-key", http.StatusOK)
	report := BuildReport(context.Background(), Options{
		WorkflowPath: writeTrackerPreflightWorkflow(t, "github", "\n  api_key: gh-key\n  endpoint: "+srv.URL),
		Mode:         "real",
		Runner:       passingRunner,
	})

	if check := findCheck(t, report, "GitHub tracker auth"); check.Status != Pass {
		t.Fatalf("GitHub tracker auth check = %+v; want PASS", check)
	}
	got := requests()
	assertRequestsHitWorkerListingSurface(t, got, "/repos/o/r/")
	// The default active states include open issues, so the client's
	// claimed-issue detection lists open pulls exactly once.
	if pulls := countRequestsContaining(got, "/pulls"); pulls != 1 {
		t.Fatalf("GitHub /pulls requests = %d in %v; want exactly 1 (the client's claimed-issue listing)", pulls, got)
	}
}

func TestBuildReportRealModeGitHubTrackerUsesEnvBaseURLFallback(t *testing.T) {
	installFakeGitOnly(t)
	srv, requests := newTrackerStub(t, "Bearer gh-key", http.StatusOK)
	t.Setenv("GITHUB_API_BASE_URL", srv.URL)
	report := BuildReport(context.Background(), Options{
		WorkflowPath: writeTrackerPreflightWorkflow(t, "github", "\n  api_key: gh-key"),
		Mode:         "real",
		Runner:       passingRunner,
	})

	if check := findCheck(t, report, "GitHub tracker auth"); check.Status != Pass {
		t.Fatalf("GitHub tracker auth check with GITHUB_API_BASE_URL fallback = %+v; want PASS", check)
	}
	assertRequestsHitWorkerListingSurface(t, requests(), "/repos/o/r/")
}

func TestBuildReportRealModeFailsGitHubTrackerOnUnauthorized(t *testing.T) {
	installFakeGitOnly(t)
	srv, _ := newTrackerStub(t, "Bearer bad-key", http.StatusUnauthorized)
	report := BuildReport(context.Background(), Options{
		WorkflowPath: writeTrackerPreflightWorkflow(t, "github", "\n  api_key: bad-key\n  endpoint: "+srv.URL),
		Mode:         "real",
		Runner:       passingRunner,
	})

	check := findCheck(t, report, "GitHub tracker auth")
	if check.Status != Fail {
		t.Fatalf("GitHub tracker auth status on 401 = %s; want %s", check.Status, Fail)
	}
	if !strings.Contains(check.Detail, "401") {
		t.Fatalf("GitHub tracker auth detail = %q; want the client's 401 status surfaced", check.Detail)
	}
}

func TestCheckGiteaTrackerFailsWithoutRepoOwnerAndName(t *testing.T) {
	r := &reportBuilder{opts: Options{Mode: "real"}}
	r.normalize()
	cfg := workflow.Config{Tracker: workflow.TrackerConfig{Kind: "gitea", APIKey: "gitea-key"}}
	r.checkGiteaTracker(context.Background(), cfg)

	check := findCheck(t, Report{Checks: r.checks}, "Gitea auth")
	if check.Status != Fail {
		t.Fatalf("checkGiteaTracker(no repo.owner/name) Gitea auth status = %s; want %s", check.Status, Fail)
	}
	if !strings.Contains(check.Fix, "repo.owner and repo.name") {
		t.Fatalf("Gitea auth fix = %q; want the repo.owner/repo.name remediation", check.Fix)
	}
}

// roundTripFunc adapts a function to http.RoundTripper so a test can inject a
// transport failure without opening a socket.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// TestCheckGiteaTrackerMasksEndpointUserinfoInErrors pins the masking
// guarantee at the client-error wrap point: the production client's transport
// errors are *url.Error values embedding the request URL (only the password
// is redacted by net/http — a token in the username position survives), so
// the FAIL detail must rebuild around the masked base URL before the text
// reaches the report. The sentinel error text also pins the HTTPClient
// injection seam: only the injected transport can produce it, so dropping
// `client.HTTP = r.opts.HTTPClient` makes the real dial error surface
// instead.
func TestCheckGiteaTrackerMasksEndpointUserinfoInErrors(t *testing.T) {
	const secret = "tracker-userinfo-secret"
	r := &reportBuilder{opts: Options{
		Mode: "real",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("injected sentinel transport failure: connection refused")
		})},
	}}
	r.normalize()
	cfg := workflow.Config{}
	cfg.Tracker.Kind = "gitea"
	cfg.Tracker.APIKey = "gitea-key"
	cfg.Tracker.Endpoint = "https://" + secret + "@127.0.0.1:1"
	// A hand-built config carries no loader defaults, so name the active
	// states explicitly; an empty set would short-circuit the listing without
	// any request reaching the failing transport.
	cfg.Tracker.ActiveStates = []string{"Todo"}
	cfg.Repo.Owner = "o"
	cfg.Repo.Name = "r"

	r.checkGiteaTracker(context.Background(), cfg)

	check := findCheck(t, Report{Checks: r.checks}, "Gitea auth")
	if check.Status != Fail {
		t.Fatalf("checkGiteaTracker(unreachable endpoint) Gitea auth status = %s; want %s", check.Status, Fail)
	}
	if strings.Contains(check.Detail, secret) {
		t.Fatalf("Gitea auth detail = %q; must not leak endpoint userinfo credentials", check.Detail)
	}
	if !strings.Contains(check.Detail, "injected sentinel transport failure") {
		t.Fatalf("Gitea auth detail = %q; want the injected transport's cause %q (pins both the error pass-through and the HTTPClient injection)", check.Detail, "injected sentinel transport failure")
	}
	if !strings.Contains(check.Detail, "127.0.0.1:1") {
		t.Fatalf("Gitea auth detail = %q; want the masked endpoint host %q kept for diagnosis", check.Detail, "127.0.0.1:1")
	}
}

// TestCheckTrackerWarnsOnUnknownKind pins the defensive default branch of the
// kind dispatch: the loader rejects unsupported kinds, but a direct caller
// must still get a visible WARN rather than a silent skip.
func TestCheckTrackerWarnsOnUnknownKind(t *testing.T) {
	r := &reportBuilder{opts: Options{Mode: "mock"}}
	r.normalize()
	r.checkTracker(context.Background(), workflow.Config{Tracker: workflow.TrackerConfig{Kind: "jira"}})

	check := findCheck(t, Report{Checks: r.checks}, "Tracker")
	if check.Status != Warn {
		t.Fatalf("checkTracker(kind=jira) Tracker status = %s; want %s", check.Status, Warn)
	}
}

// TestCheckLinearGraphQLProjectSlugErrorNamesOnlyTrackerProjectSlug pins the
// removal of the services[].tracker.project_slug clause: that schema was
// deleted in #573 (DEVIATIONS D25), so a remediation citing it steers
// operators into a config the loader rejects.
func TestCheckLinearGraphQLProjectSlugErrorNamesOnlyTrackerProjectSlug(t *testing.T) {
	r := &reportBuilder{opts: Options{Mode: "real"}}
	r.normalize()

	err := r.checkLinearGraphQL(context.Background(), workflow.Config{})
	if err == nil {
		t.Fatalf("checkLinearGraphQL(no project_slug) error = nil; want missing-slug error")
	}
	if got, want := err.Error(), "linear project_slug is required at tracker.project_slug"; got != want {
		t.Fatalf("checkLinearGraphQL(no project_slug) error = %q; want %q", got, want)
	}
}

// TestLinearProbeCarriesPerRequestDeadline pins the repo convention that the
// doctor's Linear GraphQL probe enforces its own context deadline instead of
// trusting the injectable HTTPClient's timeout: with a zero-timeout client, a
// stalled tracker would otherwise hang doctor forever. The Gitea/GitHub
// preflights need no doctor-side pin anymore: they drive the production
// clients, whose per-request deadlines are owned and tested by their own
// packages (internal/gitea requestTimeout; internal/tracker github.go
// RequestTimeout, #295).
func TestLinearProbeCarriesPerRequestDeadline(t *testing.T) {
	var sawDeadline []bool
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		_, ok := req.Context().Deadline()
		sawDeadline = append(sawDeadline, ok)
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"data":{"projects":{"nodes":[{"id":"p"}]}}}`)),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
		}, nil
	})}
	r := &reportBuilder{opts: Options{Mode: "real", HTTPClient: client}}
	r.normalize()

	cfg := workflow.Config{}
	cfg.Tracker.Kind = "linear"
	cfg.Tracker.APIKey = "lin-k"
	cfg.Tracker.ProjectSlug = "platform"
	if err := r.checkLinearGraphQL(context.Background(), cfg); err != nil {
		t.Fatalf("checkLinearGraphQL() = %v; want nil from the stub transport", err)
	}

	if len(sawDeadline) != 1 {
		t.Fatalf("probe request count = %d; want 1 (the Linear GraphQL probe)", len(sawDeadline))
	}
	if !sawDeadline[0] {
		t.Errorf("Linear probe request carried no context deadline; want trackerProbeTimeout-bounded context")
	}
}

// TestBuildReportRealModeGitHubTrackerNormalizesTrailingSlashEndpoint pins
// NewGitHubClient's trailing-slash normalization on the doctor path: a
// trailing-slash tracker.endpoint must still produce clean listing paths, not
// "//repos/...", or doctor would query a different URL than the poll loop.
func TestBuildReportRealModeGitHubTrackerNormalizesTrailingSlashEndpoint(t *testing.T) {
	installFakeGitOnly(t)
	srv, requests := newTrackerStub(t, "Bearer gh-key", http.StatusOK)
	report := BuildReport(context.Background(), Options{
		WorkflowPath: writeTrackerPreflightWorkflow(t, "github", "\n  api_key: gh-key\n  endpoint: "+srv.URL+"/"),
		Mode:         "real",
		Runner:       passingRunner,
	})

	if check := findCheck(t, report, "GitHub tracker auth"); check.Status != Pass {
		t.Fatalf("GitHub tracker auth check with trailing-slash endpoint = %+v; want PASS", check)
	}
	assertRequestsHitWorkerListingSurface(t, requests(), "/repos/o/r/")
}

// TestBuildReportRealModeGiteaTrackerNormalizesTrailingSlashEndpoint mirrors
// the GitHub trailing-slash pin on the Gitea side: BaseURLFromTrackerConfig
// owns the TrimRight there, and this test fails if that normalization (or the
// doctor's reliance on it) regresses into "//api/v1/..." listing paths.
func TestBuildReportRealModeGiteaTrackerNormalizesTrailingSlashEndpoint(t *testing.T) {
	installFakeGitOnly(t)
	srv, requests := newTrackerStub(t, "token gitea-key", http.StatusOK)
	report := BuildReport(context.Background(), Options{
		WorkflowPath: writeTrackerPreflightWorkflow(t, "gitea", "\n  api_key: gitea-key\n  endpoint: "+srv.URL+"/"),
		Mode:         "real",
		Runner:       passingRunner,
	})

	if check := findCheck(t, report, "Gitea auth"); check.Status != Pass {
		t.Fatalf("Gitea auth check with trailing-slash endpoint = %+v; want PASS", check)
	}
	assertRequestsHitWorkerListingSurface(t, requests(), "/api/v1/repos/o/r/")
}

// TestBuildReportRealModeGitHubTrackerClosedOnlyStatesSkipPullsListing pins
// the PR #801 round-4 finding as a regression: ListIssuesByStates gates the
// open-pulls claimed-issue listing on githubStatesMayIncludeOpenIssues, so a
// closed-only active_states workflow polled with an Issues-only token never
// touches /pulls — and doctor, driving the same client, must inherit that
// gating instead of false-FAILing on a hand-built unconditional /pulls probe.
func TestBuildReportRealModeGitHubTrackerClosedOnlyStatesSkipPullsListing(t *testing.T) {
	installFakeGitOnly(t)
	srv, requests := newPullsRejectingStub(t)
	report := BuildReport(context.Background(), Options{
		WorkflowPath: writeTrackerPreflightWorkflow(t, "github", "\n  api_key: gh-key\n  endpoint: "+srv.URL+"\n  active_states: [\"closed\"]"),
		Mode:         "real",
		Runner:       passingRunner,
	})

	if check := findCheck(t, report, "GitHub tracker auth"); check.Status != Pass {
		t.Fatalf("GitHub tracker auth with closed-only active_states and a pulls-403 stub = %+v; want PASS (the client never lists pulls for closed-only states)", check)
	}
	got := requests()
	assertRequestsHitWorkerListingSurface(t, got, "/repos/o/r/")
	if pulls := countRequestsContaining(got, "/pulls"); pulls != 0 {
		t.Fatalf("GitHub /pulls requests = %d in %v; want 0 (doctor must inherit the client's githubStatesMayIncludeOpenIssues gating)", pulls, got)
	}
}

// TestBuildReportRealModeFailsGitHubTrackerWhenPullsForbidden is the inverse
// of the closed-only pin: with the default open-inclusive active states the
// client's claimed-issue detection does list open pulls, so a token that can
// read issues but not pull requests must FAIL doctor exactly as it would fail
// the first poll.
func TestBuildReportRealModeFailsGitHubTrackerWhenPullsForbidden(t *testing.T) {
	installFakeGitOnly(t)
	srv, requests := newPullsRejectingStub(t)
	report := BuildReport(context.Background(), Options{
		WorkflowPath: writeTrackerPreflightWorkflow(t, "github", "\n  api_key: gh-key\n  endpoint: "+srv.URL),
		Mode:         "real",
		Runner:       passingRunner,
	})

	check := findCheck(t, report, "GitHub tracker auth")
	if check.Status != Fail {
		t.Fatalf("GitHub tracker auth with pulls 403 and open-inclusive states = %+v; want FAIL", check)
	}
	if !strings.Contains(check.Detail, "403") {
		t.Fatalf("GitHub tracker auth detail = %q; want the client's 403 status surfaced", check.Detail)
	}
	got := requests()
	if pulls := countRequestsContaining(got, "/pulls"); pulls == 0 {
		t.Fatalf("GitHub /pulls requests = 0 in %v; want at least 1 (open-inclusive states list pulls first)", got)
	}
}

// TestBuildReportRealModeFailsGiteaTrackerOnNonJSONBody pins the consumer-
// fidelity rule for 2xx responses: the production client decodes the listing
// immediately, so a login proxy or misrouted base URL answering 200 with HTML
// fails preflight with the client's own decode error, not pass on status
// alone.
func TestBuildReportRealModeFailsGiteaTrackerOnNonJSONBody(t *testing.T) {
	installFakeGitOnly(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body>login</body></html>"))
	}))
	t.Cleanup(srv.Close)
	report := BuildReport(context.Background(), Options{
		WorkflowPath: writeTrackerPreflightWorkflow(t, "gitea", "\n  api_key: gitea-key\n  endpoint: "+srv.URL),
		Mode:         "real",
		Runner:       passingRunner,
	})

	check := findCheck(t, report, "Gitea auth")
	if check.Status != Fail {
		t.Fatalf("Gitea auth with 200 HTML body = %+v; want FAIL on the client's decode error", check)
	}
	if !strings.Contains(check.Detail, "invalid character") {
		t.Fatalf("Gitea auth detail = %q; want the client's JSON decode error fragment %q", check.Detail, "invalid character")
	}
}

// TestProbeLinearProjectSlugMasksEndpointUserinfoInErrors mirrors the
// Gitea/GitHub masking pin on the Linear path: request/transport errors must
// rebuild around the masked endpoint before the text reaches the Linear auth
// FAIL detail.
func TestProbeLinearProjectSlugMasksEndpointUserinfoInErrors(t *testing.T) {
	const secret = "linear-userinfo-secret"
	r := &reportBuilder{opts: Options{
		Mode: "real",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("dial tcp 127.0.0.1:443: connection refused")
		})},
	}}
	r.normalize()

	cases := []struct {
		name     string
		endpoint string
		wantPart string
	}{
		{
			name:     "transport error keeps the inner cause",
			endpoint: "https://" + secret + "@linear.example/graphql",
			wantPart: "connection refused",
		},
		{
			name:     "request parse error drops the raw URL",
			endpoint: "https://bot:" + secret + "@linear.example/graphql\n",
			wantPart: "linear.example",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := r.probeLinearProjectSlug(context.Background(), tc.endpoint, "lin-key", "query {}", "platform")
			if err == nil {
				t.Fatalf("probeLinearProjectSlug(%q) = nil; want masked error", tc.endpoint)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("probeLinearProjectSlug(%q) error %q leaks the userinfo secret", tc.endpoint, err)
			}
			if !strings.Contains(err.Error(), tc.wantPart) {
				t.Fatalf("probeLinearProjectSlug(%q) error %q; want it to keep %q", tc.endpoint, err, tc.wantPart)
			}
		})
	}
}

// TestBuildReportRealModeFailsLinearOnNonJSONBody mirrors the Gitea non-JSON
// pin on the Linear path: a login proxy answering the GraphQL probe with 200
// HTML must FAIL with a detail naming the non-JSON body, not a bare
// json.SyntaxError.
func TestBuildReportRealModeFailsLinearOnNonJSONBody(t *testing.T) {
	installFakeCodex(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body>login</body></html>"))
	}))
	t.Cleanup(srv.Close)
	path := writeWorkflowWithEndpoint(t, srv.URL, "codex-app-server")
	t.Setenv("AIOPS_TEST_LINEAR_KEY", "lin-test")
	report := BuildReport(context.Background(), Options{
		WorkflowPath: path,
		Mode:         "real",
		Runner:       fakeRealRunner,
	})

	check := findCheck(t, report, "Linear auth")
	if check.Status != Fail {
		t.Fatalf("Linear auth with 200 HTML body = %+v; want FAIL on the non-JSON body", check)
	}
	if !strings.Contains(check.Detail, "not JSON") {
		t.Fatalf("Linear auth detail = %q; want it to name the non-JSON body", check.Detail)
	}
}

// TestGiteaTrackerStatusErrorWithUserinfoEndpointStaysMasked pins that the
// production client's non-transport error texts (HTTP status, decode) stay
// credential-free when tracker.endpoint carries basic-auth userinfo: the
// client error reaches the FAIL detail through maskedProbeError, and nothing
// in the status path interpolates the raw URL.
func TestGiteaTrackerStatusErrorWithUserinfoEndpointStaysMasked(t *testing.T) {
	installFakeGitOnly(t)
	const secret = "status-userinfo-secret"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"unauthorized"}`))
	}))
	t.Cleanup(srv.Close)
	endpoint := strings.Replace(srv.URL, "http://", "http://bot:"+secret+"@", 1)
	report := BuildReport(context.Background(), Options{
		WorkflowPath: writeTrackerPreflightWorkflow(t, "gitea", "\n  api_key: gitea-key\n  endpoint: "+endpoint),
		Mode:         "real",
		Runner:       passingRunner,
	})

	check := findCheck(t, report, "Gitea auth")
	if check.Status != Fail {
		t.Fatalf("Gitea auth with userinfo endpoint and 401 = %+v; want FAIL", check)
	}
	if strings.Contains(check.Detail, secret) || strings.Contains(check.Fix, secret) {
		t.Fatalf("Gitea auth FAIL output leaks the userinfo secret: detail=%q fix=%q", check.Detail, check.Fix)
	}
	if !strings.Contains(check.Detail, "401") {
		t.Fatalf("Gitea auth detail = %q; want the 401 status surfaced", check.Detail)
	}
}

// TestGitHubTrackerStatusErrorWithUserinfoEndpointStaysMasked is the GitHub
// twin of the Gitea status-error masking pin: a userinfo-bearing
// tracker.endpoint plus a non-transport client failure (HTTP 401) must reach
// the FAIL detail with the credential masked.
func TestGitHubTrackerStatusErrorWithUserinfoEndpointStaysMasked(t *testing.T) {
	installFakeGitOnly(t)
	const secret = "gh-status-userinfo-secret"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
	}))
	t.Cleanup(srv.Close)
	endpoint := strings.Replace(srv.URL, "http://", "http://bot:"+secret+"@", 1)
	report := BuildReport(context.Background(), Options{
		WorkflowPath: writeTrackerPreflightWorkflow(t, "github", "\n  api_key: gh-key\n  endpoint: "+endpoint),
		Mode:         "real",
		Runner:       passingRunner,
	})

	check := findCheck(t, report, "GitHub tracker auth")
	if check.Status != Fail {
		t.Fatalf("GitHub tracker auth with userinfo endpoint and 401 = %+v; want FAIL", check)
	}
	if strings.Contains(check.Detail, secret) || strings.Contains(check.Fix, secret) {
		t.Fatalf("GitHub tracker auth FAIL output leaks the userinfo secret: detail=%q fix=%q", check.Detail, check.Fix)
	}
	if !strings.Contains(check.Detail, "401") {
		t.Fatalf("GitHub tracker auth detail = %q; want the 401 status surfaced", check.Detail)
	}
}
