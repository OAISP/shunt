package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/OAISP/shunt/internal/release"
)

// The host's ledger stores hashed secrets while a freshly-resolved spec holds
// plaintext. Comparing them directly reported every key as changed on every
// deploy; diffSecrets must hash before comparing.
func TestDiffSecretsComparesAgainstHashedLedgerValues(t *testing.T) {
	old := &release.Spec{Secrets: map[string]string{
		"KEEP":    release.HashSecret("same"),
		"ROTATED": release.HashSecret("old-value"),
		"DROPPED": release.HashSecret("gone"),
	}}
	nw := &release.Spec{Secrets: map[string]string{
		"KEEP":    "same",
		"ROTATED": "new-value",
		"ADDED":   "brand-new",
	}}

	sc := diffSecrets(old, nw)
	if len(sc.Changed) != 1 || sc.Changed[0] != "ROTATED" {
		t.Errorf("Changed = %v, want [ROTATED]", sc.Changed)
	}
	if len(sc.Added) != 1 || sc.Added[0] != "ADDED" {
		t.Errorf("Added = %v, want [ADDED]", sc.Added)
	}
	if len(sc.Removed) != 1 || sc.Removed[0] != "DROPPED" {
		t.Errorf("Removed = %v, want [DROPPED]", sc.Removed)
	}
	if sc.Total != 3 {
		t.Errorf("Total = %d, want 3", sc.Total)
	}
}

func TestDiffSecretsAgainstFirstDeploy(t *testing.T) {
	sc := diffSecrets(nil, &release.Spec{Secrets: map[string]string{"A": "1", "B": "2"}})
	if len(sc.Added) != 2 || sc.Added[0] != "A" || sc.Added[1] != "B" {
		t.Errorf("Added = %v, want [A B] sorted", sc.Added)
	}
}

// A failed release leaves the host in an unknown state, so an identical
// manifest is still work to do — otherwise `shunt up` refuses to retry.
func TestChangedTreatsFailedCurrentAsWork(t *testing.T) {
	p := &Plan{
		Current:  &release.Entry{ID: "r1", Status: "failed"},
		Images:   []ImageChange{{Name: "app", Action: "unchanged"}},
		Services: []ServiceChange{{Name: "app", Action: "unchanged"}},
	}
	if !p.Changed() {
		t.Error("Changed() = false after a failed release, want true")
	}

	p.Current.Status = "active"
	if p.Changed() {
		t.Error("Changed() = true with everything unchanged and an active release")
	}
}

func TestChangedOnFirstDeploy(t *testing.T) {
	if !(&Plan{}).Changed() {
		t.Error("Changed() = false with no current release, want true")
	}
}

func TestChangedWhenAnAccessoryNeedsBooting(t *testing.T) {
	p := &Plan{
		Current:     &release.Entry{ID: "r1", Status: "active"},
		Services:    []ServiceChange{{Name: "app", Action: "unchanged"}},
		Accessories: []ServiceChange{{Name: "db", Action: "create"}},
	}
	if !p.Changed() {
		t.Error("Changed() = false with an unbooted accessory, want true")
	}
}

// Drift is reported but deliberately NOT applied, so it must not by itself make
// `shunt up` claim there is work to do.
func TestAccessoryDriftDoesNotCountAsChange(t *testing.T) {
	p := &Plan{
		Current:     &release.Entry{ID: "r1", Status: "active"},
		Services:    []ServiceChange{{Name: "app", Action: "unchanged"}},
		Accessories: []ServiceChange{{Name: "db", Action: "drift"}},
	}
	if p.Changed() {
		t.Error("Changed() = true for accessory drift alone, want false")
	}
}

func TestDiffServiceDetectsEnvAndPortChanges(t *testing.T) {
	old := release.Service{Image: "app", Restart: "always", Publish: []string{"80:3000"},
		Env: map[string]string{"KEEP": "1", "DROP": "x", "CHANGE": "a"}}
	nw := release.Service{Image: "app", Restart: "always", Publish: []string{"8080:3000"},
		Env: map[string]string{"KEEP": "1", "CHANGE": "b", "ADD": "y"}}

	reasons := diffService(old, nw)
	joined := ""
	for _, r := range reasons {
		joined += r + "\n"
	}
	for _, want := range []string{"publish", "- env DROP", "+ env ADD=y", "~ env CHANGE=a → b"} {
		if !strings.Contains(joined, want) {
			t.Errorf("reasons missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "env KEEP") {
		t.Errorf("unchanged env reported:\n%s", joined)
	}
}

func TestDiffServiceOnIdenticalServices(t *testing.T) {
	s := release.Service{Image: "app", Restart: "unless-stopped", Env: map[string]string{"A": "1"}}
	if got := diffService(s, s); len(got) != 0 {
		t.Errorf("diffService on identical services = %v, want none", got)
	}
}

// Release ids become container names and immutable image tags, so two deploys
// in the same second must not produce the same id.
func TestNewReleaseIDIsUniqueWithinASecond(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 500; i++ {
		id := NewReleaseID()
		if seen[id] {
			t.Fatalf("duplicate release id %q after %d iterations", id, i)
		}
		seen[id] = true
	}
}

// Ids are sorted lexically by the ledger and by env-file pruning, so the
// timestamp must remain the leading component.
func TestNewReleaseIDSortsByTime(t *testing.T) {
	id := NewReleaseID()
	if len(id) < 15 || id[8] != '-' {
		t.Fatalf("release id %q does not start with a YYYYMMDD-HHMMSS stamp", id)
	}
	if _, err := time.Parse("20060102-150405", id[:15]); err != nil {
		t.Errorf("release id %q has no parseable timestamp prefix: %v", id, err)
	}
}

func TestCachedPercentNeverClaimsAFlatHundred(t *testing.T) {
	// One byte short of complete must not read as "nothing to transfer".
	almost := TransferEstimate{Total: 100_000_000, Missing: 1}
	if got := almost.CachedPercent(); got >= 100 {
		t.Errorf("CachedPercent = %v, want < 100 while bytes remain", got)
	}
	if got := (TransferEstimate{Total: 100, Missing: 0}).CachedPercent(); got != 100 {
		t.Errorf("CachedPercent = %v, want exactly 100 when nothing is missing", got)
	}
	if got := (TransferEstimate{}).CachedPercent(); got != 0 {
		t.Errorf("CachedPercent on an empty estimate = %v, want 0", got)
	}
}

// The module cache stores uppercase letters as "!" plus the lowercase form,
// because module paths are case-sensitive but many file systems are not. Get
// this wrong and a `go install` build cannot find its own source.
func TestEscapeModulePath(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"github.com/OAISP/shunt", "github.com/!o!a!i!s!p/shunt"},
		{"github.com/lowercase/only", "github.com/lowercase/only"},
		{"github.com/BurntSushi/toml", "github.com/!burnt!sushi/toml"},
		{"", ""},
	} {
		if got := escapeModulePath(tc.in); got != tc.want {
			t.Errorf("escapeModulePath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Re-running an ETL and shipping only the new database is a deploy. Until this
// was fixed, `shunt up` reported "nothing to do" and silently left stale data in
// production because no image or service had changed.
func TestArtifactChangeCountsAsWork(t *testing.T) {
	base := func(a ArtifactChange) *Plan {
		return &Plan{
			Current:   &release.Entry{ID: "r1", Status: release.StatusActive},
			Images:    []ImageChange{{Name: "app", Action: "unchanged"}},
			Services:  []ServiceChange{{Name: "app", Action: "unchanged"}},
			Artifacts: []ArtifactChange{a},
		}
	}

	same := ArtifactChange{Name: "db", LocalBytes: 100, LocalMTime: 5, HostBytes: 100, HostMTime: 5}
	if base(same).Changed() {
		t.Error("identical artifact counted as work")
	}
	if got := same.Action(); got != "unchanged" {
		t.Errorf("Action = %q, want unchanged", got)
	}

	for name, a := range map[string]ArtifactChange{
		"absent on host":  {LocalBytes: 100, LocalMTime: 5, HostBytes: -1},
		"different size":  {LocalBytes: 200, LocalMTime: 5, HostBytes: 100, HostMTime: 5},
		"same size newer": {LocalBytes: 100, LocalMTime: 9, HostBytes: 100, HostMTime: 5},
	} {
		if !base(a).Changed() {
			t.Errorf("%s: not counted as work", name)
		}
		if !a.Differs() {
			t.Errorf("%s: Differs() = false", name)
		}
	}
}

func TestArtifactAction(t *testing.T) {
	if got := (ArtifactChange{HostBytes: -1}).Action(); got != "create" {
		t.Errorf("absent artifact Action = %q, want create", got)
	}
	if got := (ArtifactChange{LocalBytes: 2, HostBytes: 1}).Action(); got != "replace" {
		t.Errorf("differing artifact Action = %q, want replace", got)
	}
}
