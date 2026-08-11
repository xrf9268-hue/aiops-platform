package trackerprofile

import "testing"

func TestResolveDocumentedStringExpandsOnlyExactEnvironmentReferences(t *testing.T) {
	lookup := func(name string) (string, bool) {
		if name == "TRACKER_HOST" {
			return "https://tracker.example.test", true
		}
		return "", false
	}
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"dollar", "$TRACKER_HOST", "https://tracker.example.test"},
		{"braced", "${TRACKER_HOST}", "https://tracker.example.test"},
		{"suffix is literal", "$TRACKER_HOST/path", "$TRACKER_HOST/path"},
		{"braced suffix is literal", "${TRACKER_HOST}/path", "${TRACKER_HOST}/path"},
		{"embedded is literal", "https://$TRACKER_HOST/path", "https://$TRACKER_HOST/path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := map[string]any{"endpoint": tc.in}
			got, _, _, err := ResolveDocumentedString(provider, "endpoint", lookup)
			if err != nil {
				t.Fatalf("ResolveDocumentedString(%q): %v", tc.in, err)
			}
			if got != tc.want || provider["endpoint"] != tc.want {
				t.Fatalf("ResolveDocumentedString(%q) = %q, provider=%#v; want %q", tc.in, got, provider["endpoint"], tc.want)
			}
		})
	}
}

func TestPaginationPagesZeroUsesAdapterDefault(t *testing.T) {
	got, err := PaginationPages(map[string]any{"pagination_max_pages": 0}, "pagination_max_pages", 20)
	if err != nil || got != 20 {
		t.Fatalf("PaginationPages(0) = %d, %v; want adapter default 20", got, err)
	}
}
