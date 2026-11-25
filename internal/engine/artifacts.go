// Artifacts are the large files a release ships alongside its image — a
// database, model weights, a prebuilt index. They travel over the same rsync
// transport and are swapped in on the host.
package engine

import (
	"context"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/OAISP/shunt/internal/release"
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
func linkDestFor(a release.Artifact, present map[string]release.PathState) string {
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

// PushArtifacts transfers each artifact to its staged path on the host. present
// is what PlanState found at each destination.
func (e *Engine) PushArtifacts(ctx context.Context, spec *release.Spec, present map[string]release.PathState, verbose bool) (map[string]*transport.Stats, error) {
	stats := map[string]*transport.Stats{}
	for _, a := range spec.Artifacts {
		// The helper leaves a destination that already matches alone, so there
		// is nothing to stage for it.
		if st, ok := present[a.Dest]; ok && st.Size == a.Bytes && st.MTime == a.MTime {
			continue
		}
		st, err := transport.PushFile(ctx, transport.FileOptions{
			Client:     e.Client,
			LocalPath:  e.LocalArtifactPath(a.Name),
			RemotePath: a.Staged,
			Verbose:    verbose,
			RemoteZstd: e.facts.RsyncZstd,
			// The live tree is what an unchanged file should be linked from
			// rather than re-sent; ignored for single-file artifacts, and
			// omitted entirely until the host actually has a tree to link from,
			// since rsync warns about a --link-dest that does not exist.
			LinkDest: linkDestFor(a, present),
		})
		if err != nil {
			return nil, err
		}
		stats[a.Name] = st
	}
	return stats, nil
}

// ArtifactDests lists where a manifest's artifacts land on the host.
func (e *Engine) ArtifactDests() []string {
	dests := make([]string, 0, len(e.M.Artifacts))
	for _, a := range e.M.Artifacts {
		dests = append(dests, a.Dest)
	}
	return dests
}

// unusableDests turns destinations whose directory cannot take a new copy into
// one error naming each, and the fix.
func (e *Engine) unusableDests(paths map[string]release.PathState) error {
	var problems []string
	for _, dest := range slices.Sorted(maps.Keys(paths)) {
		if p := paths[dest].Problem; p != "" {
			problems = append(problems, "  "+p)
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
		e.M.Host, strings.Join(slices.Compact(problems), "\n"), user)
}
