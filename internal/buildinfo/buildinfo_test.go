package buildinfo

import "testing"

// TestResolvePrefersStamp pins the load-bearing rule: a real -X stamp wins over
// the VCS fallback, with surrounding whitespace trimmed (#796).
func TestResolvePrefersStamp(t *testing.T) {
	cases := map[string]string{"v1.2.3": "v1.2.3", "  v1.2.3  ": "v1.2.3"}
	for in, want := range cases {
		if got := Resolve(in); got != want {
			t.Errorf("Resolve(%q) = %q, want %q (a real stamp must win over the VCS fallback)", in, got, want)
		}
	}
}

// TestFormatRevision pins the VCS-revision formatting deterministically (it is
// the part of the fallback that does not depend on ReadBuildInfo): empty stays
// empty, long revisions truncate to 12, and a modified tree gets "-dirty".
func TestFormatRevision(t *testing.T) {
	cases := []struct {
		revision string
		modified bool
		want     string
	}{
		{"", false, ""},
		{"", true, ""},
		{"abc123", false, "abc123"},
		{"0123456789abcdef0000", false, "0123456789ab"},
		{"0123456789abcdef0000", true, "0123456789ab-dirty"},
		{"abc123", true, "abc123-dirty"},
	}
	for _, c := range cases {
		if got := formatRevision(c.revision, c.modified); got != c.want {
			t.Errorf("formatRevision(%q, %v) = %q, want %q", c.revision, c.modified, got, c.want)
		}
	}
}
