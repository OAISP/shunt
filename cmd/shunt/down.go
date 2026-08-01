package main

import (
	"context"
	"fmt"
	"os"

	"github.com/OAISP/shunt/internal/engine"
	"github.com/OAISP/shunt/internal/ui"
)

// cmdDown is the inverse of `shunt up`: it takes the project off the host.
//
// Three depths, because "stop the app for a minute" and "I am finished with this
// server" are different requests and conflating them is how a rollback target or
// a database disappears by accident:
//
//	shunt down          the services. Reversible — `shunt up` brings them back.
//	shunt down --all    the accessories too. Volumes survive, so does the data.
//	shunt down --purge  everything shunt put here, including the secrets.
//
// Volumes are never removed at any depth. `docker compose down` needs -v for
// that, and shunt has no equivalent on purpose: a flag that deletes a database
// is one tab-completion away from being pressed by mistake.
func cmdDown(ctx context.Context, args []string) error {
	var c commonFlags
	var yes, all, purge bool
	fs := newFlagSet("down", &c)
	fs.BoolVar(&all, "all", false, "also remove accessories (the database container)")
	fs.BoolVar(&purge, "purge", false, "also remove the network, images, release history and secrets")
	fs.BoolVar(&yes, "y", false, "skip the confirmation prompt")
	fs.BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	if err := parseArgs(fs, args); err != nil {
		return err
	}

	// Every other prompt in shunt proceeds when stdin is not a terminal, so CI
	// never blocks on a question nobody can answer. That is the right default
	// for a reversible operation and the wrong one here: purging destroys the
	// ledger, the images and the only copy of this project's secrets, and a
	// pipeline that reaches this line by accident has nobody to stop it. Make
	// the consent explicit instead of inferring it from a missing tty.
	if purgeNeedsConsent(purge, yes, ui.IsTerminal(os.Stdin)) {
		return fmt.Errorf("refusing to purge without a terminal to confirm at\n" +
			"  pass -y if you really mean to remove this project's images, release history and secrets")
	}

	e, err := connect(ctx, c.file, c.target)
	if err != nil {
		return err
	}
	defer e.Close()

	state, err := e.State(ctx)
	if err != nil {
		return err
	}

	s := c.out()
	if !describeDown(state, e, all || purge, purge, s) {
		fmt.Printf("\n  nothing on %s is running for %s\n\n", e.M.Host, e.M.Project)
		return nil
	}

	ok, err := confirmed(yes, "  proceed?")
	if err != nil || !ok {
		return err
	}

	// The same lock a deploy takes. Without it a `down` racing an `up`
	// interleaves removal with creation, and the host ends up with half of each.
	lock, err := e.AcquireLock(ctx, func(msg string) {
		fmt.Fprintf(os.Stderr, "  %s\n", c.err().Dim(msg))
	})
	if err != nil {
		return err
	}
	defer lock.Release()

	fmt.Fprintln(os.Stderr)
	return run(func(r engine.EventRenderer) error {
		return e.Down(ctx, engine.DownOptions{Accessories: all || purge, Purge: purge}, r)
	}, c.renderer(), "down")
}

// purgeNeedsConsent reports whether a purge must be refused for want of an
// explicit -y. Split out so the rule is assertable without a host or a tty,
// which are exactly the two things it is about.
func purgeNeedsConsent(purge, yes, interactive bool) bool {
	return purge && !yes && !interactive
}

// describeDown prints exactly what is about to be removed, and reports whether
// there was anything to remove at all.
//
// Listing before asking rather than summarising: this is the one command whose
// blast radius an operator cannot infer from the verb, and a container they did
// not expect to see in the list is the signal to answer no.
func describeDown(state *engine.RemoteState, e *engine.Engine, accessories, purge bool, s ui.Style) bool {
	fmt.Printf("\n  down %s on %s\n", s.Bold(e.M.Project), e.M.Host)

	var services, accs []engine.Container
	for _, ct := range state.Containers {
		if ct.Kind == "accessory" {
			accs = append(accs, ct)
			continue
		}
		services = append(services, ct)
	}

	found := false
	if len(services) > 0 {
		found = true
		fmt.Printf("\n    %s\n", s.Dim("services"))
		for _, ct := range services {
			fmt.Printf("      %-30s %s\n", ui.Truncate(ct.Name, 30), s.Dim(ct.Status))
		}
	}
	if len(accs) > 0 {
		if accessories {
			found = true
			fmt.Printf("\n    %s\n", s.Dim("accessories"))
		} else {
			fmt.Printf("\n    %s\n", s.Dim("accessories  (kept — pass --all to remove)"))
		}
		for _, ct := range accs {
			fmt.Printf("      %-30s %s\n", ui.Truncate(ct.Name, 30), s.Dim(ct.Status))
		}
	}

	fmt.Println()
	if found {
		fmt.Printf("  %s\n", s.Amber("this stops and removes the container(s) above"))
	}
	if purge {
		fmt.Printf("  %s\n", s.Amber("--purge also removes the network, every release-tagged image,"))
		fmt.Printf("  %s\n", s.Amber("  the release history and this project's secrets on the host"))
		if state.Ledger != nil && len(state.Ledger.Releases) > 0 {
			fmt.Printf("  %s\n", s.Red(fmt.Sprintf("there will be nothing left to `shunt rollback` to (%d release(s) recorded)",
				len(state.Ledger.Releases))))
		}
	}
	// Said at every depth, because it is the question anyone about to run this
	// is actually asking.
	fmt.Printf("  %s\n", s.Dim("named volumes are never touched — your data stays"))

	// Purge always has work — a network, images and the secrets outlive the
	// containers, and reaching for it on a host whose containers are already
	// gone is exactly how you finish decommissioning one.
	return found || purge
}
