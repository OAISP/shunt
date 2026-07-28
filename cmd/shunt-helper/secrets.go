package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/OAISP/shunt/internal/release"
)

// Secrets on the host: how a release's values are written for its containers to
// read, and how they are read back when a rollback has to replay that release.
//
// Two shapes, chosen by the manifest. An --env-file is one 0600 file per scope;
// file mode is one 0600 file per key in a 0700 directory mounted read-only. Both
// are keyed by release id, because a rollback restarts an old release and must
// hand it the environment it was deployed with, not today's.

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
	values, err := narrow(spec.Secrets, scope)
	if err != nil {
		return "", err
	}
	sub.Secrets = values
	return writeEnvFileAt(&sub, envScopePath(spec.Project, spec.ID, scope))
}

// envScopePath names a scoped env-file after a digest of the keys it holds, so
// two services asking for the same subset share one file and a third asking for
// a different subset gets its own.
func envScopePath(project, id string, scope []string) string {
	return filepath.Join(projectDir(project), "env", id+"."+release.ScopeDigest(scope)+".env")
}

func writeEnvFile(spec *release.Spec) (string, error) {
	return writeEnvFileAt(spec, envFilePath(spec.Project, spec.ID))
}

func writeEnvFileAt(spec *release.Spec, p string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	var b strings.Builder
	for _, k := range sortedKeys(spec.Secrets) {
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

// secretsDir is where a release's file-mode secrets live on the host.
func secretsDir(project, id string, scope []string) string {
	name := id
	if len(scope) > 0 {
		name = id + "." + release.ScopeDigest(scope)
	}
	return filepath.Join(projectDir(project), "secrets", name)
}

// writeSecretFiles materialises secrets as one 0600 file per key in a 0700
// directory, and returns the directory to mount.
//
// This is the alternative to --env-file. Docker copies an env-file into the
// container's configuration, so those values come back out of `docker inspect`
// and out of anything that captures it. A mount shows a path instead.
//
// It does not change who can read them: Docker socket access is root on the
// host, and root can exec into the container or read the file directly. What it
// removes is the copy that leaks by being printed.
func writeSecretFiles(spec *release.Spec, scope []string) (string, error) {
	values, err := narrow(spec.Secrets, scope)
	if err != nil {
		return "", err
	}

	dir := secretsDir(spec.Project, spec.ID, scope)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	// MkdirAll leaves an existing directory's mode alone, and this one is
	// reachable by every container that mounts it.
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}

	// Updated in place rather than recreated. Every service sharing a scope
	// shares this directory — and two services that both narrow to the same keys
	// share it, as do any two that narrow to nothing at all, which is the
	// default. Removing and recreating it unlinked the very inode an
	// already-running container had bind-mounted, so the first service to start
	// was left with an empty /run/secrets for the life of the release while the
	// last one to start saw the files. Nothing failed: the container came up,
	// passed its health check, and could not read its own credentials.
	//
	// Stale keys still have to go, which is what this loop is for: the property
	// worth keeping is "a key removed from the manifest does not linger", not the
	// unlink that used to deliver it.
	ents, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	for _, e := range ents {
		if _, keep := values[e.Name()]; !keep {
			if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
				return "", err
			}
		}
	}

	for _, k := range sortedKeys(values) {
		p := filepath.Join(dir, k)
		// No trailing newline: a file secret is the value, and an app reading it
		// whole should not have to strip anything.
		v := []byte(values[k])
		// Skip an identical rewrite, so the second service to start does not
		// truncate a file the first one's container may be reading right now.
		if cur, err := os.ReadFile(p); err == nil && bytes.Equal(cur, v) {
			continue
		}
		if err := os.WriteFile(p, v, 0o600); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// narrow reduces a release's secrets to the keys a scope asked for. An empty
// scope means all of them.
func narrow(all map[string]string, scope []string) (map[string]string, error) {
	if len(scope) == 0 {
		return all, nil
	}
	out := make(map[string]string, len(scope))
	var missing []string
	for _, k := range scope {
		v, ok := all[k]
		if !ok {
			missing = append(missing, k)
			continue
		}
		out[k] = v
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("secrets %s are not provided by this release", strings.Join(missing, ", "))
	}
	return out, nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// recoverSecrets replaces a replayed spec's redacted values with the plaintext
// the host still holds for that release.
//
// The ledger stores hashes, never values — so the spec a rollback replays has
// "h:3f2a…" where the password should be. Every path that recreates a container
// rewrites the secrets from that spec, so without this a rollback would start
// containers holding hash placeholders and, in file mode, would overwrite the
// one plaintext copy on the host with them. The retained env-file or secrets
// directory is that copy, which makes it the only thing that can restore the
// spec.
//
// A release whose secrets have aged out of retention cannot be replayed at all,
// so this reports which keys are gone rather than starting something broken.
func recoverSecrets(spec *release.Spec) error {
	if len(spec.Secrets) == 0 {
		return nil
	}
	var found map[string]string
	if spec.SecretsAsFiles() {
		found = readSecretDirs(spec.Project, spec.ID)
	} else {
		found = readEnvFile(envFilePath(spec.Project, spec.ID))
	}

	var missing []string
	values := make(map[string]string, len(spec.Secrets))
	for k := range spec.Secrets {
		v, ok := found[k]
		if !ok {
			missing = append(missing, k)
			continue
		}
		values[k] = v
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("the secrets for release %s are no longer on this host (%s); "+
			"roll back to a newer release, or redeploy that commit",
			spec.ID, strings.Join(missing, ", "))
	}
	spec.Secrets = values
	return nil
}

// readEnvFile parses an env-file this helper wrote. Values are verbatim — the
// writer rejects anything a KEY=VALUE line cannot carry — so the only parsing
// needed is the first '='.
func readEnvFile(path string) map[string]string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for _, ln := range splitLines(string(b)) {
		if k, v, ok := strings.Cut(ln, "="); ok {
			out[k] = v
		}
	}
	return out
}

// readSecretDirs merges every file-mode secrets directory belonging to one
// release.
//
// A release has one directory per distinct scope, and the unscoped one exists
// only if some service or stage asked for all the secrets. Reading the union is
// what makes a manifest whose services *all* narrow their secrets recoverable —
// checking a single well-known directory would report such a release as pruned
// when nothing had been pruned at all.
func readSecretDirs(project, id string) map[string]string {
	base := filepath.Join(projectDir(project), "secrets")
	ents, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for _, e := range ents {
		// "<id>" for the whole set, "<id>.<scope>" for a narrowed one.
		if !e.IsDir() || (e.Name() != id && !strings.HasPrefix(e.Name(), id+".")) {
			continue
		}
		keys, err := os.ReadDir(filepath.Join(base, e.Name()))
		if err != nil {
			continue
		}
		for _, k := range keys {
			if k.IsDir() {
				continue
			}
			v, err := os.ReadFile(filepath.Join(base, e.Name(), k.Name()))
			if err != nil {
				continue
			}
			out[k.Name()] = string(v)
		}
	}
	return out
}
