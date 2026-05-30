package runner

// codex_app_server_reason_test.go pins the allow-listed reason-field extractors
// — extractReasonFields and copyAllowlistedReasonFields — at their own function
// boundaries before #499 decomposes them (gocognit 15 and 18) and deduplicates
// their shared allow-list. Both are redaction-discipline guards: only an
// explicit key allow-list reaches returned error strings or the JSON event
// surface, so the field-precedence, string-vs-object dual shape, trimming, and
// nested-scrubbing behavior asserted here must hold identically across the
// refactor.
//
// The end-to-end TestCodexAppServerRunnerRedactsTurnFailureParams remains the
// authority for the runner-level redaction path; these isolate the precedence
// and dual-shape branches it only samples.

import (
	"reflect"
	"testing"
)

// TestReasonFieldAllowlistsAreStable pins the membership and order of the shared
// reason-field allow-lists. They are a security boundary — they bound what
// reaches returned error strings and the JSON event surface — and are mutable
// package-level slices, so an accidental append/reorder would silently widen the
// redaction surface without failing any behavioral test that doesn't happen to
// use the newly admitted key. This guard makes any change deliberate.
func TestReasonFieldAllowlistsAreStable(t *testing.T) {
	if want := []string{"reason", "error", "message", "error_code"}; !reflect.DeepEqual(reasonFieldKeys, want) {
		t.Errorf("reasonFieldKeys = %#v; want %#v (widening the allow-list expands the redaction surface)", reasonFieldKeys, want)
	}
	if want := []string{"message", "reason", "error_code", "code"}; !reflect.DeepEqual(nestedReasonFieldKeys, want) {
		t.Errorf("nestedReasonFieldKeys = %#v; want %#v (widening the allow-list expands the redaction surface)", nestedReasonFieldKeys, want)
	}
}

func TestExtractReasonFields(t *testing.T) {
	tests := []struct {
		name   string
		source map[string]any
		want   string
	}{
		{"empty", map[string]any{}, ""},
		{"non-allowlisted only", map[string]any{"leak": "secret"}, ""},
		{"reason string", map[string]any{"reason": "boom"}, "boom"},
		{"reason trimmed", map[string]any{"reason": "  boom  "}, "boom"},
		{"reason wins over message", map[string]any{"reason": "r", "message": "m"}, "r"},
		{"empty reason falls through to message", map[string]any{"reason": "   ", "message": "m"}, "m"},
		{"error string", map[string]any{"error": "e"}, "e"},
		{"error wins over message", map[string]any{"error": "e", "message": "m"}, "e"},
		{"message before error_code", map[string]any{"message": "m", "error_code": "ec"}, "m"},
		{"error_code string", map[string]any{"error_code": "ec"}, "ec"},
		{"error object nested message wins", map[string]any{"error": map[string]any{"message": "em", "reason": "er"}}, "em"},
		{"error object nested reason when no message", map[string]any{"error": map[string]any{"reason": "er"}}, "er"},
		{"error object nested error_code before code", map[string]any{"error": map[string]any{"error_code": "ec", "code": "c"}}, "ec"},
		{"error object nested code", map[string]any{"error": map[string]any{"code": "c"}}, "c"},
		{"error object nested value trimmed", map[string]any{"error": map[string]any{"message": "  em  "}}, "em"},
		{"error object empty nested message skipped", map[string]any{"error": map[string]any{"message": "   ", "reason": "er"}}, "er"},
		{"error object only non-allowlisted falls through to message", map[string]any{"error": map[string]any{"leak": "x"}, "message": "m"}, "m"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractReasonFields(tt.source); got != tt.want {
				t.Errorf("extractReasonFields(%#v) = %q; want %q", tt.source, got, tt.want)
			}
		})
	}
}

func TestCopyAllowlistedReasonFields(t *testing.T) {
	tests := []struct {
		name string
		src  map[string]any
		want map[string]any
	}{
		{"empty", map[string]any{}, map[string]any{}},
		{"non-allowlisted dropped", map[string]any{"leak": "secret"}, map[string]any{}},
		{"reason string", map[string]any{"reason": "boom"}, map[string]any{"reason": "boom"}},
		{"reason trimmed", map[string]any{"reason": "  boom  "}, map[string]any{"reason": "boom"}},
		{"empty reason dropped", map[string]any{"reason": "   "}, map[string]any{}},
		// Unlike extractReasonFields (first match), copy keeps every allow-listed field.
		{"keeps all allowlisted", map[string]any{"reason": "r", "message": "m", "error_code": "ec"}, map[string]any{"reason": "r", "message": "m", "error_code": "ec"}},
		{"error string", map[string]any{"error": "e"}, map[string]any{"error": "e"}},
		{"error object flattened to scrubbed submap", map[string]any{"error": map[string]any{"message": "em", "code": "c", "leak": "x"}}, map[string]any{"error": map[string]any{"message": "em", "code": "c"}}},
		{"error object all non-allowlisted not copied", map[string]any{"error": map[string]any{"leak": "x"}}, map[string]any{}},
		{"error object empty values not copied", map[string]any{"error": map[string]any{"message": "   "}}, map[string]any{}},
		{"error object nested value trimmed when stored", map[string]any{"error": map[string]any{"message": "  em  "}}, map[string]any{"error": map[string]any{"message": "em"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dst := map[string]any{}
			copyAllowlistedReasonFields(tt.src, dst)
			if !reflect.DeepEqual(dst, tt.want) {
				t.Errorf("copyAllowlistedReasonFields(%#v) dst = %#v; want %#v", tt.src, dst, tt.want)
			}
		})
	}
}
