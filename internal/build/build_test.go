package build

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// layout writes a minimal OCI layout: an index pointing at a manifest that names
// a config and two layers.
func layout(t *testing.T, nested bool) string {
	t.Helper()
	dir := t.TempDir()
	blobs := filepath.Join(dir, "blobs", "sha256")
	if err := os.MkdirAll(blobs, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name string, v any) {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(blobs, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("mani", map[string]any{
		"mediaType": "application/vnd.oci.image.manifest.v1+json",
		"config":    map[string]any{"digest": "sha256:cfg"},
		"layers": []any{
			map[string]any{"digest": "sha256:layer0"},
			map[string]any{"digest": "sha256:layer1"},
		},
	})

	top := "sha256:mani"
	if nested {
		// Some exports wrap the manifest in an index; the reader must descend.
		write("idx", map[string]any{
			"manifests": []any{map[string]any{"digest": "sha256:mani"}},
		})
		top = "sha256:idx"
	}

	index := map[string]any{
		"schemaVersion": 2,
		"manifests":     []any{map[string]any{"digest": top}},
	}
	b, _ := json.Marshal(index)
	if err := os.WriteFile(filepath.Join(dir, "index.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func readArchiveManifest(t *testing.T, dir string) []dockerArchiveEntry {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("manifest.json: %v", err)
	}
	var got []dockerArchiveEntry
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("parse manifest.json: %v", err)
	}
	return got
}

// A Docker daemon without the containerd image store only understands the
// docker-archive format; handed a bare OCI layout it fails on a missing
// blobs/json. Most servers are that daemon.
func TestWriteDockerArchiveManifest(t *testing.T) {
	dir := layout(t, false)
	if err := WriteDockerArchiveManifest(dir, "shunt/demo-app:r1"); err != nil {
		t.Fatalf("WriteDockerArchiveManifest: %v", err)
	}

	got := readArchiveManifest(t, dir)
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1", len(got))
	}
	e := got[0]
	if e.Config != "blobs/sha256/cfg" {
		t.Errorf("Config = %q", e.Config)
	}
	want := []string{"blobs/sha256/layer0", "blobs/sha256/layer1"}
	if len(e.Layers) != len(want) {
		t.Fatalf("Layers = %v", e.Layers)
	}
	for i := range want {
		if e.Layers[i] != want[i] {
			t.Errorf("Layers[%d] = %q, want %q", i, e.Layers[i], want[i])
		}
	}
	// Layer order is the image's stacking order; reversing it silently produces
	// a broken image rather than an error.
	if len(e.RepoTags) != 1 || e.RepoTags[0] != "shunt/demo-app:r1" {
		t.Errorf("RepoTags = %v", e.RepoTags)
	}
}

func TestWriteDockerArchiveManifestDescendsAnIndex(t *testing.T) {
	dir := layout(t, true)
	if err := WriteDockerArchiveManifest(dir, "shunt/demo-app:r1"); err != nil {
		t.Fatalf("WriteDockerArchiveManifest: %v", err)
	}
	if got := readArchiveManifest(t, dir)[0].Config; got != "blobs/sha256/cfg" {
		t.Errorf("Config = %q, want the nested manifest's config", got)
	}
}

// The two loaders read the tag from different places — containerd from the
// index.json annotations, the classic store from manifest.json RepoTags. If they
// disagreed, the release would land under a different name depending on the
// host, and every later reference to it would miss.
func TestRetagAndArchiveManifestAgreeOnTheTag(t *testing.T) {
	dir := layout(t, false)
	const ref = "shunt/demo-app:20260727-abc123"

	if err := Retag(dir, ref); err != nil {
		t.Fatalf("Retag: %v", err)
	}
	if err := WriteDockerArchiveManifest(dir, ref); err != nil {
		t.Fatalf("WriteDockerArchiveManifest: %v", err)
	}

	b, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var idx struct {
		Manifests []struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(b, &idx); err != nil {
		t.Fatal(err)
	}
	fromIndex := idx.Manifests[0].Annotations["org.opencontainers.image.ref.name"]
	fromArchive := readArchiveManifest(t, dir)[0].RepoTags[0]

	if fromIndex != fromArchive {
		t.Errorf("loaders would disagree: index.json says %q, manifest.json says %q", fromIndex, fromArchive)
	}
	if fromIndex != ref {
		t.Errorf("tag = %q, want %q", fromIndex, ref)
	}
}

// Guessing which platform the host wants would be worse than saying so.
func TestMultiPlatformLayoutIsRejectedClearly(t *testing.T) {
	dir := t.TempDir()
	blobs := filepath.Join(dir, "blobs", "sha256")
	if err := os.MkdirAll(blobs, 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"manifests": []any{
		map[string]any{"digest": "sha256:a"},
		map[string]any{"digest": "sha256:b"},
	}})
	if err := os.WriteFile(filepath.Join(blobs, "idx"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	ib, _ := json.Marshal(map[string]any{"manifests": []any{map[string]any{"digest": "sha256:idx"}}})
	if err := os.WriteFile(filepath.Join(dir, "index.json"), ib, 0o644); err != nil {
		t.Fatal(err)
	}

	err := WriteDockerArchiveManifest(dir, "shunt/demo-app:r1")
	if err == nil {
		t.Fatal("multi-platform layout accepted")
	}
	if !contains(err.Error(), "platform") {
		t.Errorf("error should point at `platform`: %v", err)
	}
}

func contains(h, n string) bool {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return true
		}
	}
	return false
}
