//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd

package main

import (
	"os"
	"os/signal"
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

// terminalWidth reports the live column width of f via a TIOCGWINSZ ioctl — the
// portable equivalent of the BEAM's :io.columns(). It reports ok=false when f is
// nil, not a tty, or the kernel reports a zero width, so callers fall back to the
// COLUMNS env. TIOCGWINSZ is the same constant across the unix targets (unlike
// the termios "get" request), so no per-platform constant is needed. No external
// module dependency (golang.org/x/term) is used.
func terminalWidth(f *os.File) (int, bool) {
	if f == nil {
		return 0, false
	}
	var ws struct{ Row, Col, Xpixel, Ypixel uint16 }
	_, _, errno := syscall.Syscall6(
		syscall.SYS_IOCTL,
		f.Fd(),
		uintptr(syscall.TIOCGWINSZ),
		uintptr(unsafe.Pointer(&ws)),
		0, 0, 0,
	)
	if errno != 0 || ws.Col == 0 {
		return 0, false
	}
	return int(ws.Col), true
}

// notifyResize delivers terminal-resize events (SIGWINCH) to ch and returns a
// stop function. Resize is a unix signal; tty_other.go provides a no-op so the
// render loop stays portable.
func notifyResize(ch chan os.Signal) func() {
	signal.Notify(ch, syscall.SIGWINCH)
	return func() { signal.Stop(ch) }
}
