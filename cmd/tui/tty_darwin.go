package main

import "syscall"

// macOS is a primary development platform for this tool, so it gets its own
// build-tagged file (Go's `darwin` GOOS suffix) rather than being folded into
// the generic BSD group. The isatty mechanism is the BSD-style TIOCGETA ioctl
// (Darwin has no Linux-style TCGETS); the shared implementation lives in
// tty_unix.go.
const ioctlReadTermios = syscall.TIOCGETA
