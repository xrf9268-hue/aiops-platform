//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd

package main

import (
	"os"
	"syscall"
	"unsafe"
)

// isTerminal reports whether f is a real terminal by issuing the platform's
// "get termios" ioctl (ioctlReadTermios — TCGETS on Linux, TIOCGETA on
// macOS/BSD), the same mechanism libc's isatty() uses. This is more precise
// than checking os.ModeCharDevice, which is also true for /dev/null and similar
// character devices. No external module dependency (golang.org/x/term) is used.
func isTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	var termios syscall.Termios
	_, _, errno := syscall.Syscall6(
		syscall.SYS_IOCTL,
		f.Fd(),
		uintptr(ioctlReadTermios),
		uintptr(unsafe.Pointer(&termios)),
		0, 0, 0,
	)
	return errno == 0
}
