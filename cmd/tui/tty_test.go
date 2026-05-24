package main

import (
	"os"
	"testing"
)

func TestIsTerminal_NilIsNotTTY(t *testing.T) {
	if isTerminal(nil) {
		t.Error("isTerminal(nil) = true, want false")
	}
}

func TestIsTerminal_RegularFileIsNotTTY(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "tty")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if isTerminal(f) {
		t.Error("isTerminal(regular file) = true, want false")
	}
}
