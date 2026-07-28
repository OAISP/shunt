// Artifacts are the large files a release ships alongside its image — a
// database, model weights, a prebuilt index. They travel over the same rsync
// transport and are swapped in on the host.
package engine

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/OAISP/shunt/internal/release"
	"github.com/OAISP/shunt/internal/sshx"
	"github.com/OAISP/shunt/internal/transport"
)

// SetArtifactSources points artifact transfers at explicit local paths instead
// of the manifest's src fields.
func (e *Engine) SetArtifactSources(paths map[string]string) { e.artifactSrc = paths }

// StagedPath is where an artifact lands before being swapped into place: beside
// the destination so the final rename is atomic, and scoped by release id so two
// deploys cannot land on each other's fragment.
//
// The suffix costs nothing at transfer time — rsync's --fuzzy still finds the
// current copy next door as its delta basis.
func StagedPath(dest, releaseID string) string { return dest + ".new." + releaseID }

// artifacts resolves the manifest's artifact list, dropping any whose local file
// is missing unless it is marked required.
func (e *Engine) artifacts(releaseID string) ([]release.Artifact, error) {
	out := make([]release.Artifact, 0, len(e.M.Artifacts))
	for _, a := range e.M.Artifacts {
		src := e.M.Abs(a.Src)
		fi, err := os.Stat(src)
		if err != nil {
			if a.Required {
				return nil, fmt.Errorf("artifact %q: %s is required but missing", a.Name, src)
			}
			// Not an error: the host keeps whatever it already has, which is the
			// right default for a file produced by an occasional ETL run.
			fmt.Fprintf(os.Stderr, "warning: artifact %q: %s not found, the host keeps its current copy\n", a.Name, src)
			continue
		}
		size, mtime := fi.Size(), fi.ModTime().Unix()
		if fi.IsDir() {
			// A tree has no single size or mtime, so summarise it: the plan needs
			// something to compare, and the helper needs something to validate
			// the staged copy against.
			size, _, mtime = release.TreeSummary(src)
			if a.Magic != "" {
				return nil, fmt.Errorf("artifact %q: magic cannot apply to a directory", a.Name)
			}
		}
		out = append(out, release.Artifact{
			Name:   a.Name,
			Dest:   a.Dest,
			Staged: StagedPath(a.Dest, releaseID),
			Magic:  a.Magic,
			Retain: a.Retain,
			Bytes:  size,
			MTime:  mtime,
			Dir:    fi.IsDir(),
		})
	}
	return out, nil
}

// linkDestFor names the live tree to reuse unchanged files from, or "" when the
// host has nothing there yet.
func linkDestFor(a release.Artifact, present map[string]transport.FileStat) string {
	if !a.Dir {
		return "" // single files use --fuzzy instead
	}
	if st, ok := present[a.Dest]; ok && st.Size >= 0 {
		return a.Dest
	}
	return ""
}

// LocalArtifactPath is the source file for a named artifact.
func (e *Engine) LocalArtifactPath(name string) string {
	if p, ok := e.artifactSrc[name]; ok {
		return p
	}
	for _, a := range e.M.Artifacts {
		if a.Name == name {
			return e.M.Abs(a.Src)
		}
	}
	return ""
}

// PushArtifacts transfers each artifact to its staged path on the host.
func (e *Engine) PushArtifacts(ctx context.Context, spec *release.Spec, verbose bool) (map[string]*transport.Stats, error) {
	stats := map[string]*transport.Stats{}

	// Which destinations already exist, in one round trip. A directory transfer
	// reuses the live tree via --link-dest, and pointing that at a path the host
	// does not have yet makes rsync print a warning on a first deploy — accurate
	// but alarming, and avoidable now that stats come back batched.
	dests := make([]string, 0, len(spec.Artifacts))
	for _, a := range spec.Artifacts {
		dests = append(dests, a.Dest)
	}
	present := transport.RemoteFileStats(ctx, e.Client, dests)

	for _, a := range spec.Artifacts {
		st, err := transport.PushFile(ctx, transport.FileOptions{
			Client:     e.Client,
			LocalPath:  e.LocalArtifactPath(a.Name),
			RemotePath: a.Staged,
			Verbose:    verbose,
			RemoteZstd: e.facts.RsyncZstd,
			// The live tree is what an unchanged file should be linked from
			// rather than re-sent; ignored for single-file artifacts, and
			// omitted entirely until the host actually has a tree to link from.
			LinkDest: linkDestFor(a, present),
		})
		if err != nil {
			return nil, err
		}
		stats[a.Name] = st
	}
	return stats, nil
}

// PreflightArtifacts checks the host can actually receive every artifact, before
// anything expensive happens.
//
// Building and shipping a 400 MB image only to fail on mkdir wastes minutes and
// tells the operator nothing useful, so every destination directory is probed in
// a single round trip and each failure names its own fix.
func (e *Engine) PreflightArtifacts(ctx context.Context) error {
	dests := make([]string, 0, len(e.M.Artifacts))
	for _, a := range e.M.Artifacts {
		dests = append(dests, a.Dest)
	}
	return e.PreflightArtifactDests(ctx, dests)
}

// PreflightArtifactDests is the same check driven by explicit destinations,
// which is what a bundle has: it carries a release, not a manifest, so the
// paths come from the spec. Skipping this was why applying a bundle with an
// artifact failed at rsync with a bare "No such file or directory".
func (e *Engine) PreflightArtifactDests(ctx context.Context, dests []string) error {
	if len(dests) == 0 {
		return nil
	}
	dirs := map[string]bool{}
	for _, d := range dests {
		if d != "" {
			dirs[filepath.Dir(d)] = true
		}
	}
	var script strings.Builder
	for _, d := range slices.Sorted(maps.Keys(dirs)) {
		fmt.Fprintf(&script, "if ! mkdir -p %s 2>/dev/null; then echo \"NODIR %s\"; elif [ ! -w %s ]; then echo \"NOWRITE %s\"; fi\n",
			sshx.Quote(d), d, sshx.Quote(d), d)
	}
	out, err := e.Client.Run(ctx, "sh", "-c", script.String())
	if err != nil {
		return err
	}

	var problems []string
	for _, ln := range strings.Split(strings.TrimSpace(out), "\n") {
		if ln == "" {
			continue
		}
		kind, dir, _ := strings.Cut(ln, " ")
		switch kind {
		case "NODIR":
			problems = append(problems, fmt.Sprintf("  %s could not be created", dir))
		case "NOWRITE":
			problems = append(problems, fmt.Sprintf("  %s is not writable", dir))
		}
	}
	if len(problems) == 0 {
		return nil
	}

	user := e.M.Host
	if u, _, ok := strings.Cut(e.M.Host, "@"); ok {
		user = u
	}
	return fmt.Errorf("artifact destinations are not usable on %s:\n%s\n\n"+
		"  This is a one-time setup step — /opt is root-owned by default. On the host run:\n"+
		"    sudo mkdir -p <dir> && sudo chown -R %s <dir>\n"+
		"  Or point dest= somewhere you already own, e.g. $HOME.",
		e.M.Host, strings.Join(problems, "\n"), user)
}
