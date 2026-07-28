package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OAISP/shunt/internal/release"
)

// withFake installs a fake docker for the duration of a test.
func withFake(t *testing.T, f *fakeRunner) {
	t.Helper()
	prev := docker
	docker = f
	t.Cleanup(func() { docker = prev })
}

func svcSpec(services map[string]release.Service, order ...string) *release.Spec {
	return &release.Spec{
		Project: "demo", ID: testRelease, Network: "demo-net",
		Images:   map[string]release.ImageRef{"app": {Ref: "shunt/demo-app:" + testRelease}},
		Services: services, Order: order,
	}
}

// The distinction the ledger depends on: replacing a non-proxied service stops
// the old container first, so the host has changed the moment it begins —
// whether or not the new container comes up.
func TestSwapServicesReportsMutationForAnInPlaceReplacement(t *testing.T) {
	f := newFake().on("docker run", "", errors.New("exit status 125"))
	withFake(t, f)

	spec := svcSpec(map[string]release.Service{"app": {Image: "app", Restart: "always"}}, "app")
	started, mutated, err := swapServices(spec, nil, "")

	if err == nil {
		t.Fatal("swapServices succeeded despite docker run failing")
	}
	if !mutated {
		t.Fatal("an in-place replacement that failed must still report the host as changed")
	}
	if len(started) != 0 {
		t.Fatalf("started = %v, want none", started)
	}
}

// A proxied service starts alongside the old one, so a new container that never
// comes up healthy is removed and nothing was replaced — production is intact.
func TestSwapServicesReportsNoMutationWhenABlueGreenReleaseIsRejected(t *testing.T) {
	f := newFake().
		on("inspect -f {{.State.Status}}", "running\n", nil).
		on("docker logs", "boom\n", nil).
		// stopAndRemove checks existence first and short-circuits when nothing is
		// there, so the container has to look real for the removal to be reached.
		on("ps -aq --filter name=", "cid\n", nil)
	withFake(t, f)

	spec := svcSpec(map[string]release.Service{
		"app": {
			Image: "app", Restart: "always", Expose: 3000,
			Proxy:  &release.Proxy{Kind: "traefik", Host: "x.example.com", Port: 3000},
			Health: &release.Health{Command: []string{"false"}, Retries: 1, Interval: "1ms"},
		},
	}, "app")
	// The health command runs through docker exec and must fail.
	f.on("docker exec", "", errors.New("exit status 1"))

	started, mutated, err := swapServices(spec, nil, "")
	if err == nil {
		t.Fatal("an unhealthy blue/green release was accepted")
	}
	if mutated {
		t.Fatal("nothing was replaced — the old container is still serving, so this is not a mutation")
	}
	if len(started) != 0 {
		t.Fatalf("started = %v, want none", started)
	}
	// The broken container must be pulled straight back out rather than left
	// half-live alongside the release still serving.
	if !f.did("rm -f", "demo-app-"+testRelease) {
		t.Fatalf("the rejected container was not removed; calls:\n%s", strings.Join(f.calls, "\n"))
	}
}

// A partial swap is the case the ledger calls degraded: the first service took
// over, the second could not.
func TestSwapServicesStopsAtTheFirstFailureAndReportsWhatTookOver(t *testing.T) {
	f := newFake().on("--name demo-worker", "", errors.New("exit status 125"))
	withFake(t, f)

	spec := svcSpec(map[string]release.Service{
		"app":    {Image: "app", Restart: "always"},
		"worker": {Image: "app", Restart: "always"},
	}, "app", "worker")

	started, mutated, err := swapServices(spec, nil, "")
	if err == nil {
		t.Fatal("swapServices succeeded despite the second service failing")
	}
	if !mutated {
		t.Fatal("the first service took over, so the host has changed")
	}
	if len(started) != 1 || started[0] != "app" {
		t.Fatalf("started = %v, want [app]", started)
	}
}

// A container that has already exited must not burn the whole retry budget.
func TestWaitHealthyFailsFastOnAContainerThatDied(t *testing.T) {
	f := newFake().
		on("inspect -f {{.State.Status}}", "exited\n", nil).
		on("docker exec", "", errors.New("exit status 1")).
		on("docker logs", "panic: nil map\n", nil)
	withFake(t, f)

	spec := svcSpec(map[string]release.Service{
		"app": {Image: "app", Health: &release.Health{
			Command: []string{"true"}, Retries: 50, Interval: "1h", // would hang if retried
		}},
	}, "app")

	err := waitHealthy(spec, []string{"app"}, spec.Services)
	if err == nil {
		t.Fatal("waitHealthy accepted a container that had exited")
	}
	if !strings.Contains(err.Error(), "exited during startup") {
		t.Fatalf("error should say the container exited, got: %v", err)
	}
	// The container's own logs are the useful part of the message.
	if !strings.Contains(err.Error(), "panic: nil map") {
		t.Fatalf("error should carry the container logs, got: %v", err)
	}
}

// An accessory that already exists is started but never replaced — recreating
// Postgres on a code deploy would be both pointless and destructive.
func TestEnsureAccessoriesLeavesAnExistingOneAlone(t *testing.T) {
	f := newFake().on("ps -aq --filter name=^/demo-db$", "abc123\n", nil)
	withFake(t, f)

	spec := &release.Spec{
		Project: "demo", ID: testRelease, Network: "demo-net",
		Images:         map[string]release.ImageRef{"postgres:18": {Ref: "postgres:18", External: true}},
		Accessories:    map[string]release.Service{"db": {Image: "postgres:18", Restart: "always"}},
		AccessoryOrder: []string{"db"},
	}
	ledger := &release.Ledger{Project: "demo"}
	if err := ensureAccessories(spec, ledger, ""); err != nil {
		t.Fatal(err)
	}
	if f.did("docker run", "--name demo-db") {
		t.Fatalf("an existing accessory was recreated; calls:\n%s", strings.Join(f.calls, "\n"))
	}
	if !f.did("docker start demo-db") {
		t.Fatal("an existing accessory should be started in case the host rebooted")
	}
	// Nothing was applied, so nothing should be recorded as applied.
	if len(ledger.Accessories) != 0 {
		t.Fatalf("applied state recorded for an accessory that was not touched: %v", ledger.Accessories)
	}
}

// Creating one *does* record the definition, which is what stops `shunt plan`
// reporting drift forever after.
func TestEnsureAccessoriesRecordsWhatItApplied(t *testing.T) {
	withFake(t, newFake()) // nothing exists

	acc := release.Service{Image: "postgres:18", Restart: "always"}
	spec := &release.Spec{
		Project: "demo", ID: testRelease, Network: "demo-net",
		Images:         map[string]release.ImageRef{"postgres:18": {Ref: "postgres:18", External: true}},
		Accessories:    map[string]release.Service{"db": acc},
		AccessoryOrder: []string{"db"},
	}
	ledger := &release.Ledger{Project: "demo"}
	if err := ensureAccessories(spec, ledger, ""); err != nil {
		t.Fatal(err)
	}
	if got := ledger.Accessories["db"]; got != release.HashService(acc) {
		t.Fatalf("recorded hash = %q, want the applied definition's", got)
	}
}

// Retiring a service stops every container it owns, and drains rather than
// killing — an orphan is still serving something until it stops.
func TestRetireStopsEveryContainerGracefully(t *testing.T) {
	f := newFake().
		on("ps -a --filter label=shunt.project=demo --filter label=shunt.service=old", "demo-old-a\ndemo-old-b\n", nil).
		on("ps -aq --filter name=", "present\n", nil)
	withFake(t, f)

	if err := retireContainers("demo", "old"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"demo-old-a", "demo-old-b"} {
		if !f.did("docker stop", name) {
			t.Fatalf("%s was not stopped; calls:\n%s", name, strings.Join(f.calls, "\n"))
		}
	}
	if f.did("rm -f demo-old-a") && !f.did("docker stop --timeout 10 demo-old-a") {
		t.Fatal("containers were killed rather than drained")
	}
}

// `requires` used to order startup and nothing more, so a service could be
// running and failing against a dependency that had not finished booting.
func TestSwapServicesWaitsForADependencyToBecomeHealthy(t *testing.T) {
	f := newFake().
		on("inspect -f {{.State.Status}}", "running\n", nil).
		on("docker logs", "still booting\n", nil).
		on("docker exec", "", errors.New("exit status 1")) // db never becomes healthy
	withFake(t, f)

	spec := svcSpec(map[string]release.Service{
		"db": {Image: "app", Restart: "always", Health: &release.Health{
			Command: []string{"pg_isready"}, Retries: 1, Interval: "1ms",
		}},
		"web": {Image: "app", Restart: "always", Requires: []string{"db"}},
	}, "db", "web")

	started, _, err := swapServices(spec, nil, "")
	if err == nil {
		t.Fatal("swapServices started a dependent service despite its dependency being unhealthy")
	}
	if !strings.Contains(err.Error(), "required by another service") {
		t.Fatalf("error should explain the dependency, got: %v", err)
	}
	// db started; web must not have.
	if len(started) != 1 || started[0] != "db" {
		t.Fatalf("started = %v, want only [db]", started)
	}
	if f.did("--name demo-web") {
		t.Fatal("the dependent service was started before its dependency was ready")
	}
}

// A service nothing depends on must not be serialised behind its own health
// check — the final gate already covers it, and waiting here would stack every
// service's boot time end to end for no benefit.
func TestSwapServicesDoesNotGateAServiceNothingRequires(t *testing.T) {
	f := newFake()
	withFake(t, f)

	spec := svcSpec(map[string]release.Service{
		"web":    {Image: "app", Restart: "always", Health: &release.Health{Command: []string{"true"}, Retries: 1}},
		"worker": {Image: "app", Restart: "always"},
	}, "web", "worker")

	started, _, err := swapServices(spec, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(started) != 2 {
		t.Fatalf("started = %v, want both", started)
	}
	// No health probe should have run during the swap itself.
	if f.did("docker exec", "true") {
		t.Fatal("an ungated service was health-checked mid-swap")
	}
}

// The other half of that trade: a service the swap *did* gate must not be
// probed a second time by the final gate. A `grace` is a sleep, so re-probing a
// required service with a 30s grace cost the deploy a full extra minute waiting
// on a container it had already proven healthy.
func TestHealthCheckSkipsWhatTheSwapAlreadyGated(t *testing.T) {
	f := newFake()
	withFake(t, f)

	probe := &release.Health{Command: []string{"pg_isready"}, Retries: 1, Interval: "1ms"}
	spec := svcSpec(map[string]release.Service{
		"db":  {Image: "app", Restart: "always", Health: probe},
		"web": {Image: "app", Restart: "always", Requires: []string{"db"}, Health: probe},
	}, "db", "web")

	if _, _, err := swapServices(spec, nil, ""); err != nil {
		t.Fatal(err)
	}
	during := len(f.calls)
	if err := healthCheck(spec); err != nil {
		t.Fatal(err)
	}

	// web is checked here for the first time; db was checked during the swap.
	var reprobed int
	for _, c := range f.calls[during:] {
		if strings.Contains(c, "docker exec demo-db") {
			reprobed++
		}
	}
	if reprobed > 0 {
		t.Fatalf("db was probed %d more time(s) after the swap had already gated it", reprobed)
	}
	if !f.did("docker exec demo-web") {
		t.Fatal("web was never health-checked; the final gate must still cover it")
	}
}

func TestDependedOnIgnoresAccessories(t *testing.T) {
	spec := &release.Spec{
		Services: map[string]release.Service{
			"web":    {Requires: []string{"db", "cache"}},
			"db":     {},
			"worker": {Requires: []string{"db"}},
		},
		Accessories: map[string]release.Service{"cache": {}},
	}
	got := dependedOn(spec)
	if !got["db"] {
		t.Error("db is required by two services and should be gated")
	}
	if got["cache"] {
		t.Error("accessories are already up and health-checked; they must not be gated again")
	}
	if got["web"] || got["worker"] {
		t.Error("nothing requires web or worker")
	}
}

// Auto-rollback restores the release that was *serving*, which is still
// ledger.Current when it runs — the failing attempt is appended afterwards.
//
// Using Previous() here, as `shunt rollback` correctly does, goes one step too
// far and silently reverts a release the operator was happily running. Caught
// by deploying for real; the unit test would have shared the wrong assumption.
func TestAutoRollbackRestoresTheReleaseThatWasServing(t *testing.T) {
	f := newFake().on("ps -aq --filter name=", "cid\n", nil)
	withFake(t, f)

	serving := &release.Spec{
		ID: "r2", Project: "demo", Network: "demo-net",
		Images:   map[string]release.ImageRef{"app": {Ref: "shunt/demo-app:r2"}},
		Services: map[string]release.Service{"app": {Image: "app", Restart: "always"}},
		Order:    []string{"app"},
	}
	ledger := &release.Ledger{
		Project: "demo",
		Current: "r2",
		Releases: []release.Entry{
			{ID: "r1", Status: release.StatusSuperseded, Spec: &release.Spec{ID: "r1"},
				Images: map[string]release.ImageRef{"app": {Ref: "shunt/demo-app:r1"}}},
			{ID: "r2", Status: release.StatusActive, Spec: serving,
				Images: map[string]release.ImageRef{"app": {Ref: "shunt/demo-app:r2"}}},
		},
	}

	failing := &release.Spec{ID: "r3", Project: "demo", Network: "demo-net"}
	if err := autoRollback(failing, ledger); err != nil {
		t.Fatal(err)
	}
	if ledger.Current != "r2" {
		t.Fatalf("restored %q, want r2 — the release that was serving, not the one before it", ledger.Current)
	}
	if !f.did("--name demo-app", "shunt/demo-app:r2") {
		t.Fatalf("r2's image was not the one started; calls:\n%s", strings.Join(f.calls, "\n"))
	}
}

// When the serving release is itself unusable — a degraded host being retried —
// fall back to the last one that was healthy.
func TestAutoRollbackFallsBackWhenCurrentIsNotHealthy(t *testing.T) {
	withFake(t, newFake().on("ps -aq --filter name=", "cid\n", nil))

	ledger := &release.Ledger{
		Project: "demo",
		Current: "r2",
		Releases: []release.Entry{
			{ID: "r1", Status: release.StatusSuperseded,
				Spec:   &release.Spec{ID: "r1", Project: "demo", Services: map[string]release.Service{}},
				Images: map[string]release.ImageRef{"app": {Ref: "shunt/demo-app:r1"}}},
			{ID: "r2", Status: release.StatusDegraded,
				Spec:   &release.Spec{ID: "r2", Project: "demo", Services: map[string]release.Service{}},
				Images: map[string]release.ImageRef{"app": {Ref: "shunt/demo-app:r2"}}},
		},
	}
	if err := autoRollback(&release.Spec{ID: "r3", Project: "demo"}, ledger); err != nil {
		t.Fatal(err)
	}
	if ledger.Current != "r1" {
		t.Fatalf("restored %q, want r1 — r2 was degraded and is not a safe target", ledger.Current)
	}
}

// A directory artifact must never leave its destination absent, even for the
// instant between two renames — an app that opens the path in that window gets
// ENOENT. RENAME_EXCHANGE closes it.
func TestRenameExchangeSwapsTwoDirectoriesAtomically(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "live")
	b := filepath.Join(dir, "staged")
	for path, content := range map[string]string{a: "old", b: "new"} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "marker"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := renameExchange(b, a); err != nil {
		t.Skipf("this filesystem does not support RENAME_EXCHANGE: %v", err)
	}
	if got := readFile(t, filepath.Join(a, "marker")); got != "new" {
		t.Fatalf("live = %q, want the new tree", got)
	}
	if got := readFile(t, filepath.Join(b, "marker")); got != "old" {
		t.Fatalf("staged = %q, want the old tree", got)
	}
}

// Swapping against a path that does not exist has to fail rather than create
// something, so the caller falls back to the plain rename.
func TestRenameExchangeFailsWhenAPathIsMissing(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "here")
	if err := os.MkdirAll(present, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := renameExchange(present, filepath.Join(dir, "absent")); err == nil {
		t.Fatal("renameExchange succeeded against a missing path")
	}
}

// The whole-swap behaviour, through promote: the destination ends up holding
// the new tree and the backup holds the old one.
func TestPromoteSwapsADirectoryAndKeepsTheOld(t *testing.T) {
	dir := t.TempDir()
	a := dirArtifact(t, dir, "weights", map[string]string{"model.bin": "new-weights"})
	if err := os.MkdirAll(a.Dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.Dest, "model.bin"), []byte("old-weights"), 0o644); err != nil {
		t.Fatal(err)
	}

	p, err := promote(&release.Spec{ID: testRelease}, a)
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(a.Dest, "model.bin")); got != "new-weights" {
		t.Fatalf("dest = %q, want the new tree", got)
	}
	if p.prev == "" {
		t.Fatal("no backup was recorded")
	}
	if got := readFile(t, filepath.Join(p.prev, "model.bin")); got != "old-weights" {
		t.Fatalf("backup = %q, want the old tree", got)
	}
}
