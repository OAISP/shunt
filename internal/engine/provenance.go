package engine

import (
	"os"
	"os/exec"
	"os/user"
	"strings"

	"github.com/OAISP/shunt/internal/release"
)

// Provenance answers "what is actually running, and where did it come from".
//
// A release id is a timestamp, which tells you when but not what. Recording the
// commit, whether the tree was dirty, and who deployed turns `shunt status`
// from a list of times into something you can act on at 3am — without adding a
// database, a service, or a dependency on the CI system that happened to run it.
func collectProvenance(m interface{ Dir() string }, cliVersion string) release.Provenance {
	p := release.Provenance{CLI: cliVersion}

	if u, err := user.Current(); err == nil {
		p.Deployer = u.Username
	}
	if h, err := os.Hostname(); err == nil && p.Deployer != "" {
		p.Deployer += "@" + h
	}

	// Git is best-effort by design: shunt does not require a repository, and a
	// project deployed from a tarball must not fail because git is absent.
	dir := m.Dir()
	if sha := gitOutput(dir, "rev-parse", "HEAD"); sha != "" {
		p.Commit = sha
		p.Short = shortSHA(sha)
	}
	if br := gitOutput(dir, "rev-parse", "--abbrev-ref", "HEAD"); br != "" && br != "HEAD" {
		p.Branch = br
	}
	if p.Commit != "" {
		// A dirty tree means the image does not correspond to any commit, which
		// is exactly the thing you want to know when a release misbehaves and the
		// commit it claims looks innocent.
		p.Dirty = gitOutput(dir, "status", "--porcelain") != ""
	}
	return p
}

func gitOutput(dir string, args ...string) string {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// WithProvenance stamps a spec with where it came from.
func (e *Engine) WithProvenance(spec *release.Spec, cliVersion string) {
	spec.Provenance = collectProvenance(e.M, cliVersion)
}
