package main

import (
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/OAISP/shunt/internal/release"

	"github.com/OAISP/shunt/internal/ui"
)

// writeEnvFile materialises the resolved secrets plus nothing else, 0600, owned
// by the deploying user. Containers get it via --env-file, so values never
// appear in argv or in `docker inspect`.
func writeEnvFile(spec *release.Spec) (string, error) {
	p := envFilePath(spec.Project, spec.ID)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	var b strings.Builder
	keys := make([]string, 0, len(spec.Secrets))
	for k := range spec.Secrets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		// docker --env-file takes the raw remainder of the line as the value and
		// does no unquoting, so write values verbatim. A value containing a
		// newline cannot be represented and is rejected rather than truncated.
		if strings.ContainsAny(spec.Secrets[k], "\n\r") {
			return "", fmt.Errorf("secret %s contains a newline, which --env-file cannot represent", k)
		}
		fmt.Fprintf(&b, "%s=%s\n", k, spec.Secrets[k])
	}
	if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
		return "", err
	}
	return p, nil
}

func runStages(spec *release.Spec, envFile string) error {
	for _, st := range spec.Stages {
		step("stage:"+st.Name, "running stage "+st.Name)
		if err := runStage(spec, st, envFile); err != nil {
			return fmt.Errorf("stage %q failed: %w", st.Name, err)
		}
		ok("stage:"+st.Name, "stage "+st.Name+" complete")
	}
	return nil
}

func runStage(spec *release.Spec, st release.Stage, envFile string) error {
	ref, err := imageRef(spec, st.Image)
	if err != nil {
		return err
	}
	args := []string{"run", "--rm", "--network", spec.Network}
	if envFile != "" {
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
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	cmd.Stdout = f
	var errBuf strings.Builder
	cmd.Stderr = &errBuf
	runErr := cmd.Run()
	f.Close()

	for _, ln := range splitLines(errBuf.String()) {
		logline(ln)
	}
	if runErr != nil {
		os.Remove(path)
		return runErr
	}
	if st.RequireNonEmpty {
		fi, err := os.Stat(path)
		if err != nil {
			return err
		}
		if fi.Size() == 0 {
			os.Remove(path)
			return fmt.Errorf("capture %s is empty — refusing to continue", path)
		}
		info(fmt.Sprintf("captured %s (%s)", path, ui.Bytes(fi.Size())))
	}
	pruneCaptures(path, st.Retain)
	return nil
}

func pruneCaptures(path string, retain int) {
	if retain <= 0 {
		return
	}
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	// Captures share a prefix up to the timestamp we substituted.
	prefix := base
	if i := strings.LastIndex(base, "-"); i > 0 {
		prefix = base[:i]
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var matches []string
	for _, e := range ents {
		if !e.IsDir() && strings.HasPrefix(e.Name(), prefix) {
			matches = append(matches, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(matches)))
	for i := retain; i < len(matches); i++ {
		os.Remove(filepath.Join(dir, matches[i]))
	}
}

func expandCapture(pattern, releaseID string) string {
	r := strings.NewReplacer(
		"{{.Release}}", releaseID,
		"{{.Timestamp}}", time.Now().UTC().Format("20060102-150405"),
	)
	return r.Replace(pattern)
}
