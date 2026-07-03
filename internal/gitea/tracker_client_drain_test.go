package gitea

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

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
	status        int
	body          *drainRecordingBody
	contentLength int64
}

func (t *staticResponseTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: t.status, Header: http.Header{}, Body: t.body, ContentLength: t.contentLength}, nil
}

func newDrainTestTrackerClient(status int, body *drainRecordingBody) *TrackerClient {
	client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret"}, "http://gitea.invalid", "owner", "repo")
	client.HTTP = &http.Client{Transport: &staticResponseTransport{status: status, body: body}}
	return client
}

func TestTrackerClientGetIssueByNumberDrainsNonSuccessBodies(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		wantFound bool
		checkErr  func(error) bool
		wantErr   string
	}{
		{name: "404 early return", status: http.StatusNotFound, checkErr: func(err error) bool { return err == nil }, wantErr: "nil"},
		{name: "429 rate limited", status: http.StatusTooManyRequests, checkErr: func(err error) bool { return errors.Is(err, tracker.ErrRateLimited) }, wantErr: "errors.Is(err, tracker.ErrRateLimited)"},
		{name: "500 status error", status: http.StatusInternalServerError, checkErr: func(err error) bool {
			return err != nil && err.Error() == "get Gitea issue #7 failed: status 500"
		}, wantErr: "get Gitea issue #7 failed: status 500"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := &drainRecordingBody{reader: strings.NewReader(`{"message":"detail"}`)}
			client := newDrainTestTrackerClient(tc.status, body)

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

func TestTrackerClientListIssuesPageDrainsErrorBody(t *testing.T) {
	body := &drainRecordingBody{reader: strings.NewReader(`{"message":"detail"}`)}
	client := newDrainTestTrackerClient(http.StatusInternalServerError, body)

	_, _, err := client.listIssuesPage(context.Background(), "", "open", 1)

	if want := "list Gitea issues failed: status 500"; err == nil || err.Error() != want {
		t.Fatalf("listIssuesPage error = %v; want %q", err, want)
	}
	if !body.sawEOF || !body.closed {
		t.Fatalf("body sawEOF = %t, closed = %t; want true, true (undrained bodies break connection reuse)", body.sawEOF, body.closed)
	}
}

func TestTrackerClientListIssuesPageRejectsOversizedSuccessBody(t *testing.T) {
	body := &drainRecordingBody{reader: strings.NewReader(`[]`)}
	client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret"}, "http://gitea.invalid", "owner", "repo")
	client.HTTP = &http.Client{Transport: &staticResponseTransport{
		status:        http.StatusOK,
		body:          body,
		contentLength: 1 << 62,
	}}

	_, _, err := client.listIssuesPage(context.Background(), "", "open", 1)

	if !errors.Is(err, tracker.ErrJSONResponseTooLarge) {
		t.Fatalf("listIssuesPage oversized success error = %v; want errors.Is(err, tracker.ErrJSONResponseTooLarge)", err)
	}
}

func TestTrackerClientGetIssueByNumberRejectsOversizedSuccessBody(t *testing.T) {
	body := &drainRecordingBody{reader: strings.NewReader(`{}`)}
	client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret"}, "http://gitea.invalid", "owner", "repo")
	client.HTTP = &http.Client{Transport: &staticResponseTransport{
		status:        http.StatusOK,
		body:          body,
		contentLength: 1 << 62,
	}}

	_, _, err := client.getIssueByNumber(context.Background(), 7)

	if !errors.Is(err, tracker.ErrJSONResponseTooLarge) {
		t.Fatalf("getIssueByNumber oversized success error = %v; want errors.Is(err, tracker.ErrJSONResponseTooLarge)", err)
	}
}
