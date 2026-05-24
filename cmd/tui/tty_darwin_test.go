package main

import (
	"os"
	"testing"
)

// macOS-specific guard: /dev/null is a character device, so an os.ModeCharDevice
// check would misclassify it as a terminal; the TIOCGETA ioctl must reject it.
// Runs only on darwin (file name GOOS suffix); CI is Linux, so this is exercised
// when developing locally on macOS.
func TestIsTerminal_DevNullIsNotTTY_Darwin(t *testing.T) {
	f, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if isTerminal(f) {
		t.Error("isTerminal(/dev/null) = true, want false (TIOCGETA should reject)")
	}
}
