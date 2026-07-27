package engine

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
)

// HelperVersion is cosmetic — it appears in `shunt version`. It is deliberately
// NOT what identifies the helper on the host: a hand-maintained constant is one
// forgotten bump away from silently running stale remote code, which is exactly
// the bug that motivated content-addressing below.
const HelperVersion = "0.1.0"

// helperPkg is the package built when no prebuilt helper is embedded.
const helperPkg = "./cmd/shunt-helper"

// Prebuilt helper binaries are embedded so a released `shunt` ships as a single
// file that needs no toolchain at all. `make helpers` populates this directory;
// the binaries are deliberately not committed, so a `go install` build finds it
// empty and compiles the helper on demand instead.
//
//go:embed bin
var helperFS embed.FS

// helper is a materialised helper binary plus the hash that names it on the host.
type helper struct {
	data []byte
	hash string // first 12 hex chars of sha256(data)
}

// remoteName is the filename the helper gets on the host. Because it is derived
// from the binary's own bytes, any change to the helper — a new feature, a
// rebuild, a downgrade — automatically lands at a new path and gets uploaded.
// A stale helper can never be silently reused.
func (h helper) remoteName() string { return "shunt-helper-" + h.hash }

// write materialises the binary to a temp file for upload.
func (h helper) write() (string, func(), error) {
	f, err := os.CreateTemp("", "shunt-helper-")
	if err != nil {
		return "", func() {}, err
	}
	if _, err := f.Write(h.data); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", func() {}, err
	}
	f.Close()
	if err := os.Chmod(f.Name(), 0o755); err != nil {
		os.Remove(f.Name())
		return "", func() {}, err
	}
	return f.Name(), func() { os.Remove(f.Name()) }, nil
}

// helperBinary loads the helper for the host's architecture, preferring the
// embedded copy and compiling one if this build did not include it.
func helperBinary(goarch string) (*helper, error) {
	data, err := helperFS.ReadFile(fmt.Sprintf("bin/shunt-helper-linux-%s", goarch))
	if err != nil {
		if data, err = buildHelper(goarch); err != nil {
			return nil, err
		}
	}
	sum := sha256.Sum256(data)
	return &helper{data: data, hash: hex.EncodeToString(sum[:])[:12]}, nil
}

// buildHelper compiles the helper from this module's own source.
//
// A `go install`ed shunt carries no embedded helper, because committing several
// megabytes of opaque binaries that then get uploaded to production hosts is a
// worse trade than compiling locally — this way the operator's own toolchain
// builds what runs on their server. The source is already on disk either way:
// in the working tree when developing, or in the module cache `go install`
// populated.
func buildHelper(goarch string) ([]byte, error) {
	if _, err := exec.LookPath("go"); err != nil {
		return nil, fmt.Errorf("this build has no embedded helper for linux/%s and no Go toolchain to compile one\n"+
			"  install a release binary, or build from source with `make build`", goarch)
	}
	srcDir, err := moduleSourceDir()
	if err != nil {
		return nil, err
	}

	dir, err := os.MkdirTemp("", "shunt-build-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	out := filepath.Join(dir, "shunt-helper")
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w", "-o", out, helperPkg)
	cmd.Dir = srcDir
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+goarch, "CGO_ENABLED=0")
	if stderr, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("compiling the helper for linux/%s in %s: %w\n%s",
			goarch, srcDir, err, strings.TrimSpace(string(stderr)))
	}
	return os.ReadFile(out)
}

// moduleSourceDir locates a directory holding this module's source: the working
// module when shunt runs from its own checkout, otherwise the module cache entry
// that `go install` downloaded.
func moduleSourceDir() (string, error) {
	// Developing on the repo: `go list -m` resolves the enclosing main module.
	if out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}").Output(); err == nil {
		if dir := strings.TrimSpace(string(out)); dir != "" && hasHelperSource(dir) {
			return dir, nil
		}
	}

	bi, ok := debug.ReadBuildInfo()
	if !ok || bi.Main.Path == "" {
		return "", fmt.Errorf("cannot locate shunt's own source to compile the helper from")
	}
	// A plain `go build` reports "(devel)" and has no module cache entry to fall
	// back on, so say what to do instead of failing obscurely.
	if bi.Main.Version == "" || bi.Main.Version == "(devel)" {
		return "", fmt.Errorf("this build has no embedded helper and was not installed from a released version\n"+
			"  run `make build` in the shunt checkout, or `go install %s/cmd/shunt@latest`", bi.Main.Path)
	}

	cache, err := goEnv("GOMODCACHE")
	if err != nil {
		return "", err
	}
	dir := filepath.Join(cache, escapeModulePath(bi.Main.Path)+"@"+bi.Main.Version)
	if !hasHelperSource(dir) {
		return "", fmt.Errorf("shunt's source is not in the module cache at %s\n"+
			"  reinstall with `go install %s/cmd/shunt@%s`", dir, bi.Main.Path, bi.Main.Version)
	}
	return dir, nil
}

func hasHelperSource(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "cmd", "shunt-helper"))
	return err == nil
}

func goEnv(name string) (string, error) {
	out, err := exec.Command("go", "env", name).Output()
	if err != nil {
		return "", fmt.Errorf("go env %s: %w", name, err)
	}
	v := strings.TrimSpace(string(out))
	if v == "" {
		return "", fmt.Errorf("go env %s is empty", name)
	}
	return v, nil
}

// escapeModulePath applies the module cache's case encoding: file systems are
// often case-insensitive but module paths are not, so every uppercase letter is
// stored as "!" followed by its lowercase form. github.com/OAISP/shunt therefore
// lives at github.com/!o!a!i!s!p/shunt.
func escapeModulePath(p string) string {
	var b strings.Builder
	for _, r := range p {
		if r >= 'A' && r <= 'Z' {
			b.WriteByte('!')
			b.WriteRune(r + ('a' - 'A'))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
