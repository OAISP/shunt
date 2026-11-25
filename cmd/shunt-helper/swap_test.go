package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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

// The distinction the ledger depends on. Once an in-place replacement has
// stopped the old container, the host has changed whether or not the new one
// comes up.
func TestSwapServicesReportsMutationOnceTheOldContainerIsStopped(t *testing.T) {
	f := newFake().
		on("ps -aq --filter name=^/demo-app$", "cid\n", nil).
		on("docker start", "", errors.New("exit status 1"))
	withFake(t, f)

	spec := svcSpec(map[string]release.Service{"app": {Image: "app", Restart: "always"}}, "app")
	started, mutated, err := swapServices(spec, nil, "", "", false)

	if err == nil {
		t.Fatal("swapServices succeeded despite docker start failing")
	}
	if !mutated {
		t.Fatal("a replacement that failed after stopping the old container must report the host as changed")
	}
	if len(started) != 0 {
		t.Fatalf("started = %v, want none", started)
	}
}

// The replacement is created first, so a definition docker rejects never
// reaches the old container: production is untouched, and the ledger says so.
func TestSwapServicesLeavesTheOldContainerWhenTheReplacementCannotBeCreated(t *testing.T) {
	f := newFake().
		on("ps -aq --filter name=^/demo-app$", "cid\n", nil).
		on("docker create", "invalid mount config", errors.New("exit status 125"))
	withFake(t, f)

	spec := svcSpec(map[string]release.Service{"app": {Image: "app", Restart: "always"}}, "app")
	_, mutated, err := swapServices(spec, nil, "", "", false)

	if err == nil || !strings.Contains(err.Error(), "invalid mount config") {
		t.Fatalf("err = %v, want docker's own reason", err)
	}
	if mutated {
		t.Error("a replacement that was never created reported the host as changed")
	}
	if f.did("docker stop", "demo-app") {
		t.Error("the old container was stopped although its replacement could not be created")
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

	started, mutated, err := swapServices(spec, nil, "", "", false)
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

	started, mutated, err := swapServices(spec, nil, "", "", false)
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
	if f.did("docker create", "--name demo-db") {
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
	if f.did("rm -f demo-old-a") && !f.did("docker stop -t 10 demo-old-a") {
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

	started, _, err := swapServices(spec, nil, "", "", false)
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

	started, _, err := swapServices(spec, nil, "", "", false)
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
	f := newFake().on("label=shunt.release="+testRelease, "db\nweb\n", nil)
	withFake(t, f)

	probe := &release.Health{Command: []string{"pg_isready"}, Retries: 1, Interval: "1ms"}
	spec := svcSpec(map[string]release.Service{
		"db":  {Image: "app", Restart: "always", Health: probe},
		"web": {Image: "app", Restart: "always", Requires: []string{"db"}, Health: probe},
	}, "db", "web")

	if _, _, err := swapServices(spec, nil, "", "", false); err != nil {
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
	if err := autoRollback(failing, ledger, false); err != nil {
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
	if err := autoRollback(&release.Spec{ID: "r3", Project: "demo"}, ledger, false); err != nil {
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

// A release that fails after adding a service leaves that service's container
// running: nothing in the restored release's swap ever visits it, because the
// restored release has no such service. The rollback then reported the previous
// release as restored while code from the failed one was still serving — and
// apply() dropped `mutated` on the strength of that, so the ledger called it
// `failed`, meaning production untouched.
func TestRollbackRemovesAServiceTheUndoneReleaseIntroduced(t *testing.T) {
	f := newFake().
		on("--filter label=shunt.kind=service", "demo-app\tapp\ndemo-broken\tbroken\n", nil).
		on("ps -aq --filter name=", "cid\n", nil)
	withFake(t, f)

	restored := svcSpec(map[string]release.Service{"app": {Image: "app"}}, "app")
	failed := svcSpec(map[string]release.Service{
		"app":    {Image: "app"},
		"broken": {Image: "app", Drain: "3s"},
	}, "app", "broken")

	retireUndeclaredServices(restored, failed)

	if !f.did("docker stop", "demo-broken") {
		t.Fatalf("the failed release's container was left running; calls:\n%s", strings.Join(f.calls, "\n"))
	}
	// Its own drain, not a default: it is still serving something until it stops.
	if !f.did("docker stop -t 3 demo-broken") {
		t.Error("the outgoing service's drain was not honoured")
	}
	if f.did("docker stop", "demo-app") {
		t.Error("a service the restored release does declare was removed")
	}
}

// Accessories are exempt everywhere else and must be here too. A rollback that
// destroyed the database because the release which booted it went away would be
// the worst possible reading of "restore the previous release".
func TestRollbackLeavesAccessoriesAlone(t *testing.T) {
	// The kind filter is what excludes them, so the fake answers as docker would:
	// a query for services never mentions the accessory at all.
	f := newFake().
		on("--filter label=shunt.kind=service", "demo-app\tapp\n", nil).
		on("ps -aq --filter name=", "cid\n", nil)
	withFake(t, f)

	restored := svcSpec(map[string]release.Service{"app": {Image: "app"}}, "app")
	retireUndeclaredServices(restored, nil)

	for _, c := range f.calls {
		if strings.Contains(c, "ps -a") && !strings.Contains(c, "label=shunt.kind=service") {
			t.Fatalf("containers were enumerated without excluding accessories: %q", c)
		}
	}
	if f.did("docker stop", "demo-db") {
		t.Fatal("a rollback removed an accessory")
	}
}

// A container from before shunt labelled them cannot be attributed to a service,
// so it is left alone rather than guessed at.
func TestRollbackIgnoresUnlabelledContainers(t *testing.T) {
	f := newFake().
		on("--filter label=shunt.kind=service", "demo-mystery\t\n", nil).
		on("ps -aq --filter name=", "cid\n", nil)
	withFake(t, f)

	retireUndeclaredServices(svcSpec(map[string]release.Service{"app": {Image: "app"}}, "app"), nil)

	if f.did("docker stop", "demo-mystery") {
		t.Fatal("an unattributable container was removed")
	}
}

// Passed to create rather than applied afterwards, so the container never runs
// without its limits.
func TestLimitsAreSetAtCreate(t *testing.T) {
	f := newFake()
	withFake(t, f)

	spec := svcSpec(map[string]release.Service{
		"app": {Image: "app", Restart: "always", Limits: release.Limits{
			Memory: "1g", MemorySwap: "1536m", CPUs: 1.5, PidsLimit: 200,
			ReadOnly: true, Tmpfs: []string{"/tmp"}, CapDrop: []string{"ALL"}, CapAdd: []string{"NET_BIND_SERVICE"},
			NoNewPrivileges: true, LogDriver: "local", LogOpts: map[string]string{"max-size": "10m", "max-file": "3"},
		}},
	}, "app")
	if _, _, err := swapServices(spec, nil, "", "", false); err != nil {
		t.Fatal(err)
	}
	if !f.did("docker create", "--memory 1g --memory-swap 1536m --cpus 1.5 --pids-limit 200 --read-only --tmpfs /tmp "+
		"--cap-drop ALL --cap-add NET_BIND_SERVICE --security-opt no-new-privileges "+
		"--log-driver local --log-opt max-file=3 --log-opt max-size=10m") {
		t.Errorf("create did not carry the limits:\n%s", strings.Join(f.calls, "\n"))
	}
}

// Stopping a container to start the same thing again is a gap for nothing, and
// for a proxied service the outage blue/green exists to avoid. It is found by
// label, because a proxied container carries the id of the release that
// started it, not this one's.
func TestSwapServicesLeavesAnUnchangedContainerRunning(t *testing.T) {
	svc := release.Service{
		Image: "app", Restart: "always", Expose: 3000,
		Proxy: &release.Proxy{Kind: "traefik", Host: "x.example.com", Port: 3000},
	}
	spec := svcSpec(map[string]release.Service{"app": svc}, "app")
	f := newFake().
		on("label=shunt.config="+release.HashService(svc)+" --filter label=shunt.inputs=", "demo-app-older\n", nil).
		on("ps -a --filter label=shunt.project=demo --filter label=shunt.service=app", "demo-app-older\tr0\ndemo-app-stray\tr1\n", nil).
		on("ps -aq --filter name=", "cid\n", nil)
	withFake(t, f)

	started, mutated, err := swapServices(spec, nil, "", "", false)
	if err != nil || mutated || len(started) != 1 {
		t.Fatalf("started = %v, mutated = %v, err = %v; want the service counted and nothing changed", started, mutated, err)
	}
	if f.did("docker create") || f.did("docker stop", "demo-app-older") {
		t.Errorf("an unchanged container was replaced:\n%s", strings.Join(f.calls, "\n"))
	}
	if !f.did("docker stop", "demo-app-stray") {
		t.Error("what a newer release left beside the kept container was not retired")
	}

	// Data swapped in under it is the one change its labels cannot show.
	if _, _, err := swapServices(spec, nil, "", "", true); err != nil {
		t.Fatal(err)
	}
	if !f.did("docker create", "--name demo-app-"+testRelease) {
		t.Error("a container was left running over an artifact that had been replaced")
	}
}

// The label that decides whether a container is left alone has to move with
// everything the container is created from and with nothing else.
func TestInputsDigestFollowsWhatAContainerIsCreatedFrom(t *testing.T) {
	withFake(t, newFake())
	base := func() *release.Spec {
		s := svcSpec(nil)
		s.Images["app"] = release.ImageRef{Ref: "shunt/demo-app:r1", Digest: "sha256:a", Fingerprint: "f1"}
		s.Secrets = map[string]string{"DB": "one", "STRIPE": "two"}
		return s
	}
	scoped := release.Service{Image: "app", Secrets: []string{"DB"}}
	digest := func(s *release.Spec) string {
		t.Helper()
		d, err := inputsDigest(s, scoped, "salt")
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	want := digest(base())

	same := map[string]func(*release.Spec){
		"a relabelled image": func(s *release.Spec) {
			s.Images["app"] = release.ImageRef{Ref: "shunt/demo-app:r2", Digest: "sha256:b", Fingerprint: "f1"}
		},
		"a secret it is not sent": func(s *release.Spec) { s.Secrets["STRIPE"] = "rotated" },
		"a new release id":        func(s *release.Spec) { s.ID = "r2" },
	}
	for name, change := range same {
		s := base()
		change(s)
		if digest(s) != want {
			t.Errorf("%s changed the digest", name)
		}
	}
	differs := map[string]func(*release.Spec){
		"a rebuilt image":     func(s *release.Spec) { s.Images["app"] = release.ImageRef{Ref: "shunt/demo-app:r2", Fingerprint: "f2"} },
		"a secret it is sent": func(s *release.Spec) { s.Secrets["DB"] = "rotated" },
		"the network":         func(s *release.Spec) { s.Network = "other" },
		"secret delivery":     func(s *release.Spec) { s.SecretMode = "file" },
	}
	for name, change := range differs {
		s := base()
		change(s)
		if digest(s) == want {
			t.Errorf("%s did not change the digest", name)
		}
	}
}

// A stop that fails is followed by a kill, and a deploy that never drains must
// not look like one that does.
func TestStopAndRemoveReportsAStopThatFailed(t *testing.T) {
	events := captureEvents(t)
	withFake(t, newFake().
		on("ps -aq --filter name=", "cid\n", nil).
		on("docker stop -t 10 demo-app", "unknown flag: --timeout\n", errors.New("exit status 125")).
		on("docker stop -t 10 demo-gone", "Error response from daemon: No such container: demo-gone\n", errors.New("exit status 1")))

	stopAndRemove("demo-app", 10)
	if !strings.Contains(events.String(), "killed without draining: unknown flag: --timeout") {
		t.Errorf("a failed stop went unreported: %s", events)
	}
	events.Reset()
	stopAndRemove("demo-gone", 10)
	if events.Len() != 0 {
		t.Errorf("a container that vanished by itself was reported: %s", events)
	}
}

// Only a container docker had to kill is reported as killed: one that exits by
// itself right at the deadline took just as long.
func TestStopAndRemoveReportsAKillOnlyWhenDockerKilled(t *testing.T) {
	for code, wantReport := range map[string]bool{"137": true, "0": false} {
		events := captureEvents(t)
		prev := docker
		docker = runnerFunc(func(_ string, args ...string) (string, error) {
			switch args[0] {
			case "ps":
				return "cid\n", nil
			case "stop":
				time.Sleep(time.Second)
			case "inspect":
				return code + "\n", nil
			}
			return "", nil
		})
		stopAndRemove("demo-app", 1)
		docker = prev
		if got := strings.Contains(events.String(), "was killed"); got != wantReport {
			t.Errorf("exit code %s: reported killed = %v, want %v (%s)", code, got, wantReport, events)
		}
		if strings.Contains(events.String(), "ignored SIGTERM") {
			t.Errorf("the report claims to know why it was still running: %s", events)
		}
	}
}

// A service with no health check can still be held to staying up: without
// that, a worker that dies on startup restarts forever under an active release.
func TestWaitHealthyFailsAContainerThatKeepsRestarting(t *testing.T) {
	spec := svcSpec(map[string]release.Service{"worker": {Image: "app", Restart: "always"}}, "worker")
	for state, healthy := range map[string]bool{
		"running 0 0":    true,
		"exited 0 0":     true, // a one-shot that finished
		"restarting 3 1": false,
		"running 2 0":    false, // caught between two crashes
		"exited 0 1":     false,
	} {
		withFake(t, newFake().
			on("{{.State.Status}} {{.RestartCount}} {{.State.ExitCode}}", state+"\n", nil).
			on("docker logs", "panic: no DATABASE_URL\n", nil))
		err := waitHealthy(spec, []string{"worker"}, spec.Services)
		if (err == nil) != healthy {
			t.Errorf("%q: err = %v, want healthy = %v", state, err, healthy)
		}
		if err != nil && !strings.Contains(err.Error(), "panic: no DATABASE_URL") {
			t.Errorf("%q: the error should carry the container's logs, got %v", state, err)
		}
	}
}

// An accessory is found again by its stable name, so a proxied one must not be
// named per release: every deploy would boot another copy on the same volumes.
func TestAProxiedAccessoryKeepsItsStableName(t *testing.T) {
	acc := release.Service{Image: "app", Proxy: &release.Proxy{Kind: "traefik", Host: "s3.example.com", Port: 9000}}
	spec := svcSpec(nil)
	spec.Accessories = map[string]release.Service{"minio": acc}
	if got := serviceContainer(spec, "minio", acc); got != "demo-minio" {
		t.Errorf("container = %q, want demo-minio", got)
	}
}

// An accessory is never recreated, so it goes on mounting the secrets directory
// of the release that booted it long after the ledger has forgotten that one.
func TestPruneSecretDirsKeepsWhatAContainerStillMounts(t *testing.T) {
	t.Setenv("SHUNT_ROOT", t.TempDir())
	withFake(t, newFake().on("docker ps -a --filter label=shunt.project=demo", "r-booted\nr-current\n", nil))
	for _, id := range []string{"r-booted", "r-current", "r-gone", "r-gone.scope"} {
		if err := os.MkdirAll(filepath.Join(projectDir("demo"), "secrets", id), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	pruneSecretDirs("demo", map[string]bool{"r-current": true})

	got := lsNames(t, filepath.Join(projectDir("demo"), "secrets"))
	if want := []string{"r-booted", "r-current"}; !slices.Equal(got, want) {
		t.Errorf("after prune = %v, want %v", got, want)
	}
}
