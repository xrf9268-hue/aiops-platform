//go:build linux

package main

import (
	"os"
	"syscall"
	"unsafe"
)

// isTerminal reports whether f is a real terminal by issuing the TCGETS ioctl —
// the same mechanism libc's isatty() uses. This is more precise than checking
// os.ModeCharDevice, which is also true for /dev/null and similar character
// devices. No external module dependency (golang.org/x/term) is required.
func isTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	var termios syscall.Termios
	_, _, errno := syscall.Syscall6(
		syscall.SYS_IOCTL,
		f.Fd(),
		syscall.TCGETS,
		uintptr(unsafe.Pointer(&termios)),
		0, 0, 0,
	)
	return errno == 0
}
