package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/OAISP/shunt/internal/build"
	"github.com/OAISP/shunt/internal/manifest"
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
	state := &RemoteState{Containers: dbContainer("running"), Ledger: &release.Ledger{
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
	state := &RemoteState{Containers: dbContainer("running"), Ledger: &release.Ledger{
		Accessories: map[string]string{"db": release.HashService(svc)},
	}}
	spec := &release.Spec{Accessories: map[string]release.Service{"db": svc}}

	got := (&Engine{}).planAccessories(&Plan{}, spec, state, nil)
	if got[0].Action != "unchanged" {
		t.Fatalf("accessory action = %q, want unchanged", got[0].Action)
	}
}

func dbContainer(state string) []Container {
	return []Container{{Name: "demo-db", Service: "db", Kind: "accessory", State: state}}
}

// The ledger records that an accessory was booted, not that it is still there.
// A deploy boots one that is gone and starts one that stopped, so the plan has
// to call both work rather than "running".
func TestAnAccessoryThatIsGoneOrStoppedIsWork(t *testing.T) {
	svc := release.Service{Image: "postgres:18-alpine", Restart: "unless-stopped"}
	spec := &release.Spec{Accessories: map[string]release.Service{"db": svc}}
	ledger := &release.Ledger{
		Current:     "r1",
		Releases:    []release.Entry{{ID: "r1", Status: release.StatusActive}},
		Accessories: map[string]string{"db": release.HashService(svc)},
	}
	for want, containers := range map[string][]Container{"create": nil, "start": dbContainer("exited")} {
		state := &RemoteState{Ledger: ledger, Containers: containers}
		p := &Plan{Current: &ledger.Releases[0]}
		p.Accessories = (&Engine{}).planAccessories(p, spec, state, nil)
		if got := p.Accessories[0].Action; got != want {
			t.Errorf("accessory action = %q, want %q", got, want)
		}
		if !p.Changed() {
			t.Errorf("a plan whose accessory needs a %s reported nothing to do", want)
		}
	}
}

// No service definition mentions a secret's value, so a rotation shows up
// nowhere else in the plan — and used to print "value changed", then "nothing
// to do".
func TestASecretChangeIsWork(t *testing.T) {
	p := &Plan{Current: &release.Entry{Status: release.StatusActive}}
	if p.Changed() {
		t.Fatal("an empty plan reported work")
	}
	p.Secrets.Changed = []string{"DATABASE_URL"}
	if !p.Changed() {
		t.Error("a rotated secret was not counted as a change")
	}
}

// One deploy after a service is dropped, the last release no longer mentions
// it, and only its container says it is still running.
func TestAnOrphanIsFoundFromItsContainer(t *testing.T) {
	spec := &release.Spec{ID: "r3", Services: map[string]release.Service{"app": {Image: "app"}}}
	state := &RemoteState{
		Ledger: &release.Ledger{Current: "r2", Releases: []release.Entry{{ID: "r2", Status: release.StatusActive, Spec: spec}}},
		Containers: []Container{
			{Name: "demo-app", Service: "app", Kind: "service", State: "running", Config: release.HashService(spec.Services["app"])},
			{Name: "demo-worker", Service: "worker", Kind: "service", State: "running"},
		},
	}
	p, err := (&Engine{}).BuildPlan(t.Context(), spec, nil, state)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Services) != 2 || p.Services[1].Name != "worker" || p.Services[1].Action != "orphaned" {
		t.Errorf("services = %+v, want worker reported as orphaned", p.Services)
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
	if len(got) != 1 || !strings.Contains(got[0], "no container on the host") {
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

// Settings that belong to the release rather than to a service change how every
// container is created but appear in no service diff. Switching secrets from
// environment to files silently did nothing until something else forced a
// deploy — the same shape of bug stages had.
func TestReleaseWideSettingChangesCountAsWork(t *testing.T) {
	old := &release.Spec{Network: "demo-net"}
	nw := &release.Spec{Network: "demo-net", SecretMode: "file"}

	p := &Plan{
		Current: &release.Entry{ID: "r1", Status: release.StatusActive},
		Release: diffRelease(old, nw),
	}
	if len(p.Release) != 1 || !strings.Contains(p.Release[0], "secret delivery") {
		t.Fatalf("Release = %v, want the secret mode change", p.Release)
	}
	if !p.Changed() {
		t.Fatal("switching secret delivery was not treated as work")
	}
}

func TestRenamingTheNetworkCountsAsWork(t *testing.T) {
	got := diffRelease(&release.Spec{Network: "old-net"}, &release.Spec{Network: "new-net"})
	if len(got) != 1 || !strings.Contains(got[0], "network") {
		t.Fatalf("diffRelease = %v, want the network change", got)
	}
}

func TestUnchangedReleaseSettingsAreNotWork(t *testing.T) {
	same := &release.Spec{Network: "demo-net", SecretMode: "file"}
	if got := diffRelease(same, same); len(got) != 0 {
		t.Fatalf("diffRelease = %v, want none", got)
	}
	// A first deploy has nothing to compare against.
	if got := diffRelease(nil, same); len(got) != 0 {
		t.Fatalf("diffRelease against no previous release = %v, want none", got)
	}
}

// GitLab checks out a detached HEAD and runs as a service account, so without
// the CI's own variables a release would record no branch and the wrong person.
func TestProvenanceFromGitLabCI(t *testing.T) {
	env := map[string]string{
		"GITLAB_CI": "true", "GITLAB_USER_LOGIN": "ada", "CI_COMMIT_REF_NAME": "main",
		"CI_COMMIT_SHA": "0123456789abcdef0123", "CI_JOB_URL": "https://gitlab.example.com/acme/-/jobs/42",
	}
	p := release.Provenance{Deployer: "gitlab-runner@ci-host"}
	fromCI(&p, func(k string) string { return env[k] })
	if p.Deployer != "ada@gitlab-ci" || p.Branch != "main" || p.Short != "0123456789ab" || p.Run != env["CI_JOB_URL"] {
		t.Errorf("provenance = %+v", p)
	}

	// Git's answer wins where it has one.
	p = release.Provenance{Branch: "release-1", Commit: "feedface", Short: "feedface"}
	fromCI(&p, func(k string) string { return env[k] })
	if p.Branch != "release-1" || p.Commit != "feedface" {
		t.Errorf("CI variables overrode git: %+v", p)
	}
}

func TestProvenanceOutsideCIIsUntouched(t *testing.T) {
	p := release.Provenance{Deployer: "me@laptop"}
	fromCI(&p, func(string) string { return "" })
	if p.Deployer != "me@laptop" || p.Run != "" {
		t.Errorf("provenance changed outside CI: %+v", p)
	}
}

// Sized from the delta: a few KB of new layer must not be refused because the
// whole image would not fit twice.
func TestPreflightSpaceIsSizedFromTheDelta(t *testing.T) {
	e := &Engine{M: &manifest.Manifest{Host: "deploy@example.com"}}
	small := &TransferEstimate{Total: 2 << 30, Missing: 5 << 10}
	if err := e.PreflightSpace(small, &release.Disk{StoreFree: 1 << 30, DockerFree: 1 << 30}); err != nil {
		t.Errorf("a 5 KB delta was refused on a host with 1 GB free: %v", err)
	}

	big := &TransferEstimate{Total: 2 << 30, Missing: 1 << 30}
	if err := e.PreflightSpace(big, &release.Disk{StoreFree: 10 << 30, DockerFree: 2 << 30}); err == nil ||
		!strings.Contains(err.Error(), "docker's data root") {
		t.Errorf("1 GB to unpack into 2 GB of docker root was allowed, or blamed the wrong place: %v", err)
	}
	// On one filesystem the store and the daemon draw on the same free space.
	if err := e.PreflightSpace(big, &release.Disk{StoreFree: 3 << 30, DockerFree: 3 << 30, Shared: true}); err == nil {
		t.Error("store and daemon needs on one filesystem were not added up")
	}
	if err := e.PreflightSpace(big, nil); err != nil {
		t.Errorf("unknown disk state refused a deploy: %v", err)
	}
}

func TestEstimateTransferCountsWhatTheStoreLacks(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "app")
	blobs := filepath.Join(dir, "blobs", "sha256")
	if err := os.MkdirAll(blobs, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"have": "123", "need": "12345"} {
		if err := os.WriteFile(filepath.Join(blobs, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	est, err := EstimateTransfer(map[string]*build.Result{"app": {Dir: dir}},
		&RemoteState{Store: map[string][]string{"app": {"have"}}})
	if err != nil {
		t.Fatal(err)
	}
	if est.Total != 8 || est.Missing != 5 || est.Blobs != 1 {
		t.Errorf("estimate = %+v, want total 8, missing 5 in 1 blob", est)
	}
}

func TestMemoryChangeIsAServiceChange(t *testing.T) {
	plain := release.Service{Image: "app"}
	limited := release.Service{Image: "app", Limits: release.Limits{Memory: "1g", MemorySwap: "1536m"}}
	if r := diffService(plain, limited); !slices.Equal(r, []string{"memory unset → 1g", "memory_swap unset → 1536m"}) {
		t.Errorf("reasons = %v, want both limits reported", r)
	}
	logged := release.Service{Image: "app", Limits: release.Limits{LogOpts: map[string]string{"max-size": "10m"}, PidsLimit: 100}}
	if r := diffService(plain, logged); !slices.Equal(r, []string{"pids_limit unset → 100", "log_opts unset → map[max-size:10m]"}) {
		t.Errorf("reasons = %v, want every limit reported under its manifest key", r)
	}
	if release.HashService(plain) == release.HashService(limited) {
		t.Error("a limit did not change the hash, so plan would not see it")
	}
	// Containers created before limits existed carry this hash in a label; if it
	// moved, upgrading shunt alone would read as drift on every service.
	if got := release.HashService(plain); got != "h:0496518af39e0f84" {
		t.Errorf("hash of a service with no limits = %s; it must not move", got)
	}
}

func TestInitChangeIsAServiceChange(t *testing.T) {
	if r := diffService(release.Service{Image: "app"}, release.Service{Image: "app", Init: true}); len(r) != 1 {
		t.Errorf("reasons = %v, want the init change reported", r)
	}
	// Absent from an older spec's hash, so existing containers are not drifted.
	if release.HashService(release.Service{Image: "app"}) != release.HashService(release.Service{Image: "app", Init: false}) {
		t.Error("init = false changed the hash of an existing service")
	}
}

// What `up` does to a service has to be what `plan` said: a container is left
// running unless its definition, its image, a secret it is handed or the data
// under it changed.
func TestPlanSaysWhichServicesADeployReplaces(t *testing.T) {
	web := release.Service{Image: "app", Secrets: []string{"DATABASE_URL"}}
	side := release.Service{Image: "app", Secrets: []string{"QUEUE_URL"}}
	images := func(digest string) map[string]release.ImageRef {
		return map[string]release.ImageRef{"app": {Ref: "r", Digest: digest, Fingerprint: "f1"}}
	}
	old := &release.Spec{
		Images:   images("sha256:one"),
		Services: map[string]release.Service{"web": web, "side": side},
		Secrets: map[string]string{
			"DATABASE_URL": release.HashSecret(testSalt, "db"),
			"QUEUE_URL":    release.HashSecret(testSalt, "queue"),
		},
	}
	state := func(inputs string) *RemoteState {
		return &RemoteState{
			Ledger: &release.Ledger{Current: "r1", Salt: testSalt, Releases: []release.Entry{
				{ID: "r1", Status: release.StatusActive, Spec: old, Images: old.Images}}},
			Containers: []Container{
				{Name: "demo-web", Service: "web", Kind: "service", State: "running", Config: release.HashService(web), Inputs: inputs},
				{Name: "demo-side", Service: "side", Kind: "service", State: "running", Config: release.HashService(side), Inputs: inputs},
			},
			Paths: map[string]release.PathState{"/srv/data.db": {Size: 10, MTime: 5}},
		}
	}
	// A relabelled image: a new digest over the same fingerprint.
	next := func() *release.Spec {
		return &release.Spec{
			ID: "r2", Images: images("sha256:two"), Services: old.Services,
			Secrets:   map[string]string{"DATABASE_URL": "db", "QUEUE_URL": "queue"},
			Artifacts: []release.Artifact{{Name: "data", Dest: "/srv/data.db", Bytes: 10, MTime: 5}},
		}
	}
	actions := func(spec *release.Spec, st *RemoteState) map[string]string {
		t.Helper()
		p, err := (&Engine{}).BuildPlan(t.Context(), spec, nil, st)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{"changed": fmt.Sprint(p.HasChanges)}
		for _, s := range p.Services {
			out[s.Name] = s.Action
		}
		return out
	}
	check := func(name string, got map[string]string, web, side, changed string) {
		t.Helper()
		if got["web"] != web || got["side"] != side || got["changed"] != changed {
			t.Errorf("%s: web=%s side=%s changed=%s, want %s %s %s", name, got["web"], got["side"], got["changed"], web, side, changed)
		}
	}

	check("a relabelled image", actions(next(), state("i")), "unchanged", "unchanged", "false")

	rotated := next()
	rotated.Secrets["DATABASE_URL"] = "rotated"
	check("a secret only web is sent", actions(rotated, state("i")), "update", "unchanged", "true")

	data := next()
	data.Artifacts[0].MTime = 6
	check("an artifact", actions(data, state("i")), "update", "update", "true")

	// Started before inputs were recorded: replaced once, but only by a deploy
	// that was happening anyway.
	check("an older container, nothing to do", actions(next(), state("")), "unchanged", "unchanged", "false")
	check("an older container, riding a deploy", actions(rotated, state("")), "update", "update", "true")

	// A bundle planned where its secrets cannot be resolved has none to compare,
	// which must not read as every one of them having been removed.
	blind := next()
	blind.Secrets = nil
	p, err := (&Engine{SecretsUnknown: true}).BuildPlan(t.Context(), blind, nil, state("i"))
	if err != nil || p.HasChanges || !p.SecretsUnknown {
		t.Errorf("unresolved secrets: changes = %v, unknown = %v, err = %v; want a clean plan that says so", p.HasChanges, p.SecretsUnknown, err)
	}
}

// Rollback is the safe choice until the release it leaves rotated a secret or
// migrated a database, and neither shows unless it is said before the prompt.
func TestRollbackCautions(t *testing.T) {
	target := &release.Entry{ID: "r1", Spec: &release.Spec{Secrets: map[string]string{"DB": "h:old", "KEEP": "h:same"}}}
	cur := &release.Entry{ID: "r2", Spec: &release.Spec{Secrets: map[string]string{"DB": "h:new", "KEEP": "h:same", "ADDED": "h:x"}}}

	got := RollbackCautions(cur, target)
	if len(got) != 1 || !strings.Contains(got[0], "DB") || strings.Contains(got[0], "KEEP") || strings.Contains(got[0], "h:") {
		t.Errorf("cautions = %q, want one naming DB alone and no value", got)
	}
	cur.Irreversible = "migrate"
	if got := RollbackCautions(cur, target); len(got) != 2 || !strings.Contains(got[1], "migrate") {
		t.Errorf("cautions = %q, want the irreversible stage named too", got)
	}
	cur.Irreversible, cur.Spec.Secrets["DB"] = "", "h:old"
	if got := RollbackCautions(cur, target); got != nil {
		t.Errorf("cautions = %q for a rollback with nothing to warn about", got)
	}
}
