//go:build darwin

package ui

// TIOCGETA is the BSD equivalent of Linux's TCGETS.
const tcGetAttr = 0x40487413
