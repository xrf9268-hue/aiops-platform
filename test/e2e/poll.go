//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"
)

// pollUntil retries fn every interval until it returns (true, nil) or
// fails the test on timeout.
func pollUntil(t *testing.T, timeout, interval time.Duration, fn func(context.Context) (bool, error)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
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
