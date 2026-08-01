package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OAISP/shunt/internal/release"
)

// downLedger is a host that has one release serving two services and an
// accessory, which is the shape every ordering question below turns on.
func downLedger(t *testing.T) {
	t.Helper()
	l := &release.Ledger{
		Project: "demo", Current: testRelease,
		Releases: []release.Entry{{
			ID: testRelease, Status: release.StatusActive,
			Spec: &release.Spec{
				Project: "demo", ID: testRelease, Network: "demo-net",
				Order: []string{"db", "web"}, // web requires db
				Services: map[string]release.Service{
					"db":  {Image: "app", Drain: "30s"},
					"web": {Image: "app"},
				},
			},
		}},
	}
	if err := saveLedger(l); err != nil {
		t.Fatal(err)
	}
}

const psFilter = "ps -a --filter label=shunt.project=demo"

// A dependent must stop before whatever it depends on, and an accessory after
// every service — a service on its way down may still be talking to it.
func TestDownRemovesDependentsFirstAndAccessoriesLast(t *testing.T) {
	t.Setenv("SHUNT_ROOT", t.TempDir())
	downLedger(t)
	f := newFake().
		on(psFilter, "demo-db\tservice\tdb\ndemo-cache\taccessory\tcache\ndemo-web\tservice\tweb\n", nil).
		on("ps -aq --filter name=", "cid\n", nil)
	withFake(t, f)

	if err := down("demo", "demo-net", true, false); err != nil {
		t.Fatal(err)
	}

	var order []string
	for _, c := range f.calls {
		if name, found := strings.CutPrefix(c, "docker stop --timeout 30 "); found {
			order = append(order, name)
		}
	}
	want := []string{"demo-web", "demo-db", "demo-cache"}
	if len(order) != 3 || order[0] != want[0] || order[1] != want[1] || order[2] != want[2] {
		t.Fatalf("removal order = %v, want %v", order, want)
	}
}

// The default keeps stateful containers. Removing a database as the obvious
// reading of "down" is exactly the accident this command must not enable.
func TestDownKeepsAccessoriesUnlessAsked(t *testing.T) {
	t.Setenv("SHUNT_ROOT", t.TempDir())
	downLedger(t)
	f := newFake().
		on(psFilter, "demo-web\tservice\tweb\ndemo-cache\taccessory\tcache\n", nil).
		on("ps -aq --filter name=", "cid\n", nil)
	withFake(t, f)

	if err := down("demo", "demo-net", false, false); err != nil {
		t.Fatal(err)
	}
	if !f.did("docker stop", "demo-web") {
		t.Fatal("the service was not removed")
	}
	if f.did("docker stop", "demo-cache") {
		t.Fatal("an accessory was removed without --all")
	}
}

// Without --purge the host keeps everything that makes `shunt up` and
// `shunt rollback` work.
func TestDownWithoutPurgeLeavesTheProjectRecoverable(t *testing.T) {
	root := t.TempDir()
	t.Setenv("SHUNT_ROOT", root)
	downLedger(t)
	f := newFake().
		on(psFilter, "demo-web\tservice\tweb\n", nil).
		on("ps -aq --filter name=", "cid\n", nil)
	withFake(t, f)

	if err := down("demo", "demo-net", false, false); err != nil {
		t.Fatal(err)
	}
	if f.did("network rm") {
		t.Error("the network was removed without --purge")
	}
	if f.did("docker rmi") {
		t.Error("images were removed without --purge")
	}
	if _, err := os.Stat(ledgerPath("demo")); err != nil {
		t.Error("the release history was destroyed without --purge")
	}
}

// --purge exists to remove the plaintext secrets. If it left them behind it
// would be worse than useless: it would look like it had cleaned up.
func TestPurgeRemovesSecretsAndHistory(t *testing.T) {
	root := t.TempDir()
	t.Setenv("SHUNT_ROOT", root)
	downLedger(t)
	if _, err := writeSecretFiles(&release.Spec{
		Project: "demo", ID: testRelease, SecretMode: "file",
		Secrets: map[string]string{"DATABASE_URL": "postgres://real"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	f := newFake().
		on(psFilter, "demo-web\tservice\tweb\n", nil).
		on("ps -aq --filter name=", "cid\n", nil).
		on("docker images", "shunt/demo-app:"+testRelease+"\n", nil)
	withFake(t, f)

	if err := down("demo", "demo-net", true, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "demo")); !os.IsNotExist(err) {
		t.Fatal("purge left this project's secrets and history on the host")
	}
	if !f.did("network rm", "demo-net") {
		t.Error("purge left the network behind")
	}
	if !f.did("docker rmi", "shunt/demo-app:") {
		t.Error("purge left the release-tagged images behind")
	}
}

// The manifest may have been edited since the release was deployed. Removing
// the network it currently names would leave the real one behind.
func TestPurgeRemovesTheNetworkTheReleaseActuallyUsed(t *testing.T) {
	t.Setenv("SHUNT_ROOT", t.TempDir())
	downLedger(t) // the serving release used demo-net
	f := newFake().on(psFilter, "", nil)
	withFake(t, f)

	if err := down("demo", "renamed-net", true, true); err != nil {
		t.Fatal(err)
	}
	if f.did("network rm", "renamed-net") {
		t.Fatal("purge removed the network the manifest names today, not the one in use")
	}
	if !f.did("network rm", "demo-net") {
		t.Fatal("purge did not remove the network the serving release used")
	}
}

// Nothing here may remove a volume, at any depth. That is where the data is.
func TestDownNeverTouchesVolumes(t *testing.T) {
	t.Setenv("SHUNT_ROOT", t.TempDir())
	downLedger(t)
	f := newFake().
		on(psFilter, "demo-web\tservice\tweb\ndemo-cache\taccessory\tcache\n", nil).
		on("ps -aq --filter name=", "cid\n", nil)
	withFake(t, f)

	if err := down("demo", "demo-net", true, true); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.calls {
		if strings.Contains(c, "volume") || strings.Contains(c, "docker rm -v") {
			t.Fatalf("a teardown touched a volume: %q", c)
		}
	}
}

// A project that never deployed, or whose containers are already gone, must be
// a clean no-op rather than an error.
func TestDownOnAnEmptyHostSucceeds(t *testing.T) {
	t.Setenv("SHUNT_ROOT", t.TempDir())
	f := newFake().on(psFilter, "", nil)
	withFake(t, f)

	if err := down("demo", "demo-net", true, false); err != nil {
		t.Fatalf("down on a host with nothing running: %v", err)
	}
}
