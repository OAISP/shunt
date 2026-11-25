package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// A client that vanishes without closing the connection sends neither a spec
// nor EOF, and would hold the lock until TCP noticed.
func TestASilentClientLosesTheLock(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	in, heard := untilSilent(pr, 20*time.Millisecond)
	defer heard.Stop()

	go pw.Write([]byte("\n")) // one heartbeat, then nothing
	var spec release.Spec
	if err := json.NewDecoder(in).Decode(&spec); !errors.Is(err, errSilent) {
		t.Fatalf("err = %v, want the session given up", err)
	}
}

// A container started by an attempt that failed its gate, or died before it,
// carries the same labels as one that passed. The ledger is what tells them
// apart, so it has to say an attempt is under way before the first change and
// go on saying so until one ends cleanly.
func TestAnAttemptThatDidNotEndCleanlyIsNotTrusted(t *testing.T) {
	t.Setenv("SHUNT_ROOT", t.TempDir())
	captureEvents(t)
	attempt := func(id string) (*release.Ledger, error) {
		t.Helper()
		err := apply(&release.Spec{
			Protocol: release.Protocol, Project: "demo", ID: id, Network: "demo-net",
			Images:   map[string]release.ImageRef{"app": {Ref: "shunt/demo-app:" + id}},
			Services: map[string]release.Service{"app": {Image: "app", Restart: "always"}},
			Order:    []string{"app"},
		})
		l, lerr := loadLedger("demo")
		if lerr != nil {
			t.Fatal(lerr)
		}
		return l, err
	}

	var during string
	creates, deployed, crashing := 0, false, true
	prev := docker
	t.Cleanup(func() { docker = prev })
	docker = runnerFunc(func(_ string, args ...string) (string, error) {
		line := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(line, "create"):
			creates++
			l, _ := loadLedger("demo")
			during = l.Changing
		// Once deployed, what is running matches: so does the container a failed
		// attempt left behind.
		case strings.Contains(line, "label=shunt.inputs=") && deployed:
			return "demo-app\n", nil
		case strings.Contains(line, "label=shunt.release="):
			return "app\n", nil
		case strings.Contains(line, "RestartCount") && crashing:
			return "restarting 3 1\n", nil
		}
		return "", nil
	})

	l, err := attempt("r1")
	if err == nil || during != "r1" || l.Changing != "r1" {
		t.Fatalf("err = %v, marked %q while changing and %q after; want a failure on record as r1 throughout",
			err, during, l.Changing)
	}
	deployed = true
	if _, err = attempt("r2"); err == nil || creates != 2 {
		t.Fatalf("err = %v after %d creates; want the retry to replace the container and fail on it again", err, creates)
	}
	crashing = false
	if l, err = attempt("r3"); err != nil || creates != 3 || l.Changing != "" {
		t.Fatalf("err = %v, %d creates, still marked %q; want a clean deploy to settle the host", err, creates, l.Changing)
	}
	if _, err = attempt("r4"); err != nil || creates != 3 {
		t.Fatalf("err = %v, %d creates; want an unchanged container left running", err, creates)
	}
}
