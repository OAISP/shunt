package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/OAISP/shunt/internal/release"
	"github.com/OAISP/shunt/internal/ui"
)

// swapArtifacts promotes every staged file onto its destination.
//
// This runs after stages and immediately before services are replaced, which is
// as late as the swap can be: it is the point of no return for data.
//
// The two phases are the point. Every artifact is validated before *any* is
// promoted, so a manifest shipping three files cannot leave one swapped and two
// stale — and if a promotion still fails partway, the ones already promoted are
// put back. Without that, the surrounding error saying "no services were
// replaced — production is untouched" would be a lie about the data.
func swapArtifacts(spec *release.Spec) error {
	if len(spec.Artifacts) == 0 {
		return nil
	}

	for _, a := range spec.Artifacts {
		step("artifact:"+a.Name, "checking "+filepath.Base(a.Staged))
		if err := validateStaged(a); err != nil {
			return fmt.Errorf("artifact %q: %w", a.Name, err)
		}
	}

	var done []promotion
	for _, a := range spec.Artifacts {
		step("artifact:"+a.Name, "swapping in "+a.Dest)
		p, err := promote(spec, a)
		if err != nil {
			restored, failed := rollbackPromotions(done)
			msg := fmt.Errorf("artifact %q: %w", a.Name, err)
			if restored > 0 {
				msg = fmt.Errorf("%w\n  %d earlier artifact(s) were restored to their previous contents", msg, restored)
			}
			if len(failed) > 0 {
				msg = fmt.Errorf("%w\n  COULD NOT restore: %s — recover these by hand before restarting the app",
					msg, strings.Join(failed, ", "))
			}
			return msg
		}
		done = append(done, p)
		ok("artifact:"+a.Name, fmt.Sprintf("%s in place (%s)", a.Dest, ui.Bytes(p.bytes)))
	}

	// Retention and stale-fragment cleanup run only once everything has landed —
	// until then the superseded copies are the restore path.
	for _, a := range spec.Artifacts {
		prunePrevious(a.Dest, a.Retain)
		clearStaleStaged(a)
	}
	return nil
}

// validateStaged checks a transferred file is exactly what the release said it
// would be, before anything is overwritten.
func validateStaged(a release.Artifact) error {
	fi, err := os.Stat(a.Staged)
	if err != nil {
		return fmt.Errorf("staged file %s is missing — the transfer did not complete", a.Staged)
	}
	if fi.IsDir() {
		return fmt.Errorf("staged path %s is a directory", a.Staged)
	}
	// Exact size, not merely non-empty. rsync --partial deliberately leaves a
	// truncated file behind when a large transfer is interrupted, and a 400 MB
	// database that arrived 90% complete is both non-empty and useless. The
	// release already carries the byte count, so there is no reason to guess.
	if a.Bytes > 0 && fi.Size() != a.Bytes {
		// Kept, not deleted: --partial exists so the next run resumes instead of
		// re-sending hundreds of megabytes. Deleting it would throw that away.
		return fmt.Errorf("staged file %s is %s, expected %s — the transfer was interrupted; "+
			"rerun the deploy and rsync will resume it",
			a.Staged, ui.Bytes(fi.Size()), ui.Bytes(a.Bytes))
	}
	if fi.Size() == 0 {
		os.Remove(a.Staged)
		return fmt.Errorf("staged file %s is empty — refusing to swap it in", a.Staged)
	}
	if err := checkMagic(a.Staged, a.Magic); err != nil {
		// A magic mismatch is the wrong file, not a partial one. Leaving it would
		// give the next run's --fuzzy a bogus basis to diff against.
		os.Remove(a.Staged)
		return err
	}
	return nil
}

// promotion records what one swap did, so it can be undone if a later one fails.
type promotion struct {
	dest  string
	prev  string // previous contents, hard-linked aside; empty if there were none
	name  string
	bytes int64
}

// promote swaps one staged file onto its destination.
//
// The backup is a hard link rather than a rename, so the destination is never
// absent: renaming it aside first leaves a window in which a failure means the
// app has no file at all. Linking and then renaming over the top means the
// destination only ever holds either the old contents or the new ones.
//
// This does not by itself protect a running reader. A process that already has
// the file open keeps the old inode and is unaffected until it reopens; one that
// opens the path per request sees the new contents immediately.
func promote(spec *release.Spec, a release.Artifact) (promotion, error) {
	p := promotion{dest: a.Dest, name: a.Name}
	fi, err := os.Stat(a.Staged)
	if err != nil {
		return p, err
	}
	p.bytes = fi.Size()

	if _, err := os.Stat(a.Dest); err == nil {
		prev := previousPath(a.Dest, spec.ID)
		os.Remove(prev) // a retried release must not trip over its own backup
		if err := os.Link(a.Dest, prev); err != nil {
			// Filesystems without hard links are rare on a Linux host but not
			// impossible. Fall back to the older rename-aside, and say so rather
			// than quietly offering a weaker guarantee than the docs promise.
			info(fmt.Sprintf("%s: no hard links on this filesystem, backing up by rename (brief window with no file at %s)",
				a.Name, a.Dest))
			if err := os.Rename(a.Dest, prev); err != nil {
				return p, fmt.Errorf("keeping the previous copy: %w", err)
			}
		}
		p.prev = prev
	}

	if err := os.Rename(a.Staged, a.Dest); err != nil {
		return p, fmt.Errorf("promoting %s: %w", a.Staged, err)
	}
	if p.prev != "" {
		info(fmt.Sprintf("previous %s kept as %s", filepath.Base(a.Dest), filepath.Base(p.prev)))
	}
	return p, nil
}

// rollbackPromotions undoes swaps that already succeeded, newest first. It
// reports how many were restored and names any that could not be, because that
// is the one case where an operator has to intervene by hand.
func rollbackPromotions(done []promotion) (restored int, failed []string) {
	for i := len(done) - 1; i >= 0; i-- {
		p := done[i]
		if p.prev == "" {
			// Nothing was there before this release, so restoring means removing.
			if err := os.Remove(p.dest); err != nil && !os.IsNotExist(err) {
				failed = append(failed, p.name)
				continue
			}
			restored++
			continue
		}
		if err := os.Rename(p.prev, p.dest); err != nil {
			failed = append(failed, p.name)
			continue
		}
		restored++
	}
	return restored, failed
}

// clearStaleStaged drops staging fragments left by other releases. They are dead
// by definition once this release has promoted its own copy, and leaving them
// means a directory that quietly accumulates half-transferred databases.
func clearStaleStaged(a release.Artifact) {
	matches, err := filepath.Glob(a.Dest + ".new.*")
	if err != nil {
		return
	}
	for _, m := range matches {
		if m != a.Staged {
			os.Remove(m)
		}
	}
	// A fragment from a release that predates release-scoped staging.
	os.Remove(a.Dest + ".new")
}

// checkMagic verifies the file begins with the expected literal.
//
// Every SQLite database starts with "SQLite format 3"; a truncated or
// half-written upload almost never does, and promoting one would take the site
// down with a database the app cannot open. Cheap check, catastrophic to miss.
func checkMagic(path, magic string) error {
	if magic == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	buf := make([]byte, len(magic))
	n, _ := f.Read(buf)
	if got := string(buf[:n]); got != magic {
		return fmt.Errorf("%s does not start with %q — refusing to swap in a file that is not what it claims to be", path, magic)
	}
	return nil
}

// previousPath names a superseded copy after the release that replaced it, so
// the operator can see at a glance which generation they are restoring.
func previousPath(dest, releaseID string) string {
	return dest + ".prev." + releaseID
}

// prunePrevious keeps the newest retain generations. Release ids sort lexically
// by time, so ordering the names orders the generations.
func prunePrevious(dest string, retain int) {
	if retain < 0 {
		retain = 0
	}
	matches, err := filepath.Glob(dest + ".prev.*")
	if err != nil || len(matches) <= retain {
		return
	}
	slices.Sort(matches)
	slices.Reverse(matches)
	for _, old := range matches[retain:] {
		os.Remove(old)
	}
}

// artifactRecovery describes, in copy-pasteable form, how to put the previous
// copies back. It is appended to a failed health check because that is exactly
// when an operator needs it and least wants to work it out.
func artifactRecovery(spec *release.Spec) string {
	if len(spec.Artifacts) == 0 {
		return ""
	}
	var b strings.Builder
	for _, a := range spec.Artifacts {
		prev := previousPath(a.Dest, spec.ID)
		if _, err := os.Stat(prev); err != nil {
			continue
		}
		fmt.Fprintf(&b, "    mv %s %s\n", prev, a.Dest)
	}
	if b.Len() == 0 {
		return ""
	}
	return "\n  to restore the previous data:\n" + b.String()
}
