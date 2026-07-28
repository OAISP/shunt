//go:build linux || darwin

package ui

import (
	"os"
	"syscall"
	"unsafe"
)

// isTerminal reports whether f is a terminal.
//
// Deliberately an ioctl rather than a mode check. os.ModeCharDevice is set for
// /dev/null just as it is for a tty, so the obvious `fi.Mode()&os.ModeCharDevice`
// test answers "yes" for redirected input — which made `shunt up < /dev/null`
// try to prompt, read EOF, and abort the deploy as though the operator had said
// no. Asking the kernel for the terminal attributes is the only test that
// distinguishes them.
func isTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	var termios [64]byte // larger than termios on both platforms
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(),
		uintptr(tcGetAttr), uintptr(unsafe.Pointer(&termios[0])))
	return errno == 0
}
