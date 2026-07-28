package main

import (
	"archive/tar"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/OAISP/shunt/internal/oci"
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

// verifyLayout rehashes blobs whose size or mtime moved and returns the set it
// rewrote, which is what the loader sends. A blob is immutable, so one that
// verified once stays verified; hashing the whole image every deploy would cost
// more than the transfer it is checking.
//
// The trade: a blob rewritten with the same size *and* mtime is trusted.
// Nothing rsync does produces that, so what this gives up is silent on-disk
// corruption between deploys. SHUNT_NO_VERIFY_CACHE=1 restores the full rehash.
func verifyLayout(dir string) (map[string]bool, error) {
	blobs, err := oci.Blobs(dir)
	if err != nil {
		return nil, err
	}
	if os.Getenv("SHUNT_NO_VERIFY") == "1" {
		// Nothing is known to be fresh, so the loader sends everything.
		return nil, nil
	}

	var known map[string]verifiedRecord
	if os.Getenv("SHUNT_NO_VERIFY_CACHE") != "1" {
		known = loadVerified(dir)
	}
	fresh := make(map[string]verifiedRecord, len(blobs))
	// The blobs rsync actually rewrote this transfer. They are exactly the ones
	// the daemon cannot already have, which is what makes a partial load safe.
	written := map[string]bool{}

	for digest := range blobs {
		path := filepath.Join(oci.BlobDir(dir), digest)
		fi, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		rec := verifiedRecord{Size: fi.Size(), MTime: fi.ModTime().UnixNano()}
		if prev, ok := known[digest]; ok && prev == rec {
			fresh[digest] = rec
			continue
		}
		if _, err := oci.VerifyBlob(path, digest); err != nil {
			return nil, err
		}
		written[digest] = true
		fresh[digest] = rec
	}

	if len(written) > 0 {
		info(fmt.Sprintf("verified %d new blob(s); %d already checked", len(written), len(fresh)-len(written)))
	}
	saveVerified(dir, fresh)
	return written, nil
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
// onlyBlobs names the blobs rsync rewrote this transfer; every other layer is
// left out, because the daemon already holds it. Both the containerd and the
// classic overlay2 store accept a layout whose known layers are absent —
// measured at 404 MB of tar becoming 40 KB. A nil map sends everything, which is
// what a first deploy and the fallback do.
//
// Built here rather than shelled out to `tar`, which is how the filtering is
// expressed and why the host no longer needs tar.
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
