package main

import (
	"errors"
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
