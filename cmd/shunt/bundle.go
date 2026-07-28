package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/OAISP/shunt/internal/build"
	"github.com/OAISP/shunt/internal/bundle"
	"github.com/OAISP/shunt/internal/engine"
	"github.com/OAISP/shunt/internal/manifest"
	"github.com/OAISP/shunt/internal/secrets"
	"github.com/OAISP/shunt/internal/ui"
)

// cmdBundle builds a release and writes it to a file instead of a host.
//
// Everything up to the transfer is what `shunt up` already does; the difference
// is where the result goes. That is the whole feature: a release you can carry
// to a machine the builder cannot reach.
func cmdBundle(ctx context.Context, args []string) error {
	var c commonFlags
	var out string
	var noCache bool
	fs := newFlagSet("bundle", &c)
	fs.StringVar(&out, "o", "", "write to this path (default <project>-<release>.shuntpkg)")
	fs.StringVar(&out, "output", "", "write to this path")
	fs.BoolVar(&noCache, "no-cache", false, "build without the layer cache")
	if err := parseArgs(fs, args); err != nil {
		return err
	}

	m, err := loadManifest(c.file, c.target)
	if err != nil {
		return err
	}

	// Bundling builds but never connects: the point is to work where the target
	// is unreachable. That means no host probe, and a release id minted locally.
	id := engine.NewReleaseID()
	e := engine.New(m)
	s := c.err()

	fmt.Fprintf(os.Stderr, "\n%s\n", s.Bold("building"))
	built, err := e.Build(ctx, id, engine.BuildOptions{NoCache: noCache, Verbose: c.verbose})
	if err != nil {
		return err
	}
	for _, name := range sortedNames(built) {
		fmt.Fprintf(os.Stderr, "  %s %s %s (%s)\n", s.Tick(), name,
			ui.ShortDigest(built[name].Digest), ui.Bytes(built[name].Bytes))
	}

	spec, err := e.Spec(ctx, id, built)
	if err != nil {
		return err
	}
	e.WithProvenance(spec, version)

	// The keys travel so `plan` and the ledger can still reason about them; the
	// values are resolved by whoever applies the bundle.
	spec.Secrets = nil

	contents := bundle.Contents{
		Meta: bundle.Meta{
			Created: time.Now().UTC().Format(time.RFC3339),
			Host:    m.Host,
			Secrets: m.Secrets,
			Spec:    spec,
		},
		ImageDirs:     map[string]string{},
		ArtifactPaths: map[string]string{},
	}
	for name, r := range built {
		contents.ImageDirs[name] = r.Dir
	}
	for _, a := range spec.Artifacts {
		contents.ArtifactPaths[a.Name] = e.LocalArtifactPath(a.Name)
	}

	if out == "" {
		out = fmt.Sprintf("%s-%s.shuntpkg", m.Project, id)
	}
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	if err := bundle.Write(f, contents); err != nil {
		f.Close()
		os.Remove(out)
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	fi, _ := os.Stat(out)
	fmt.Printf("\n  %s %s  %s\n", s.Tick(), s.Bold(out), s.Dim(ui.Bytes(fi.Size())))
	fmt.Printf("    %s\n", s.Dim(fmt.Sprintf("release %s · for %s", id, m.Host)))
	if m.Secrets != nil {
		fmt.Printf("    %s\n", s.Dim("secret values are not included; they are resolved when it is applied"))
	}
	fmt.Printf("\n  %s\n\n", s.Dim("shunt apply "+out))
	return nil
}

// cmdApply deploys a bundle.
//
// It reuses the ordinary deploy path wholesale — the same lock held across the
// transfer, the same expected_current check, the same helper — because a bundle
// changes where a release came from and nothing about how it should land. A
// second deploy route would be a second set of failure modes to get right.
func cmdApply(ctx context.Context, args []string) error {
	var c commonFlags
	var host string
	var yes, rollbackOnFail bool
	fs := newFlagSet("apply", &c)
	fs.StringVar(&host, "host", "", "deploy to this host instead of the one recorded in the bundle")
	fs.BoolVar(&yes, "y", false, "skip the confirmation prompt")
	fs.BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	fs.BoolVar(&rollbackOnFail, "rollback-on-failure", false,
		"restore the previous release if this one fails after replacing a container")
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	path := fs.Arg(0)
	if path == "" {
		return fmt.Errorf("usage: shunt apply <bundle>")
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	work, err := os.MkdirTemp("", "shunt-bundle-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)

	b, err := bundle.Read(f, work)
	if err != nil {
		return err
	}
	spec := b.Meta.Spec
	if host == "" {
		host = b.Meta.Host
	}
	if host == "" {
		return fmt.Errorf("this bundle records no host; pass --host")
	}

	s := c.err()
	fmt.Fprintf(os.Stderr, "\n%s %s\n", s.Bold("apply"), filepath.Base(path))
	fmt.Fprintf(os.Stderr, "  %s\n", s.Dim(fmt.Sprintf("release %s · built %s · to %s",
		spec.ID, b.Meta.Created, host)))
	if prov := spec.Provenance.Describe(); prov != "" {
		fmt.Fprintf(os.Stderr, "  %s %s\n", s.Dim("from"), prov)
	}

	// Secret values were deliberately left out of the file, so they are resolved
	// here, from this machine's own provider. That means whoever applies a bundle
	// needs access to the secrets — which is the correct requirement.
	if b.Meta.Secrets != nil {
		resolved, err := secrets.Resolve(&manifest.Manifest{Secrets: b.Meta.Secrets})
		if err != nil {
			return fmt.Errorf("%w\n  the bundle records the provider but not the values; run this where they are reachable", err)
		}
		spec.Secrets = resolved
		fmt.Fprintf(os.Stderr, "  %s\n", s.Dim(fmt.Sprintf("resolved %d secret(s) locally", len(resolved))))
	}
	spec.RollbackOnFailure = rollbackOnFail

	// A synthetic manifest carries only what the engine needs to reach the host;
	// everything about the release itself comes from the bundle.
	e := engine.New(&manifest.Manifest{
		Project: spec.Project,
		Host:    host,
		Network: spec.Network,
		Retain:  spec.Retain,
	})
	if err := e.Connect(ctx); err != nil {
		return err
	}
	defer e.Close()

	// The bundle's layouts stand in for a local build, and its artifacts for the
	// manifest's source paths.
	built := map[string]*build.Result{}
	for name, dir := range b.ImageDirs {
		if ref, ok := spec.Images[name]; ok && !ref.External {
			built[name] = &build.Result{Name: name, Dir: dir, Digest: ref.Digest, Bytes: dirBytes(dir)}
		}
	}
	e.SetArtifactSources(b.ArtifactPaths)

	// The destinations come from the spec rather than a manifest, but they still
	// have to exist and be writable before hundreds of megabytes move.
	dests := make([]string, 0, len(spec.Artifacts))
	for _, a := range spec.Artifacts {
		dests = append(dests, a.Dest)
	}
	if err := e.PreflightArtifactDests(ctx, dests); err != nil {
		return err
	}

	// The store path is a host path baked at build time by a machine that may
	// never have seen this host; re-resolve it here.
	spec.StorePath = e.StorePath()

	state, err := e.State(ctx)
	if err != nil {
		return err
	}
	if state.Ledger != nil && state.Ledger.Find(spec.ID) != nil {
		return fmt.Errorf("release %s has already been applied to %s", spec.ID, host)
	}

	ok, err := confirmed(yes, fmt.Sprintf("  apply %s to %s?", spec.ID, host))
	if err != nil || !ok {
		return err
	}

	lock, err := e.AcquireLock(ctx, func(msg string) {
		fmt.Fprintf(os.Stderr, "  %s\n", s.Dim(msg))
	})
	if err != nil {
		return err
	}
	defer lock.Release()
	spec.ExpectedCurrent = state.ExpectedCurrent()

	fmt.Fprintf(os.Stderr, "\n%s\n", s.Bold("shipping"))
	stats, err := e.Push(ctx, built, c.verbose)
	if err != nil {
		return err
	}
	var sent, total int64
	for _, st := range stats {
		sent += st.Sent
		total += st.Total
	}
	fmt.Fprintf(os.Stderr, "  %s %s on the wire (image total %s)\n", s.Tick(), ui.Bytes(sent), ui.Bytes(total))

	if len(spec.Artifacts) > 0 {
		if _, err := e.PushArtifacts(ctx, spec, c.verbose); err != nil {
			return err
		}
	}

	fmt.Fprintf(os.Stderr, "\n%s\n", s.Bold("applying"))
	return run(func(r engine.EventRenderer) error {
		return e.Apply(ctx, spec, r)
	}, c.renderer(), "apply")
}

func sortedNames(built map[string]*build.Result) []string {
	out := make([]string, 0, len(built))
	for k := range built {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func dirBytes(dir string) int64 {
	var n int64
	filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if fi, err := d.Info(); err == nil {
			n += fi.Size()
		}
		return nil
	})
	return n
}
