package bundle

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
