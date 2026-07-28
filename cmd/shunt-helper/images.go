package main

import (
	"archive/tar"
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
		fresh, err := verifyLayout(dir)
		if err != nil {
			return fmt.Errorf("image %s: %w", name, err)
		}
		if err := dockerLoadDir(dir, fresh); err != nil {
			return fmt.Errorf("image %s: %w", name, err)
		}
		if !docker.Ok("docker", "image", "inspect", img.Ref) {
			// A partial load is an optimisation, never a requirement. If the
			// daemon did not accept one — an older version, a store that wants
			// every layer present — send the whole layout and carry on.
			info("partial load did not produce " + img.Ref + "; sending the whole layout")
			if err := dockerLoadDir(dir, nil); err != nil {
				return fmt.Errorf("image %s: %w", name, err)
			}
			if !docker.Ok("docker", "image", "inspect", img.Ref) {
				return fmt.Errorf("image %s: %s is not present after load — the layout may be tagged differently", name, img.Ref)
			}
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
func verifyLayout(dir string) (map[string]bool, error) {
	blobs := filepath.Join(dir, "blobs", "sha256")
	ents, err := os.ReadDir(blobs)
	if err != nil {
		return nil, fmt.Errorf("read layout %s: %w", blobs, err)
	}
	if os.Getenv("SHUNT_NO_VERIFY") == "1" {
		// Nothing is known to be fresh, so the loader sends everything.
		return nil, nil
	}

	var known map[string]verifiedRecord
	if os.Getenv("SHUNT_NO_VERIFY_CACHE") != "1" {
		known = loadVerified(dir)
	}
	fresh := make(map[string]verifiedRecord, len(ents))
	// The blobs rsync actually rewrote this transfer. They are exactly the ones
	// the daemon cannot already have, which is what makes a partial load safe.
	written := map[string]bool{}

	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			return nil, err
		}
		rec := verifiedRecord{Size: fi.Size(), MTime: fi.ModTime().UnixNano()}
		if prev, ok := known[e.Name()]; ok && prev == rec {
			fresh[e.Name()] = rec
			continue
		}
		if err := hashBlob(filepath.Join(blobs, e.Name()), e.Name()); err != nil {
			return nil, err
		}
		written[e.Name()] = true
		fresh[e.Name()] = rec
	}

	if len(written) > 0 {
		info(fmt.Sprintf("verified %d new blob(s); %d already checked", len(written), len(fresh)-len(written)))
	}
	saveVerified(dir, fresh)
	return written, nil
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

// dockerLoadDir streams the OCI layout into `docker load`.
//
// When onlyBlobs is non-nil it names the blobs rsync rewrote this transfer, and
// every other layer blob is left out of the stream. The daemon already holds
// those — they are unchanged from a release it has loaded before — and both the
// containerd and the classic overlay2 stores accept a layout whose known layers
// are absent. Measured on a 404 MB image with a code-only change: 404 MB of tar
// becomes 40 KB, which is what turns a 12-second apply into an instant one.
//
// A nil map sends everything, which is what a first deploy and the fallback do.
//
// The archive is built here rather than shelled out to `tar`, which is both how
// the filtering is expressed and why the host no longer needs tar at all.
func dockerLoadDir(dir string, onlyBlobs map[string]bool) error {
	loadCmd := exec.Command("docker", "load")
	stdin, err := loadCmd.StdinPipe()
	if err != nil {
		return err
	}
	var out strings.Builder
	loadCmd.Stdout, loadCmd.Stderr = &out, &out
	if err := loadCmd.Start(); err != nil {
		return err
	}

	writeErr := writeLayoutTar(stdin, dir, onlyBlobs)
	stdin.Close()

	if err := loadCmd.Wait(); err != nil {
		return fmt.Errorf("docker load: %w: %s", err, strings.TrimSpace(out.String()))
	}
	return writeErr
}

// writeLayoutTar tars a layout, optionally keeping only the named blobs.
func writeLayoutTar(w io.Writer, dir string, onlyBlobs map[string]bool) error {
	tw := tar.NewWriter(w)
	blobPrefix := filepath.Join("blobs", "sha256") + string(filepath.Separator)

	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		// Host-side bookkeeping, not part of the layout docker reads.
		if strings.HasPrefix(filepath.Base(rel), ".shunt-verified.json") {
			return nil
		}
		if onlyBlobs != nil && strings.HasPrefix(rel, blobPrefix) {
			if !onlyBlobs[filepath.Base(rel)] {
				return nil
			}
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{
			Name: filepath.ToSlash(rel), Mode: int64(fi.Mode().Perm()),
			Size: fi.Size(), Typeflag: tar.TypeReg, ModTime: fi.ModTime(),
		}); err != nil {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		_, err = io.Copy(tw, f)
		f.Close()
		return err
	})
	if err != nil {
		tw.Close()
		return fmt.Errorf("tar layout: %w", err)
	}
	return tw.Close()
}

func imageRef(spec *release.Spec, name string) (string, error) {
	if img, ok := spec.Images[name]; ok {
		return img.Ref, nil
	}
	return "", fmt.Errorf("image %q is not part of this release", name)
}
