package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
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

// A deploy that fails before replacing anything must leave the serving release
// alone. Moving Current onto the failure made `shunt status` contradict the
// "production is untouched" error the operator had just been shown.
func TestRecordOutcomeLeavesTheServingReleaseAloneOnACleanFailure(t *testing.T) {
	l := &release.Ledger{
		Project:  "demo",
		Current:  "r1",
		Releases: []release.Entry{{ID: "r1", Status: release.StatusActive, Spec: &release.Spec{ID: "r1"}}},
	}
	entry := release.Entry{ID: "r2"}
	recordOutcome(l, &entry, false, errors.New("stage \"migrate\" failed"), 5)

	if l.Current != "r1" {
		t.Fatalf("Current = %q, want it to stay on the serving release r1", l.Current)
	}
	if got := l.Find("r1").Status; got != release.StatusActive {
		t.Fatalf("r1 status = %q, want it to stay active", got)
	}
	if got := l.Find("r2").Status; got != release.StatusFailed {
		t.Fatalf("r2 status = %q, want failed", got)
	}
	if l.LastAttempt != "r2" {
		t.Fatalf("LastAttempt = %q, want r2", l.LastAttempt)
	}
}

// A deploy that failed *after* replacing a container did change the host, so it
// becomes current — flagged degraded, because the host is running a mix.
func TestRecordOutcomeMarksAPartialSwapDegraded(t *testing.T) {
	l := &release.Ledger{
		Project:  "demo",
		Current:  "r1",
		Releases: []release.Entry{{ID: "r1", Status: release.StatusActive, Spec: &release.Spec{ID: "r1"}}},
	}
	entry := release.Entry{ID: "r2"}
	recordOutcome(l, &entry, true, errors.New("web did not become healthy"), 5)

	if l.Current != "r2" {
		t.Fatalf("Current = %q, want r2 — it replaced running containers", l.Current)
	}
	if got := l.Find("r2").Status; got != release.StatusDegraded {
		t.Fatalf("r2 status = %q, want degraded", got)
	}
	if got := l.Find("r1").Status; got != release.StatusSuperseded {
		t.Fatalf("r1 status = %q, want superseded", got)
	}
}

func TestRecordOutcomeActivatesOnSuccess(t *testing.T) {
	l := &release.Ledger{
		Project:  "demo",
		Current:  "r1",
		Releases: []release.Entry{{ID: "r1", Status: release.StatusActive, Spec: &release.Spec{ID: "r1"}}},
	}
	entry := release.Entry{ID: "r2"}
	recordOutcome(l, &entry, true, nil, 5)

	if l.Current != "r2" || l.Find("r2").Status != release.StatusActive {
		t.Fatalf("Current = %q status = %q, want r2 active", l.Current, l.Find("r2").Status)
	}
	if got := l.Find("r1").Status; got != release.StatusSuperseded {
		t.Fatalf("r1 status = %q, want superseded", got)
	}
}

// canOverlap decides whether a blue/green swap is safe by comparing proxy
// labels. Feeding it a failed attempt's spec would have it reason about labels
// no running container carries.
func TestPreviousSpecPrefersTheServingRelease(t *testing.T) {
	l := &release.Ledger{
		Project: "demo",
		Current: "r1",
		Releases: []release.Entry{
			{ID: "r1", Status: release.StatusActive, Spec: &release.Spec{ID: "r1"}},
			{ID: "r2", Status: release.StatusFailed, Spec: &release.Spec{ID: "r2"}},
		},
	}
	if got := previousSpec(l); got == nil || got.ID != "r1" {
		t.Fatalf("previousSpec = %v, want the serving release r1", got)
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
// Retention used to derive its prefix from the *rendered* filename by cutting at
// the last dash, which yielded a prefix unique to the file just written — so
// `retain` matched one file, deleted nothing, and dumps grew until the disk did.
func TestPruneCapturesKeepsNewestAcrossReleases(t *testing.T) {
	dir := t.TempDir()
	template := filepath.Join(dir, "pre-migrate-{{.Release}}.sql")

	// Release ids carry their own dashes, which is what broke the old prefix.
	for _, id := range []string{
		"20260720-100000-aaa111", "20260721-100000-bbb222",
		"20260722-100000-ccc333", "20260723-100000-ddd444",
	} {
		if err := os.WriteFile(expandCapture(template, id), []byte("dump"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// An unrelated capture from a different stage must survive untouched.
	other := filepath.Join(dir, "backup-20260720-100000-aaa111.sql")
	if err := os.WriteFile(other, []byte("dump"), 0o600); err != nil {
		t.Fatal(err)
	}

	pruneCaptures(template, 2)

	got := lsNames(t, dir)
	want := []string{
		"backup-20260720-100000-aaa111.sql",
		"pre-migrate-20260722-100000-ccc333.sql",
		"pre-migrate-20260723-100000-ddd444.sql",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("after prune = %v, want %v", got, want)
	}
}

// A template with no placeholder names one fixed path that each release
// overwrites. There are no generations to prune, and a prefix match would sweep
// up every sibling file in the directory.
func TestPruneCapturesIgnoresATemplateWithoutAPlaceholder(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"dump.sql", "dump.sql.old", "unrelated.txt"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pruneCaptures(filepath.Join(dir, "dump.sql"), 1)
	if got := lsNames(t, dir); len(got) != 3 {
		t.Fatalf("prune touched files it should not have: %v", got)
	}
}

// A pg_dump is every row of production. os.Create's 0644 left it readable by
// every other user and container on the host.
func TestCapturesAreWrittenPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dump.sql")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("capture mode = %04o, want no group/other access", perm)
	}
}

func lsNames(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		out = append(out, e.Name())
	}
	slices.Sort(out)
	return out
}

// artifactFixture writes a destination with `old` contents and a staged file
// with `staged` contents, and returns the release.Artifact describing them.
func artifactFixture(t *testing.T, dir, name, old, staged string) release.Artifact {
	t.Helper()
	dest := filepath.Join(dir, name+".db")
	if old != "" {
		if err := os.WriteFile(dest, []byte(old), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	a := release.Artifact{
		Name:   name,
		Dest:   dest,
		Staged: dest + ".new." + testRelease,
		Bytes:  int64(len(staged)),
		Retain: 1,
	}
	if err := os.WriteFile(a.Staged, []byte(staged), 0o644); err != nil {
		t.Fatal(err)
	}
	return a
}

const testRelease = "20260727-120000-abc123"

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// unpromotable returns an artifact whose staged file is valid but whose
// destination directory does not exist, so the final rename fails — after any
// earlier artifact in the same release has already been swapped.
func unpromotable(t *testing.T, dir, name string) release.Artifact {
	t.Helper()
	const contents = "NEW"
	a := release.Artifact{
		Name:   name,
		Dest:   filepath.Join(dir, "no-such-dir", name+".db"),
		Staged: filepath.Join(dir, name+".db.new."+testRelease),
		Bytes:  int64(len(contents)),
		Retain: 1,
	}
	if err := os.WriteFile(a.Staged, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	return a
}

// The core guarantee: if the second artifact cannot be promoted, the first must
// not be left swapped. Otherwise "production is untouched" is false for data.
func TestSwapArtifactsRestoresEarlierPromotionsOnFailure(t *testing.T) {
	dir := t.TempDir()
	a := artifactFixture(t, dir, "first", "OLD-FIRST", "NEW-FIRST")
	b := unpromotable(t, dir, "second")

	spec := &release.Spec{ID: testRelease, Artifacts: []release.Artifact{a, b}}
	err := swapArtifacts(spec)
	if err == nil {
		t.Fatal("swapArtifacts succeeded despite an unpromotable artifact")
	}
	if got := readFile(t, a.Dest); got != "OLD-FIRST" {
		t.Fatalf("first artifact = %q, want it restored to %q", got, "OLD-FIRST")
	}
	if !strings.Contains(err.Error(), "restored") {
		t.Fatalf("error does not mention the restore: %v", err)
	}
}

// When a destination did not exist before this release, restoring it means
// removing what was just written — not leaving a file the operator never had.
func TestSwapArtifactsRemovesFirstTimeFilesOnRollback(t *testing.T) {
	dir := t.TempDir()
	a := artifactFixture(t, dir, "first", "", "NEW-FIRST") // no prior copy
	b := unpromotable(t, dir, "second")

	spec := &release.Spec{ID: testRelease, Artifacts: []release.Artifact{a, b}}
	if err := swapArtifacts(spec); err == nil {
		t.Fatal("swapArtifacts succeeded despite an unpromotable artifact")
	}
	if _, err := os.Stat(a.Dest); !os.IsNotExist(err) {
		t.Fatalf("first artifact should have been removed on rollback, stat err = %v", err)
	}
}

// Validation covers every artifact before any is promoted, so a bad third file
// must leave the first two alone rather than swapping them and then failing.
func TestSwapArtifactsValidatesAllBeforePromotingAny(t *testing.T) {
	dir := t.TempDir()
	a := artifactFixture(t, dir, "first", "OLD-FIRST", "NEW-FIRST")
	b := artifactFixture(t, dir, "second", "OLD-SECOND", "NEW-SECOND")
	c := artifactFixture(t, dir, "third", "OLD-THIRD", "NEW-THIRD")
	c.Bytes = 9999 // as if the transfer had been cut short

	spec := &release.Spec{ID: testRelease, Artifacts: []release.Artifact{a, b, c}}
	if err := swapArtifacts(spec); err == nil {
		t.Fatal("swapArtifacts accepted a truncated artifact")
	}
	for _, want := range []struct{ path, contents string }{
		{a.Dest, "OLD-FIRST"}, {b.Dest, "OLD-SECOND"}, {c.Dest, "OLD-THIRD"},
	} {
		if got := readFile(t, want.path); got != want.contents {
			t.Fatalf("%s = %q, want untouched %q", filepath.Base(want.path), got, want.contents)
		}
	}
}

// A truncated transfer must be kept, not deleted: --partial exists so the next
// run resumes rather than re-sending hundreds of megabytes.
func TestValidateStagedKeepsATruncatedTransferForResume(t *testing.T) {
	dir := t.TempDir()
	a := artifactFixture(t, dir, "data", "OLD", "PARTIAL")
	a.Bytes = 500000

	err := validateStaged(a)
	if err == nil {
		t.Fatal("validateStaged accepted a short file")
	}
	if !strings.Contains(err.Error(), "resume") {
		t.Fatalf("error should point at resuming, got: %v", err)
	}
	if _, err := os.Stat(a.Staged); err != nil {
		t.Fatalf("staged fragment was deleted, defeating --partial: %v", err)
	}
}

// A magic mismatch is the wrong file rather than a partial one, so it is removed
// — leaving it would give the next run's --fuzzy a bogus delta basis.
func TestValidateStagedRemovesAFileOfTheWrongType(t *testing.T) {
	dir := t.TempDir()
	a := artifactFixture(t, dir, "data", "OLD", "not-a-database")
	a.Magic = "SQLite format 3"

	if err := validateStaged(a); err == nil {
		t.Fatal("validateStaged accepted a file failing its magic check")
	}
	if _, err := os.Stat(a.Staged); !os.IsNotExist(err) {
		t.Fatalf("staged file of the wrong type was kept, stat err = %v", err)
	}
}

// The destination must never be absent, even momentarily: the backup is a hard
// link, so the old contents are reachable from two names before the rename.
func TestPromoteBacksUpByHardLink(t *testing.T) {
	dir := t.TempDir()
	a := artifactFixture(t, dir, "data", "OLD", "NEW")

	spec := &release.Spec{ID: testRelease}
	p, err := promote(spec, a)
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, a.Dest); got != "NEW" {
		t.Fatalf("dest = %q, want NEW", got)
	}
	if got := readFile(t, p.prev); got != "OLD" {
		t.Fatalf("backup = %q, want OLD", got)
	}
}

// Fragments from abandoned deploys must not accumulate beside the destination.
func TestSwapArtifactsClearsStaleStagedFiles(t *testing.T) {
	dir := t.TempDir()
	a := artifactFixture(t, dir, "data", "OLD", "NEW")
	stale := a.Dest + ".new.20260101-000000-old999"
	legacy := a.Dest + ".new"
	for _, p := range []string{stale, legacy} {
		if err := os.WriteFile(p, []byte("fragment"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	spec := &release.Spec{ID: testRelease, Artifacts: []release.Artifact{a}}
	if err := swapArtifacts(spec); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{stale, legacy} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("stale fragment %s survived", filepath.Base(p))
		}
	}
}

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
		publish    []string
		host, port string
		ok         bool
	}{
		{[]string{"9090:3000"}, "127.0.0.1", "9090", true},
		{[]string{"127.0.0.1:9090:3000"}, "127.0.0.1", "9090", true},
		// The bind address must be honoured, not assumed: probing loopback for a
		// service bound elsewhere failed health after the swap had happened.
		{[]string{"10.0.0.5:9090:3000"}, "10.0.0.5", "9090", true},
		// "every interface" includes loopback, which is what the host can reach.
		{[]string{"0.0.0.0:9090:3000"}, "127.0.0.1", "9090", true},
		{[]string{"9090:3000/tcp"}, "127.0.0.1", "9090", true},
		// A bare container port means docker picks a random host port.
		{[]string{"3000"}, "", "", false},
		{nil, "", "", false},
	} {
		host, port, ok := publishedHostPort(release.Service{Publish: tc.publish})
		if host != tc.host || port != tc.port || ok != tc.ok {
			t.Errorf("publishedHostPort(%v) = (%q, %q, %v), want (%q, %q, %v)",
				tc.publish, host, port, ok, tc.host, tc.port, tc.ok)
		}
	}
}
