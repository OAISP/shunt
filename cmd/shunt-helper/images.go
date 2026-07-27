package main

import (
	"crypto/sha256"
	"encoding/hex"
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
	if err := exec.Command("docker", "network", "inspect", name).Run(); err == nil {
		ok("network", name+" exists")
		return nil
	}
	out, err := exec.Command("docker", "network", "create", name).CombinedOutput()
	if err != nil {
		// A concurrent create is fine; anything else is not.
		if strings.Contains(string(out), "already exists") {
			ok("network", name+" exists")
			return nil
		}
		return fmt.Errorf("create network %s: %s", name, strings.TrimSpace(string(out)))
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
			if out, err := exec.Command("docker", "pull", "--quiet", img.Ref).CombinedOutput(); err != nil {
				return fmt.Errorf("pull %s: %s", img.Ref, strings.TrimSpace(string(out)))
			}
			ok("pull", img.Ref)
			continue
		}

		dir := filepath.Join(spec.StorePath, name)
		step("load", "loading "+name)
		if err := verifyLayout(dir); err != nil {
			return fmt.Errorf("image %s: %w", name, err)
		}
		if err := dockerLoadDir(dir); err != nil {
			return fmt.Errorf("image %s: %w", name, err)
		}
		if err := exec.Command("docker", "image", "inspect", img.Ref).Run(); err != nil {
			return fmt.Errorf("image %s: %s is not present after load — the layout may be tagged differently", name, img.Ref)
		}
		ok("load", img.Ref)
	}
	return nil
}

// verifyLayout rehashes every blob and compares it to its filename. Blob names
// are content hashes, so this is a complete end-to-end integrity check of the
// transfer — rsync decides what to skip from size and mtime alone, which a
// same-size corruption would slip past.
func verifyLayout(dir string) error {
	if os.Getenv("SHUNT_NO_VERIFY") == "1" {
		return nil
	}
	blobs := filepath.Join(dir, "blobs", "sha256")
	ents, err := os.ReadDir(blobs)
	if err != nil {
		return fmt.Errorf("read layout %s: %w", blobs, err)
	}
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		f, err := os.Open(filepath.Join(blobs, e.Name()))
		if err != nil {
			return err
		}
		h := sha256.New()
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			return err
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != e.Name() {
			return fmt.Errorf("blob %s is corrupt (hashes to %s) — rerun the deploy", e.Name()[:16], got[:16])
		}
	}
	return nil
}

// dockerLoadDir streams the OCI layout directory into `docker load`. Docker
// accepts an OCI layout tarball directly and reads the image name from the
// index.json annotations the CLI set.
func dockerLoadDir(dir string) error {
	tarCmd := exec.Command("tar", "-cf", "-", "-C", dir, ".")
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
