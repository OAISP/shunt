// Package secrets resolves the manifest's secret reference into concrete values,
// in memory, on the operator's machine.
//
// Resolved values are streamed to the host inside the release Spec over the ssh
// channel and land in a 0600 env-file there. They are never written to a local
// temp file, never passed as process arguments, and never baked into an image —
// so they cannot leak through `ps`, shell history, `docker inspect`, or a
// published layer.
package secrets

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/OAISP/shunt/internal/manifest"
)

// Resolve returns the secret set described by the manifest, or an empty map if
// the manifest declares none.
func Resolve(m *manifest.Manifest) (map[string]string, error) {
	if m.Secrets == nil {
		return map[string]string{}, nil
	}
	switch m.Secrets.Provider {
	case "file":
		return fromDotenvFile(m.Abs(m.Secrets.Path))
	case "env":
		return fromEnv(m.Secrets.Keys)
	case "sops":
		return fromSops(m.Abs(m.Secrets.Path))
	default:
		return nil, fmt.Errorf("secrets: unknown provider %q", m.Secrets.Provider)
	}
}

func fromDotenvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("secrets: %w", err)
	}
	defer f.Close()

	// A secrets file that the whole machine can read is a finding, not a
	// preference — say so, but do not block the deploy.
	if fi, err := f.Stat(); err == nil && fi.Mode().Perm()&0o077 != 0 {
		fmt.Fprintf(os.Stderr, "warning: %s is mode %04o — consider chmod 600\n", path, fi.Mode().Perm())
	}
	return parseDotenv(f, path)
}

// parseDotenv reads KEY=VALUE lines. name is used only for error messages.
func parseDotenv(r io.Reader, name string) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	line := 0
	for sc.Scan() {
		line++
		s := strings.TrimSpace(sc.Text())
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		s = strings.TrimPrefix(s, "export ")
		k, v, ok := strings.Cut(s, "=")
		if !ok {
			return nil, fmt.Errorf("secrets: %s:%d: expected KEY=VALUE", name, line)
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		// Strip one layer of matching quotes, the way docker --env-file does not
		// but every hand-written .env does.
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		if k == "" {
			return nil, fmt.Errorf("secrets: %s:%d: empty key", name, line)
		}
		out[k] = v
	}
	return out, sc.Err()
}

func fromEnv(keys []string) (map[string]string, error) {
	out := map[string]string{}
	var missing []string
	for _, k := range keys {
		v, ok := os.LookupEnv(k)
		if !ok {
			missing = append(missing, k)
			continue
		}
		out[k] = v
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("secrets: these vars are not set in the environment: %s", strings.Join(missing, ", "))
	}
	return out, nil
}

// fromSops shells out to sops so decryption uses whatever key material the
// operator already has configured (age, KMS, PGP) — shunt never touches keys.
func fromSops(path string) (map[string]string, error) {
	if _, err := exec.LookPath("sops"); err != nil {
		return nil, fmt.Errorf("secrets: provider \"sops\" needs the sops binary on PATH")
	}
	out, err := exec.Command("sops", "--decrypt", "--output-type", "dotenv", path).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("secrets: sops failed: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("secrets: sops: %w", err)
	}
	res := map[string]string{}
	for i, ln := range strings.Split(string(out), "\n") {
		s := strings.TrimSpace(ln)
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		k, v, ok := strings.Cut(s, "=")
		if !ok {
			return nil, fmt.Errorf("secrets: sops output line %d: expected KEY=VALUE", i+1)
		}
		res[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return res, nil
}

// Keys returns the sorted key names — safe to print, unlike the values.
func Keys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return ks
}

// Interpolate expands ${env:NAME} references in manifest values. Only this one
// form is supported: it is explicit enough that nobody mistakes a literal $ for
// a variable, and it keeps the manifest itself free of secrets.
func Interpolate(s string) (string, error) {
	var b strings.Builder
	for {
		i := strings.Index(s, "${env:")
		if i < 0 {
			b.WriteString(s)
			return b.String(), nil
		}
		j := strings.Index(s[i:], "}")
		if j < 0 {
			return "", fmt.Errorf("unterminated ${env:...} in %q", s)
		}
		name := s[i+6 : i+j]
		v, ok := os.LookupEnv(name)
		if !ok {
			return "", fmt.Errorf("${env:%s} is referenced but %s is not set", name, name)
		}
		b.WriteString(s[:i])
		b.WriteString(v)
		s = s[i+j+1:]
	}
}

// InterpolateMap applies Interpolate to every value in a map.
func InterpolateMap(m map[string]string) (map[string]string, error) {
	if m == nil {
		return nil, nil
	}
	out := make(map[string]string, len(m))
	for _, k := range Keys(m) {
		v, err := Interpolate(m[k])
		if err != nil {
			return nil, fmt.Errorf("key %s: %w", k, err)
		}
		out[k] = v
	}
	return out, nil
}
