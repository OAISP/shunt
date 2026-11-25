package build

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// The check that explains the most common first-build failure used to look for
// "Driver: docker" with a single space. buildx pads its labels into a column,
// so it never matched, and the one error message that could have named the fix
// stayed silent. These are verbatim outputs.
func TestParseBuildxDriver(t *testing.T) {
	const defaultDriver = `Name:          default
Driver:        docker
Last Activity: 2026-07-28 12:43:12 +0000 UTC

Nodes:
Name:             default
Endpoint:         default
Status:           running
BuildKit version: v0.30.0
`
	const container = `Name:   shunt
Driver: docker-container

Nodes:
Name:      shunt0
Endpoint:  unix:///var/run/docker.sock
Status:    running
`
	for _, tc := range []struct{ name, out, want string }{
		{"padded default driver", defaultDriver, "docker"},
		{"single-space container driver", container, "docker-container"},
		{"no driver line", "Name: default\n", ""},
		{"empty", "", ""},
	} {
		if got := parseBuildxDriver(tc.out); got != tc.want {
			t.Errorf("%s: parseBuildxDriver = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The rule is the combination, not the driver alone: the classic docker driver
// exports a layout perfectly well once the daemon runs the containerd image
// store, and refuses outright when it does not.
func TestOCIExportOKIsTheCombination(t *testing.T) {
	for _, tc := range []struct {
		driver     string
		containerd bool
		want       bool
	}{
		{"docker", false, false}, // the CI runner, and most laptops
		{"docker", true, true},   // containerd image store turned on
		{"docker-container", false, true},
		{"remote", false, true},
		{"", false, true}, // undeterminable must never block a deploy
	} {
		got := OCIExport{Driver: tc.driver, Containerd: tc.containerd}.supported()
		if got != tc.want {
			t.Errorf("driver=%q containerd=%v: OK = %v, want %v", tc.driver, tc.containerd, got, tc.want)
		}
	}
}

func TestCacheForGitHubActions(t *testing.T) {
	c := CacheFor("gha", "acme-app")
	if c.From != "type=gha,scope=shunt-acme-app" {
		t.Errorf("From = %q", c.From)
	}
	// A cache outage must cost a slower build, never a failed deploy.
	if !strings.Contains(c.To, "mode=max") || !strings.Contains(c.To, "ignore-error=true") ||
		!strings.Contains(c.To, "scope=shunt-acme-app") {
		t.Errorf("To = %q", c.To)
	}
	if CacheFor("", "acme-app") != nil {
		t.Error("an unset cache produced cache flags")
	}
}

// buildx never collects garbage in a local cache it exports into, so each build
// writes beside the old cache and is swapped over it afterwards.
func TestCacheForADirectoryRotates(t *testing.T) {
	root := t.TempDir()
	c := CacheFor(root, "acme-app")
	if c.From != "" {
		t.Errorf("first build imported from a cache that does not exist yet: %q", c.From)
	}
	next := filepath.Join(root, "acme-app.next")
	if c.To != "type=local,mode=max,dest="+next {
		t.Errorf("To = %q", c.To)
	}

	if err := os.MkdirAll(next, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(next, "index.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.rotate(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(next); !os.IsNotExist(err) {
		t.Error("the exported cache was left beside the live one")
	}

	again := CacheFor(root, "acme-app")
	if again.From != "type=local,src="+filepath.Join(root, "acme-app") {
		t.Errorf("second build did not import the rotated cache: %q", again.From)
	}
}

// ociLayout writes a layout as registry tools and buildx produce it: blobs named
// by digest, and an index.json with one entry pointing at top.
type ociLayout struct {
	t   *testing.T
	dir string
}

func newLayout(t *testing.T) *ociLayout {
	t.Helper()
	l := &ociLayout{t, t.TempDir()}
	l.file("oci-layout", `{"imageLayoutVersion":"1.0.0"}`)
	return l
}

func (l *ociLayout) file(rel, body string) {
	l.t.Helper()
	p := filepath.Join(l.dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		l.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		l.t.Fatal(err)
	}
}

func (l *ociLayout) blob(digest string, v any) {
	l.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		l.t.Fatal(err)
	}
	l.file(blobPath(digest), string(b))
}

// image adds a manifest, its config and one layer, all named after tag.
func (l *ociLayout) image(tag, arch string) string {
	l.blob("sha256:cfg-"+tag, map[string]any{"os": "linux", "architecture": arch})
	l.file(blobPath("sha256:layer-"+tag), "layer "+tag)
	l.blob("sha256:man-"+tag, map[string]any{
		"mediaType": "application/vnd.oci.image.manifest.v1+json",
		"config":    map[string]any{"digest": "sha256:cfg-" + tag},
		"layers":    []any{map[string]any{"digest": "sha256:layer-" + tag}},
	})
	return "sha256:man-" + tag
}

func (l *ociLayout) top(digest string) string {
	b, _ := json.Marshal(map[string]any{"schemaVersion": 2, "manifests": []any{map[string]any{"digest": digest}}})
	l.file("index.json", string(b))
	return l.dir
}

func entry(digest, os, arch string, ann map[string]string) map[string]any {
	return map[string]any{"digest": digest, "platform": map[string]any{"os": os, "architecture": arch}, "annotations": ann}
}

// What `buildx build --provenance --sbom --output type=oci` writes, and what
// build-push-action pushes by default: the image beside an attestation manifest.
func attested(t *testing.T) string {
	l := newLayout(t)
	l.image("att", "unknown")
	l.blob("sha256:idx", map[string]any{"manifests": []any{
		entry(l.image("amd", "amd64"), "linux", "amd64", nil),
		entry("sha256:man-att", "unknown", "unknown", map[string]string{"vnd.docker.reference.type": "attestation-manifest"}),
	}})
	return l.top("sha256:idx")
}

// What `regctl image copy <multi-arch ref> ocidir://` writes.
func multiPlatform(t *testing.T) string {
	l := newLayout(t)
	l.blob("sha256:idx", map[string]any{"manifests": []any{
		entry(l.image("amd", "amd64"), "linux", "amd64", nil),
		entry(l.image("arm", "arm64"), "linux", "arm64", nil),
	}})
	return l.top("sha256:idx")
}

func importOK(t *testing.T, src, platform string) (*Result, string) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "app")
	r, err := Import(src, out, "app", platform)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	return r, out
}

// The result must look like a fresh single-image export: one index entry for the
// chosen manifest, only that image's blobs, and pinned blob times.
func TestImportOfASinglePlatformLayout(t *testing.T) {
	l := newLayout(t)
	src := l.top(l.image("amd", "amd64"))
	l.file("notes.txt", "not part of the layout")

	r, out := importOK(t, src, "")
	if r.Digest != "sha256:man-amd" {
		t.Errorf("Digest = %q, want sha256:man-amd", r.Digest)
	}
	if _, err := os.Stat(filepath.Join(out, "notes.txt")); !os.IsNotExist(err) {
		t.Errorf("a file outside the layout was copied: %v", err)
	}
	fi, err := os.Stat(filepath.Join(out, blobPath("sha256:layer-amd")))
	if err != nil {
		t.Fatal(err)
	}
	if !fi.ModTime().Equal(blobEpoch) {
		t.Errorf("blob mtime = %v, want the pinned epoch", fi.ModTime())
	}
	if err := WriteDockerArchiveManifest(out, "shunt/demo-app:r1"); err != nil {
		t.Errorf("imported layout is not loadable: %v", err)
	}
}

func TestImportSkipsAttestations(t *testing.T) {
	r, out := importOK(t, attested(t), "")
	if r.Digest != "sha256:man-amd" {
		t.Errorf("Digest = %q, want the image, not the index or attestation", r.Digest)
	}
	if _, err := os.Stat(filepath.Join(out, blobPath("sha256:layer-att"))); !os.IsNotExist(err) {
		t.Error("attestation blobs were shipped")
	}
}

func TestImportPicksTheRequestedPlatform(t *testing.T) {
	r, out := importOK(t, multiPlatform(t), "linux/arm64")
	if r.Digest != "sha256:man-arm" {
		t.Errorf("Digest = %q, want sha256:man-arm", r.Digest)
	}
	if _, err := os.Stat(filepath.Join(out, blobPath("sha256:layer-amd"))); !os.IsNotExist(err) {
		t.Error("blobs of another platform were shipped")
	}
}

func TestImportOfAMultiPlatformLayoutNeedsAPlatform(t *testing.T) {
	_, err := Import(multiPlatform(t), filepath.Join(t.TempDir(), "app"), "app", "")
	if err == nil || !strings.Contains(err.Error(), "linux/amd64, linux/arm64") || !strings.Contains(err.Error(), "set platform") {
		t.Fatalf("err = %v, want the platforms listed and the fix named", err)
	}
	_, err = Import(multiPlatform(t), filepath.Join(t.TempDir(), "app"), "app", "linux/riscv64")
	if err == nil || !strings.Contains(err.Error(), "no image for linux/riscv64") {
		t.Fatalf("err = %v, want a missing-platform error", err)
	}
}

// Pinning blob times on a hard link would rewrite the source's mtimes too, and
// age-based cleanup of a CI cache would then read them as decades old.
func TestImportLeavesTheSourceAlone(t *testing.T) {
	src := attested(t)
	blob := filepath.Join(src, blobPath("sha256:layer-amd"))
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(blob, old, old); err != nil {
		t.Fatal(err)
	}
	index, _ := os.ReadFile(filepath.Join(src, "index.json"))

	_, out := importOK(t, src, "")
	if err := Retag(out, "shunt/demo-app:r1"); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(blob)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.ModTime().Equal(old) {
		t.Errorf("source blob mtime changed to %v", fi.ModTime())
	}
	copied, _ := os.Stat(filepath.Join(out, blobPath("sha256:layer-amd")))
	if os.SameFile(fi, copied) {
		t.Error("source blob is linked into the import, not copied")
	}
	if after, _ := os.ReadFile(filepath.Join(src, "index.json")); string(after) != string(index) {
		t.Errorf("source index.json changed:\n%s", after)
	}
}

func TestImportRejectsALayoutWithTwoImages(t *testing.T) {
	l := newLayout(t)
	l.image("amd", "amd64")
	l.file("index.json", `{"manifests":[{"digest":"sha256:man-amd"},{"digest":"sha256:man-amd"}]}`)
	_, err := Import(l.dir, filepath.Join(t.TempDir(), "app"), "app", "")
	if err == nil || !strings.Contains(err.Error(), "empty directory") {
		t.Fatalf("err = %v, want a refusal naming the fix", err)
	}
}

func TestImportOfAMissingLayoutSaysSo(t *testing.T) {
	_, err := Import(filepath.Join(t.TempDir(), "nope"), filepath.Join(t.TempDir(), "app"), "app", "")
	if err == nil || !strings.Contains(err.Error(), "not an OCI image layout") {
		t.Fatalf("err = %v, want a missing-layout error", err)
	}
}

func TestLayoutPlatform(t *testing.T) {
	for _, tc := range []struct{ src, platform, want string }{
		{attested(t), "", "linux/amd64"},
		{multiPlatform(t), "linux/arm64", "linux/arm64"},
	} {
		got, err := LayoutPlatform(tc.src, tc.platform)
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("LayoutPlatform(%q) = %q, want %q", tc.platform, got, tc.want)
		}
	}
}

// A layout's digests become paths, on the read and on the write.
func TestImportRejectsADigestThatIsNotOne(t *testing.T) {
	l := newLayout(t)
	l.blob("sha256:man", map[string]any{
		"config": map[string]any{"digest": "sha256:cfg"},
		"layers": []any{map[string]any{"digest": "sha256:../../../escaped"}},
	})
	l.blob("sha256:cfg", map[string]any{"os": "linux", "architecture": "amd64"})
	out := filepath.Join(t.TempDir(), "app")
	if _, err := Import(l.top("sha256:man"), out, "app", ""); err == nil || !strings.Contains(err.Error(), "not a digest") {
		t.Fatalf("err = %v, want the digest refused", err)
	}
}

// CI stamps the commit into every image, so each build has a new digest even
// when it would run exactly as the last one did.
func TestFingerprintIgnoresWhatDoesNotRun(t *testing.T) {
	config := func(change func(cfg, run map[string]any)) string {
		run := map[string]any{"Cmd": []string{"serve"}, "Env": []string{"A=1"}, "Labels": map[string]string{"rev": "one"}}
		cfg := map[string]any{
			"os": "linux", "architecture": "amd64", "created": "2026-01-01T00:00:00Z",
			"history": []any{map[string]any{"created_by": "RUN make"}},
			"rootfs":  map[string]any{"type": "layers", "diff_ids": []string{"sha256:l1"}},
			"config":  run,
		}
		change(cfg, run)
		l := newLayout(t)
		digest := l.image("app", "amd64")
		l.blob("sha256:cfg-app", cfg)
		fp, err := Fingerprint(l.top(digest))
		if err != nil {
			t.Fatal(err)
		}
		return fp
	}
	want := config(func(_, _ map[string]any) {})

	same := map[string]func(cfg, run map[string]any){
		"labels":  func(_, run map[string]any) { run["Labels"] = map[string]string{"rev": "two"} },
		"created": func(cfg, _ map[string]any) { cfg["created"] = "2026-02-02T00:00:00Z" },
		"history": func(cfg, _ map[string]any) { cfg["history"] = []any{} },
	}
	for name, change := range same {
		if config(change) != want {
			t.Errorf("a change to %s alone moved the fingerprint", name)
		}
	}
	differs := map[string]func(cfg, run map[string]any){
		"a layer": func(cfg, _ map[string]any) {
			cfg["rootfs"] = map[string]any{"type": "layers", "diff_ids": []string{"sha256:l2"}}
		},
		"the command": func(_, run map[string]any) { run["Cmd"] = []string{"work"} },
		"the env":     func(_, run map[string]any) { run["Env"] = []string{"A=2"} },
	}
	for name, change := range differs {
		if config(change) == want {
			t.Errorf("a change to %s did not move the fingerprint", name)
		}
	}
}
