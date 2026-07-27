package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

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
func redactSecrets(spec *release.Spec) *release.Spec {
	cp := *spec
	if len(spec.Secrets) == 0 {
		cp.Secrets = nil
		return &cp
	}
	red := make(map[string]string, len(spec.Secrets))
	for k, v := range spec.Secrets {
		red[k] = release.HashSecret(v)
	}
	cp.Secrets = red
	return &cp
}

// previousSpec returns the most recent release that recorded a spec, which is
// what the current deploy is replacing.
func previousSpec(ledger *release.Ledger) *release.Spec {
	for i := len(ledger.Releases) - 1; i >= 0; i-- {
		if ledger.Releases[i].Spec != nil {
			return ledger.Releases[i].Spec
		}
	}
	return nil
}
