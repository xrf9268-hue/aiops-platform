//go:build !linux

package main

import "os"

// isTerminal falls back to a character-device check on non-Linux platforms.
// Note: this also reports true for /dev/null; the Linux build uses a precise
// TCGETS ioctl instead.
func isTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
