package runner

import (
	"fmt"
	"math"
)

// responseForID reports whether msg is the JSON-RPC response to request id. When
// it is (matched=true), it returns the result map — defaulting a missing or null
// result to an empty map — or a CategoryResponseError carrying the `error`
// member. A non-matching msg (matched=false) is an interleaved notification the
// caller must dispatch and keep reading past.
func responseForID(msg map[string]any, id int) (result map[string]any, matched bool, err error) {
	// A JSON-RPC response never carries a method member: a message with both
	// id and method is a server->client *request* (approval prompt, tool
	// call, elicitation) the caller must answer — even when the server's id
	// numerically collides with our pending request id or is not an integer.
	if _, hasMethod := msg["method"]; hasMethod {
		return nil, false, nil
	}
	gotID, ok := numberID(msg["id"])
	if !ok {
		// A present-but-non-integer id is a malformed response: numberID will not
		// truncate it (#671), so we cannot reliably tell which pending request it
		// answers — surface it as a response error rather than skip-and-wait. An
		// absent id is an interleaved notification the caller must dispatch.
		if _, present := msg["id"]; present {
			return nil, true, NewError(CategoryResponseError, fmt.Sprintf("codex app-server sent a non-integer JSON-RPC id: %v", msg["id"]), nil)
		}
		return nil, false, nil
	}
	if gotID != id {
		return nil, false, nil
	}
	if e, ok := msg["error"]; ok {
		return nil, true, NewError(CategoryResponseError, fmt.Sprintf("rpc error: %v", e), nil)
	}
	result, _ = msg["result"].(map[string]any)
	if result == nil {
		result = map[string]any{}
	}
	return result, true, nil
}

// numberID extracts an exact integer JSON-RPC id from a decoded protocol value.
func numberID(v any) (int, bool) {
	switch x := v.(type) {
	case float64:
		// JSON numbers decode to float64. A JSON-RPC id must be an exact integer
		// that fits in int: reject NaN/Inf and fractional values (e.g. 1.5)
		// instead of silently truncating to a plausible-but-wrong id that then
		// matches the wrong pending request (#671).
		if math.IsNaN(x) || math.IsInf(x, 0) || math.Trunc(x) != x {
			return 0, false
		}
		// Reject magnitudes that would overflow int. float64(math.MaxInt) rounds
		// up to 2^63 on 64-bit, so the upper bound is strict; math.MinInt
		// (-2^63) is exactly representable, so its bound is inclusive.
		if x < float64(math.MinInt) || x >= float64(math.MaxInt) {
			return 0, false
		}
		return int(x), true
	case int:
		return x, true
	default:
		return 0, false
	}
}
