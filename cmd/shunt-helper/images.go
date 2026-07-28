package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/OAISP/shunt/internal/release"
)

func ensureNetwork(name string) error {
	if name == "" {
		return nil
	}
	step("network", "ensuring network "+name)
	if docker.Ok("docker", "network", "inspect", name) {
		ok("network", name+" exists")
		return nil
	}
	out, err := docker.Run("docker", "network", "create", name)
	if err != nil {
		// A concurrent create is fine; anything else is not.
		if strings.Contains(out, "already exists") {
			ok("network", name+" exists")
			return nil
		}
		return fmt.Errorf("create network %s: %s", name, strings.TrimSpace(out))
	}
	ok("network", "created "+name)
	return nil
}

// loadImages imports the rsync'd OCI layout and pulls any external images.
func loadImages(spec *release.Spec) error {
	for _, name := range slices.Sorted(maps.Keys(spec.Images)) {
		img := spec.Images[name]
		if img.External {
			step("pull", "pulling "+img.Ref)
			if out, err := docker.Run("docker", "pull", "--quiet", img.Ref); err != nil {
				return fmt.Errorf("pull %s: %s", img.Ref, strings.TrimSpace(out))
			}
			ok("pull", img.Ref)
			continue
		}

		dir := filepath.Join(spec.StorePath, name)
		// Retrying a release whose image is already loaded — after a failed stage,
		// say — need not re-read and re-import the whole layout.
		if docker.Ok("docker", "image", "inspect", img.Ref) {
			ok("load", img.Ref+" already present")
			continue
		}
		step("load", "loading "+name)
		if err := verifyLayout(dir); err != nil {
			return fmt.Errorf("image %s: %w", name, err)
		}
		if err := dockerLoadDir(dir); err != nil {
			return fmt.Errorf("image %s: %w", name, err)
		}
		if !docker.Ok("docker", "image", "inspect", img.Ref) {
			return fmt.Errorf("image %s: %s is not present after load — the layout may be tagged differently", name, img.Ref)
		}
		ok("load", img.Ref)
	}
	return nil
}

// verifiedRecord is what the sidecar remembers about a blob already checked.
type verifiedRecord struct {
	Size  int64 `json:"size"`
	MTime int64 `json:"mtime"`
}

// verifiedPath is where a layout records which blobs it has already validated.
func verifiedPath(dir string) string { return filepath.Join(dir, ".shunt-verified.json") }

// verifyLayout rehashes blobs and compares each to its filename. Blob names are
// content hashes, so this is a complete end-to-end integrity check of the
// transfer — rsync decides what to skip from size and mtime alone, which a
// same-size corruption would slip past.
//
// Only *new or changed* blobs are hashed. A blob is content-addressed and
// immutable, so one that verified once stays verified for as long as its size
// and mtime are untouched; rsync rewrites exactly the blobs that changed, which
// is precisely the set worth re-reading. Without this the host paid a full-image
// sha256 on every deploy — shipping 5 KB and then hashing 1 GB, which quietly
// undid the saving the whole design exists to produce.
//
// The trade, stated plainly: a blob rewritten with the same size *and* the same
// mtime is trusted. Nothing rsync does produces that — writing a file moves its
// mtime — so what this gives up is detection of silent on-disk corruption
// between deploys. SHUNT_NO_VERIFY_CACHE=1 restores the full rehash;
// SHUNT_NO_VERIFY=1 still skips verification altogether.
func verifyLayout(dir string) error {
	if os.Getenv("SHUNT_NO_VERIFY") == "1" {
		return nil
	}
	blobs := filepath.Join(dir, "blobs", "sha256")
	ents, err := os.ReadDir(blobs)
	if err != nil {
		return fmt.Errorf("read layout %s: %w", blobs, err)
	}

	var known map[string]verifiedRecord
	if os.Getenv("SHUNT_NO_VERIFY_CACHE") != "1" {
		known = loadVerified(dir)
	}
	fresh := make(map[string]verifiedRecord, len(ents))
	hashed := 0

	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			return err
		}
		rec := verifiedRecord{Size: fi.Size(), MTime: fi.ModTime().UnixNano()}
		if prev, ok := known[e.Name()]; ok && prev == rec {
			fresh[e.Name()] = rec
			continue
		}
		if err := hashBlob(filepath.Join(blobs, e.Name()), e.Name()); err != nil {
			return err
		}
		hashed++
		fresh[e.Name()] = rec
	}

	if hashed > 0 {
		info(fmt.Sprintf("verified %d new blob(s); %d already checked", hashed, len(fresh)-hashed))
	}
	saveVerified(dir, fresh)
	return nil
}

func hashBlob(path, want string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(h, f)
	f.Close()
	if err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("blob %s is corrupt (hashes to %s) — rerun the deploy", want[:16], got[:16])
	}
	return nil
}

// loadVerified reads the sidecar. Any problem simply means everything gets
// hashed again, which is the safe direction to fail in.
func loadVerified(dir string) map[string]verifiedRecord {
	b, err := os.ReadFile(verifiedPath(dir))
	if err != nil {
		return nil
	}
	var m map[string]verifiedRecord
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	return m
}

// saveVerified rewrites the sidecar with exactly the blobs present now, so it
// cannot grow as the mirrored store turns over.
func saveVerified(dir string, m map[string]verifiedRecord) {
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	tmp := verifiedPath(dir) + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, verifiedPath(dir))
	}
}

// dockerLoadDir streams the OCI layout directory into `docker load`. Docker
// accepts an OCI layout tarball directly and reads the image name from the
// index.json annotations the CLI set.
func dockerLoadDir(dir string) error {
	// The sidecar is host-side bookkeeping, not part of the layout docker reads.
	tarCmd := exec.Command("tar", "-cf", "-", "--exclude", ".shunt-verified.json*", "-C", dir, ".")
	loadCmd := exec.Command("docker", "load")

	pipe, err := tarCmd.StdoutPipe()
	if err != nil {
		return err
	}
	loadCmd.Stdin = pipe
	var out strings.Builder
	loadCmd.Stdout = &out
	loadCmd.Stderr = &out

	if err := loadCmd.Start(); err != nil {
		return err
	}
	if err := tarCmd.Run(); err != nil {
		loadCmd.Wait()
		return fmt.Errorf("tar layout: %w", err)
	}
	if err := loadCmd.Wait(); err != nil {
		return fmt.Errorf("docker load: %w: %s", err, strings.TrimSpace(out.String()))
	}
	return nil
}

func imageRef(spec *release.Spec, name string) (string, error) {
	if img, ok := spec.Images[name]; ok {
		return img.Ref, nil
	}
	return "", fmt.Errorf("image %q is not part of this release", name)
}
