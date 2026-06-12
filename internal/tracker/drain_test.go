package tracker

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// infiniteReader never returns EOF and counts how many bytes the caller
// consumed, modeling a misbehaving server that streams an error body forever.
type infiniteReader struct {
	read int64
}

func (r *infiniteReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	r.read += int64(len(p))
	return len(p), nil
}

// drainRecordingBody records whether the response body was read to EOF (the
// precondition for HTTP/1.1 connection reuse) and closed.
type drainRecordingBody struct {
	reader io.Reader
	sawEOF bool
	closed bool
}

func (b *drainRecordingBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	if errors.Is(err, io.EOF) {
		b.sawEOF = true
	}
	return n, err
}

func (b *drainRecordingBody) Close() error {
	b.closed = true
	return nil
}

// staticResponseTransport serves one canned response so per-status drain
// coverage does not need a live server.
type staticResponseTransport struct {
	status int
	header http.Header
	body   *drainRecordingBody
}

func (t *staticResponseTransport) RoundTrip(*http.Request) (*http.Response, error) {
	header := t.header
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{StatusCode: t.status, Header: header, Body: t.body}, nil
}

func TestDrainAndCloseBoundsHugeBody(t *testing.T) {
	source := &infiniteReader{}
	body := &drainRecordingBody{reader: source}

	DrainAndClose(&http.Response{Body: body})

	if source.read != drainBodyLimit {
		t.Fatalf("DrainAndClose read %d bytes from an unbounded body; want exactly %d (drainBodyLimit)", source.read, int64(drainBodyLimit))
	}
	if !body.closed {
		t.Fatalf("DrainAndClose closed = %t; want true", body.closed)
	}
}

// countingErrorThenSuccessServer returns an HTTP/1.1 httptest server whose
// first response is the given error response and whose subsequent responses
// are 200 with the given success body, counting distinct TCP connections.
func countingErrorThenSuccessServer(t *testing.T, writeError func(http.ResponseWriter), successBody string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var requests atomic.Int64
	conns := &atomic.Int64{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if requests.Add(1) == 1 {
			writeError(w)
			return
		}
		_, _ = io.WriteString(w, successBody)
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			conns.Add(1)
		}
	}
	// httptest serves plain HTTP, and a plain *http.Transport never
	// negotiates h2c, so this pins HTTP/1.1 reuse semantics.
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, conns
}

// reuseTestGitHubClient wires a GitHub client to srv through a fresh
// transport. MaxConnsPerHost=1 makes the reuse assertion deterministic
// without sleeps: the second request cannot dial until the first connection
// is either back in the idle pool (drained) or torn down (undrained).
func reuseTestGitHubClient(t *testing.T, srv *httptest.Server) *GitHubClient {
	t.Helper()
	transport := &http.Transport{MaxConnsPerHost: 1}
	t.Cleanup(transport.CloseIdleConnections)
	client := NewGitHubClient(workflow.TrackerConfig{APIKey: "test-token"}, srv.URL, "acme", "api")
	client.HTTP = &http.Client{Transport: transport}
	return client
}

func TestGitHubClientReusesConnectionAfterErrorResponse(t *testing.T) {
	srv, conns := countingErrorThenSuccessServer(t, func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"message":"upstream exploded","documentation_url":"https://docs.github.com"}`)
	}, `[]`)
	client := reuseTestGitHubClient(t, srv)

	_, _, err := client.listIssuesPage(context.Background(), "open", "", 1)
	if want := "list GitHub issues failed: status 500"; err == nil || err.Error() != want {
		t.Fatalf("first listIssuesPage error = %v; want %q", err, want)
	}
	if _, _, err := client.listIssuesPage(context.Background(), "open", "", 1); err != nil {
		t.Fatalf("second listIssuesPage error = %v; want nil", err)
	}
	if got := conns.Load(); got != 1 {
		t.Fatalf("distinct connections = %d; want 1 (undrained error body forces a new dial)", got)
	}
}

// TestGitHubClientReusesConnectionAfterSecondaryRateLimitProbe pins the #761
// composition: githubSecondaryLimitBody consumes a bounded prefix of the 403
// body during classification, and the deferred drain consumes whatever
// remains, so the classification stays rate-limited and the connection is
// still reused.
func TestGitHubClientReusesConnectionAfterSecondaryRateLimitProbe(t *testing.T) {
	srv, conns := countingErrorThenSuccessServer(t, func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again.","documentation_url":"https://docs.github.com/rest/overview/rate-limits-for-the-rest-api#about-secondary-rate-limits"}`)
	}, `[]`)
	client := reuseTestGitHubClient(t, srv)

	_, _, err := client.listIssuesPage(context.Background(), "open", "", 1)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("first listIssuesPage error = %v; want errors.Is(err, ErrRateLimited)", err)
	}
	if _, _, err := client.listIssuesPage(context.Background(), "open", "", 1); err != nil {
		t.Fatalf("second listIssuesPage error = %v; want nil", err)
	}
	if got := conns.Load(); got != 1 {
		t.Fatalf("distinct connections = %d; want 1 (probe + drain must together reach EOF)", got)
	}
}

func TestGitHubGetIssueByNumberDrainsNonSuccessBodies(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		wantFound bool
		checkErr  func(error) bool
		wantErr   string
	}{
		{name: "404 early return", status: http.StatusNotFound, checkErr: func(err error) bool { return err == nil }, wantErr: "nil"},
		{name: "410 early return", status: http.StatusGone, checkErr: func(err error) bool { return err == nil }, wantErr: "nil"},
		{name: "429 rate limited", status: http.StatusTooManyRequests, checkErr: func(err error) bool { return errors.Is(err, ErrRateLimited) }, wantErr: "errors.Is(err, ErrRateLimited)"},
		{name: "500 status error", status: http.StatusInternalServerError, checkErr: func(err error) bool {
			return err != nil && err.Error() == "get GitHub issue #7 failed: status 500"
		}, wantErr: "get GitHub issue #7 failed: status 500"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := &drainRecordingBody{reader: strings.NewReader(`{"message":"detail"}`)}
			client := NewGitHubClient(workflow.TrackerConfig{APIKey: "test-token"}, "http://github.invalid", "acme", "api")
			client.HTTP = &http.Client{Transport: &staticResponseTransport{status: tc.status, body: body}}

			_, found, err := client.getIssueByNumber(context.Background(), 7)

			if found != tc.wantFound || !tc.checkErr(err) {
				t.Fatalf("getIssueByNumber(7) with status %d = (found %t, err %v); want (found %t, err %s)", tc.status, found, err, tc.wantFound, tc.wantErr)
			}
			if !body.sawEOF || !body.closed {
				t.Fatalf("status %d body sawEOF = %t, closed = %t; want true, true (undrained bodies break connection reuse)", tc.status, body.sawEOF, body.closed)
			}
		})
	}
}

func TestGitHubListOpenPullRequestsPageDrainsErrorBody(t *testing.T) {
	body := &drainRecordingBody{reader: strings.NewReader(`{"message":"detail"}`)}
	client := NewGitHubClient(workflow.TrackerConfig{APIKey: "test-token"}, "http://github.invalid", "acme", "api")
	client.HTTP = &http.Client{Transport: &staticResponseTransport{status: http.StatusInternalServerError, body: body}}

	_, _, err := client.listOpenPullRequestsPage(context.Background(), 1)

	if want := "list GitHub pull requests failed: status 500"; err == nil || err.Error() != want {
		t.Fatalf("listOpenPullRequestsPage error = %v; want %q", err, want)
	}
	if !body.sawEOF || !body.closed {
		t.Fatalf("body sawEOF = %t, closed = %t; want true, true (undrained bodies break connection reuse)", body.sawEOF, body.closed)
	}
}

func TestLinearGraphQLDrainsErrorResponseBodies(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		checkErr func(error) bool
		wantErr  string
	}{
		{name: "429 rate limited", status: http.StatusTooManyRequests, checkErr: func(err error) bool { return errors.Is(err, ErrRateLimited) }, wantErr: "errors.Is(err, ErrRateLimited)"},
		{name: "500 status error", status: http.StatusInternalServerError, checkErr: func(err error) bool {
			return err != nil && strings.HasSuffix(err.Error(), "linear request failed: status 500")
		}, wantErr: "suffix \"linear request failed: status 500\""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := &drainRecordingBody{reader: strings.NewReader(`{"errors":[{"message":"detail"}]}`)}
			client := NewLinearClient(workflow.TrackerConfig{APIKey: "test-key"})
			client.BaseURL = "http://linear.invalid"
			client.HTTP = &http.Client{Transport: &staticResponseTransport{status: tc.status, body: body}}

			err := client.graphql(context.Background(), "query Probe { viewer { id } }", nil, nil)

			if !tc.checkErr(err) {
				t.Fatalf("graphql() with status %d error = %v; want %s", tc.status, err, tc.wantErr)
			}
			if !body.sawEOF || !body.closed {
				t.Fatalf("status %d body sawEOF = %t, closed = %t; want true, true (undrained bodies break connection reuse)", tc.status, body.sawEOF, body.closed)
			}
		})
	}
}
