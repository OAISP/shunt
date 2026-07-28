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
	if err := ensureSalt(ledger); err != nil {
		return err
	}
	// Checked under the lock, against the state the plan was computed from. The
	// build can take minutes, so the host may have moved on between planning and
	// applying — and applying a plan whose premise has expired is how one deploy
	// silently reverts another.
	if spec.ExpectedCurrent != "" && ledger.Current != "" && spec.ExpectedCurrent != ledger.Current {
		return fmt.Errorf("this plan was built when %s was serving, but %s is serving now — "+
			"another deploy or rollback landed in between\n  rerun `shunt up` to plan against the current state",
			spec.ExpectedCurrent, ledger.Current)
	}
	entry := release.Entry{
		ID:         spec.ID,
		Status:     release.StatusFailed, // pessimistic until proven otherwise
		StartedAt:  time.Now().UTC(),
		Images:     spec.Images,
		Provenance: spec.Provenance,
		Spec:       redactSecrets(spec, ledger.Salt),
	}

	// mutated records whether any running container was replaced. It is what
	// separates a failure that left the host exactly as it was from one that
	// left it running a mix of two releases.
	mutated := false

	// finish records the outcome no matter which step failed, so `shunt status`
	// always reflects reality rather than the last success.
	finish := func(runErr error) error {
		// A degraded host is the one outcome an operator cannot leave alone: some
		// services are on the new release and some on the old. Restoring is only
		// attempted when asked, because it is the wrong move for a release whose
		// stages already migrated a database — the code would go back and the
		// schema would not.
		if runErr != nil && mutated && spec.RollbackOnFailure {
			if rbErr := autoRollback(spec, ledger); rbErr != nil {
				runErr = fmt.Errorf("%w\n  automatic rollback also failed: %v", runErr, rbErr)
			} else {
				runErr = fmt.Errorf("%w\n  the previous release was restored automatically", runErr)
				// The host is back on the previous release, so this attempt did not
				// take anything over after all.
				mutated = false
			}
		}
		recordOutcome(ledger, &entry, mutated, runErr, spec.Retain)
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
	// In file mode the per-service directories are written as containers start,
	// so there is no single env-file to prepare here.
	envFile := ""
	if !spec.SecretsAsFiles() {
		var err error
		if envFile, err = writeEnvFile(spec); err != nil {
			return finish(err)
		}
	}

	// Accessories come up first so stages have a database to talk to. Existing
	// ones are left untouched.
	if err := ensureAccessories(spec, ledger, envFile); err != nil {
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

	started, swapped, err := swapServices(spec, previousSpec(ledger), envFile)
	mutated = mutated || swapped
	entry.Services = started
	if err != nil {
		if mutated {
			err = fmt.Errorf("%w\n  this host is now running a mix of releases — `shunt rollback` restores the previous one%s",
				err, artifactRecovery(spec))
		}
		return finish(err)
	}

	if err := healthCheck(spec); err != nil {
		// Reached only once services were swapped, so the host is mixed by
		// definition even if swapServices itself reported nothing replaced.
		mutated = true
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
	pruneSecretDirs(spec.Project, keepIDs)

	emit(release.Event{Kind: release.KindResult, Release: spec.ID, Status: release.StatusActive})
	return finish(nil)
}
