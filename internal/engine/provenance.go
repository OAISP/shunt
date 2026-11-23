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
	fromCI(&p, os.Getenv)
	return p
}

// fromCI fills in what git and the OS cannot answer on a CI runner. GitLab
// checks out a detached HEAD, so there is no branch; the OS user is the
// runner's service account rather than whoever pushed; and the run itself is
// the most useful link a release can carry.
func fromCI(p *release.Provenance, env func(string) string) {
	var actor, system, branch, sha string
	switch {
	case env("GITHUB_ACTIONS") == "true":
		system, actor, branch, sha = "github-actions", env("GITHUB_ACTOR"), env("GITHUB_REF_NAME"), env("GITHUB_SHA")
		if id := env("GITHUB_RUN_ID"); id != "" {
			p.Run = env("GITHUB_SERVER_URL") + "/" + env("GITHUB_REPOSITORY") + "/actions/runs/" + id
		}
	case env("GITLAB_CI") == "true":
		system, actor, branch, sha = "gitlab-ci", env("GITLAB_USER_LOGIN"), env("CI_COMMIT_REF_NAME"), env("CI_COMMIT_SHA")
		p.Run = env("CI_JOB_URL")
	default:
		return
	}
	if actor != "" {
		p.Deployer = actor + "@" + system
	}
	if p.Branch == "" {
		p.Branch = branch
	}
	if p.Commit == "" && sha != "" {
		p.Commit, p.Short = sha, shortSHA(sha)
	}
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
