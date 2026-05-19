package tracker

import (
	"testing"
	"time"
)

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
