//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"
)

// pollUntil retries fn every interval until it returns (true, nil) or
// fails the test on timeout. The poll context derives from the caller's ctx
// so a cancelled/expired parent (e.g. the test's deadline) stops the poll
// instead of being ignored.
func pollUntil(ctx context.Context, t *testing.T, timeout, interval time.Duration, fn func(context.Context) (bool, error)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var lastErr error
	for time.Now().Before(deadline) {
		ok, err := fn(ctx)
		if err == nil && ok {
			return
		}
		lastErr = err
		select {
		case <-time.After(interval):
		case <-ctx.Done():
			t.Fatalf("pollUntil timed out after %s; last err: %v", timeout, lastErr)
		}
	}
	t.Fatalf("pollUntil timed out after %s; last err: %v", timeout, lastErr)
}
