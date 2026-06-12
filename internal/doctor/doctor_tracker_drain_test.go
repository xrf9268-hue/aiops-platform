package doctor

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
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
