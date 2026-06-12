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

// NewRateLimitedError classifies a rate-limit response (HTTP 429, or
// GitHub's documented 403 form) for operation (e.g. "list GitHub issues")
// under CategoryRateLimited, parsing the retry hint from Retry-After or the
// epoch-seconds X-RateLimit-Reset fallback. The message carries the numeric
// status code only: the reason phrase in resp.Status is proxy-controlled
// text and is kept out of the tracker clients' error strings (this sweep's
// class; agent-tool and diagnostic surfaces are outside it).
func NewRateLimitedError(operation string, statusCode int, header http.Header) *Error {
	return NewError(
		CategoryRateLimited,
		fmt.Sprintf("%s failed: status %d", operation, statusCode),
		&RateLimitedError{RetryAfter: retryAfterHint(header, time.Now())},
	)
}

// retryAfterHint prefers the standard Retry-After header (GitHub secondary
// limits, Gitea/Linear 429s) and falls back to the de-facto-standard
// epoch-seconds X-RateLimit-Reset that GitHub primary limits carry when
// Retry-After is absent.
func retryAfterHint(header http.Header, now time.Time) time.Duration {
	if d := parseRetryAfter(header.Get("Retry-After"), now); d > 0 {
		return d
	}
	return parseRateLimitReset(header.Get("X-RateLimit-Reset"), now)
}

// parseRateLimitReset converts an epoch-seconds reset stamp to a duration
// from now. Missing, unparseable, non-positive, or already-elapsed stamps
// yield zero; time.Sub saturates, so oversized stamps cannot overflow.
func parseRateLimitReset(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	epoch, err := strconv.ParseInt(value, 10, 64)
	if err != nil || epoch <= 0 {
		return 0
	}
	if remaining := time.Unix(epoch, 0).Sub(now); remaining > 0 {
		return remaining
	}
	return 0
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
