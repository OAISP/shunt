package main

import (
	"os"
	"syscall"
	"unsafe"
)

// renameExchange atomically swaps two existing paths.
//
// It is what closes the one gap a directory artifact otherwise has. A file can
// be promoted with a hard link and a rename, so its destination always holds
// either the old contents or the new ones. A directory cannot be hard-linked,
// and rename refuses to replace a non-empty one, so the old tree had to be moved
// aside first — leaving a window, one syscall wide, in which the destination did
// not exist at all. An app that opened a path in that window got ENOENT.
//
// RENAME_EXCHANGE swaps the two entries in a single atomic step instead: after
// it, the destination holds the new tree and the staging path holds the old one,
// with no moment in between. Linux 3.15 and later, and not every filesystem
// implements it — callers fall back.
func renameExchange(a, b string) error {
	ap, err := syscall.BytePtrFromString(a)
	if err != nil {
		return err
	}
	bp, err := syscall.BytePtrFromString(b)
	if err != nil {
		return err
	}
	// AT_FDCWD is -100, and has to reach the kernel as an unsigned word of the
	// same width. Go refuses to convert a negative *constant* to uintptr, so it
	// goes through a variable, which is a runtime conversion and is allowed.
	atFd := -100
	atFdCwd := uintptr(atFd)
	const renameExchangeFlag = 1 << 1

	_, _, errno := syscall.Syscall6(sysRenameat2,
		atFdCwd, uintptr(unsafe.Pointer(ap)),
		atFdCwd, uintptr(unsafe.Pointer(bp)),
		uintptr(renameExchangeFlag), 0)
	if errno != 0 {
		return &os.LinkError{Op: "renameexchange", Old: a, New: b, Err: errno}
	}
	return nil
}
