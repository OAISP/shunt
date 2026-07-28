package release

import (
	"fmt"
	"slices"
	"testing"
)

func ledger(entries ...Entry) *Ledger {
	l := &Ledger{Project: "demo", Releases: entries}
	if len(entries) > 0 {
		l.Current = entries[len(entries)-1].ID
	}
	return l
}

// good builds a restorable entry: reached a healthy state and kept its spec.
func good(id string) Entry {
	return Entry{ID: id, Status: StatusSuperseded, Spec: &Spec{ID: id}}
}

func bad(id string) Entry { return Entry{ID: id, Status: StatusFailed, Spec: &Spec{ID: id}} }

func ids(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// The regression this whole retention path exists to prevent: a good release
// followed by a run of failed deploys must still be restorable. Counting the
// failures toward `retain` would evict it — and the operator only finds out when
// they try to roll back, which is the worst possible moment to find out.
func TestKeepIDsDoesNotCountFailedReleases(t *testing.T) {
	l := ledger(good("r1"), bad("r2"), bad("r3"), bad("r4"), bad("r5"), bad("r6"))
	keep := l.KeepIDs(5)
	if !keep["r1"] {
		t.Fatalf("KeepIDs dropped the last good release; kept %v", ids(keep))
	}
}

// Whatever is currently active is kept regardless of how it is classified —
// its containers are running right now.
func TestKeepIDsAlwaysKeepsCurrent(t *testing.T) {
	l := ledger(good("r1"), bad("r2"))
	if keep := l.KeepIDs(5); !keep["r2"] {
		t.Fatalf("KeepIDs dropped the current release; kept %v", ids(keep))
	}
}

func TestKeepIDsBoundsToRetain(t *testing.T) {
	l := ledger(good("r1"), good("r2"), good("r3"), good("r4"))
	keep := l.KeepIDs(2)
	// The newest two restorable, plus current — which is r4, already among them.
	if got, want := ids(keep), []string{"r3", "r4"}; !slices.Equal(got, want) {
		t.Fatalf("KeepIDs(2) = %v, want %v", got, want)
	}
}

// An entry with no retained spec cannot be replayed, so it is not a rollback
// target and must not consume a retain slot.
func TestKeepIDsSkipsEntriesWithoutASpec(t *testing.T) {
	l := ledger(good("r1"), Entry{ID: "r2", Status: StatusSuperseded}, good("r3"))
	keep := l.KeepIDs(2)
	if !keep["r1"] {
		t.Fatalf("a spec-less entry consumed a retain slot; kept %v", ids(keep))
	}
}

// Trimming the history must not be another way to lose the last good release.
func TestTrimKeepsRestorableReleasesOutsideTheWindow(t *testing.T) {
	entries := []Entry{good("r00")}
	for i := 1; i <= 12; i++ {
		entries = append(entries, bad(fmt.Sprintf("r%02d", i)))
	}
	l := ledger(entries...)
	l.Trim(2) // window of 4 — r00 is far outside it

	if l.Find("r00") == nil {
		t.Fatalf("Trim evicted the last good release; kept %v", entryIDs(l))
	}
	if l.Find("r12") == nil {
		t.Fatalf("Trim evicted the current release; kept %v", entryIDs(l))
	}
}

func TestTrimIsAnOrderPreservingNoOpBelowTheWindow(t *testing.T) {
	l := ledger(good("r1"), good("r2"), good("r3"))
	l.Trim(5)
	if got, want := entryIDs(l), []string{"r1", "r2", "r3"}; !slices.Equal(got, want) {
		t.Fatalf("Trim = %v, want %v", got, want)
	}
}

func TestTrimPreservesChronologicalOrder(t *testing.T) {
	entries := []Entry{good("r00")}
	for i := 1; i <= 10; i++ {
		entries = append(entries, bad(fmt.Sprintf("r%02d", i)))
	}
	l := ledger(entries...)
	l.Trim(2)
	got := entryIDs(l)
	if !slices.IsSorted(got) {
		t.Fatalf("Trim reordered the history: %v", got)
	}
}

func entryIDs(l *Ledger) []string {
	out := make([]string, 0, len(l.Releases))
	for i := range l.Releases {
		out = append(out, l.Releases[i].ID)
	}
	return out
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
	const salt = "0123456789abcdef"
	a, b := HashSecret(salt, "hunter2"), HashSecret(salt, "hunter2")
	if a != b {
		t.Errorf("HashSecret is not deterministic: %q vs %q", a, b)
	}
	if a == HashSecret(salt, "hunter3") {
		t.Error("distinct values collided")
	}
	if len(a) != 18 || a[:2] != "h:" {
		t.Errorf("HashSecret = %q, want an 18-char h:-prefixed digest", a)
	}
	if HashSecret(salt, "") == "" {
		t.Error("empty value must still hash")
	}
	// The salt is the point: the same value on another host must not produce a
	// digest an attacker can recognise from a rainbow table built elsewhere.
	if a == HashSecret("fedcba9876543210", "hunter2") {
		t.Error("hashes did not depend on the salt")
	}
}

func TestNewSaltIsRandomAndHexEncoded(t *testing.T) {
	a, err := NewSalt()
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewSalt()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Error("NewSalt returned the same value twice")
	}
	if len(a) != 32 {
		t.Errorf("NewSalt = %q, want 32 hex chars", a)
	}
}

// Accessory drift is detected by comparing definition hashes, so the hash has
// to be stable for an unchanged definition and move for a changed one.
func TestHashService(t *testing.T) {
	a := Service{Image: "postgres:18-alpine", Volumes: []string{"pg:/var/lib/postgresql"}}
	if HashService(a) != HashService(a) {
		t.Error("HashService is not deterministic")
	}
	b := a
	b.Image = "postgres:17-alpine"
	if HashService(a) == HashService(b) {
		t.Error("a changed image did not change the hash")
	}
}
