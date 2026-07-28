package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/OAISP/shunt/internal/bundle"
	"github.com/OAISP/shunt/internal/release"
	"github.com/OAISP/shunt/internal/ui"
)

// cmdBundleInspect prints what a bundle contains without applying it.
//
// This is the command someone handed a .shuntpkg through an approval queue
// actually needs: which release, built from which commit by whom, for which
// host, running what, and which secrets it will want. It reads only the
// description — bundle.json is the first entry in the archive — so it answers
// instantly regardless of how large the payload is.
func cmdBundleInspect(_ context.Context, args []string) error {
	var c commonFlags
	fs := newFlagSet("bundle inspect", &c)
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	path := fs.Arg(0)
	if path == "" {
		return fmt.Errorf("usage: shunt bundle inspect <bundle>")
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	meta, err := bundle.ReadMeta(f)
	if err != nil {
		return err
	}
	if c.asJSON {
		return json.NewEncoder(os.Stdout).Encode(meta)
	}

	s := c.out()
	spec := meta.Spec
	fi, _ := os.Stat(path)

	fmt.Printf("\n%s %s  %s\n", s.Bold(filepath.Base(path)), s.Dim("·"), s.Dim(ui.Bytes(fi.Size())))
	fmt.Printf("\n  %s %s\n", s.Dim("release "), s.Bold(spec.ID))
	fmt.Printf("  %s %s\n", s.Dim("built   "), meta.Created)
	fmt.Printf("  %s %s\n", s.Dim("for     "), meta.Host)
	if prov := spec.Provenance.Describe(); prov != "" {
		fmt.Printf("  %s %s\n", s.Dim("from    "), prov)
	}
	if spec.Provenance.Dirty {
		// A dirty tree means the image matches no commit, which is exactly what
		// an approver needs to know and exactly what the commit id hides.
		fmt.Printf("  %s\n", s.Amber("built from a modified working tree — the commit above does not describe this image"))
	}

	fmt.Printf("\n  %s\n", s.Bold("images"))
	for _, name := range slices.Sorted(maps.Keys(spec.Images)) {
		img := spec.Images[name]
		if img.External {
			fmt.Printf("    %-14s %s\n", name, s.Dim("pulled on the host · "+img.Ref))
			continue
		}
		fmt.Printf("    %-14s %s\n", name, s.Dim(ui.ShortDigest(img.Digest)))
	}

	if len(spec.Accessories) > 0 {
		fmt.Printf("\n  %s %s\n", s.Bold("accessories"), s.Dim("(created only if absent)"))
		for _, name := range slices.Sorted(maps.Keys(spec.Accessories)) {
			fmt.Printf("    %-14s %s\n", name, s.Dim(spec.Accessories[name].Image))
		}
	}

	if len(spec.Stages) > 0 {
		names := make([]string, 0, len(spec.Stages))
		for _, st := range spec.Stages {
			names = append(names, st.Name)
		}
		fmt.Printf("\n  %s %s\n", s.Bold("stages"), s.Dim("(run before any service is replaced)"))
		fmt.Printf("    %s\n", strings.Join(names, " → "))
	}

	if len(spec.Artifacts) > 0 {
		fmt.Printf("\n  %s\n", s.Bold("artifacts"))
		for _, a := range spec.Artifacts {
			kind := ""
			if a.Dir {
				kind = " (directory)"
			}
			fmt.Printf("    %-14s %s\n", a.Name, s.Dim(ui.Bytes(a.Bytes)+" → "+a.Dest+kind))
		}
	}

	fmt.Printf("\n  %s\n", s.Bold("services"))
	for _, name := range slices.Sorted(maps.Keys(spec.Services)) {
		svc := spec.Services[name]
		fmt.Printf("    %-14s %s\n", name, s.Dim(serviceSummary(svc)))
	}

	fmt.Printf("\n  %s\n", s.Bold("secrets"))
	switch {
	case len(meta.SecretKeys) == 0:
		fmt.Printf("    %s\n", s.Dim("none"))
	default:
		// Names only — the values are not in the file, by design.
		fmt.Printf("    %s\n", s.Dim(fmt.Sprintf("%d required, resolved where this is applied:", len(meta.SecretKeys))))
		for _, k := range meta.SecretKeys {
			fmt.Printf("      %s\n", k)
		}
		if meta.Secrets != nil {
			fmt.Printf("    %s\n", s.Dim("provider: "+meta.Secrets.Provider+" "+meta.Secrets.Path))
		}
	}

	fmt.Printf("\n  %s\n\n", s.Dim("shunt bundle verify "+filepath.Base(path)+"  ·  shunt apply --plan "+filepath.Base(path)))
	return nil
}

// serviceSummary describes a service in one line: what it runs and how it is
// reached.
func serviceSummary(svc release.Service) string {
	var parts []string
	parts = append(parts, "image "+svc.Image)
	if len(svc.Publish) > 0 {
		parts = append(parts, "publish "+strings.Join(svc.Publish, ","))
	}
	if svc.Proxy != nil {
		parts = append(parts, "proxy "+svc.Proxy.Kind+" "+svc.Proxy.Host)
	} else if svc.Expose > 0 {
		parts = append(parts, fmt.Sprintf("expose %d", svc.Expose))
	}
	switch {
	case svc.Health == nil:
		parts = append(parts, "no health check")
	case svc.Health.URL != "":
		parts = append(parts, "health "+svc.Health.URL)
	default:
		parts = append(parts, "health command")
	}
	return strings.Join(parts, " · ")
}

// cmdBundleVerify rehashes every blob in a bundle against its own filename.
//
// The same check the host performs on load, run without a host — so a bundle
// can be proven intact before it is carried somewhere that retrying is
// expensive, or after it comes off a stick.
func cmdBundleVerify(_ context.Context, args []string) error {
	var c commonFlags
	fs := newFlagSet("bundle verify", &c)
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	path := fs.Arg(0)
	if path == "" {
		return fmt.Errorf("usage: shunt bundle verify <bundle>")
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	work, err := os.MkdirTemp("", "shunt-verify-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)

	ex, rep, err := bundle.Verify(f, work)
	if err != nil {
		return err
	}

	s := c.out()
	if c.asJSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{
			"valid": true, "release": ex.Meta.Spec.ID,
			"blobs": rep.Blobs, "bytes": rep.Bytes,
			"images": rep.Images, "artifacts": rep.Artifacts,
		})
	}
	fmt.Printf("\n  %s %s\n", s.Tick(), s.Bold(filepath.Base(path)))
	fmt.Printf("    %s\n", s.Dim(fmt.Sprintf("release %s · %d blob(s), %s, all hashing to their names",
		ex.Meta.Spec.ID, rep.Blobs, ui.Bytes(rep.Bytes))))
	if len(rep.Artifacts) > 0 {
		fmt.Printf("    %s\n", s.Dim(fmt.Sprintf("artifacts present: %s", strings.Join(rep.Artifacts, ", "))))
	}
	// Worth stating plainly, because "verified" invites a stronger reading than
	// this check supports.
	fmt.Printf("\n  %s\n\n", s.Dim("this proves the bytes are intact, not that the release works"))
	return nil
}
