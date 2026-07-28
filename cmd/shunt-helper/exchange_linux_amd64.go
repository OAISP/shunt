//go:build linux && amd64

package main

// syscall numbers are per-architecture; renameat2 arrived in Linux 3.15.
const sysRenameat2 = 316
