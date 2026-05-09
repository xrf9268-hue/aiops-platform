//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"log"
	"os"
	"testing"
)

var bed *testbed

func TestMain(m *testing.M) {
	ctx := context.Background()
	var err error
	bed, err = setupTestbed(ctx)
	if err != nil {
		log.Fatalf("setupTestbed: %v", err)
	}
	code := m.Run()
	bed.close(ctx)
	os.Exit(code)
}

// fixtureContent reads a fixture file, fataling on any error.
func fixtureContent(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(fmt.Sprintf("fixtures/%s", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}
