package tracker

import (
	"reflect"
	"testing"
	"time"
)

func TestIssueRefsFromIDsTrimsAndSkipsEmptyIDs(t *testing.T) {
	got := IssueRefsFromIDs([]string{" 123 ", "", " \t\n", "#7 "})
	want := []IssueRef{{ID: "123"}, {ID: "#7"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("IssueRefsFromIDs = %#v, want %#v", got, want)
	}
}

func TestTimeStringReturnsEmptyForZeroTime(t *testing.T) {
	if got := TimeString(time.Time{}); got != "" {
		t.Fatalf("TimeString(zero) = %q, want empty string", got)
	}
}

func TestTimeStringNormalizesOffsetsToUTC(t *testing.T) {
	input, err := time.Parse(time.RFC3339Nano, "2026-05-08T12:30:00.123456789+02:00")
	if err != nil {
		t.Fatalf("parse test time: %v", err)
	}
	if got := TimeString(input); got != "2026-05-08T10:30:00.123456789Z" {
		t.Fatalf("TimeString(offset time) = %q, want UTC RFC3339Nano", got)
	}
}
