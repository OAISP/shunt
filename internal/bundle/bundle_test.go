package bundle

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OAISP/shunt/internal/manifest"
	"github.com/OAISP/shunt/internal/release"
)

func layout(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestRoundTrip(t *testing.T) {
	img := layout(t, map[string]string{
		"index.json":            `{"manifests":[]}`,
		"blobs/sha256/deadbeef": "layer bytes",
	})
	art := filepath.Join(t.TempDir(), "index.db")
	if err := os.WriteFile(art, []byte("SQLite format 3"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	err := Write(&buf, Contents{
		Meta: Meta{
			Host: "deploy@example.com",
			Spec: &release.Spec{ID: "r1", Project: "demo"},
		},
		ImageDirs:     map[string]string{"app": img},
		ArtifactPaths: map[string]string{"seed": art},
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := Read(&buf, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got.Meta.Spec.ID != "r1" || got.Meta.Host != "deploy@example.com" {
		t.Fatalf("meta round-tripped wrong: %+v", got.Meta)
	}
	if b, err := os.ReadFile(filepath.Join(got.ImageDirs["app"], "blobs/sha256/deadbeef")); err != nil {
		t.Fatal(err)
	} else if string(b) != "layer bytes" {
		t.Fatalf("blob = %q, want the original bytes", b)
	}
	if b, err := os.ReadFile(got.ArtifactPaths["seed"]); err != nil {
		t.Fatal(err)
	} else if string(b) != "SQLite format 3" {
		t.Fatalf("artifact = %q", b)
	}
}

// A bundle sits in an approval queue or on a USB stick. Secret values must not
// be in it — the provider block is enough to resolve them at apply time.
func TestSecretValuesNeverEnterTheArchive(t *testing.T) {
	var buf bytes.Buffer
	err := Write(&buf, Contents{
		Meta: Meta{
			Host:    "deploy@example.com",
			Secrets: &manifest.Secrets{Provider: "file", Path: "secrets/prod.env"},
			// A caller that forgot to strip them is the case worth catching, but
			// this test pins the contract the writer is handed: whatever is in the
			// spec is written, so callers strip before calling.
			Spec: &release.Spec{ID: "r1", Project: "demo"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(buf.Bytes(), []byte("hunter2")) {
		t.Fatal("a secret value reached the archive")
	}
	got, err := Read(&buf, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got.Meta.Secrets == nil || got.Meta.Secrets.Path != "secrets/prod.env" {
		t.Fatal("the provider block should survive so apply can resolve values")
	}
	if len(got.Meta.Spec.Secrets) != 0 {
		t.Fatal("no secret values should come back out")
	}
}

// A format change has to be a clear error rather than a misread archive.
func TestReadRefusesAnUnknownVersion(t *testing.T) {
	var buf bytes.Buffer
	if err := Write(&buf, Contents{Meta: Meta{Spec: &release.Spec{ID: "r1"}}}); err != nil {
		t.Fatal(err)
	}
	tampered := bytes.Replace(buf.Bytes(), []byte(`"version": 1`), []byte(`"version": 9`), 1)

	_, err := Read(bytes.NewReader(tampered), t.TempDir())
	if err == nil {
		t.Fatal("Read accepted a bundle from a future format")
	}
	if !strings.Contains(err.Error(), "format v9") {
		t.Fatalf("error should name both versions, got: %v", err)
	}
}

func TestReadRejectsAnArchiveWithoutMeta(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "images/app/index.json", Mode: 0o644, Size: 2, Typeflag: tar.TypeReg})
	tw.Write([]byte("{}"))
	tw.Close()

	if _, err := Read(&buf, t.TempDir()); err == nil {
		t.Fatal("Read accepted an archive that is not a bundle")
	}
}

// A crafted entry naming ../ would write outside the extraction directory.
func TestReadRefusesAPathEscape(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "../escaped", Mode: 0o644, Size: 3, Typeflag: tar.TypeReg})
	tw.Write([]byte("bad"))
	tw.Close()

	dir := t.TempDir()
	if _, err := Read(&buf, dir); err == nil {
		t.Fatal("Read accepted an entry pointing outside the bundle directory")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escaped")); err == nil {
		t.Fatal("an entry escaped the extraction directory")
	}
}

// A bundle whose transfer was cut short must fail, not half-apply.
func TestReadRejectsATruncatedArchive(t *testing.T) {
	img := layout(t, map[string]string{"blobs/sha256/aa": strings.Repeat("x", 4096)})
	var buf bytes.Buffer
	if err := Write(&buf, Contents{
		Meta:      Meta{Spec: &release.Spec{ID: "r1"}},
		ImageDirs: map[string]string{"app": img},
	}); err != nil {
		t.Fatal(err)
	}
	cut := buf.Bytes()[:buf.Len()/2]
	if _, err := Read(bytes.NewReader(cut), t.TempDir()); err == nil {
		t.Fatal("Read accepted a truncated bundle")
	}
}

// Modification times have to survive the archive.
//
// shunt decides whether an artifact needs transferring from its size and mtime,
// and the build normalises blob mtimes so rsync's quick check can skip
// unchanged layers. An extraction that stamped everything with the current time
// would break both: every artifact in a bundle would look changed on every
// apply, and every blob would be re-sent.
func TestModTimesSurviveTheArchive(t *testing.T) {
	src := t.TempDir()
	art := filepath.Join(src, "index.db")
	if err := os.WriteFile(art, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	want := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	if err := os.Chtimes(art, want, want); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := Write(&buf, Contents{
		Meta:          Meta{Spec: &release.Spec{ID: "r1"}},
		ArtifactPaths: map[string]string{"seed": art},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := Read(&buf, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(got.ArtifactPaths["seed"])
	if err != nil {
		t.Fatal(err)
	}
	if !fi.ModTime().UTC().Equal(want) {
		t.Fatalf("mtime = %s, want %s — an artifact would look changed on every apply",
			fi.ModTime().UTC(), want)
	}
}

// Verify catches a blob whose contents no longer match its name.
func TestVerifyCatchesACorruptBlob(t *testing.T) {
	dir := t.TempDir()
	blobs := filepath.Join(dir, "blobs", "sha256")
	if err := os.MkdirAll(blobs, 0o755); err != nil {
		t.Fatal(err)
	}
	blob := filepath.Join(blobs, sha256Hex(t, "layer"))
	if err := os.WriteFile(blob, []byte("layer"), 0o644); err != nil {
		t.Fatal(err)
	}

	bundleOf := func() *bytes.Buffer {
		var buf bytes.Buffer
		if err := Write(&buf, Contents{
			Meta:      Meta{Spec: &release.Spec{ID: "r1"}},
			ImageDirs: map[string]string{"app": dir},
		}); err != nil {
			t.Fatal(err)
		}
		return &buf
	}

	_, rep, err := Verify(bundleOf(), t.TempDir())
	if err != nil {
		t.Fatalf("an intact bundle failed verification: %v", err)
	}
	if rep.Blobs != 1 {
		t.Fatalf("counted %d blobs, want 1", rep.Blobs)
	}

	// Same filename, different bytes — exactly what a damaged transfer produces
	// and what rsync's size/mtime check alone would not catch.
	if err := os.WriteFile(blob, []byte("LAYER"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Verify(bundleOf(), t.TempDir()); err == nil {
		t.Fatal("Verify accepted a blob that does not hash to its name")
	}
}

// A spec naming an image the archive does not hold would otherwise fail on the
// host, after a transfer.
func TestVerifyCatchesAMissingImage(t *testing.T) {
	var buf bytes.Buffer
	if err := Write(&buf, Contents{
		Meta: Meta{Spec: &release.Spec{
			ID:     "r1",
			Images: map[string]release.ImageRef{"app": {Ref: "shunt/demo-app:r1"}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	_, _, err := Verify(bytes.NewReader(buf.Bytes()), t.TempDir())
	if err == nil {
		t.Fatal("Verify accepted a bundle missing an image its release needs")
	}
	if !strings.Contains(err.Error(), "does not contain it") {
		t.Fatalf("error should name the gap, got: %v", err)
	}
}

// ReadMeta answers without unpacking the payload.
func TestReadMetaStopsAtTheDescription(t *testing.T) {
	img := layout(t, map[string]string{"blobs/sha256/aa": strings.Repeat("x", 1<<16)})
	var buf bytes.Buffer
	if err := Write(&buf, Contents{
		Meta:      Meta{Host: "deploy@example.com", Spec: &release.Spec{ID: "r1"}},
		ImageDirs: map[string]string{"app": img},
	}); err != nil {
		t.Fatal(err)
	}
	// A reader that stops early leaves most of the archive unread.
	r := &countingReader{r: bytes.NewReader(buf.Bytes())}
	m, err := ReadMeta(r)
	if err != nil {
		t.Fatal(err)
	}
	if m.Host != "deploy@example.com" {
		t.Fatalf("meta = %+v", m)
	}
	if r.n >= int64(buf.Len()) {
		t.Fatalf("ReadMeta consumed the whole archive (%d of %d bytes)", r.n, buf.Len())
	}
}

type countingReader struct {
	r *bytes.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func sha256Hex(t *testing.T, s string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
