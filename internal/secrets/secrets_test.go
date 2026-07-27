package secrets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OAISP/shunt/internal/manifest"
)

func TestParseDotenv(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "prod.env")
	content := strings.Join([]string{
		"# a comment",
		"",
		"PLAIN=value",
		"export EXPORTED=yes",
		`QUOTED="has spaces"`,
		"SINGLE='single quoted'",
		"  SPACED  =  trimmed  ",
		"URL=postgres://u:p@host:5432/db?sslmode=require",
		"EMPTY=",
	}, "\n")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := fromDotenvFile(p)
	if err != nil {
		t.Fatalf("fromDotenvFile: %v", err)
	}
	want := map[string]string{
		"PLAIN":    "value",
		"EXPORTED": "yes",
		"QUOTED":   "has spaces",
		"SINGLE":   "single quoted",
		"SPACED":   "trimmed",
		// A value containing '=' must survive intact — connection strings are
		// the single most common secret and they are full of them.
		"URL":   "postgres://u:p@host:5432/db?sslmode=require",
		"EMPTY": "",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d keys, want %d: %v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestParseDotenvRejectsMalformedLine(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bad.env")
	if err := os.WriteFile(p, []byte("NOT_AN_ASSIGNMENT\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := fromDotenvFile(p); err == nil {
		t.Fatal("expected an error for a line without '='")
	}
}

func TestFromEnvReportsEveryMissingKey(t *testing.T) {
	t.Setenv("SHUNT_TEST_PRESENT", "1")
	_, err := fromEnv([]string{"SHUNT_TEST_PRESENT", "SHUNT_TEST_ABSENT_A", "SHUNT_TEST_ABSENT_B"})
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"SHUNT_TEST_ABSENT_A", "SHUNT_TEST_ABSENT_B"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "SHUNT_TEST_PRESENT,") {
		t.Errorf("present key reported as missing: %v", err)
	}
}

func TestInterpolate(t *testing.T) {
	t.Setenv("SHUNT_TEST_NAME", "acme")
	t.Setenv("SHUNT_TEST_PORT", "8080")

	for _, tc := range []struct{ in, want string }{
		{"plain", "plain"},
		{"${env:SHUNT_TEST_NAME}", "acme"},
		{"https://${env:SHUNT_TEST_NAME}.example.com:${env:SHUNT_TEST_PORT}/x", "https://acme.example.com:8080/x"},
		// A bare $ is not a reference and must survive untouched — passwords
		// contain them constantly.
		{"pa$$word", "pa$$word"},
		{"${notenv:X}", "${notenv:X}"},
	} {
		got, err := Interpolate(tc.in)
		if err != nil {
			t.Errorf("Interpolate(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("Interpolate(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestInterpolateFailsOnUnsetAndUnterminated(t *testing.T) {
	if _, err := Interpolate("${env:SHUNT_TEST_DEFINITELY_UNSET}"); err == nil {
		t.Error("expected an error for an unset variable")
	}
	if _, err := Interpolate("${env:UNTERMINATED"); err == nil {
		t.Error("expected an error for an unterminated reference")
	}
}

func TestResolveWithoutSecretsBlock(t *testing.T) {
	got, err := Resolve(&manifest.Manifest{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}
