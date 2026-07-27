package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// write lays down a manifest plus a Dockerfile, since Validate checks that the
// referenced Dockerfile actually exists.
func write(t *testing.T, toml string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "shunt.toml")
	if err := os.WriteFile(p, []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const minimal = `
project = "demo"
host = "deploy@example.com"
[images.app]
context = "."
[services.app]
image = "app"
`

func TestLoadAppliesDefaults(t *testing.T) {
	m, err := Load(write(t, minimal))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if m.Network != "demo-net" {
		t.Errorf("Network = %q, want demo-net", m.Network)
	}
	if m.Retain != 5 {
		t.Errorf("Retain = %d, want 5", m.Retain)
	}
	if got := m.Services["app"].Restart; got != "unless-stopped" {
		t.Errorf("Restart = %q, want unless-stopped", got)
	}
}

// A typo'd key that silently does nothing is worse than a failed load, because
// the deploy appears to succeed while ignoring what the operator asked for.
func TestLoadRejectsUnknownKeys(t *testing.T) {
	_, err := Load(write(t, minimal+"\nreplicas = 3\n"))
	if err == nil || !strings.Contains(err.Error(), "unknown key") {
		t.Fatalf("err = %v, want an unknown-key error", err)
	}
}

func TestValidateReportsAllProblemsAtOnce(t *testing.T) {
	_, err := Load(write(t, `
[images.app]
context = "."
[services.web]
image = "nosuchimage"
`))
	if err == nil {
		t.Fatal("expected validation errors")
	}
	for _, want := range []string{"project is required", "host is required", "nosuchimage"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q:\n%s", want, err)
		}
	}
}

func TestServiceMayReferenceAPullableImage(t *testing.T) {
	// "postgres:18-alpine" is not in [images.*] but is clearly a registry ref,
	// so it must be accepted and treated as external.
	if _, err := Load(write(t, `
project = "demo"
host = "h"
[services.db]
image = "postgres:18-alpine"
`)); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

func TestStartOrderRespectsRequires(t *testing.T) {
	m, err := Load(write(t, `
project = "demo"
host = "h"
[images.app]
context = "."
[services.app]
image = "app"
requires = ["cache"]
[services.cache]
image = "redis:7"
[services.worker]
image = "app"
requires = ["app"]
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	order := m.StartOrder()
	pos := map[string]int{}
	for i, n := range order {
		pos[n] = i
	}
	if pos["cache"] > pos["app"] {
		t.Errorf("cache must start before app: %v", order)
	}
	if pos["app"] > pos["worker"] {
		t.Errorf("app must start before worker: %v", order)
	}
}

// Accessories are booted before services, so a requires edge pointing at one
// must not drag it into the service ordering.
func TestStartOrderExcludesAccessories(t *testing.T) {
	m, err := Load(write(t, `
project = "demo"
host = "h"
[images.app]
context = "."
[accessories.db]
image = "postgres:18-alpine"
[services.app]
image = "app"
requires = ["db"]
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	order := m.StartOrder()
	if len(order) != 1 || order[0] != "app" {
		t.Errorf("StartOrder() = %v, want [app]", order)
	}
	if got := m.AccessoryOrder(); len(got) != 1 || got[0] != "db" {
		t.Errorf("AccessoryOrder() = %v, want [db]", got)
	}
}

func TestCycleIsRejected(t *testing.T) {
	_, err := Load(write(t, `
project = "demo"
host = "h"
[images.app]
context = "."
[services.a]
image = "app"
requires = ["b"]
[services.b]
image = "app"
requires = ["a"]
`))
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("err = %v, want a cycle error", err)
	}
}

func TestNameDeclaredAsBothServiceAndAccessoryIsRejected(t *testing.T) {
	_, err := Load(write(t, `
project = "demo"
host = "h"
[accessories.db]
image = "postgres:18-alpine"
[services.db]
image = "postgres:18-alpine"
`))
	if err == nil || !strings.Contains(err.Error(), "both a service and an accessory") {
		t.Fatalf("err = %v, want a service/accessory clash error", err)
	}
}

func TestRequireNonEmptyWithoutCaptureIsRejected(t *testing.T) {
	_, err := Load(write(t, minimal+`
[[stages]]
name = "backup"
image = "app"
command = ["true"]
require_nonempty = true
`))
	if err == nil || !strings.Contains(err.Error(), "require_nonempty") {
		t.Fatalf("err = %v, want a require_nonempty error", err)
	}
}

func TestFindWalksUp(t *testing.T) {
	p := write(t, minimal)
	deep := filepath.Join(filepath.Dir(p), "a", "b")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := Find(deep)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if got != p {
		t.Errorf("Find = %q, want %q", got, p)
	}
}

const proxied = `
project = "demo"
host = "h"
[images.app]
context = "."
[services.app]
image  = "app"
expose = 3000
[services.app.proxy]
kind = "traefik"
host = "app.example.com"
[services.app.health]
url = "/health"
`

func TestProxyDefaults(t *testing.T) {
	m, err := Load(write(t, proxied))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	svc := m.Services["app"]
	if !svc.Proxied() {
		t.Fatal("service should be proxied")
	}
	if svc.ProxyPort() != 3000 {
		t.Errorf("ProxyPort = %d, want 3000 (from expose)", svc.ProxyPort())
	}
	if svc.Proxy.Path != "/" {
		t.Errorf("Path = %q, want /", svc.Proxy.Path)
	}
	// Retry defaults on rather than off: the residual errors in a blue/green
	// swap are keep-alive connections closing mid-request, which retry fixes.
	if got := svc.Proxy.RetryAttempts(); got != 2 {
		t.Errorf("RetryAttempts = %d, want 2", got)
	}
	if svc.Drain.Duration.Seconds() != 10 {
		t.Errorf("Drain = %v, want 10s", svc.Drain.Duration)
	}
}

func TestProxyRetryCanBeDisabled(t *testing.T) {
	m, err := Load(write(t, `
project = "demo"
host = "h"
[images.app]
context = "."
[services.app]
image  = "app"
expose = 3000
[services.app.proxy]
kind  = "traefik"
host  = "app.example.com"
retry = 0
[services.app.health]
url = "/health"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := m.Services["app"].Proxy.RetryAttempts(); got != 0 {
		t.Errorf("RetryAttempts = %d, want 0", got)
	}
}

// A published host port pins the service to one container at a time, which is
// exactly what proxying exists to avoid.
func TestProxyWithPublishIsRejected(t *testing.T) {
	_, err := Load(write(t, `
project = "demo"
host = "h"
[images.app]
context = "."
[services.app]
image   = "app"
expose  = 3000
publish = ["80:3000"]
[services.app.proxy]
kind = "traefik"
host = "app.example.com"
[services.app.health]
url = "/health"
`))
	if err == nil || !strings.Contains(err.Error(), "cannot combine") {
		t.Fatalf("err = %v, want a publish/proxy conflict error", err)
	}
}

// Without a health block a broken release would be put straight into rotation.
func TestProxyWithoutHealthIsRejected(t *testing.T) {
	_, err := Load(write(t, `
project = "demo"
host = "h"
[images.app]
context = "."
[services.app]
image  = "app"
expose = 3000
[services.app.proxy]
kind = "traefik"
host = "app.example.com"
`))
	if err == nil || !strings.Contains(err.Error(), "requires a health block") {
		t.Fatalf("err = %v, want a missing-health error", err)
	}
}

func TestProxyRequiresHostAndPort(t *testing.T) {
	_, err := Load(write(t, `
project = "demo"
host = "h"
[images.app]
context = "."
[services.app]
image = "app"
[services.app.proxy]
kind = "traefik"
[services.app.health]
command = ["true"]
`))
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"proxy needs a host", "proxy needs `expose`"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q:\n%s", want, err)
		}
	}
}

func TestUnsupportedProxyKindIsRejected(t *testing.T) {
	_, err := Load(write(t, `
project = "demo"
host = "h"
[images.app]
context = "."
[services.app]
image  = "app"
expose = 3000
[services.app.proxy]
kind = "nginx"
host = "app.example.com"
[services.app.health]
url = "/health"
`))
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("err = %v, want an unsupported-kind error", err)
	}
}

// A bare-path health url is resolved against the container's own IP, which
// requires knowing the port.
func TestPathHealthUrlNeedsExpose(t *testing.T) {
	_, err := Load(write(t, `
project = "demo"
host = "h"
[images.app]
context = "."
[services.app]
image = "app"
[services.app.health]
url = "/health"
`))
	if err == nil || !strings.Contains(err.Error(), "needs `expose`") {
		t.Fatalf("err = %v, want an expose-required error", err)
	}
}

const withArtifact = `
project = "demo"
host = "h"
[images.app]
context = "."
[[artifacts]]
name = "db"
src  = "data/x.db"
dest = "/opt/demo/data/x.db"
[services.app]
image  = "app"
expose = 3000
[services.app.health]
url = "/health"
`

func TestArtifactDefaults(t *testing.T) {
	m, err := Load(write(t, withArtifact))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(m.Artifacts) != 1 {
		t.Fatalf("got %d artifacts", len(m.Artifacts))
	}
	// One generation kept by default: enough to undo a bad ETL with a rename
	// instead of a re-upload.
	if got := m.Artifacts[0].Retain; got != 1 {
		t.Errorf("Retain = %d, want 1", got)
	}
}

// dest is a path on the host; a relative one has no meaning there.
func TestArtifactDestMustBeAbsoluteFile(t *testing.T) {
	for _, bad := range []string{"data/x.db", "/opt/demo/data/"} {
		_, err := Load(write(t, `
project = "demo"
host = "h"
[images.app]
context = "."
[[artifacts]]
name = "db"
src  = "data/x.db"
dest = "`+bad+`"
[services.app]
image = "app"
`))
		if err == nil {
			t.Errorf("dest %q accepted", bad)
		}
	}
}

func TestArtifactRequiresNameSrcDest(t *testing.T) {
	_, err := Load(write(t, `
project = "demo"
host = "h"
[images.app]
context = "."
[[artifacts]]
name = "db"
[services.app]
image = "app"
`))
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"src is required", "dest is required"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q:\n%s", want, err)
		}
	}
}

func TestDuplicateArtifactNameIsRejected(t *testing.T) {
	_, err := Load(write(t, `
project = "demo"
host = "h"
[images.app]
context = "."
[[artifacts]]
name = "db"
src  = "a"
dest = "/opt/a"
[[artifacts]]
name = "db"
src  = "b"
dest = "/opt/b"
[services.app]
image = "app"
`))
	if err == nil || !strings.Contains(err.Error(), "duplicate name") {
		t.Fatalf("err = %v, want a duplicate-name error", err)
	}
}

// A bare health path is resolvable from a published host port as well as from
// expose; requiring expose would have rejected the common published-port shape.
func TestHealthPathAcceptsPublishedPort(t *testing.T) {
	if _, err := Load(write(t, `
project = "demo"
host = "h"
[images.app]
context = "."
[services.app]
image   = "app"
publish = ["127.0.0.1:9350:3000"]
[services.app.health]
url    = "/"
follow = true
`)); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

func TestHealthPathWithoutAnyPortIsRejected(t *testing.T) {
	_, err := Load(write(t, `
project = "demo"
host = "h"
[images.app]
context = "."
[services.app]
image = "app"
[services.app.health]
url = "/health"
`))
	if err == nil || !strings.Contains(err.Error(), "expose") {
		t.Fatalf("err = %v, want an expose/publish error", err)
	}
}
