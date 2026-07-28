package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/OAISP/shunt/internal/release"
)

const testSalt = "0123456789abcdef"

// The host's ledger stores hashed secrets while a freshly-resolved spec holds
// plaintext. Comparing them directly reported every key as changed on every
// deploy; diffSecrets must hash before comparing.
func TestDiffSecretsComparesAgainstHashedLedgerValues(t *testing.T) {
	old := &release.Spec{Secrets: map[string]string{
		"KEEP":    release.HashSecret(testSalt, "same"),
		"ROTATED": release.HashSecret(testSalt, "old-value"),
		"DROPPED": release.HashSecret(testSalt, "gone"),
	}}
	nw := &release.Spec{Secrets: map[string]string{
		"KEEP":    "same",
		"ROTATED": "new-value",
		"ADDED":   "brand-new",
	}}

	sc := diffSecrets(old, nw, testSalt)
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
	sc := diffSecrets(nil, &release.Spec{Secrets: map[string]string{"A": "1", "B": "2"}}, testSalt)
	if len(sc.Added) != 2 || sc.Added[0] != "A" || sc.Added[1] != "B" {
		t.Errorf("Added = %v, want [A B] sorted", sc.Added)
	}
}

// Drift is measured against what the host recorded as *applied*, not against
// the last release's spec.
//
// The regression: `up` records the manifest it was handed even though it
// deliberately leaves an existing accessory alone. Diffing against that made a
// reported drift disappear after any unrelated deploy, while the container went
// on running the old config — the warning vanished and the problem did not.
func TestAccessoryDriftSurvivesAnUnrelatedDeploy(t *testing.T) {
	running := release.Service{Image: "postgres:17-alpine", Restart: "unless-stopped"}
	wanted := release.Service{Image: "postgres:18-alpine", Restart: "unless-stopped"}

	// The previous deploy already recorded the *new* definition in its spec,
	// which is exactly the state that used to hide the drift.
	oldSpec := &release.Spec{Accessories: map[string]release.Service{"db": wanted}}
	state := &RemoteState{Ledger: &release.Ledger{
		Current:     "r1",
		Releases:    []release.Entry{{ID: "r1", Status: release.StatusActive, Spec: oldSpec}},
		Accessories: map[string]string{"db": release.HashService(running)},
	}}
	spec := &release.Spec{
		ID:          "r2",
		Accessories: map[string]release.Service{"db": wanted},
		Services:    map[string]release.Service{},
	}

	p := &Plan{}
	e := &Engine{}
	got := e.planAccessories(p, spec, state, oldSpec)
	if len(got) != 1 {
		t.Fatalf("planAccessories returned %d entries, want 1", len(got))
	}
	if got[0].Action != "drift" {
		t.Fatalf("accessory action = %q, want drift — the container still runs the old image", got[0].Action)
	}
}

// Once `shunt boot` has actually applied the definition, the drift is resolved
// and must stop being reported.
func TestAccessoryIsUnchangedOnceApplied(t *testing.T) {
	svc := release.Service{Image: "postgres:18-alpine", Restart: "unless-stopped"}
	state := &RemoteState{Ledger: &release.Ledger{
		Accessories: map[string]string{"db": release.HashService(svc)},
	}}
	spec := &release.Spec{Accessories: map[string]release.Service{"db": svc}}

	got := (&Engine{}).planAccessories(&Plan{}, spec, state, nil)
	if got[0].Action != "unchanged" {
		t.Fatalf("accessory action = %q, want unchanged", got[0].Action)
	}
}

// Found by deploying for real: adding a migration stage to the manifest moved
// nothing else, so Changed() returned false, `shunt up` reported "nothing to
// do", and the migration silently never ran.
func TestAddingAStageCountsAsWork(t *testing.T) {
	old := &release.Spec{Stages: []release.Stage{}}
	nw := &release.Spec{Stages: []release.Stage{
		{Name: "migrate", Image: "app", Command: []string{"npm", "run", "migrate"}},
	}}

	p := &Plan{
		Current: &release.Entry{ID: "r1", Status: release.StatusActive},
		Stages:  diffStages(old, nw),
	}
	if p.Stages[0].Action != "create" {
		t.Fatalf("stage action = %q, want create", p.Stages[0].Action)
	}
	if !p.Changed() {
		t.Fatal("adding a stage was not treated as work — the migration would never run")
	}
}

func TestChangingAStageCommandCountsAsWork(t *testing.T) {
	old := &release.Spec{Stages: []release.Stage{{Name: "migrate", Command: []string{"old"}}}}
	nw := &release.Spec{Stages: []release.Stage{{Name: "migrate", Command: []string{"new"}}}}

	got := diffStages(old, nw)
	if got[0].Action != "update" {
		t.Fatalf("stage action = %q, want update", got[0].Action)
	}
}

// An unchanged pipeline is not itself a reason to deploy; it just runs when
// something else triggers one.
func TestAnUnchangedStagePipelineIsNotWork(t *testing.T) {
	st := []release.Stage{{Name: "migrate", Command: []string{"same"}}}
	p := &Plan{
		Current: &release.Entry{ID: "r1", Status: release.StatusActive},
		Stages:  diffStages(&release.Spec{Stages: st}, &release.Spec{Stages: st}),
	}
	if p.Stages[0].Action != "run" {
		t.Fatalf("stage action = %q, want run", p.Stages[0].Action)
	}
	if p.Changed() {
		t.Fatal("an unchanged stage pipeline should not force a deploy")
	}
}

func TestRemovedStageIsReported(t *testing.T) {
	old := &release.Spec{Stages: []release.Stage{{Name: "migrate"}, {Name: "backup"}}}
	nw := &release.Spec{Stages: []release.Stage{{Name: "migrate"}}}

	got := diffStages(old, nw)
	if len(got) != 2 || got[1].Name != "backup" || got[1].Action != "remove" {
		t.Fatalf("diffStages = %+v, want backup marked removed", got)
	}
}

func deployed(l *release.Ledger) *release.Ledger {
	if l == nil {
		l = &release.Ledger{Current: "r1"}
	}
	return l
}

// A plan built from the ledger alone describes what shunt last did, not what
// the host is doing. Deleting a container by hand used to read as "unchanged".
func TestReconcileReportsAMissingContainer(t *testing.T) {
	state := &RemoteState{Ledger: deployed(nil)}
	got := reconcileService(state, "app", release.Service{Image: "app"})
	if len(got) != 1 || !strings.Contains(got[0], "removed outside shunt") {
		t.Fatalf("reconcileService = %v, want a missing-container reason", got)
	}
}

func TestReconcileReportsAStoppedContainer(t *testing.T) {
	svc := release.Service{Image: "app"}
	state := &RemoteState{
		Ledger: deployed(nil),
		Containers: []Container{{
			Name: "demo-app", Service: "app", State: "exited",
			Config: release.HashService(svc),
		}},
	}
	got := reconcileService(state, "app", svc)
	if len(got) != 1 || !strings.Contains(got[0], "exited") {
		t.Fatalf("reconcileService = %v, want a not-running reason", got)
	}
}

// A container someone replaced by hand carries a different config hash.
func TestReconcileReportsAReplacedContainer(t *testing.T) {
	state := &RemoteState{
		Ledger: deployed(nil),
		Containers: []Container{{
			Name: "demo-app", Service: "app", State: "running", Config: "h:somethingelse",
		}},
	}
	got := reconcileService(state, "app", release.Service{Image: "app"})
	if len(got) != 1 || !strings.Contains(got[0], "different configuration") {
		t.Fatalf("reconcileService = %v, want a config-mismatch reason", got)
	}
}

func TestReconcileIsQuietWhenTheHostMatches(t *testing.T) {
	svc := release.Service{Image: "app", Restart: "unless-stopped"}
	state := &RemoteState{
		Ledger: deployed(nil),
		Containers: []Container{{
			Name: "demo-app", Service: "app", State: "running", Config: release.HashService(svc),
		}},
	}
	if got := reconcileService(state, "app", svc); got != nil {
		t.Fatalf("reconcileService = %v, want nil", got)
	}
}

// Containers created before shunt wrote a config label must not all suddenly
// report drift the first time an existing host is planned.
func TestReconcileToleratesAnUnlabelledContainer(t *testing.T) {
	state := &RemoteState{
		Ledger:     deployed(nil),
		Containers: []Container{{Name: "demo-app", Service: "app", State: "running"}},
	}
	if got := reconcileService(state, "app", release.Service{Image: "app"}); got != nil {
		t.Fatalf("reconcileService = %v, want nil for an unlabelled container", got)
	}
}

// Before the first deploy there is nothing to reconcile against; the service
// diff already calls that a create.
func TestReconcileIsQuietBeforeTheFirstDeploy(t *testing.T) {
	if got := reconcileService(&RemoteState{Ledger: &release.Ledger{}}, "app", release.Service{}); got != nil {
		t.Fatalf("reconcileService = %v, want nil", got)
	}
}

// An orphan is reported but never acted on, so it must not make the plan
// permanently dirty with a change `shunt up` will never make.
func TestOrphanedServiceIsNotCountedAsWork(t *testing.T) {
	state := &RemoteState{
		Ledger:     deployed(nil),
		Containers: []Container{{Name: "demo-worker", Service: "worker", State: "running"}},
	}
	sc := orphanChange(state, "worker")
	if sc.Action != "orphaned" {
		t.Fatalf("action = %q, want orphaned", sc.Action)
	}
	if !strings.Contains(sc.Reasons[0], "shunt retire worker") {
		t.Fatalf("reason should name the fix, got %q", sc.Reasons[0])
	}

	p := &Plan{
		Current:  &release.Entry{ID: "r1", Status: release.StatusActive},
		Services: []ServiceChange{sc},
	}
	if p.Changed() {
		t.Fatal("an orphaned service made the plan dirty; it would never come clean")
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
