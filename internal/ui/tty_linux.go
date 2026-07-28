//go:build linux

package ui

// TCGETS: fetching terminal attributes fails with ENOTTY on anything that is
// not a terminal, which is the only reliable test.
const tcGetAttr = 0x5401
