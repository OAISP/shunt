package main

import (
	"context"
	"fmt"
	"os"

	"github.com/OAISP/shunt/internal/engine"
)

// cmdRollback restores a previous release from images already on the host — no
// rebuild, no transfer.
func cmdRollback(ctx context.Context, args []string) error {
	var c commonFlags
	var yes bool
	fs := newFlagSet("rollback", &c)
	fs.BoolVar(&yes, "y", false, "skip the confirmation prompt")
	fs.BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	target := fs.Arg(0)

	e, err := connect(ctx, c.file, c.target)
	if err != nil {
		return err
	}
	defer e.Close()

	st, err := e.State(ctx)
	if err != nil {
		return err
	}
	if st.Ledger == nil || len(st.Ledger.Releases) == 0 {
		return fmt.Errorf("no deploy history on %s", e.M.Host)
	}
	want := target
	if want == "" {
		prev := st.Ledger.Previous()
		if prev == nil {
			return fmt.Errorf("no previous successful release to roll back to")
		}
		want = prev.ID
	}

	s := c.err()
	fmt.Fprintf(os.Stderr, "\n  roll back %s → %s\n", st.Ledger.Current, s.Bold(want))
	ok, err := confirmed(yes, "  proceed?")
	if err != nil || !ok {
		return err
	}

	fmt.Fprintln(os.Stderr)
	return run(func(r engine.EventRenderer) error {
		// `want`, not `target`: target may be empty, which would let the helper
		// resolve "previous" a second time and act on a different release than
		// the one just confirmed. Deploy or roll back concurrently and the two
		// answers genuinely differ.
		return e.Rollback(ctx, want, r)
	}, c.renderer(), "rollback")
}

// cmdRetire stops the containers of a service that is no longer in the manifest.
// A deploy only reports such an orphan — stopping containers nobody asked it to
// stop is not its job — so this is its own verb.
func cmdRetire(ctx context.Context, args []string) error {
	var c commonFlags
	var yes bool
	fs := newFlagSet("retire", &c)
	fs.BoolVar(&yes, "y", false, "skip the confirmation prompt")
	fs.BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	name := fs.Arg(0)
	if name == "" {
		return usagef("usage: shunt retire <service>")
	}

	e, err := connect(ctx, c.file, c.target)
	if err != nil {
		return err
	}
	defer e.Close()

	// Refusing to retire a declared service is the guard that stops this being
	// an accidental way to take production down.
	if _, present := e.M.Services[name]; present {
		return fmt.Errorf("%q is still declared in shunt.toml — remove it from the manifest first, then retire it", name)
	}
	if _, present := e.M.Accessories[name]; present {
		return fmt.Errorf("%q is an accessory, not a retired service — remove it from shunt.toml first if you mean to drop it", name)
	}

	state, err := e.State(ctx)
	if err != nil {
		return err
	}
	running := state.ServiceContainers(name)
	if len(running) == 0 {
		fmt.Fprintf(os.Stderr, "\n  nothing on %s is running for %q\n", e.M.Host, name)
		return nil
	}

	s := c.err()
	fmt.Fprintf(os.Stderr, "\n  retire %s on %s\n", s.Bold(name), e.M.Host)
	for _, ct := range running {
		fmt.Fprintf(os.Stderr, "    %s  %s\n", ct.Name, s.Dim(ct.Status))
	}
	fmt.Fprintf(os.Stderr, "  %s\n", s.Amber("this stops and removes the container(s) above"))
	ok, err := confirmed(yes, "  proceed?")
	if err != nil || !ok {
		return err
	}

	fmt.Fprintln(os.Stderr)
	return run(func(r engine.EventRenderer) error {
		return e.Retire(ctx, name, r)
	}, c.renderer(), "retire")
}

// cmdBoot recreates one accessory. Deliberately not part of `up`: destroying a
// database container should never be a side effect of shipping code.
func cmdBoot(ctx context.Context, args []string) error {
	var c commonFlags
	var yes bool
	fs := newFlagSet("boot", &c)
	fs.BoolVar(&yes, "y", false, "skip the confirmation prompt")
	fs.BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	name := fs.Arg(0)
	if name == "" {
		return usagef("usage: shunt boot <accessory>")
	}

	e, err := connect(ctx, c.file, c.target)
	if err != nil {
		return err
	}
	defer e.Close()

	if _, present := e.M.Accessories[name]; !present {
		return fmt.Errorf("%q is not declared under [accessories.*] in shunt.toml", name)
	}
	state, err := e.State(ctx)
	if err != nil {
		return err
	}
	// boot does not build, so any locally-built image an accessory uses has to
	// come from the release currently on the host.
	spec, err := e.BootSpec(ctx, state)
	if err != nil {
		return err
	}

	s := c.err()
	fmt.Fprintf(os.Stderr, "\n  recreate accessory %s on %s\n", s.Bold(name), e.M.Host)
	fmt.Fprintf(os.Stderr, "  %s — data survives only in named volumes\n", s.Amber("this destroys the running container"))
	ok, err := confirmed(yes, "  proceed?")
	if err != nil || !ok {
		return err
	}

	fmt.Fprintln(os.Stderr)
	return run(func(r engine.EventRenderer) error {
		return e.Boot(ctx, name, spec, r)
	}, c.renderer(), "boot")
}
