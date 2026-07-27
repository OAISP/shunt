package main

import (
	"strings"
)

// splitLines splits command output into lines, discarding a trailing newline so
// callers never emit a spurious empty log event.
func splitLines(s string) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
