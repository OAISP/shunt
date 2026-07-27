package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/OAISP/shunt/internal/build"
	"github.com/OAISP/shunt/internal/engine"
	"github.com/OAISP/shunt/internal/manifest"
	"github.com/OAISP/shunt/internal/release"
	"github.com/OAISP/shunt/internal/ui"
)

const protocolVersion = release.Protocol

// errReported means the failure has already been shown to the user in detail —
// by the event renderer, typically as a red ✗ with the remote's own message.
// main exits non-zero without printing anything further, so the operator sees
// one explanation instead of two.
var errReported = errors.New("already reported")

// commonFlags are accepted by every command that talks to a host.
type commonFlags struct {
	file    string
	verbose bool
	asJSON  bool
}

func newFlagSet(name string, c *commonFlags) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.StringVar(&c.file, "f", "", "path to shunt.toml")
	fs.StringVar(&c.file, "file", "", "path to shunt.toml")
	fs.BoolVar(&c.verbose, "v", false, "verbose output")
	fs.BoolVar(&c.verbose, "verbose", false, "verbose output")
	fs.BoolVar(&c.asJSON, "json", false, "machine-readable output")
	return fs
}

// out is the styler for prose and plans, which go to stdout so they can be
// piped. err is for progress, which goes to stderr so it does not pollute a
// redirected plan.
func (c *commonFlags) out() ui.Style { return ui.NewStyle(os.Stdout) }
func (c *commonFlags) err() ui.Style { return ui.NewStyle(os.Stderr) }

func (c *commonFlags) renderer() engine.EventRenderer {
	if c.asJSON {
		return &engine.JSONRenderer{Out: os.Stdout}
	}
	return &engine.HumanRenderer{Out: os.Stderr, Style: c.err(), Verbose: c.verbose}
}

// run drives a helper operation and normalises how failures are reported: if the
// renderer already showed the remote's own error, do not restate it.
func run(op func(engine.EventRenderer) error, r engine.EventRenderer, what string) error {
	err := op(r)
	if r.Failed() {
		return errReported
	}
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
}

// parseArgs parses flags that appear anywhere, including after a positional
// argument.
//
// Go's flag package stops at the first non-flag argument, which silently drops
// everything after it: `shunt logs app --follow` would not follow, and
// `shunt boot db -f path` would ignore the manifest. Nobody expects that, so
// flags are permuted ahead of positionals before parsing.
func parseArgs(fs *flag.FlagSet, args []string) error {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" { // everything after is positional, by convention
			positional = append(positional, args[i+1:]...)
			break
		}
		if len(a) < 2 || !strings.HasPrefix(a, "-") {
			positional = append(positional, a)
			continue
		}
		flags = append(flags, a)
		// A non-boolean flag written as `--tail 100` takes the next argument
		// with it; `--tail=100` already carries its value.
		if !strings.Contains(a, "=") && i+1 < len(args) {
			if f := fs.Lookup(strings.TrimLeft(a, "-")); f != nil && !isBoolFlag(f) {
				i++
				flags = append(flags, args[i])
			}
		}
	}
	// The terminator has to survive re-assembly: without it a positional that
	// begins with a dash would be re-interpreted as a flag.
	return fs.Parse(append(append(flags, "--"), positional...))
}

func isBoolFlag(f *flag.Flag) bool {
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

// loadManifest resolves -f/--file or walks up from the working directory.
func loadManifest(path string) (*manifest.Manifest, error) {
	if path == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		if path, err = manifest.Find(wd); err != nil {
			return nil, err
		}
	}
	return manifest.Load(path)
}

// connect loads the manifest and opens the host connection, which is what every
// command except init needs first.
func connect(ctx context.Context, file string) (*engine.Engine, error) {
	m, err := loadManifest(file)
	if err != nil {
		return nil, err
	}
	e := engine.New(m)
	if err := e.Connect(ctx); err != nil {
		return nil, err
	}
	return e, nil
}

// buildOut is everything plan and up need before they diverge.
type buildOut struct {
	engine *engine.Engine
	spec   *release.Spec
	built  map[string]*build.Result
	state  *engine.RemoteState
}

// prepare connects, builds every image, assembles the release spec and reads the
// host's current state. Callers own closing the engine on success.
func prepare(ctx context.Context, c *commonFlags, noCache bool) (*buildOut, error) {
	e, err := connect(ctx, c.file)
	if err != nil {
		return nil, err
	}
	s := c.err()
	fmt.Fprintln(os.Stderr, s.Dim(fmt.Sprintf("host %s · docker %s · %s",
		e.M.Host, e.Facts().DockerVersion, e.Facts().Arch)))

	// Any failure past this point must not leak the ssh master connection.
	fail := func(err error) (*buildOut, error) {
		e.Close()
		return nil, err
	}

	// Everything checkable is checked before the expensive steps: building and
	// shipping hundreds of megabytes only to fail on mkdir wastes minutes and
	// tells the operator nothing useful.
	if err := e.PreflightArtifacts(ctx); err != nil {
		return fail(err)
	}

	id := engine.NewReleaseID()
	fmt.Fprintf(os.Stderr, "\n%s\n", s.Bold("building"))
	built, err := e.Build(ctx, id, engine.BuildOptions{NoCache: noCache, Verbose: c.verbose})
	if err != nil {
		return fail(err)
	}
	for _, name := range slices.Sorted(maps.Keys(built)) {
		fmt.Fprintf(os.Stderr, "  %s %s %s (%s)\n", s.Tick(), name,
			ui.ShortDigest(built[name].Digest), ui.Bytes(built[name].Bytes))
	}

	spec, err := e.Spec(ctx, id, built)
	if err != nil {
		return fail(err)
	}
	state, err := e.State(ctx)
	if err != nil {
		return fail(err)
	}
	return &buildOut{engine: e, spec: spec, built: built, state: state}, nil
}

// confirm asks a yes/no question. Callers must only reach it when stdin is a
// terminal, so CI never blocks on a prompt nobody can answer.
func confirm(prompt string) (bool, error) {
	fmt.Printf("%s [y/N] ", prompt)
	sc := bufio.NewScanner(os.Stdin)
	if !sc.Scan() {
		return false, sc.Err()
	}
	s := strings.ToLower(strings.TrimSpace(sc.Text()))
	return s == "y" || s == "yes", nil
}

// confirmed reports whether the operation may proceed: either the user passed
// -y, or there is no terminal to ask at, or they said yes.
func confirmed(yes bool, prompt string) (bool, error) {
	if yes || !ui.IsTerminal(os.Stdin) {
		return true, nil
	}
	ok, err := confirm(prompt)
	if err != nil {
		return false, err
	}
	if !ok {
		fmt.Println("  aborted")
	}
	return ok, nil
}
