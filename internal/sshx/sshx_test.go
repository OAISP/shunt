package sshx

import (
	"strings"
	"testing"
)

// Every remote argument passes through a shell on the far side, so quoting is
// the boundary between "runs a command" and "runs whatever was in a value".
func TestShellQuote(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", "''"},
		{"plain", "plain"},
		{"/var/lib/shunt/demo", "/var/lib/shunt/demo"},
		{"KEY=value", "KEY=value"},
		{"user@host", "user@host"},
		{"has space", "'has space'"},
		{"semi;colon", "'semi;colon'"},
		{"$(whoami)", "'$(whoami)'"},
		{"back`tick`", "'back`tick`'"},
		{"pipe|rm -rf /", "'pipe|rm -rf /'"},
		{"it's", `'it'\''s'`},
	} {
		if got := shellQuote(tc.in); got != tc.want {
			t.Errorf("shellQuote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestGoArch(t *testing.T) {
	for _, tc := range []struct{ uname, want string }{
		{"x86_64", "amd64"},
		{"amd64", "amd64"},
		{"aarch64", "arm64"},
		{"arm64", "arm64"},
		{"riscv64", "riscv64"},
	} {
		if got := (Facts{Arch: tc.uname}).GoArch(); got != tc.want {
			t.Errorf("GoArch(%q) = %q, want %q", tc.uname, got, tc.want)
		}
	}
}

// Host key checking must never be disabled, and a hung password prompt inside a
// deploy is worse than a clean failure.
func TestBaseArgsAreSafeByDefault(t *testing.T) {
	args := New("host").baseArgs()
	joined := ""
	for _, a := range args {
		joined += a + " "
	}
	if !strings.Contains(joined, "BatchMode=yes") {
		t.Errorf("BatchMode missing from %q", joined)
	}
	if strings.Contains(joined, "StrictHostKeyChecking=no") {
		t.Errorf("host key checking disabled in %q", joined)
	}
}
