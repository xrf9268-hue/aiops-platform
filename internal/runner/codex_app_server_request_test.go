package runner

// codex_app_server_request_test.go pins (*appServerClient).request — the
// JSON-RPC request/response round-trip — at its own function boundary before
// #499 decomposes its response-matching loop into a helper. The subprocess
// suite drives request only through full startup; these isolate the
// response-vs-notification fork directly, including the interleaved-notification
// skip the suite does not pin.

import (
	"context"
	"math"
	"testing"
)

func TestRequest_ReturnsMatchingResult(t *testing.T) {
	c, _ := newTurnLoopClient(t, []string{
		`{"jsonrpc":"2.0","id":0,"result":{"thread":{"id":"t-1"}}}`,
	})
	got, err := c.request(context.Background(), "thread/start", map[string]any{"x": 1})
	if err != nil {
		t.Fatalf("request() err = %v; want nil", err)
	}
	thread, _ := got["thread"].(map[string]any)
	if thread["id"] != "t-1" {
		t.Errorf("request() result = %#v; want result.thread.id = t-1", got)
	}
}

func TestRequest_ErrorResponseReturnsResponseErrorCategory(t *testing.T) {
	c, _ := newTurnLoopClient(t, []string{
		`{"jsonrpc":"2.0","id":0,"error":{"code":-32600,"message":"bad request"}}`,
	})
	_, err := c.request(context.Background(), "thread/start", nil)
	if err == nil {
		t.Fatalf("request() err = nil; want a response error")
	}
	if cat, ok := ErrorCategory(err); !ok || cat != CategoryResponseError {
		t.Errorf("ErrorCategory(%v) = (%v, %v); want (CategoryResponseError, true)", err, cat, ok)
	}
}

func TestRequest_MissingResultReturnsEmptyMap(t *testing.T) {
	c, _ := newTurnLoopClient(t, []string{
		`{"jsonrpc":"2.0","id":0}`, // matching id, no result field
	})
	got, err := c.request(context.Background(), "initialize", nil)
	if err != nil {
		t.Fatalf("request() err = %v; want nil", err)
	}
	if got == nil {
		t.Errorf("request() result = nil; want a non-nil empty map")
	}
	if len(got) != 0 {
		t.Errorf("request() result = %#v; want an empty map", got)
	}
}

func TestRequest_SkipsInterleavedNotificationThenReturns(t *testing.T) {
	c, _ := newTurnLoopClient(t, []string{
		// A notification (no id) arrives before the response; request must
		// dispatch it via handleNotification and keep reading.
		`{"jsonrpc":"2.0","method":"turn/progress","params":{"message":"working"}}`,
		`{"jsonrpc":"2.0","id":0,"result":{"ok":true}}`,
	})
	got, err := c.request(context.Background(), "turn/start", nil)
	if err != nil {
		t.Fatalf("request() err = %v; want nil", err)
	}
	if got["ok"] != true {
		t.Errorf("request() result = %#v; want result.ok = true after skipping the notification", got)
	}
	if c.lastMessage != "working" {
		t.Errorf("c.lastMessage = %q; want %q (the interleaved notification must be handled, not dropped)", c.lastMessage, "working")
	}
}

func TestRequest_SkipsDifferentIdMessageThenReturns(t *testing.T) {
	c, _ := newTurnLoopClient(t, []string{
		// A message carrying a DIFFERENT numeric id must not be mistaken for this
		// request's response (id 0); it is dispatched as a notification and the
		// loop keeps reading for the real response.
		`{"jsonrpc":"2.0","id":99,"params":{"message":"other"},"result":{"stray":true}}`,
		`{"jsonrpc":"2.0","id":0,"result":{"ok":true}}`,
	})
	got, err := c.request(context.Background(), "turn/start", nil)
	if err != nil {
		t.Fatalf("request() err = %v; want nil", err)
	}
	if got["ok"] != true {
		t.Errorf("request() result = %#v; want this request's response (id 0), not the id-99 message", got)
	}
	if c.lastMessage != "other" {
		t.Errorf("c.lastMessage = %q; want %q (the different-id message must be handled, then skipped)", c.lastMessage, "other")
	}
}

func TestRequest_ReadErrorPropagates(t *testing.T) {
	c, _ := newTurnLoopClient(t, nil) // empty stream: the read hits EOF before any response
	_, err := c.request(context.Background(), "initialize", nil)
	if err == nil {
		t.Fatalf("request() err = nil; want a read error on an empty stream")
	}
}

func TestNumberID(t *testing.T) {
	tests := []struct {
		name   string
		in     any
		want   int
		wantOK bool
	}{
		{name: "integral float preserved", in: float64(7), want: 7, wantOK: true},
		{name: "zero float preserved", in: float64(0), want: 0, wantOK: true},
		{name: "negative integral float preserved", in: float64(-3), want: -3, wantOK: true},
		{name: "int preserved", in: 42, want: 42, wantOK: true},
		{name: "fractional rejected", in: 1.5, want: 0, wantOK: false},
		{name: "NaN rejected", in: math.NaN(), want: 0, wantOK: false},
		{name: "positive infinity rejected", in: math.Inf(1), want: 0, wantOK: false},
		{name: "negative infinity rejected", in: math.Inf(-1), want: 0, wantOK: false},
		{name: "out-of-range magnitude rejected", in: 1e19, want: 0, wantOK: false},
		{name: "non-number rejected", in: "0", want: 0, wantOK: false},
		{name: "nil rejected", in: nil, want: 0, wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := numberID(tt.in)
			if got != tt.want || ok != tt.wantOK {
				t.Fatalf("numberID(%v) = (%d, %v); want (%d, %v)", tt.in, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestRequest_FractionalIDRejectedNotTruncated(t *testing.T) {
	// The old numberID truncated id 0.5 to 0 and wrongly matched this request
	// (id 0), returning the stray result. A fractional id must now be surfaced as
	// a malformed response instead of truncate-matching the wrong request (#671).
	c, _ := newTurnLoopClient(t, []string{
		`{"jsonrpc":"2.0","id":0.5,"result":{"stray":true}}`,
	})
	_, err := c.request(context.Background(), "thread/start", nil)
	if err == nil {
		t.Fatalf("request() err = nil; want a response error for a fractional id")
	}
	if cat, ok := ErrorCategory(err); !ok || cat != CategoryResponseError {
		t.Errorf("ErrorCategory(%v) = (%v, %v); want (CategoryResponseError, true)", err, cat, ok)
	}
}

func TestRequest_OutOfRangeIDReturnsResponseError(t *testing.T) {
	c, _ := newTurnLoopClient(t, []string{
		`{"jsonrpc":"2.0","id":1e19,"result":{"ok":true}}`,
	})
	_, err := c.request(context.Background(), "thread/start", nil)
	if err == nil {
		t.Fatalf("request() err = nil; want a response error for an out-of-range id")
	}
	if cat, ok := ErrorCategory(err); !ok || cat != CategoryResponseError {
		t.Errorf("ErrorCategory(%v) = (%v, %v); want (CategoryResponseError, true)", err, cat, ok)
	}
}

// TestResponseForID pins the extracted response-matching helper directly at its
// own boundary (the request-loop tests above exercise it only indirectly): a
// matching id returns the result or a CategoryResponseError, a mismatched
// integer id and an absent id are both skipped as interleaved messages, and a
// present-but-non-integer id is surfaced as a CategoryResponseError (#671).
func TestResponseForID(t *testing.T) {
	const id = 7
	tests := []struct {
		name        string
		msg         map[string]any
		wantMatched bool
		wantResult  map[string]any
		wantErrCat  RunnerErrorCategory // "" => want nil err
	}{
		{
			name:        "matching id returns result",
			msg:         map[string]any{"id": float64(7), "result": map[string]any{"ok": true}},
			wantMatched: true,
			wantResult:  map[string]any{"ok": true},
		},
		{
			name:        "matching id with error member is a response error",
			msg:         map[string]any{"id": float64(7), "error": map[string]any{"code": float64(-1)}},
			wantMatched: true,
			wantErrCat:  CategoryResponseError,
		},
		{
			name:        "mismatched integer id is skipped",
			msg:         map[string]any{"id": float64(99), "result": map[string]any{"stray": true}},
			wantMatched: false,
		},
		{
			name:        "absent id is skipped as a notification",
			msg:         map[string]any{"method": "turn/progress"},
			wantMatched: false,
		},
		{
			name:        "present non-integer id is a response error",
			msg:         map[string]any{"id": 1.5, "result": map[string]any{"stray": true}},
			wantMatched: true,
			wantErrCat:  CategoryResponseError,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, matched, err := responseForID(tt.msg, id)
			if matched != tt.wantMatched {
				t.Fatalf("responseForID(%v, %d) matched = %v; want %v", tt.msg, id, matched, tt.wantMatched)
			}
			if tt.wantErrCat == "" {
				if err != nil {
					t.Fatalf("responseForID(%v, %d) err = %v; want nil", tt.msg, id, err)
				}
			} else if cat, ok := ErrorCategory(err); !ok || cat != tt.wantErrCat {
				t.Fatalf("responseForID(%v, %d) ErrorCategory = (%v, %v); want (%v, true)", tt.msg, id, cat, ok, tt.wantErrCat)
			}
			for k, want := range tt.wantResult {
				if result[k] != want {
					t.Fatalf("responseForID(%v, %d) result[%q] = %v; want %v", tt.msg, id, k, result[k], want)
				}
			}
		})
	}
}
