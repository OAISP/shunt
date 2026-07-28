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

	e, err := connect(ctx, c.file)
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

	s := c.out()
	fmt.Printf("\n  roll back %s → %s\n", st.Ledger.Current, s.Bold(want))
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
		return fmt.Errorf("usage: shunt boot <accessory>")
	}

	e, err := connect(ctx, c.file)
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

	s := c.out()
	fmt.Printf("\n  recreate accessory %s on %s\n", s.Bold(name), e.M.Host)
	fmt.Printf("  %s — data survives only in named volumes\n", s.Amber("this destroys the running container"))
	ok, err := confirmed(yes, "  proceed?")
	if err != nil || !ok {
		return err
	}

	fmt.Fprintln(os.Stderr)
	return run(func(r engine.EventRenderer) error {
		return e.Boot(ctx, name, spec, r)
	}, c.renderer(), "boot")
}
