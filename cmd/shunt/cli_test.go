package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/OAISP/shunt/internal/engine"
	"github.com/OAISP/shunt/internal/release"
)

// stubRenderer lets command plumbing be tested without a host.
type stubRenderer struct{ failed bool }

func (s *stubRenderer) Handle(ev release.Event) {
	if ev.Kind == release.KindFail {
		s.failed = true
	}
}
func (s *stubRenderer) Failed() bool { return s.failed }

// The renderer already printed the remote's own explanation, so run must not
// stack a second, vaguer message on top of it.
func TestRunCollapsesAnAlreadyReportedFailure(t *testing.T) {
	r := &stubRenderer{}
	err := run(func(er engine.EventRenderer) error {
		er.Handle(release.Event{Kind: release.KindFail, Message: "stage \"migrate\" failed"})
		return errors.New("exit status 1")
	}, r, "deploy")

	if !errors.Is(err, errReported) {
		t.Fatalf("err = %v, want errReported", err)
	}
}

// A transport failure produces no fail event, so it must surface in full.
func TestRunSurfacesATransportFailure(t *testing.T) {
	err := run(func(engine.EventRenderer) error {
		return errors.New("ssh: connection refused")
	}, &stubRenderer{}, "deploy")

	if errors.Is(err, errReported) {
		t.Fatal("a transport error was swallowed as already-reported")
	}
	if !strings.Contains(err.Error(), "connection refused") || !strings.Contains(err.Error(), "deploy") {
		t.Errorf("err = %v, want it to name both the operation and the cause", err)
	}
}

func TestRunPassesSuccessThrough(t *testing.T) {
	if err := run(func(engine.EventRenderer) error { return nil }, &stubRenderer{}, "deploy"); err != nil {
		t.Errorf("err = %v, want nil", err)
	}
}

// Every command in the usage text must actually be routed, and vice versa.
// `shunt init` once wrote its file and then exited 2 with "unknown command",
// because a switch case fell through into the dispatch lookup.
func TestUsageAndDispatchTableAgree(t *testing.T) {
	for name := range commands {
		if name == "deploy" {
			continue // documented alias for up
		}
		if !strings.Contains(usage, "shunt "+name) {
			t.Errorf("command %q is routed but missing from the usage text", name)
		}
	}
	for _, line := range strings.Split(usage, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "shunt" {
			continue
		}
		name := fields[1]
		if name == "version" {
			continue // handled directly in main; it returns rather than erroring
		}
		if _, ok := commands[name]; !ok {
			t.Errorf("usage advertises %q but nothing routes it", name)
		}
	}
}

// Go's flag package stops at the first positional, silently dropping every flag
// after it. `shunt logs app --follow` not following is the kind of bug users
// blame themselves for, so the permutation is worth pinning down.
func TestParseArgsAcceptsFlagsAfterPositionals(t *testing.T) {
	var c commonFlags
	var follow bool
	var tail string
	fs := newFlagSet("logs", &c)
	fs.BoolVar(&follow, "follow", false, "")
	fs.StringVar(&tail, "tail", "100", "")

	if err := parseArgs(fs, []string{"app", "--follow", "--tail", "5", "-f", "/tmp/shunt.toml"}); err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if fs.Arg(0) != "app" {
		t.Errorf("positional = %q, want app", fs.Arg(0))
	}
	if !follow {
		t.Error("--follow after a positional was dropped")
	}
	if tail != "5" {
		t.Errorf("--tail = %q, want 5", tail)
	}
	if c.file != "/tmp/shunt.toml" {
		t.Errorf("-f = %q, want /tmp/shunt.toml", c.file)
	}
}

func TestParseArgsHandlesEqualsFormAndDoubleDash(t *testing.T) {
	var c commonFlags
	fs := newFlagSet("rollback", &c)

	if err := parseArgs(fs, []string{"20260726-1200", "--file=/x/shunt.toml"}); err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if c.file != "/x/shunt.toml" {
		t.Errorf("--file= form = %q", c.file)
	}
	if fs.Arg(0) != "20260726-1200" {
		t.Errorf("positional = %q", fs.Arg(0))
	}

	// After --, a leading dash is a value, not a flag.
	var c2 commonFlags
	fs2 := newFlagSet("logs", &c2)
	if err := parseArgs(fs2, []string{"--", "-weird-name"}); err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if fs2.Arg(0) != "-weird-name" {
		t.Errorf("after -- got %q, want -weird-name", fs2.Arg(0))
	}
}

func TestParseArgsPreservesPositionalOrder(t *testing.T) {
	var c commonFlags
	fs := newFlagSet("x", &c)
	if err := parseArgs(fs, []string{"one", "-v", "two", "three"}); err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if got := fs.Args(); len(got) != 3 || got[0] != "one" || got[1] != "two" || got[2] != "three" {
		t.Errorf("positionals = %v, want [one two three]", got)
	}
	if !c.verbose {
		t.Error("-v was dropped")
	}
}

// Every other prompt proceeds when stdin is not a terminal, so CI never blocks.
// Purging is the one operation where inferring consent from a missing tty is
// wrong: it destroys the ledger, the images and the only copy of the project's
// secrets, and a pipeline that reaches it by accident has nobody to stop it.
func TestPurgeRefusesToInferConsentFromAMissingTerminal(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		purge, yes, interactive, want bool
	}{
		{"piped purge with no -y is refused", true, false, false, true},
		{"piped purge with -y proceeds", true, true, false, false},
		{"interactive purge asks rather than refusing", true, false, true, false},
		{"a plain down is reversible and never refused", false, false, false, false},
	} {
		if got := purgeNeedsConsent(tc.purge, tc.yes, tc.interactive); got != tc.want {
			t.Errorf("%s: purgeNeedsConsent = %v, want %v", tc.name, got, tc.want)
		}
	}
}
