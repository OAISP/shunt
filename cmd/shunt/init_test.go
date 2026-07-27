package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/OAISP/shunt/internal/manifest"
)

// The scaffold is the first thing a new user sees, so it must load cleanly —
// including through Validate, which rejects unknown keys.
func TestInitProducesALoadableManifest(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"),
		[]byte("FROM node:22-slim\nEXPOSE 8080\nCMD [\"node\",\"x.js\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	if err := cmdInit([]string{"--host", "deploy@example.com"}, io.Discard); err != nil {
		t.Fatalf("cmdInit: %v", err)
	}

	m, err := manifest.Load(filepath.Join(dir, "shunt.toml"))
	if err != nil {
		t.Fatalf("generated manifest does not load: %v", err)
	}
	if m.Host != "deploy@example.com" {
		t.Errorf("Host = %q", m.Host)
	}
	if _, ok := m.Services["app"]; !ok {
		t.Error("generated manifest has no app service")
	}
	// The port must come from EXPOSE, not the 3000 fallback.
	if got := m.Services["app"].Publish; len(got) != 1 || got[0] != "127.0.0.1:8080:8080" {
		t.Errorf("Publish = %v, want 127.0.0.1:8080:8080", got)
	}
}

func TestInitRefusesToClobber(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	if err := cmdInit(nil, io.Discard); err != nil {
		t.Fatalf("first cmdInit: %v", err)
	}
	if err := cmdInit(nil, io.Discard); err == nil {
		t.Error("second cmdInit overwrote an existing manifest")
	}
	if err := cmdInit([]string{"--force"}, io.Discard); err != nil {
		t.Errorf("cmdInit --force: %v", err)
	}
}

func TestInitRequiresADockerfile(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := cmdInit(nil, io.Discard); err == nil {
		t.Error("expected an error when there is no Dockerfile")
	}
}

func TestSniffPortFallsBackWhenThereIsNoExpose(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "Dockerfile")
	if err := os.WriteFile(p, []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := sniffPort(p); got != 3000 {
		t.Errorf("sniffPort = %d, want 3000", got)
	}
}

func TestSanitizeName(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"myapp", "myapp"},
		{"My App", "my-app"},
		{"web.service", "web-service"},
		{"--leading", "leading"},
		{"", "app"},
	} {
		if got := sanitizeName(tc.in); got != tc.want {
			t.Errorf("sanitizeName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
