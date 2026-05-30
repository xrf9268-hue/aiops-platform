package runner

// codex_app_server_request_test.go pins (*appServerClient).request — the
// JSON-RPC request/response round-trip — at its own function boundary before
// #499 decomposes its response-matching loop into a helper. The subprocess
// suite drives request only through full startup; these isolate the
// response-vs-notification fork directly, including the interleaved-notification
// skip the suite does not pin.

import (
	"context"
	"testing"
)

func TestRequest_ReturnsMatchingResult(t *testing.T) {
	c, _ := newTurnLoopClient([]string{
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
	c, _ := newTurnLoopClient([]string{
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
	c, _ := newTurnLoopClient([]string{
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
	c, _ := newTurnLoopClient([]string{
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
	c, _ := newTurnLoopClient([]string{
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
	c, _ := newTurnLoopClient(nil) // empty stream: the read hits EOF before any response
	_, err := c.request(context.Background(), "initialize", nil)
	if err == nil {
		t.Fatalf("request() err = nil; want a read error on an empty stream")
	}
}
