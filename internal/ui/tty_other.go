//go:build !linux && !darwin

package ui

import "os"

// isTerminal falls back to the mode check on platforms shunt does not target.
// It over-reports for /dev/null, which is why the unix build does not use it.
func isTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
