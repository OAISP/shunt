package main

import (
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/OAISP/shunt/internal/release"
	"github.com/OAISP/shunt/internal/ui"
)

// stageOutput is where a stage finds the file it can report back through, the
// way a CI step does with GITHUB_OUTPUT.
const stageOutput = "/run/shunt/output"

// runStages runs the pipeline in order and names the last stage that reported
// an irreversible change, which is what tells a failed deploy not to put the
// old code back on top of it.
func runStages(spec *release.Spec, envFile string) (irreversible string, err error) {
	for _, st := range spec.Stages {
		step("stage:"+st.Name, "running stage "+st.Name)
		report, err := os.CreateTemp(projectDir(spec.Project), "stage-output-")
		if err != nil {
			return irreversible, err
		}
		report.Close()
		// Written by whatever user the image runs as, not by the deploy user.
		os.Chmod(report.Name(), 0o666)
		runErr := runStage(spec, st, envFile, report.Name())
		said, _ := os.ReadFile(report.Name())
		os.Remove(report.Name())
		// Read even when the stage failed: a migration that died halfway has
		// still changed the schema.
		if slices.Contains(splitLines(string(said)), "irreversible=true") {
			irreversible = st.Name
			info("stage " + st.Name + " reported an irreversible change — this release will not be rolled back automatically")
		}
		if runErr != nil {
			return irreversible, fmt.Errorf("stage %q failed: %w", st.Name, runErr)
		}
		ok("stage:"+st.Name, "stage "+st.Name+" complete")
	}
	return irreversible, nil
}

func runStage(spec *release.Spec, st release.Stage, envFile, report string) error {
	ref, err := imageRef(spec, st.Image)
	if err != nil {
		return err
	}
	// Guarded the same way startContainer guards it: `--network ""` is not "no
	// network" to docker, it is an error naming the empty string.
	args := []string{"run", "--rm", "-v", report + ":" + stageOutput, "-e", "SHUNT_STAGE_OUTPUT=" + stageOutput}
	if spec.Network != "" {
		args = append(args, "--network", spec.Network)
	}
	if spec.SecretsAsFiles() {
		if len(spec.Secrets) > 0 {
			dir, err := writeSecretFiles(spec, nil)
			if err != nil {
				return err
			}
			args = append(args, "-v", dir+":"+release.SecretMountPath+":ro")
		}
	} else if envFile != "" {
		args = append(args, "--env-file", envFile)
	}
	for _, k := range slices.Sorted(maps.Keys(st.Env)) {
		args = append(args, "-e", k+"="+st.Env[k])
	}
	args = append(args, ref)
	args = append(args, st.Command...)

	cmd := exec.Command("docker", args...)

	if st.Capture == "" {
		out, err := cmd.CombinedOutput()
		for _, ln := range splitLines(string(out)) {
			logline(ln)
		}
		return err
	}

	// Captured stages stream stdout to a timestamped file on the host. The
	// canonical use is a pre-migration pg_dump, which is why an empty result can
	// be treated as fatal.
	path := expandCapture(st.Capture, spec.ID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// Written beside the path and renamed over it only on success, so a failed
	// run never costs a fixed-path capture its last good copy.
	//
	// 0600, not os.Create's 0644. The canonical capture is a pg_dump: every row
	// of production, sitting world-readable on a host that by definition has
	// other people's containers on it.
	partial := path + ".partial"
	f, err := os.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer os.Remove(partial)
	cmd.Stdout = f
	var errBuf strings.Builder
	cmd.Stderr = &errBuf
	runErr := cmd.Run()
	f.Close()

	for _, ln := range splitLines(errBuf.String()) {
		logline(ln)
	}
	if runErr != nil {
		return runErr
	}
	if st.RequireNonEmpty {
		fi, err := os.Stat(partial)
		if err != nil {
			return err
		}
		if fi.Size() == 0 {
			return fmt.Errorf("capture %s is empty — refusing to continue", path)
		}
		info(fmt.Sprintf("captured %s (%s)", path, ui.Bytes(fi.Size())))
	}
	if err := os.Rename(partial, path); err != nil {
		return err
	}
	pruneCaptures(st.Capture, st.Retain)
	return nil
}

// generationRE matches what a capture placeholder expands to: a timestamp, or a
// release id, which is a timestamp with a suffix.
var generationRE = regexp.MustCompile(`^\d{8}-\d{6}(-[0-9a-f]+)?$`)

// pruneCaptures keeps the newest `retain` generations of one stage's capture.
//
// It takes the *template*, not the rendered path: the rendered name contains the
// release id, so a prefix cut from it would match only the file just written
// and nothing would ever be pruned.
func pruneCaptures(template string, retain int) {
	if retain <= 0 {
		return
	}
	dir := filepath.Dir(template)
	prefix, suffix := capturePattern(filepath.Base(template))
	if prefix == "" && suffix == "" {
		return // no placeholder: every release overwrites one fixed path
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	// Matched on the generation itself, not only on what surrounds it: a template
	// such as `{{.Release}}.sql` must not claim every .sql file in the directory.
	var matches []string
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || len(n) <= len(prefix)+len(suffix) || !strings.HasPrefix(n, prefix) || !strings.HasSuffix(n, suffix) {
			continue
		}
		if generationRE.MatchString(n[len(prefix) : len(n)-len(suffix)]) {
			matches = append(matches, n)
		}
	}
	// Release ids and timestamps both sort lexically by time, so sorting the
	// names orders the generations.
	sort.Sort(sort.Reverse(sort.StringSlice(matches)))
	for i := retain; i < len(matches); i++ {
		os.Remove(filepath.Join(dir, matches[i]))
	}
}

// capturePattern splits a capture template around its placeholder, giving the
// literal prefix and suffix every generation of that stage shares.
func capturePattern(base string) (prefix, suffix string) {
	for _, ph := range []string{"{{.Release}}", "{{.Timestamp}}"} {
		if i := strings.Index(base, ph); i >= 0 {
			return base[:i], base[i+len(ph):]
		}
	}
	return "", ""
}

func expandCapture(pattern, releaseID string) string {
	r := strings.NewReplacer(
		"{{.Release}}", releaseID,
		"{{.Timestamp}}", time.Now().UTC().Format("20060102-150405"),
	)
	return r.Replace(pattern)
}
