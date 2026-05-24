//go:build !linux && !darwin && !dragonfly && !freebsd && !netbsd && !openbsd

package main

import "os"

// isTerminal falls back to a character-device check on platforms without a
// termios ioctl wired up here (e.g. Windows, Plan 9). Note: this also reports
// true for /dev/null; the unix builds use a precise termios ioctl instead.
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

// terminalWidth has no portable live-width query without the TIOCGWINSZ ioctl,
// so it reports ok=false and callers fall back to the COLUMNS env.
func terminalWidth(*os.File) (int, bool) {
	return 0, false
}

// notifyResize is a no-op where SIGWINCH is unavailable (e.g. Windows): there
// are no resize events to deliver, so the returned stop function does nothing.
func notifyResize(chan os.Signal) func() {
	return func() {}
}
