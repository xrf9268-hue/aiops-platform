package doctor

import (
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

// TestDecodeLinearProjectProbeDrainsResponseBodies pins the #771 conversion
// of the doctor's Linear project probe to tracker.DrainAndClose: the probe
// loops over every configured slug through one shared HTTPClient, so neither
// the non-2xx early return nor the decoder's leftover may close the body
// unread.
func TestDecodeLinearProjectProbeDrainsResponseBodies(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		checkErr func(error) bool
		wantErr  string
	}{
		{
			name:     "non-2xx early return",
			status:   http.StatusInternalServerError,
			body:     `{"errors":[{"message":"detail"}]}`,
			checkErr: func(err error) bool { return err != nil && err.Error() == "linear returned 500 Internal Server Error" },
			wantErr:  `"linear returned 500 Internal Server Error"`,
		},
		{
			name:     "2xx with bytes trailing the decoded value",
			status:   http.StatusOK,
			body:     `{"data":{"projects":{"nodes":[{"id":"prj-1"}]}}}` + "\n",
			checkErr: func(err error) bool { return err == nil },
			wantErr:  "nil",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := &drainRecordingBody{reader: strings.NewReader(tc.body)}
			resp := &http.Response{
				Status:     fmt.Sprintf("%d %s", tc.status, http.StatusText(tc.status)),
				StatusCode: tc.status,
				Body:       body,
			}
			var out struct {
				Data struct {
					Projects struct {
						Nodes []struct {
							ID string `json:"id"`
						} `json:"nodes"`
					} `json:"projects"`
				} `json:"data"`
			}

			err := decodeLinearProjectProbe(resp, &out)

			if !tc.checkErr(err) {
				t.Fatalf("decodeLinearProjectProbe() with status %d error = %v; want %s", tc.status, err, tc.wantErr)
			}
			if !body.sawEOF || !body.closed {
				t.Fatalf("status %d body sawEOF = %t, closed = %t; want true, true (undrained bodies break connection reuse)", tc.status, body.sawEOF, body.closed)
			}
		})
	}
}

func TestDecodeLinearProjectProbeRejectsOversizedSuccessBody(t *testing.T) {
	body := &drainRecordingBody{reader: strings.NewReader(`{"data":{"projects":{"nodes":[]}}}`)}
	resp := &http.Response{
		Status:        "200 OK",
		StatusCode:    http.StatusOK,
		Body:          body,
		ContentLength: 1 << 62,
	}
	var out struct {
		Data struct {
			Projects struct {
				Nodes []struct {
					ID string `json:"id"`
				} `json:"nodes"`
			} `json:"projects"`
		} `json:"data"`
	}

	err := decodeLinearProjectProbe(resp, &out)

	if !errors.Is(err, tracker.ErrJSONResponseTooLarge) {
		t.Fatalf("decodeLinearProjectProbe() oversized success error = %v; want errors.Is(err, tracker.ErrJSONResponseTooLarge)", err)
	}
}

// The Gitea/GitHub tracker preflight has no doctor-side drain tests anymore:
// it drives the worker's own tracker clients, whose poll paths own the
// response-body lifecycle (tracker.DrainAndClose in internal/gitea
// tracker_client.go and internal/tracker github.go, pinned by those
// packages' drain tests — #762/#771).
