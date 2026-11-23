package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OAISP/shunt/internal/release"
)

// captureEvents redirects the helper's event stream into a buffer.
func captureEvents(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := enc
	enc = json.NewEncoder(&buf)
	t.Cleanup(func() { enc = prev })
	return &buf
}

// The CLI checks its plan against what this announces before shipping
// anything, so it has to name the release that is actually serving.
func TestApplyAnnouncesTheLockWithTheServingRelease(t *testing.T) {
	t.Setenv("SHUNT_ROOT", t.TempDir())
	if err := saveLedger(&release.Ledger{Project: "demo", Current: testRelease}); err != nil {
		t.Fatal(err)
	}
	out := captureEvents(t)
	withFake(t, newFake())

	// EOF where the spec should be: the CLI gave up during the transfer.
	if err := cmdApply([]string{"demo"}, strings.NewReader("")); err != nil {
		t.Fatalf("a session the CLI abandoned should end quietly, got %v", err)
	}
	var ev release.Event
	if err := json.Unmarshal(out.Bytes(), &ev); err != nil {
		t.Fatalf("no event emitted: %v (%q)", err, out)
	}
	if ev.Kind != release.KindHeld || ev.Release != testRelease {
		t.Errorf("first event = %+v, want held naming %s", ev, testRelease)
	}
	l, err := loadLedger("demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Releases) != 0 || l.Current != testRelease {
		t.Errorf("an abandoned session changed the ledger: %+v", l)
	}
}

// The lock was taken for one project; a spec for another must not ride it.
func TestApplyRefusesASpecForAnotherProject(t *testing.T) {
	t.Setenv("SHUNT_ROOT", t.TempDir())
	captureEvents(t)
	withFake(t, newFake())

	spec, _ := json.Marshal(release.Spec{Protocol: release.Protocol, Project: "other", ID: testRelease})
	err := cmdApply([]string{"demo"}, bytes.NewReader(spec))
	if err == nil || !strings.Contains(err.Error(), "locked \"demo\"") {
		t.Fatalf("expected a refusal naming the locked project, got %v", err)
	}
}

// One round trip answers everything a plan asks the host, so each part has to
// be there: what the store holds, and the state of each artifact destination.
func TestPlanReportsStoreAndArtifactDestinations(t *testing.T) {
	root := t.TempDir()
	t.Setenv("SHUNT_ROOT", root)
	withFake(t, newFake().on("docker info", root+"\n", nil))

	blobs := filepath.Join(root, "demo", "store", "app", "blobs", "sha256")
	if err := os.MkdirAll(blobs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blobs, "abc"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(t.TempDir(), "data")
	if err := os.WriteFile(filepath.Join(data+".db"), []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(locked, 0o500); err != nil {
		t.Fatal(err)
	}

	r, w, _ := os.Pipe()
	stdout := os.Stdout
	os.Stdout = w
	err := cmdPlan([]string{"demo", data + ".db", filepath.Join(data, "absent"), filepath.Join(locked, "x.db")})
	w.Close()
	os.Stdout = stdout
	if err != nil {
		t.Fatal(err)
	}
	var st hostStatus
	if err := json.NewDecoder(r).Decode(&st); err != nil {
		t.Fatal(err)
	}

	if got := st.Store["app"]; len(got) != 1 || got[0] != "abc" {
		t.Errorf("store = %v, want app: [abc]", st.Store)
	}
	if p := st.Paths[data+".db"]; p.Size != 5 || p.Problem != "" {
		t.Errorf("existing file = %+v, want size 5 and no problem", p)
	}
	if p := st.Paths[filepath.Join(data, "absent")]; p.Size != -1 {
		t.Errorf("absent destination = %+v, want size -1", p)
	}
	if os.Geteuid() != 0 {
		if p := st.Paths[filepath.Join(locked, "x.db")]; !strings.Contains(p.Problem, "not writable") {
			t.Errorf("read-only directory = %+v, want it reported as not writable", p)
		}
	}
	if st.Disk == nil || st.Disk.StoreFree <= 0 || !st.Disk.Shared {
		t.Errorf("disk = %+v, want free space on one shared filesystem", st.Disk)
	}
}
