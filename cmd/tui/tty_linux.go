//go:build linux

package main

import "syscall"

// ioctlReadTermios is the Linux "get termios" ioctl request.
const ioctlReadTermios = syscall.TCGETS
