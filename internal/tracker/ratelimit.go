package tracker

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RateLimitedError carries the Retry-After hint parsed from an HTTP 429
// response so logs/state can show when the tracker expects the next attempt.
// RetryAfter is zero when the header is missing or unparseable; the response
// is still classified as rate limited either way. It travels as the wrapped
// Err of a CategoryRateLimited *Error, so callers classify with
// errors.Is(err, ErrRateLimited) and extract the hint with errors.As. The
// Gitea tracker client reuses this type rather than growing a parallel one:
// it already depends on this package for its typed-error categories.
type RateLimitedError struct {
	RetryAfter time.Duration
}

func (e *RateLimitedError) Error() string {
	if e == nil {
		return ""
	}
	if e.RetryAfter > 0 {
		return fmt.Sprintf("rate limited, retry after %s", e.RetryAfter)
	}
	return "rate limited"
}

// maxRetryAfter caps parsed Retry-After hints at the largest representable
// duration so delta-seconds arithmetic can never overflow.
const maxRetryAfter = time.Duration(1<<63 - 1)

// NewRateLimitedError classifies an HTTP 429 for operation (e.g. "list
// GitHub issues") under CategoryRateLimited, parsing the response's
// Retry-After header. The message carries the numeric status code only: the
// reason phrase in resp.Status is proxy-controlled text and is kept out of
// the tracker clients' error strings (this sweep's class; agent-tool and
// diagnostic surfaces are outside it).
func NewRateLimitedError(operation string, header http.Header) *Error {
	return NewError(
		CategoryRateLimited,
		fmt.Sprintf("%s failed: status %d", operation, http.StatusTooManyRequests),
		&RateLimitedError{RetryAfter: parseRetryAfter(header.Get("Retry-After"), time.Now())},
	)
}

// parseRetryAfter parses the two RFC 9110 §10.2.3 Retry-After forms:
// delta-seconds and HTTP-date (converted to a duration relative to now).
// Missing, unparseable, non-positive, or already-elapsed values yield zero —
// the caller still classifies the response as rate limited.
func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds <= 0 {
			return 0
		}
		// Saturate instead of multiplying: a hostile or buggy proxy can send
		// a delta large enough that seconds*time.Second wraps negative,
		// bypassing the non-positive clamp above — same class, so it must be
		// closed here too. Mirrors the HTTP-date branch, whose time.Sub
		// saturates positive.
		if int64(seconds) > int64(maxRetryAfter/time.Second) {
			return maxRetryAfter
		}
		return time.Duration(seconds) * time.Second
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return 0
	}
	if remaining := when.Sub(now); remaining > 0 {
		return remaining
	}
	return 0
}
