package ui

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// notATTY stands in for a pipe or a redirect.
func notATTY(t *testing.T) *os.File {
	t.Helper()
	f, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// The zero Style must be safe: it is what tests and any un-plumbed caller get.
func TestZeroStyleEmitsNoEscapes(t *testing.T) {
	var s Style
	for name, got := range map[string]string{
		"Bold": s.Bold("x"), "Dim": s.Dim("x"), "Red": s.Red("x"),
		"Green": s.Green("x"), "Amber": s.Amber("x"),
		"Tick": s.Tick(), "Cross": s.Cross(), "Add": s.Add(),
	} {
		if strings.Contains(got, "\033") {
			t.Errorf("%s emitted an escape sequence from the zero Style: %q", name, got)
		}
	}
	if s.Enabled() {
		t.Error("zero Style reports colour enabled")
	}
}

// Colour on a non-terminal corrupts piped output; NO_COLOR is a convention we
// do not get to opt out of.
func TestColorSuppression(t *testing.T) {
	f := notATTY(t)
	if NewStyle(f).Enabled() {
		t.Error("colour enabled on a non-terminal")
	}
	t.Setenv("NO_COLOR", "1")
	if colorOK(f) {
		t.Error("colour enabled despite NO_COLOR")
	}
	os.Unsetenv("NO_COLOR")
	t.Setenv("TERM", "dumb")
	if colorOK(f) {
		t.Error("colour enabled on a dumb terminal")
	}
}

func TestStyleWrapsAndClosesEverySequence(t *testing.T) {
	s := Style{color: true}
	got := s.Bold("hello")
	if !strings.HasPrefix(got, ansiBold) || !strings.HasSuffix(got, ansiReset) {
		t.Errorf("Bold = %q, want bold-wrapped and reset", got)
	}
	// An empty string must not emit a dangling escape pair.
	if s.Dim("") != "" {
		t.Errorf("Dim(\"\") = %q, want empty", s.Dim(""))
	}
}

func TestBannerSuppression(t *testing.T) {
	f := notATTY(t)
	if BannerAllowed(f, false) {
		t.Error("banner allowed on a non-terminal")
	}
	if BannerAllowed(f, true) {
		t.Error("banner allowed with --json")
	}
	t.Setenv("SHUNT_NO_BANNER", "1")
	if BannerAllowed(f, false) {
		t.Error("banner allowed despite SHUNT_NO_BANNER")
	}
}

func TestBannerContent(t *testing.T) {
	var b bytes.Buffer
	Banner(&b, Plain(), "9.9.9")
	out := b.String()
	if strings.Contains(out, "\033") {
		t.Errorf("escape sequences in an uncoloured banner: %q", out)
	}
	for _, want := range []string{Tagline, "9.9.9"} {
		if !strings.Contains(out, want) {
			t.Errorf("banner missing %q", want)
		}
	}
}

// A wordmark that is not rectangular looks broken in half the terminals it
// lands in, and a wide one wraps over ssh.
func TestWordmarkGeometry(t *testing.T) {
	lines := strings.Split(wordmark, "\n")
	if len(lines) != 2 {
		t.Fatalf("wordmark has %d lines, want 2", len(lines))
	}
	a, b := len([]rune(lines[0])), len([]rune(lines[1]))
	if a != b {
		t.Errorf("wordmark lines differ: %d vs %d runes", a, b)
	}
	if a > 60 {
		t.Errorf("wordmark is %d columns wide, too wide for a narrow terminal", a)
	}
}

func TestBytes(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{999, "999 B"},
		{1024, "1.0 KB"},
		{5323, "5.2 KB"},
		{85379846, "81.4 MB"},
		{2 << 30, "2.0 GB"},
	} {
		if got := Bytes(tc.in); got != tc.want {
			t.Errorf("Bytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestShortDigest(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", "-"},
		{"sha256:73c104cb60d7a2e9", "73c104cb60d7"},
		{"73c104cb60d7a2e9", "73c104cb60d7"},
		{"short", "short"},
	} {
		if got := ShortDigest(tc.in); got != tc.want {
			t.Errorf("ShortDigest(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestTruncateAndFirstLine(t *testing.T) {
	if got := Truncate("abcdefghij", 5); got != "abcd…" {
		t.Errorf("Truncate = %q", got)
	}
	if got := Truncate("abc", 10); got != "abc" {
		t.Errorf("Truncate should not pad or cut: %q", got)
	}
	if got := FirstLine("one\ntwo"); got != "one" {
		t.Errorf("FirstLine = %q", got)
	}
	if got := FirstLine("only"); got != "only" {
		t.Errorf("FirstLine = %q", got)
	}
}

// A multi-line error must stay visually attached to its bullet.
func TestIndent(t *testing.T) {
	if got := Indent("first\nsecond"); got != "first\n  second" {
		t.Errorf("Indent = %q", got)
	}
	if got := Indent("single"); got != "single" {
		t.Errorf("Indent altered a single line: %q", got)
	}
}
