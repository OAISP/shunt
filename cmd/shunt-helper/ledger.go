package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/OAISP/shunt/internal/release"
)

func loadLedger(project string) (*release.Ledger, error) {
	b, err := os.ReadFile(ledgerPath(project))
	if errors.Is(err, os.ErrNotExist) {
		return &release.Ledger{Project: project}, nil
	}
	if err != nil {
		return nil, err
	}
	var l release.Ledger
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, fmt.Errorf("corrupt ledger %s: %w", ledgerPath(project), err)
	}
	if l.Project == "" {
		l.Project = project
	}
	return &l, nil
}

// saveLedger writes atomically: a half-written ledger after a crash would make
// the host's deploy history unreadable.
func saveLedger(l *release.Ledger) error {
	if err := os.MkdirAll(projectDir(l.Project), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	tmp := ledgerPath(l.Project) + ".tmp"
	// 0600 as defence in depth. The ledger holds only redacted specs (see
	// redactSecrets), but deploy history is still nobody else's business.
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, ledgerPath(l.Project))
}

// redactSecrets returns a copy of the spec safe to persist and to send back to
// the CLI: every secret value is replaced by a short hash of itself.
//
// That keeps exactly one plaintext copy of each secret on the host — the 0600
// env-file — while still letting `shunt plan` report *which* secrets changed,
// because equal values hash equally.
func redactSecrets(spec *release.Spec, salt string) *release.Spec {
	cp := *spec
	if len(spec.Secrets) == 0 {
		cp.Secrets = nil
		return &cp
	}
	red := make(map[string]string, len(spec.Secrets))
	for k, v := range spec.Secrets {
		red[k] = release.HashSecret(salt, v)
	}
	cp.Secrets = red
	return &cp
}

// ensureSalt gives a project its secret-hash salt on first use. An older ledger
// has none, so one is minted in place; the hashes it already holds are simply
// re-derived on the next deploy, which reports those keys as changed once.
func ensureSalt(ledger *release.Ledger) error {
	if ledger.Salt != "" {
		return nil
	}
	s, err := release.NewSalt()
	if err != nil {
		return fmt.Errorf("generate secret salt: %w", err)
	}
	ledger.Salt = s
	return nil
}

// recordOutcome classifies a finished attempt and folds it into the ledger.
//
// The classification is the load-bearing part. `mutated` says whether any
// running container was replaced, which is what separates a failure that left
// the host exactly as it was from one that left it running a mix — and only the
// second kind should move what `shunt status` calls the serving release.
func recordOutcome(ledger *release.Ledger, entry *release.Entry, mutated bool, runErr error, retain int) {
	entry.FinishedAt = time.Now().UTC()
	switch {
	case runErr == nil:
		entry.Status = release.StatusActive
	case mutated:
		entry.Status = release.StatusDegraded
		entry.Error = runErr.Error()
	default:
		entry.Status = release.StatusFailed
		entry.Error = runErr.Error()
	}

	// Only a release that actually took something over supersedes the old one.
	// An attempt that failed before touching a single container leaves the
	// previous release both active and serving — marking it superseded and
	// pointing Current at the failure would make `shunt status` contradict the
	// "production is untouched" error just printed, and would send the next
	// `shunt rollback` somewhere nobody asked for.
	if entry.Status != release.StatusFailed {
		for i := range ledger.Releases {
			if ledger.Releases[i].Status == release.StatusActive {
				ledger.Releases[i].Status = release.StatusSuperseded
			}
		}
		ledger.Current = entry.ID
	}
	ledger.LastAttempt = entry.ID
	ledger.Releases = append(ledger.Releases, *entry)
	ledger.Trim(retain)
}

// previousSpec returns the spec of the release currently serving, which is what
// the incoming deploy is replacing.
//
// Deliberately the *active* release rather than the newest entry carrying a
// spec: after a failed attempt the newest entry describes a release that never
// took over, and comparing against it would have canOverlap decide whether a
// blue/green swap is safe by looking at labels no running container carries.
func previousSpec(ledger *release.Ledger) *release.Spec {
	if cur := ledger.Find(ledger.Current); cur != nil && cur.Spec != nil {
		return cur.Spec
	}
	for i := len(ledger.Releases) - 1; i >= 0; i-- {
		if e := &ledger.Releases[i]; e.Healthy() && e.Spec != nil {
			return e.Spec
		}
	}
	return nil
}
