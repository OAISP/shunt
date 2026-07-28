package main

import (
	"crypto/sha256"
	"encoding/hex"
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

// writeEnvFileScoped materialises the release's secrets for a given scope,
// 0600 and owned by the deploying user.
//
// scope names the subset a service asked for; empty means all of them. A
// separate file per distinct scope is what keeps a worker from holding the
// payment credentials merely because the web app needs them — docker copies
// --env-file into the container config, so narrowing it at the file is the
// only place the narrowing actually takes effect.
func writeEnvFileScoped(spec *release.Spec, scope []string) (string, error) {
	if len(scope) == 0 {
		return writeEnvFile(spec)
	}
	sub := *spec
	sub.Secrets = map[string]string{}
	var missing []string
	for _, k := range scope {
		v, ok := spec.Secrets[k]
		if !ok {
			missing = append(missing, k)
			continue
		}
		sub.Secrets[k] = v
	}
	if len(missing) > 0 {
		return "", fmt.Errorf("secrets %s are not provided by this release", strings.Join(missing, ", "))
	}
	return writeEnvFileAt(&sub, envScopePath(spec.Project, spec.ID, scope))
}

// envScopePath names a scoped env-file after a digest of the keys it holds, so
// two services asking for the same subset share one file and a third asking for
// a different subset gets its own.
func envScopePath(project, id string, scope []string) string {
	keys := append([]string(nil), scope...)
	sort.Strings(keys)
	sum := sha256.Sum256([]byte(strings.Join(keys, "\x00")))
	return filepath.Join(projectDir(project), "env", id+"."+hex.EncodeToString(sum[:])[:8]+".env")
}

func writeEnvFile(spec *release.Spec) (string, error) {
	return writeEnvFileAt(spec, envFilePath(spec.Project, spec.ID))
}

func writeEnvFileAt(spec *release.Spec, p string) (string, error) {
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
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// 0600, not os.Create's 0644. The canonical capture is a pg_dump: every row
	// of production, sitting world-readable on a host that by definition has
	// other people's containers on it.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
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
	pruneCaptures(st.Capture, st.Retain)
	return nil
}

// pruneCaptures keeps the newest `retain` generations of one stage's capture.
//
// It takes the *template*, not the rendered path. Deriving the prefix from the
// rendered name — by cutting at its last dash — produced a prefix containing the
// release id, which matches exactly one file: the one just written. Retention
// silently never deleted anything, and a nightly pre-migration dump grew without
// bound until the disk filled.
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
	var matches []string
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || len(n) <= len(prefix)+len(suffix) {
			continue
		}
		if strings.HasPrefix(n, prefix) && strings.HasSuffix(n, suffix) {
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
