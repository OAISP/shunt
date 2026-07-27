package release

import "testing"

func ledger(entries ...Entry) *Ledger {
	l := &Ledger{Project: "demo", Releases: entries}
	if len(entries) > 0 {
		l.Current = entries[len(entries)-1].ID
	}
	return l
}

// Previous is what `shunt rollback` with no argument targets, so it must skip
// failed releases — rolling back onto a release that never came up healthy
// would just repeat the outage.
func TestPreviousSkipsFailedReleases(t *testing.T) {
	l := ledger(
		Entry{ID: "r1", Status: "superseded"},
		Entry{ID: "r2", Status: "failed"},
		Entry{ID: "r3", Status: "failed"},
	)
	got := l.Previous()
	if got == nil || got.ID != "r1" {
		t.Fatalf("Previous() = %v, want r1", got)
	}
}

func TestPreviousIgnoresReleasesAfterCurrent(t *testing.T) {
	// After a rollback, Current points at an older entry; the entries recorded
	// after it are not "previous" and must not be offered.
	l := ledger(
		Entry{ID: "r1", Status: "superseded"},
		Entry{ID: "r2", Status: "active"},
		Entry{ID: "r3", Status: "superseded"},
	)
	l.Current = "r2"
	got := l.Previous()
	if got == nil || got.ID != "r1" {
		t.Fatalf("Previous() = %v, want r1", got)
	}
}

func TestPreviousReturnsNilWhenThereIsNoCandidate(t *testing.T) {
	if got := ledger(Entry{ID: "r1", Status: "active"}).Previous(); got != nil {
		t.Fatalf("Previous() = %v, want nil", got)
	}
	if got := (&Ledger{}).Previous(); got != nil {
		t.Fatalf("Previous() on empty ledger = %v, want nil", got)
	}
}

func TestFind(t *testing.T) {
	l := ledger(Entry{ID: "r1"}, Entry{ID: "r2"})
	if got := l.Find("r2"); got == nil || got.ID != "r2" {
		t.Errorf("Find(r2) = %v", got)
	}
	if got := l.Find("nope"); got != nil {
		t.Errorf("Find(nope) = %v, want nil", got)
	}
}

// Both ends hash independently, so equal values must hash equally and the
// output must never contain the input.
func TestHashSecret(t *testing.T) {
	a, b := HashSecret("hunter2"), HashSecret("hunter2")
	if a != b {
		t.Errorf("HashSecret is not deterministic: %q vs %q", a, b)
	}
	if a == HashSecret("hunter3") {
		t.Error("distinct values collided")
	}
	if len(a) != 18 || a[:2] != "h:" {
		t.Errorf("HashSecret = %q, want an 18-char h:-prefixed digest", a)
	}
	if HashSecret("") == "" {
		t.Error("empty value must still hash")
	}
}
