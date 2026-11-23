package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/OAISP/shunt/internal/build"
	"github.com/OAISP/shunt/internal/engine"
	"github.com/OAISP/shunt/internal/release"
	"github.com/OAISP/shunt/internal/ui"
)

// cmdPlan builds and diffs, but changes nothing on the host.
func cmdPlan(ctx context.Context, args []string) error {
	var c commonFlags
	var noCache, detailed bool
	fs := newFlagSet("plan", &c)
	fs.BoolVar(&noCache, "no-cache", false, "build without the layer cache")
	fs.BoolVar(&detailed, "detailed-exitcode", false, "exit 2 when there are changes to apply")
	if err := parseArgs(fs, args); err != nil {
		return err
	}

	b, err := prepare(ctx, &c, noCache)
	if err != nil {
		return err
	}
	defer b.engine.Close()

	p, err := b.engine.BuildPlan(ctx, b.spec, b.built, b.state)
	if err != nil {
		return err
	}
	if c.asJSON {
		if err := json.NewEncoder(os.Stdout).Encode(p); err != nil {
			return err
		}
	} else {
		p.Render(os.Stdout, c.out())
		if p.Changed() {
			fmt.Println("  run `shunt up` to apply")
		} else {
			fmt.Println("  nothing to do — the host already matches this manifest")
		}
	}
	// Opt-in, as with terraform: a plan step in a pull request should not fail
	// the job merely because there is something to deploy.
	if detailed && p.Changed() {
		return errExitChanges
	}
	return nil
}

// cmdUp runs the whole pipeline: build, ship, stages, swap, health.
func cmdUp(ctx context.Context, args []string) error {
	var c commonFlags
	var noCache, yes, skipPlan, rollbackOnFail bool
	fs := newFlagSet("up", &c)
	fs.BoolVar(&noCache, "no-cache", false, "build without the layer cache")
	fs.BoolVar(&yes, "y", false, "skip the confirmation prompt")
	fs.BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	fs.BoolVar(&skipPlan, "no-plan", false, "do not show the plan first")
	fs.BoolVar(&rollbackOnFail, "rollback-on-failure", false,
		"restore the previous release if this one fails after replacing a container")
	if err := parseArgs(fs, args); err != nil {
		return err
	}

	start := time.Now()
	b, err := prepare(ctx, &c, noCache)
	if err != nil {
		return err
	}
	defer b.engine.Close()

	p, err := b.engine.BuildPlan(ctx, b.spec, b.built, b.state)
	if err != nil {
		return err
	}
	if !c.asJSON && !skipPlan {
		p.Render(os.Stdout, c.out())
	}
	// Checked outside the rendering branch on purpose. Nesting it there meant
	// --json and --no-plan shipped and recreated every service on every run,
	// which is precisely the CI path where a redundant deploy costs the most.
	if !p.Changed() {
		if !c.asJSON {
			fmt.Println("  nothing to do — the host already matches this manifest")
		}
		return nil
	}
	if !c.asJSON && !skipPlan {
		ok, err := confirmed(yes, "  apply this plan?")
		if err != nil || !ok {
			return err
		}
	}

	b.engine.WithProvenance(b.spec, version)
	b.spec.RollbackOnFailure = rollbackOnFail
	t, err := deploy(ctx, b.engine, b.spec, b.built, b.state, &c)
	if err != nil {
		return err
	}
	if !c.asJSON {
		// Broken down rather than one total. The wire cost is the number shunt
		// advertises, but the host still has to load the image and swap
		// containers, and an operator wondering where twelve seconds went should
		// not have to guess which phase owns them.
		fmt.Fprintf(os.Stderr, "  %s\n", c.err().Dim(fmt.Sprintf(
			"done in %s  ·  build %s · ship %s · apply %s",
			round(time.Since(start)), round(b.buildTook), round(t.ship), round(t.apply))))
	}
	return nil
}

// phases is where a deploy's wall clock went after the build.
type phases struct{ ship, apply time.Duration }

// deploy takes the project lock on the host, ships the release and hands it to
// the helper, which holds the lock until the release is applied.
//
// The plan was built before the lock was taken — a build can take minutes — so
// the release it was built against must still be serving once the lock is
// ours. Checked before anything is shipped: that is what stops a deploy that
// waited behind another from silently reverting it, and finding out after an
// upload would waste the upload.
func deploy(ctx context.Context, e *engine.Engine, spec *release.Spec, built map[string]*build.Result,
	state *engine.RemoteState, c *commonFlags) (phases, error) {
	var t phases
	expected := state.ExpectedCurrent()
	s := c.err()
	d, err := e.BeginDeploy(ctx, func(msg string) { fmt.Fprintf(os.Stderr, "  %s\n", s.Dim(msg)) })
	if err != nil {
		return t, err
	}
	defer d.Close()
	if d.Serving != expected {
		return t, fmt.Errorf("this plan was built when %s was serving, but %s is serving now — "+
			"another deploy or rollback landed while this one waited for the lock\n"+
			"  rerun it to plan against the current state", orNone(expected), orNone(d.Serving))
	}
	spec.ExpectedCurrent = expected

	start := time.Now()
	if err := ship(ctx, e, spec, built, state, c); err != nil {
		return t, err
	}
	t.ship = time.Since(start)

	fmt.Fprintf(os.Stderr, "\n%s\n", s.Bold("applying"))
	start = time.Now()
	err = run(func(r engine.EventRenderer) error { return d.Apply(ctx, spec, r) }, c.renderer(), "deploy")
	t.apply = time.Since(start)
	return t, err
}

func orNone(id string) string {
	if id == "" {
		return "nothing"
	}
	return id
}

// round keeps sub-second phases legible instead of collapsing them all to 0s.
func round(d time.Duration) time.Duration {
	if d < time.Second {
		return d.Round(10 * time.Millisecond)
	}
	return d.Round(100 * time.Millisecond)
}

// ship mirrors the built layouts and any artifacts to the host, and reports
// what actually crossed the wire — the number that justifies the whole design.
func ship(ctx context.Context, e *engine.Engine, spec *release.Spec, built map[string]*build.Result,
	state *engine.RemoteState, c *commonFlags) error {
	s := c.err()
	fmt.Fprintf(os.Stderr, "\n%s\n", s.Bold("shipping"))

	st, err := e.Push(ctx, built, c.verbose)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "  %s %s on the wire (image total %s) %s\n", s.Tick(), ui.Bytes(st.Sent),
		ui.Bytes(st.Total), s.Dim(fmt.Sprintf("· %.1f%% already on host", st.DedupPercent())))

	if len(spec.Artifacts) == 0 {
		return nil
	}
	astats, err := e.PushArtifacts(ctx, spec, state.Paths, c.verbose)
	if err != nil {
		return err
	}
	for _, a := range spec.Artifacts {
		if st := astats[a.Name]; st != nil {
			fmt.Fprintf(os.Stderr, "  %s %s: %s sent of %s %s\n", s.Tick(), a.Name,
				ui.Bytes(st.Sent), ui.Bytes(a.Bytes),
				s.Dim(fmt.Sprintf("(%.1f%% unchanged)", st.DedupPercent())))
		}
	}
	return nil
}
