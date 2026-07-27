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

// swapArtifacts promotes each staged file onto its destination.
//
// This runs after stages and immediately before services are replaced, which is
// as late as it can be: the swap is the point of no return for data, and the
// rename happens while the old container is still serving. That container holds
// an open handle to the old inode, so its readers are unaffected until the
// restart below picks up the new file — replacing the bytes in place instead
// would hand a running app a torn database.
func swapArtifacts(spec *release.Spec) error {
	for _, a := range spec.Artifacts {
		step("artifact:"+a.Name, "swapping in "+a.Dest)
		if err := swapArtifact(spec, a); err != nil {
			return fmt.Errorf("artifact %q: %w", a.Name, err)
		}
	}
	return nil
}

func swapArtifact(spec *release.Spec, a release.Artifact) error {
	fi, err := os.Stat(a.Staged)
	if err != nil {
		return fmt.Errorf("staged file %s is missing — the transfer did not complete", a.Staged)
	}
	// rsync --partial deliberately leaves a truncated file behind when a large
	// transfer is interrupted. Promoting one over a good copy is unrecoverable
	// without re-uploading everything, so size and shape are both checked.
	if fi.Size() == 0 {
		os.Remove(a.Staged)
		return fmt.Errorf("staged file %s is empty — refusing to swap it in", a.Staged)
	}
	if err := checkMagic(a.Staged, a.Magic); err != nil {
		os.Remove(a.Staged)
		return err
	}

	if _, err := os.Stat(a.Dest); err == nil {
		prev := previousPath(a.Dest, spec.ID)
		if err := os.Rename(a.Dest, prev); err != nil {
			return fmt.Errorf("keeping the previous copy: %w", err)
		}
		info(fmt.Sprintf("previous %s kept as %s", filepath.Base(a.Dest), filepath.Base(prev)))
	}
	if err := os.Rename(a.Staged, a.Dest); err != nil {
		return fmt.Errorf("promoting %s: %w", a.Staged, err)
	}

	prunePrevious(a.Dest, a.Retain)
	// Any other .new beside this destination is stale by definition: this run
	// either replaced it or was never going to. Clearing it means the next
	// deploy cannot inherit a fragment.
	os.Remove(a.Staged)

	ok("artifact:"+a.Name, fmt.Sprintf("%s in place (%s)", a.Dest, ui.Bytes(fi.Size())))
	return nil
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
	b.WriteString("\n  to restore the previous data:\n")
	for _, a := range spec.Artifacts {
		prev := previousPath(a.Dest, spec.ID)
		if _, err := os.Stat(prev); err != nil {
			continue
		}
		fmt.Fprintf(&b, "    mv %s %s\n", prev, a.Dest)
	}
	return b.String()
}
