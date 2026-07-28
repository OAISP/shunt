// Package ui owns everything shunt prints for a human: colour, the banner, and
// the handful of formatting helpers used across commands.
//
// It exists so colour is decided in exactly one place. Escape codes scattered
// through command code inevitably means some output honours NO_COLOR and some
// does not, which is worse than having no colour at all.
package ui

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// ANSI codes, kept unexported so nothing outside this package can hardcode one.
const (
	ansiReset = "\033[0m"
	ansiBold  = "\033[1m"
	ansiDim   = "\033[2m"
	ansiRed   = "\033[31m"
	ansiGreen = "\033[32m"
	ansiAmber = "\033[33m"
)

// Style renders text for one destination. The zero value is a valid,
// colour-free styler, so tests and pipes need no setup.
type Style struct{ color bool }

// NewStyle decides whether w can take colour, honouring the NO_COLOR convention
// (https://no-color.org), dumb terminals, and redirection.
func NewStyle(w *os.File) Style { return Style{color: colorOK(w)} }

// Plain is a styler that emits no escape sequences.
func Plain() Style { return Style{} }

// Enabled reports whether this styler emits colour.
func (s Style) Enabled() bool { return s.color }

func (s Style) wrap(code, v string) string {
	if !s.color || v == "" {
		return v
	}
	return code + v + ansiReset
}

func (s Style) Bold(v string) string  { return s.wrap(ansiBold, v) }
func (s Style) Dim(v string) string   { return s.wrap(ansiDim, v) }
func (s Style) Red(v string) string   { return s.wrap(ansiRed, v) }
func (s Style) Green(v string) string { return s.wrap(ansiGreen, v) }
func (s Style) Amber(v string) string { return s.wrap(ansiAmber, v) }

// Markers used in plan output and progress. Kept here so a create is the same
// green plus everywhere it appears.
func (s Style) Tick() string   { return s.Green("✓") }
func (s Style) Cross() string  { return s.Red("✗") }
func (s Style) Bullet() string { return s.Green("●") }
func (s Style) Add() string    { return s.Green("+") }
func (s Style) Change() string { return s.Amber("~") }
func (s Style) Remove() string { return s.Red("-") }
func (s Style) Warn() string   { return s.Amber("!") }

func colorOK(w *os.File) bool {
	if w == nil || os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	return isTerminal(w)
}

// IsTerminal is the exported form, used to decide whether to prompt.
func IsTerminal(w *os.File) bool { return isTerminal(w) }

// ---- banner ----------------------------------------------------------------

// The wordmark is drawn with half-block characters so it stays two lines tall.
// Anything bigger is a banner you resent by the tenth time you see it.
const wordmark = `█▀▀ █ █ █ █ █▄ █ ▀█▀
▄▄█ █▀█ █▄█ █ ▀█  █ `

// Tagline is the one-line description of the tool, shared by the banner and the
// generated manifest header.
const Tagline = "registry-free docker deploys over ssh"

// BannerAllowed reports whether a banner is appropriate. Banners are for humans
// reading a terminal; they are noise in a pipe, a CI log, or a --json consumer.
func BannerAllowed(w *os.File, asJSON bool) bool {
	if asJSON || os.Getenv("SHUNT_NO_BANNER") != "" {
		return false
	}
	return isTerminal(w)
}

// Banner writes the wordmark, tagline and version.
func Banner(w io.Writer, s Style, version string) {
	for _, line := range strings.Split(wordmark, "\n") {
		fmt.Fprintln(w, s.Bold(line))
	}
	fmt.Fprintln(w, s.Dim(fmt.Sprintf("%s · v%s", Tagline, version)))
}

// ---- formatting ------------------------------------------------------------

// Bytes formats a byte count for human-facing output.
func Bytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// ShortDigest trims a sha256 reference to something readable in a table.
func ShortDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if d == "" {
		return "-"
	}
	if len(d) > 12 {
		return d[:12]
	}
	return d
}

// Truncate shortens s to at most n columns, marking the cut.
func Truncate(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// FirstLine returns the first line of a multi-line message, for summaries.
func FirstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// Indent adds two spaces to every line after the first, so a multi-line error
// stays visually attached to its bullet.
func Indent(s string) string {
	lines := strings.Split(s, "\n")
	for i := 1; i < len(lines); i++ {
		lines[i] = "  " + lines[i]
	}
	return strings.Join(lines, "\n")
}
