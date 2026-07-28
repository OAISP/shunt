package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/OAISP/shunt/internal/release"
)

func cmdApply(in io.Reader) error {
	var spec release.Spec
	if err := json.NewDecoder(in).Decode(&spec); err != nil {
		return fmt.Errorf("decode release spec: %w", err)
	}
	if spec.Protocol != release.Protocol {
		return fmt.Errorf("protocol mismatch: helper speaks v%d, CLI sent v%d — upgrade both ends",
			release.Protocol, spec.Protocol)
	}
	if spec.Project == "" || spec.ID == "" {
		return errors.New("release spec is missing project or id")
	}
	return withLock(spec.Project, func() error { return apply(&spec) })
}

func apply(spec *release.Spec) error {
	ledger, err := loadLedger(spec.Project)
	if err != nil {
		return err
	}
	entry := release.Entry{
		ID:        spec.ID,
		Status:    "failed", // pessimistic until proven otherwise
		StartedAt: time.Now().UTC(),
		Images:    spec.Images,
		Spec:      redactSecrets(spec),
	}

	// finish records the outcome no matter which step failed, so `shunt status`
	// always reflects reality rather than the last success.
	finish := func(runErr error) error {
		entry.FinishedAt = time.Now().UTC()
		if runErr != nil {
			entry.Error = runErr.Error()
		} else {
			entry.Status = release.StatusActive
		}
		for i := range ledger.Releases {
			if ledger.Releases[i].Status == release.StatusActive {
				ledger.Releases[i].Status = release.StatusSuperseded
			}
		}
		ledger.Releases = append(ledger.Releases, entry)
		ledger.Current = entry.ID
		ledger.Trim(spec.Retain)
		if err := saveLedger(ledger); err != nil {
			return errors.Join(runErr, err)
		}
		return runErr
	}

	if err := ensureNetwork(spec.Network); err != nil {
		return finish(err)
	}
	if err := loadImages(spec); err != nil {
		return finish(err)
	}
	envFile, err := writeEnvFile(spec)
	if err != nil {
		return finish(err)
	}

	// Accessories come up first so stages have a database to talk to. Existing
	// ones are left untouched.
	if err := ensureAccessories(spec, envFile); err != nil {
		return finish(fmt.Errorf("%w\n  no services were replaced — production is untouched", err))
	}

	// Stages run BEFORE any running service container is touched. This is the
	// safety invariant of the whole tool: a failed backup or migration must
	// leave production exactly as it was.
	if err := runStages(spec, envFile); err != nil {
		return finish(fmt.Errorf("%w\n  no containers were replaced — production is untouched", err))
	}

	// Data lands after stages and before services, so the swap is as late as it
	// can be while the old container still holds the old inode.
	if err := swapArtifacts(spec); err != nil {
		return finish(fmt.Errorf("%w\n  no services were replaced — production is untouched", err))
	}

	started, err := swapServices(spec, previousSpec(ledger), envFile)
	if err != nil {
		entry.Services = started
		return finish(err)
	}
	entry.Services = started

	if err := healthCheck(spec); err != nil {
		return finish(fmt.Errorf("%w\n  containers are running but unhealthy — `shunt rollback` restores the previous release%s",
			err, artifactRecovery(spec)))
	}

	// This release is not in the ledger yet, so it has to be pinned into the keep
	// set explicitly — otherwise the deploy prunes what it just loaded.
	keepIDs := ledger.KeepIDs(retainFor(ledger, spec))
	keepIDs[spec.ID] = true
	if err := pruneImages(spec.Project, keepImageRefs(ledger, keepIDs, spec)); err != nil {
		info("image prune: " + err.Error())
	}
	pruneEnvFiles(spec.Project, keepIDs)

	emit(release.Event{Kind: release.KindResult, Release: spec.ID, Status: release.StatusActive})
	return finish(nil)
}
