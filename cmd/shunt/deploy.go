package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"time"

	"github.com/OAISP/shunt/internal/engine"
	"github.com/OAISP/shunt/internal/ui"
)

// cmdPlan builds and diffs, but changes nothing on the host.
func cmdPlan(ctx context.Context, args []string) error {
	var c commonFlags
	var noCache bool
	fs := newFlagSet("plan", &c)
	fs.BoolVar(&noCache, "no-cache", false, "build without the layer cache")
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
		return json.NewEncoder(os.Stdout).Encode(p)
	}
	p.Render(os.Stdout, c.out())
	if p.Changed() {
		fmt.Println("  run `shunt up` to apply")
	} else {
		fmt.Println("  nothing to do — the host already matches this manifest")
	}
	return nil
}

// cmdUp runs the whole pipeline: build, ship, stages, swap, health.
func cmdUp(ctx context.Context, args []string) error {
	var c commonFlags
	var noCache, yes, skipPlan bool
	fs := newFlagSet("up", &c)
	fs.BoolVar(&noCache, "no-cache", false, "build without the layer cache")
	fs.BoolVar(&yes, "y", false, "skip the confirmation prompt")
	fs.BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	fs.BoolVar(&skipPlan, "no-plan", false, "do not show the plan first")
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

	if err := ship(ctx, b, &c); err != nil {
		return err
	}

	s := c.err()
	fmt.Fprintf(os.Stderr, "\n%s\n", s.Bold("applying"))
	r := c.renderer()
	if err := run(func(r engine.EventRenderer) error {
		return b.engine.Apply(ctx, b.spec, r)
	}, r, "deploy"); err != nil {
		return err
	}
	if !c.asJSON {
		fmt.Fprintf(os.Stderr, "  %s\n", s.Dim(fmt.Sprintf("done in %s", time.Since(start).Round(time.Second))))
	}
	return nil
}

// ship mirrors the built layouts to the host and reports what actually crossed
// the wire — the number that justifies the whole design.
func ship(ctx context.Context, b *buildOut, c *commonFlags) error {
	s := c.err()
	fmt.Fprintf(os.Stderr, "\n%s\n", s.Bold("shipping"))

	stats, err := b.engine.Push(ctx, b.built, c.verbose)
	if err != nil {
		return err
	}
	var sent, total int64
	// Sorted so repeated deploys produce identical output, which makes a diff of
	// two deploy logs meaningful.
	for _, name := range slices.Sorted(maps.Keys(stats)) {
		st := stats[name]
		sent += st.Sent
		total += st.Total
		if c.verbose {
			fmt.Fprintf(os.Stderr, "  %s %s: %s sent of %s %s\n", s.Tick(), name,
				ui.Bytes(st.Sent), ui.Bytes(st.Total),
				s.Dim(fmt.Sprintf("(%.1f%% already on host)", st.DedupPercent())))
		}
	}
	if !c.verbose {
		fmt.Fprintf(os.Stderr, "  %s %s on the wire (image total %s)\n",
			s.Tick(), ui.Bytes(sent), ui.Bytes(total))
	}

	return shipArtifacts(ctx, b, c)
}

// shipArtifacts transfers data files to their staged paths on the host. They are
// only swapped into place later, by the helper, once stages have passed.
func shipArtifacts(ctx context.Context, b *buildOut, c *commonFlags) error {
	if len(b.spec.Artifacts) == 0 {
		return nil
	}
	s := c.err()
	stats, err := b.engine.PushArtifacts(ctx, b.spec, c.verbose)
	if err != nil {
		return err
	}
	for _, a := range b.spec.Artifacts {
		st := stats[a.Name]
		if st == nil {
			continue
		}
		fmt.Fprintf(os.Stderr, "  %s %s: %s sent of %s %s\n", s.Tick(), a.Name,
			ui.Bytes(st.Sent), ui.Bytes(a.Bytes),
			s.Dim(fmt.Sprintf("(%.1f%% unchanged)", st.DedupPercent())))
	}
	return nil
}
