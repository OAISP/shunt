// Package build drives buildx to produce an OCI layout directory per image.
//
// The OCI layout is what makes registry-free deploys cheap: every blob is stored
// under blobs/sha256/<digest>, so its filename *is* its content hash. rsync then
// gives layer-level deduplication for free — a rebuild that only changes the app
// layer transfers only the app layer, because every other filename already
// exists on the host with an identical size.
package build

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

type Result struct {
	Name   string // manifest image name
	Dir    string // local OCI layout directory
	Digest string // OCI manifest digest, e.g. sha256:73c1...
	Bytes  int64  // on-disk size of the layout
}

type Options struct {
	Name       string
	Context    string // absolute
	Dockerfile string // absolute
	Platform   string
	Target     string
	Args       map[string]string
	OutDir     string // absolute; overwritten on each build
	NoCache    bool
	Progress   string    // buildx --progress value
	Stdout     io.Writer // build log sink; discard it when not verbose
}

// Build exports one image to an OCI layout directory and returns its digest.
func Build(ctx context.Context, o Options) (*Result, error) {
	// buildx refuses to write into a non-empty directory that is not already a
	// valid layout, and stale index.json entries from a previous build would
	// otherwise linger. Start clean; the *remote* store is what accumulates.
	if err := os.RemoveAll(o.OutDir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(o.OutDir, 0o755); err != nil {
		return nil, err
	}

	args := []string{
		"buildx", "build",
		"--file", o.Dockerfile,
		"--output", fmt.Sprintf("type=oci,dest=%s,tar=false", o.OutDir),
		"--tag", "shunt-build/" + o.Name + ":latest",
	}
	if o.Platform != "" {
		args = append(args, "--platform", o.Platform)
	}
	if o.Target != "" {
		args = append(args, "--target", o.Target)
	}
	if o.NoCache {
		args = append(args, "--no-cache")
	}
	if o.Progress != "" {
		args = append(args, "--progress", o.Progress)
	}
	for _, k := range sortedKeys(o.Args) {
		args = append(args, "--build-arg", k+"="+o.Args[k])
	}
	args = append(args, o.Context)

	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout = o.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("build image %q: %w%s", o.Name, err, buildxHint())
	}

	dg, err := readIndexDigest(o.OutDir)
	if err != nil {
		return nil, fmt.Errorf("build image %q: %w", o.Name, err)
	}

	// buildx leaves an empty ingest/ scratch directory behind; it is not part of
	// the layout and must not be shipped.
	os.RemoveAll(filepath.Join(o.OutDir, "ingest"))

	return &Result{Name: o.Name, Dir: o.OutDir, Digest: dg, Bytes: dirSize(o.OutDir)}, nil
}

// ociIndex is the subset of the OCI image index we care about.
type ociIndex struct {
	Manifests []struct {
		MediaType   string            `json:"mediaType"`
		Digest      string            `json:"digest"`
		Size        int64             `json:"size"`
		Annotations map[string]string `json:"annotations"`
	} `json:"manifests"`
}

func readIndexDigest(dir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if err != nil {
		return "", fmt.Errorf("read index.json: %w", err)
	}
	var idx ociIndex
	if err := json.Unmarshal(b, &idx); err != nil {
		return "", fmt.Errorf("parse index.json: %w", err)
	}
	if len(idx.Manifests) == 0 {
		return "", fmt.Errorf("index.json contains no manifests")
	}
	return idx.Manifests[0].Digest, nil
}

// Retag rewrites index.json so the layout carries the release-specific reference
// the host should end up with. docker load reads this annotation to name the
// image, which is how a release gets an immutable, rollback-addressable tag.
func Retag(dir, ref string) error {
	p := filepath.Join(dir, "index.json")
	b, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	ms, _ := raw["manifests"].([]any)
	if len(ms) == 0 {
		return fmt.Errorf("index.json contains no manifests")
	}
	for _, m := range ms {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		ann, _ := mm["annotations"].(map[string]any)
		if ann == nil {
			ann = map[string]any{}
		}
		ann["org.opencontainers.image.ref.name"] = ref
		// containerd's importer keys off io.containerd.image.name; setting both
		// keeps `docker load` naming consistent across storage drivers.
		ann["io.containerd.image.name"] = "docker.io/" + ref
		mm["annotations"] = ann
	}
	out, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	return os.WriteFile(p, out, 0o644)
}

func buildxHint() string {
	out, err := exec.Command("docker", "buildx", "inspect").CombinedOutput()
	if err != nil {
		return ""
	}
	if strings.Contains(string(out), "docker-container") || strings.Contains(string(out), "Driver: docker\n") {
		return "\n  hint: this builder may not support OCI export — try `docker buildx create --use --name shunt`"
	}
	return ""
}

func dirSize(dir string) int64 {
	var n int64
	filepath.Walk(dir, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			n += fi.Size()
		}
		return nil
	})
	return n
}

// sortedKeys orders build args deterministically, so the same manifest always
// produces the same buildx invocation and therefore the same cache key.
func sortedKeys(m map[string]string) []string {
	return slices.Sorted(maps.Keys(m))
}
