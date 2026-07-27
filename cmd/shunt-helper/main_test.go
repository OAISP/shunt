package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OAISP/shunt/internal/release"
)

func proxy() *release.Proxy {
	return &release.Proxy{Kind: "traefik", Host: "app.example.com", Path: "/", Port: 3000, Retry: 2}
}

// A label-discovered router is defined by every container carrying its labels.
// Traefik drops a router that two containers define differently — a total
// outage, not a blip — so overlap is only safe when the proxy config matches.
func TestSameProxy(t *testing.T) {
	a := proxy()
	if !sameProxy(a, proxy()) {
		t.Error("identical proxies compared unequal")
	}
	if sameProxy(nil, a) || sameProxy(a, nil) {
		t.Error("nil must not compare equal to a configured proxy")
	}
	if !sameProxy(nil, nil) {
		t.Error("two nil proxies should compare equal")
	}

	for name, mutate := range map[string]func(*release.Proxy){
		"host":        func(p *release.Proxy) { p.Host = "other.example.com" },
		"kind":        func(p *release.Proxy) { p.Kind = "caddy" },
		"path":        func(p *release.Proxy) { p.Path = "/api" },
		"port":        func(p *release.Proxy) { p.Port = 8080 },
		"retry":       func(p *release.Proxy) { p.Retry = 0 },
		"entrypoints": func(p *release.Proxy) { p.EntryPoints = []string{"websecure"} },
	} {
		b := proxy()
		mutate(b)
		if sameProxy(a, b) {
			t.Errorf("change to %s was not detected", name)
		}
	}
}

func TestCanOverlap(t *testing.T) {
	svc := release.Service{Proxy: proxy()}

	if !canOverlap(nil, "app", svc) {
		t.Error("first deploy should be allowed to overlap")
	}
	if !canOverlap(&release.Spec{Services: map[string]release.Service{}}, "app", svc) {
		t.Error("a service absent from the previous release should be allowed to overlap")
	}

	same := &release.Spec{Services: map[string]release.Service{"app": {Proxy: proxy()}}}
	if !canOverlap(same, "app", svc) {
		t.Error("unchanged proxy config should overlap")
	}

	changed := proxy()
	changed.Host = "new.example.com"
	diff := &release.Spec{Services: map[string]release.Service{"app": {Proxy: changed}}}
	if canOverlap(diff, "app", svc) {
		t.Error("changed proxy config must not overlap — it would drop the router")
	}
}

func TestProxyLabelsTraefik(t *testing.T) {
	spec := &release.Spec{Project: "demo", Network: "demo-net"}
	svc := release.Service{
		Proxy:  &release.Proxy{Kind: "traefik", Host: "app.example.com", Path: "/", Port: 3000, Retry: 2, EntryPoints: []string{"web"}},
		Health: &release.Health{URL: "/health"},
	}
	got := strings.Join(proxyLabels(spec, "app", svc), " ")

	for _, want := range []string{
		"traefik.enable=true",
		"traefik.http.routers.demo-app.rule=Host(`app.example.com`)",
		"traefik.http.routers.demo-app.entrypoints=web",
		"traefik.http.services.demo-app.loadbalancer.server.port=3000",
		"traefik.docker.network=demo-net",
		// The proxy-side health check keeps a still-booting container out of
		// rotation during the overlap.
		"traefik.http.services.demo-app.loadbalancer.healthcheck.path=/health",
		// Retry turns a keep-alive connection torn down mid-request into a
		// served request rather than a 502.
		"traefik.http.middlewares.demo-app-retry.retry.attempts=2",
		"traefik.http.routers.demo-app.middlewares=demo-app-retry",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing label %q in:\n%s", want, got)
		}
	}
}

func TestProxyLabelsPathPrefix(t *testing.T) {
	spec := &release.Spec{Project: "demo"}
	svc := release.Service{Proxy: &release.Proxy{Kind: "traefik", Host: "x.example.com", Path: "/api", Port: 80}}
	got := strings.Join(proxyLabels(spec, "app", svc), " ")
	if !strings.Contains(got, "PathPrefix(`/api`)") {
		t.Errorf("path prefix missing from:\n%s", got)
	}
}

func TestProxyLabelsCaddy(t *testing.T) {
	spec := &release.Spec{Project: "demo"}
	svc := release.Service{Proxy: &release.Proxy{Kind: "caddy", Host: "app.example.com", Port: 3000}}
	got := strings.Join(proxyLabels(spec, "app", svc), " ")
	for _, want := range []string{"caddy=app.example.com", "caddy.reverse_proxy={{upstreams 3000}}"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing label %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "traefik") {
		t.Errorf("traefik labels leaked into a caddy service:\n%s", got)
	}
}

func TestProxyLabelsNoneWhenUnproxied(t *testing.T) {
	if got := proxyLabels(&release.Spec{Project: "demo"}, "worker", release.Service{}); got != nil {
		t.Errorf("proxyLabels on an unproxied service = %v, want nil", got)
	}
}

// Proxied services need a unique name per release so two can coexist; everything
// else keeps a stable name, because a published host port allows only one.
func TestServiceContainerNaming(t *testing.T) {
	spec := &release.Spec{Project: "demo", ID: "20260726-120000"}

	if got := serviceContainer(spec, "worker", release.Service{}); got != "demo-worker" {
		t.Errorf("unproxied container = %q, want demo-worker", got)
	}
	if got := serviceContainer(spec, "app", release.Service{Proxy: proxy()}); got != "demo-app-20260726-120000" {
		t.Errorf("proxied container = %q, want demo-app-20260726-120000", got)
	}
}

func TestDrainSeconds(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"", 10},
		{"garbage", 10},
		{"0s", 10},
		{"30s", 30},
		{"1m", 60},
	} {
		if got := drainSeconds(release.Service{Drain: tc.in}); got != tc.want {
			t.Errorf("drainSeconds(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestPreviousSpec(t *testing.T) {
	l := &release.Ledger{Releases: []release.Entry{
		{ID: "r1", Spec: &release.Spec{ID: "r1"}},
		{ID: "r2", Spec: nil},
		{ID: "r3", Spec: &release.Spec{ID: "r3"}},
	}}
	if got := previousSpec(l); got == nil || got.ID != "r3" {
		t.Errorf("previousSpec = %v, want r3", got)
	}
	if got := previousSpec(&release.Ledger{}); got != nil {
		t.Errorf("previousSpec on empty ledger = %v, want nil", got)
	}
}

// A service that loses its proxy block leaves per-release containers behind, and
// those still carry their proxy labels — so retiring them is what stops the old
// code from serving forever. The keep-name must be the one just started.
func TestRetireKeepsOnlyTheCurrentContainer(t *testing.T) {
	spec := &release.Spec{Project: "demo", ID: "r2"}

	// Unproxied: the stable name is reused, so that is what must survive.
	if got := serviceContainer(spec, "app", release.Service{}); got != "demo-app" {
		t.Errorf("unproxied keep-name = %q, want demo-app", got)
	}
	// Proxied: the new per-release name must survive, not the old one.
	keep := serviceContainer(spec, "app", release.Service{Proxy: proxy()})
	if keep != "demo-app-r2" {
		t.Errorf("proxied keep-name = %q, want demo-app-r2", keep)
	}
	if keep == releaseContainerName("demo", "app", "r1") {
		t.Error("keep-name matches the previous release's container")
	}
}

// Every SQLite database starts with this string. A truncated or half-written
// upload almost never does, and promoting one takes the app down with a database
// it cannot open — the single most valuable check in the artifact path.
func TestCheckMagic(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.db")
	if err := os.WriteFile(good, []byte("SQLite format 3\x00rest of the file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkMagic(good, "SQLite format 3"); err != nil {
		t.Errorf("valid file rejected: %v", err)
	}
	// No magic configured means no opinion.
	if err := checkMagic(good, ""); err != nil {
		t.Errorf("empty magic should skip the check: %v", err)
	}

	bad := filepath.Join(dir, "bad.db")
	if err := os.WriteFile(bad, []byte("\x28\xd0\x10\x85 random garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkMagic(bad, "SQLite format 3"); err == nil {
		t.Error("garbage accepted as a SQLite database")
	}

	// A fragment shorter than the magic itself must not slip through.
	frag := filepath.Join(dir, "frag.db")
	if err := os.WriteFile(frag, []byte("SQLite"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkMagic(frag, "SQLite format 3"); err == nil {
		t.Error("truncated fragment accepted")
	}
}

// Generations are named after the release that superseded them, so they sort by
// time and the operator can see which one they are restoring.
func TestPrunePreviousKeepsNewest(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "danbooru.db")
	ids := []string{"20260101-000000-aaa", "20260102-000000-bbb", "20260103-000000-ccc"}
	for _, id := range ids {
		if err := os.WriteFile(previousPath(dest, id), []byte(id), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	prunePrevious(dest, 1)

	left, _ := filepath.Glob(dest + ".prev.*")
	if len(left) != 1 {
		t.Fatalf("kept %d generations, want 1: %v", len(left), left)
	}
	if !strings.HasSuffix(left[0], ids[2]) {
		t.Errorf("kept %q, want the newest (%s)", left[0], ids[2])
	}
}

func TestPrunePreviousRetainZeroKeepsNothing(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "x.db")
	if err := os.WriteFile(previousPath(dest, "20260101-000000-aaa"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	prunePrevious(dest, 0)
	if left, _ := filepath.Glob(dest + ".prev.*"); len(left) != 0 {
		t.Errorf("retain=0 kept %v", left)
	}
}

// A bare health path has to resolve against whatever the service is actually
// reachable on.
func TestPublishedHostPort(t *testing.T) {
	for _, tc := range []struct {
		publish []string
		want    string
		ok      bool
	}{
		{[]string{"127.0.0.1:9350:3000"}, "9350", true},
		{[]string{"9350:3000"}, "9350", true},
		{[]string{"0.0.0.0:8080:3000/tcp"}, "8080", true},
		{[]string{"3000"}, "", false}, // random host port; nothing stable to probe
		{nil, "", false},
	} {
		got, ok := publishedHostPort(release.Service{Publish: tc.publish})
		if got != tc.want || ok != tc.ok {
			t.Errorf("publishedHostPort(%v) = (%q, %v), want (%q, %v)", tc.publish, got, ok, tc.want, tc.ok)
		}
	}
}
