package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/OAISP/shunt/internal/engine"
	"github.com/OAISP/shunt/internal/ui"
)

// cmdFetch copies an artifact or a stage capture back down from the host.
//
// rsync does not care which end is the source, so the pull direction costs
// almost nothing on top of the push. Nothing is overwritten without saying so:
// the obvious mistake is pulling production data over the local copy you were
// about to deploy.
func cmdFetch(ctx context.Context, args []string) error {
	var c commonFlags
	var out string
	var yes bool
	fs := newFlagSet("fetch", &c)
	fs.StringVar(&out, "o", "", "write to this path instead of the artifact's source")
	fs.StringVar(&out, "output", "", "write to this path instead of the artifact's source")
	fs.BoolVar(&yes, "y", false, "skip the confirmation prompt")
	fs.BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	name := fs.Arg(0)

	e, err := connect(ctx, c.file, c.target)
	if err != nil {
		return err
	}
	defer e.Close()

	if name == "" {
		return listFetchable(ctx, e, c.out())
	}

	// An artifact is addressed by name; anything else is treated as a path on
	// the host, which is what makes captures reachable without a second command.
	remote, isDir, local := "", false, out
	for _, a := range e.M.Artifacts {
		if a.Name != name {
			continue
		}
		remote = a.Dest
		if local == "" {
			local = e.M.Abs(a.Src)
		}
		if fi, err := os.Stat(e.M.Abs(a.Src)); err == nil {
			isDir = fi.IsDir()
		}
		break
	}
	if remote == "" {
		if !strings.HasPrefix(name, "/") {
			return fmt.Errorf("no artifact named %q in shunt.toml; pass an absolute host path to fetch something else", name)
		}
		remote = name
		if local == "" {
			local = filepath.Base(name)
		}
	}

	// Ask the host what it actually is, rather than assuming from the local copy.
	hostIsDir, size, err := e.RemoteKind(ctx, remote)
	if err != nil {
		return err
	}
	isDir = hostIsDir

	s := c.out()
	fmt.Printf("\n  fetch %s\n", s.Bold(remote))
	fmt.Printf("    from %s (%s)\n", e.M.Host, ui.Bytes(size))
	fmt.Printf("    to   %s\n", local)
	if _, err := os.Stat(local); err == nil {
		fmt.Printf("  %s\n", s.Amber("this overwrites the local copy"))
	}
	ok, err := confirmed(yes, "  proceed?")
	if err != nil || !ok {
		return err
	}

	if dir := filepath.Dir(local); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	st, err := e.Fetch(ctx, remote, local, isDir, c.verbose)
	if err != nil {
		return err
	}
	fmt.Printf("\n  %s %s received of %s %s\n\n", s.Tick(),
		ui.Bytes(st.Moved(true)), ui.Bytes(st.Total),
		s.Dim(fmt.Sprintf("(%.1f%% already local)", st.DedupPercent())))
	return nil
}

// listFetchable shows what a bare `shunt fetch` could retrieve, rather than
// making the operator guess at names.
func listFetchable(ctx context.Context, e *engine.Engine, s ui.Style) error {
	if len(e.M.Artifacts) == 0 {
		fmt.Printf("\n  %s\n", s.Dim("no artifacts declared in shunt.toml"))
	} else {
		fmt.Printf("\n  %s\n", s.Bold("artifacts"))
		for _, a := range e.M.Artifacts {
			isDir, size, err := e.RemoteKind(ctx, a.Dest)
			switch {
			case err != nil:
				fmt.Printf("    %-14s %s\n", a.Name, s.Dim("not on the host"))
			default:
				fmt.Printf("    %-14s %s  %s\n", a.Name, ui.Bytes(size),
					s.Dim(a.Dest+kindSuffix(isDir)))
			}
		}
	}

	// Captures are the other thing worth pulling down — a pre-migration dump is
	// only useful if you can get at it.
	caps, err := e.Captures(ctx)
	if err == nil && len(caps) > 0 {
		fmt.Printf("\n  %s %s\n", s.Bold("captures"), s.Dim("(fetch by absolute path)"))
		for _, c := range caps {
			fmt.Printf("    %s\n", c)
		}
	}
	fmt.Printf("\n  %s\n\n", s.Dim("shunt fetch <name>  ·  shunt fetch /absolute/host/path"))
	return nil
}

func kindSuffix(isDir bool) string {
	if isDir {
		return "/ (directory)"
	}
	return ""
}
