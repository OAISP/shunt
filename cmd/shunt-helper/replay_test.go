package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/OAISP/shunt/internal/release"
)

// ledgerEntry builds the shape a rollback actually reads: a spec whose secret
// values have already been replaced by hashes, because that is all the ledger
// ever holds.
func ledgerEntry(mode string, services map[string]release.Service, keys ...string) *release.Entry {
	redacted := map[string]string{}
	for _, k := range keys {
		redacted[k] = release.HashSecret("salt", "value-of-"+k)
	}
	return &release.Entry{
		ID:     testRelease,
		Status: release.StatusSuperseded,
		Spec: &release.Spec{
			Project: "demo", ID: testRelease, Network: "demo-net",
			Images:     map[string]release.ImageRef{"db": {Ref: "postgres:17", External: true}},
			Services:   services,
			SecretMode: mode,
			Secrets:    redacted,
		},
	}
}

// The bug this exists to prevent: replaying a ledger spec verbatim starts the
// containers with "h:3f2a…" where the password should be. Every path that
// recreates a container rewrites the secrets from the spec it is handed, so the
// plaintext has to be read back off the host first.
func TestReplaySpecRestoresPlaintextFromTheEnvFile(t *testing.T) {
	t.Setenv("SHUNT_ROOT", t.TempDir())
	entry := ledgerEntry("", map[string]release.Service{"app": {Image: "app"}}, "DATABASE_URL")

	// What the original deploy left behind.
	if _, err := writeEnvFileAt(&release.Spec{
		Project: "demo", ID: testRelease,
		Secrets: map[string]string{"DATABASE_URL": "postgres://real"},
	}, envFilePath("demo", testRelease)); err != nil {
		t.Fatal(err)
	}

	spec, err := replaySpec(entry)
	if err != nil {
		t.Fatal(err)
	}
	if got := spec.Secrets["DATABASE_URL"]; got != "postgres://real" {
		t.Fatalf("DATABASE_URL = %q, want the plaintext the release was deployed with", got)
	}
	// The ledger must keep holding hashes; the correction is for this replay only.
	if entry.Spec.Secrets["DATABASE_URL"] == "postgres://real" {
		t.Fatal("replaySpec wrote plaintext back into the ledger's copy of the spec")
	}
}

// File mode is where replaying the hashes was destructive as well as wrong:
// startContainer rewrites the directory from the spec, so it would have
// overwritten the one plaintext copy on the host with the hashes.
func TestReplaySpecRestoresPlaintextFromScopedSecretDirs(t *testing.T) {
	t.Setenv("SHUNT_ROOT", t.TempDir())
	entry := ledgerEntry("file", map[string]release.Service{
		"web":    {Image: "app", Secrets: []string{"STRIPE_KEY"}},
		"worker": {Image: "app", Secrets: []string{"DATABASE_URL"}},
	}, "STRIPE_KEY", "DATABASE_URL")

	// Every service narrowed its secrets, so the host holds two scoped
	// directories and no unscoped one — the case that used to report a release
	// as pruned when nothing had been pruned.
	deployed := &release.Spec{
		Project: "demo", ID: testRelease, SecretMode: "file",
		Secrets: map[string]string{"STRIPE_KEY": "sk_live", "DATABASE_URL": "postgres://real"},
	}
	for _, scope := range [][]string{{"STRIPE_KEY"}, {"DATABASE_URL"}} {
		if _, err := writeSecretFiles(deployed, scope); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(secretsDir("demo", testRelease, nil)); !os.IsNotExist(err) {
		t.Fatal("this test is meaningless unless the unscoped directory is absent")
	}

	spec, err := replaySpec(entry)
	if err != nil {
		t.Fatalf("a release whose services all narrowed their secrets could not be replayed: %v", err)
	}
	for k, want := range map[string]string{"STRIPE_KEY": "sk_live", "DATABASE_URL": "postgres://real"} {
		if got := spec.Secrets[k]; got != want {
			t.Fatalf("%s = %q, want %q", k, got, want)
		}
	}
}

// A release whose secrets have aged out cannot be replayed, and saying so is
// the whole point — starting containers holding hashes would "succeed".
func TestReplaySpecRefusesWhenTheSecretsArePruned(t *testing.T) {
	t.Setenv("SHUNT_ROOT", t.TempDir())
	entry := ledgerEntry("", map[string]release.Service{"app": {Image: "app"}}, "DATABASE_URL")

	if _, err := replaySpec(entry); err == nil {
		t.Fatal("replaySpec accepted a release whose only plaintext copy is gone")
	}
}

// The stored spec marks images external because that is how they were described
// at build time. A replay must not act on that, and must not persist the
// correction either.
func TestReplaySpecClearsExternalWithoutTouchingTheLedger(t *testing.T) {
	t.Setenv("SHUNT_ROOT", t.TempDir())
	entry := ledgerEntry("", map[string]release.Service{"app": {Image: "db"}})

	spec, err := replaySpec(entry)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Images["db"].External {
		t.Fatal("a replayed image would be pulled again instead of reused from this host")
	}
	if !entry.Spec.Images["db"].External {
		t.Fatal("replaySpec rewrote the ledger's record of how the image was obtained")
	}
}

// A release with no secrets has no env-file, and never had one.
func TestReplaySpecAcceptsAReleaseWithNoSecrets(t *testing.T) {
	t.Setenv("SHUNT_ROOT", t.TempDir())
	entry := ledgerEntry("", map[string]release.Service{"app": {Image: "app"}})

	if _, err := replaySpec(entry); err != nil {
		t.Fatalf("a release that never had secrets could not be replayed: %v", err)
	}
}

// Values are written verbatim and read back verbatim; anything else silently
// changes a password.
func TestEnvFileRoundTripsAwkwardValues(t *testing.T) {
	t.Setenv("SHUNT_ROOT", t.TempDir())
	want := map[string]string{
		"WITH_EQUALS": "a=b=c",
		"WITH_QUOTES": `"quoted"`,
		"WITH_SPACES": "  padded  ",
		"EMPTY":       "",
	}
	p, err := writeEnvFileAt(&release.Spec{Project: "demo", ID: testRelease, Secrets: want}, envFilePath("demo", testRelease))
	if err != nil {
		t.Fatal(err)
	}
	got := readEnvFile(p)
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

// One release's secrets must not be recovered from another's, or a rollback
// would hand an old container a newer release's credentials.
func TestReadSecretDirsIgnoresOtherReleases(t *testing.T) {
	root := t.TempDir()
	t.Setenv("SHUNT_ROOT", root)
	other := "20260728-120000-def456"
	for id, val := range map[string]string{testRelease: "mine", other: "theirs"} {
		if _, err := writeSecretFiles(&release.Spec{
			Project: "demo", ID: id, SecretMode: "file",
			Secrets: map[string]string{"TOKEN": val},
		}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if got := readSecretDirs("demo", testRelease)["TOKEN"]; got != "mine" {
		t.Fatalf("TOKEN = %q, want this release's own value", got)
	}
	// A prefix match on the id alone would sweep in a sibling; the separator is
	// what keeps them apart.
	if _, err := os.Stat(filepath.Join(root, "demo", "secrets", other)); err != nil {
		t.Fatal(err)
	}
}
