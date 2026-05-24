//go:build dragonfly || freebsd || netbsd || openbsd

package main

import "syscall"

// ioctlReadTermios is the BSD "get termios" ioctl request (shared with macOS,
// which has its own file in tty_darwin.go).
const ioctlReadTermios = syscall.TIOCGETA
