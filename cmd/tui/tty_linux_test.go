//go:build linux

package main

import (
	"os"
	"testing"
)

// /dev/null is a character device, so the old os.ModeCharDevice check
// misclassified it as a terminal; the TCGETS ioctl correctly rejects it.
func TestIsTerminal_DevNullIsNotTTY(t *testing.T) {
	f, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if isTerminal(f) {
		t.Error("isTerminal(/dev/null) = true, want false (ioctl should reject)")
	}
}
