package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
)

// drainRecordingBody records whether the response body was read to EOF (the
// precondition for HTTP/1.1 connection reuse) and closed. Mirrors the EOF
// witness from internal/tracker/drain_test.go (#762).
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
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", t.status, http.StatusText(t.status)),
		StatusCode:    t.status,
		Header:        http.Header{},
		Body:          t.body,
		ContentLength: t.contentLength,
	}, nil
}

// TestLookupWorkflowStateIDsDrainsResponseBodies pins the #771 conversion of
// the workflowStates lookup to tracker.DrainAndClose: the non-2xx early
// return must not close the Linear error body unread, and the 2xx path must
// drain whatever json.Decoder leaves behind after the first value.
func TestLookupWorkflowStateIDsDrainsResponseBodies(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		checkOut func(ids []string, err error) bool
		wantOut  string
	}{
		{
			name:   "non-2xx early return",
			status: http.StatusInternalServerError,
			body:   `{"errors":[{"message":"detail"}]}`,
			checkOut: func(ids []string, err error) bool {
				return ids == nil && err != nil && err.Error() == "linear workflowStates lookup failed: 500 Internal Server Error"
			},
			wantOut: `(nil, "linear workflowStates lookup failed: 500 Internal Server Error")`,
		},
		{
			name:   "2xx with bytes trailing the decoded value",
			status: http.StatusOK,
			body:   `{"data":{"workflowStates":{"nodes":[{"id":"state-1"}]}}}` + "\n",
			checkOut: func(ids []string, err error) bool {
				return err == nil && len(ids) == 1 && ids[0] == "state-1"
			},
			wantOut: `(["state-1"], nil)`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := &drainRecordingBody{reader: strings.NewReader(tc.body)}
			proxy := linearGraphQLProxy{
				apiKey:  "test-key",
				baseURL: "http://linear.invalid",
				http:    &http.Client{Transport: &staticResponseTransport{status: tc.status, body: body}},
			}

			ids, err := proxy.lookupWorkflowStateIDs(context.Background(), "Done", "")

			if !tc.checkOut(ids, err) {
				t.Fatalf("lookupWorkflowStateIDs(Done) with status %d = (%v, %v); want %s", tc.status, ids, err, tc.wantOut)
			}
			if !body.sawEOF || !body.closed {
				t.Fatalf("status %d body sawEOF = %t, closed = %t; want true, true (undrained bodies break connection reuse)", tc.status, body.sawEOF, body.closed)
			}
		})
	}
}

func TestLookupWorkflowStateIDsRejectsOversizedSuccessBody(t *testing.T) {
	body := &drainRecordingBody{reader: strings.NewReader(`{"data":{"workflowStates":{"nodes":[]}}}`)}
	proxy := linearGraphQLProxy{
		apiKey:  "test-key",
		baseURL: "http://linear.invalid",
		http: &http.Client{Transport: &staticResponseTransport{
			status:        http.StatusOK,
			body:          body,
			contentLength: 1 << 62,
		}},
	}

	ids, err := proxy.lookupWorkflowStateIDs(context.Background(), "Done", "")

	if ids != nil || !errors.Is(err, tracker.ErrJSONResponseTooLarge) {
		t.Fatalf("lookupWorkflowStateIDs(Done) oversized success = (%v, %v); want nil, errors.Is(err, tracker.ErrJSONResponseTooLarge)", ids, err)
	}
}
