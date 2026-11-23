package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OAISP/shunt/internal/manifest"
	"github.com/OAISP/shunt/internal/release"
)

// Promoting a bundle to another target must move every name the host keys on,
// including the tag the layout itself tells docker to load the image under.
func TestRetargetMovesTheDestinationAndNothingElse(t *testing.T) {
	dir := t.TempDir()
	blobs := filepath.Join(dir, "blobs", "sha256")
	if err := os.MkdirAll(blobs, 0o755); err != nil {
		t.Fatal(err)
	}
	mani := `{"config":{"digest":"sha256:cfg"},"layers":[{"digest":"sha256:l0"}]}`
	if err := os.WriteFile(filepath.Join(blobs, "mani"), []byte(mani), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.json"), []byte(`{"manifests":[{"digest":"sha256:mani"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	spec := &release.Spec{
		Project: "acme-staging", ID: "r1", Network: "acme-staging-net",
		Images: map[string]release.ImageRef{
			"app": {Ref: "shunt/acme-staging-app:r1", Digest: "sha256:mani"},
			"db":  {Ref: "postgres:16", External: true},
		},
		Services: map[string]release.Service{"app": {Image: "app"}},
	}
	d := manifest.Destination{Host: "prod@example.com", Project: "acme", Network: "acme-net",
		Secrets: &manifest.Secrets{Provider: "env", Mode: "file"}}
	if err := retarget(spec, map[string]string{"app": dir}, d); err != nil {
		t.Fatal(err)
	}

	if spec.Project != "acme" || spec.Network != "acme-net" || !spec.SecretsAsFiles() {
		t.Errorf("spec = %s/%s/%q, want acme/acme-net/file", spec.Project, spec.Network, spec.SecretMode)
	}
	if got := spec.Images["app"].Ref; got != "shunt/acme-app:r1" {
		t.Errorf("app ref = %s, want shunt/acme-app:r1", got)
	}
	if got := spec.Images["app"].Digest; got != "sha256:mani" {
		t.Errorf("retargeting changed the image digest to %s", got)
	}
	if got := spec.Images["db"].Ref; got != "postgres:16" {
		t.Errorf("an external image was renamed to %s", got)
	}

	idx, _ := os.ReadFile(filepath.Join(dir, "index.json"))
	if !strings.Contains(string(idx), `"shunt/acme-app:r1"`) {
		t.Errorf("index.json still names the old tag: %s", idx)
	}
	var archive []struct{ RepoTags []string }
	b, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err := json.Unmarshal(b, &archive); err != nil || len(archive) != 1 || archive[0].RepoTags[0] != "shunt/acme-app:r1" {
		t.Errorf("manifest.json = %s, want it tagged shunt/acme-app:r1", b)
	}
}
