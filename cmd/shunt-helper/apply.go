package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/OAISP/shunt/internal/release"
)

// cmdApply is one deploy session, and holds the project lock for all of it —
// the CLI opens it before shipping anything, so two deploys can never rsync
// into the same store at once.
//
// Once held, the lock is announced along with the release then serving, which
// lets the CLI check its plan still holds before spending minutes on the wire.
// The spec follows on stdin when the transfer is done. EOF in its place means
// the CLI gave up, and the session ends having changed nothing.
func cmdApply(args []string, in io.Reader) error {
	if len(args) < 1 {
		return errors.New("usage: shunt-helper apply <project> < spec.json")
	}
	project := args[0]
	return withLock(project, func() error {
		ledger, err := loadLedger(project)
		if err != nil {
			return err
		}
		emit(release.Event{Kind: release.KindHeld, Release: ledger.Current})

		var spec release.Spec
		in, heard := untilSilent(in, release.HeartbeatTimeout)
		err = json.NewDecoder(in).Decode(&spec)
		heard.Stop()
		switch {
		case errors.Is(err, io.EOF):
			return nil
		case errors.Is(err, errSilent):
			return err
		case err != nil:
			return fmt.Errorf("decode release spec: %w", err)
		}
		if spec.Protocol != release.Protocol {
			return fmt.Errorf("protocol mismatch: helper speaks v%d, CLI sent v%d — upgrade both ends",
				release.Protocol, spec.Protocol)
		}
		if spec.Project != project {
			return fmt.Errorf("release spec is for project %q, but this session locked %q", spec.Project, project)
		}
		if spec.ID == "" {
			return errors.New("release spec has no id")
		}
		return apply(&spec)
	})
}

var errSilent = errors.New("the CLI went silent before sending the release — giving up the deploy lock")

// untilSilent relays in, and fails the read once nothing has arrived for limit.
// The returned timer is stopped when the spec is in: applying it takes as long
// as it takes.
func untilSilent(in io.Reader, limit time.Duration) (io.Reader, *time.Timer) {
	pr, pw := io.Pipe()
	silent := time.AfterFunc(limit, func() { pw.CloseWithError(errSilent) })
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := in.Read(buf)
			silent.Reset(limit)
			if n > 0 {
				pw.Write(buf[:n])
			}
			if err != nil {
				pw.CloseWithError(err)
				return
			}
		}
	}()
	return pr, silent
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
	// dataChanged records whether an artifact was swapped in, which is the one
	// change no container's own inputs show.
	dataChanged := false

	// finish records the outcome no matter which step failed, so `shunt status`
	// always reflects reality rather than the last success.
	finish := func(runErr error) error {
		// A degraded host is the one outcome an operator cannot leave alone: some
		// services are on the new release and some on the old. Restoring is only
		// attempted when asked, because it is the wrong move for a release whose
		// stages already migrated a database — the code would go back and the
		// schema would not.
		if runErr != nil && mutated && spec.RollbackOnFailure {
			if entry.Irreversible != "" {
				runErr = fmt.Errorf("%w\n  not rolled back automatically: stage %q reported an irreversible change, "+
					"and the previous release would run against it", runErr, entry.Irreversible)
			} else if rbErr := autoRollback(spec, ledger, dataChanged); rbErr != nil {
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
	entry.Irreversible, err = runStages(spec, envFile)
	if err != nil {
		return finish(fmt.Errorf("%w\n  no containers were replaced — production is untouched", err))
	}

	// Data lands after stages and before services, so the swap is as late as it
	// can be while the old container still holds the old inode.
	promoted, err := swapArtifacts(spec)
	if err != nil {
		return finish(fmt.Errorf("%w\n  no services were replaced — production is untouched", err))
	}

	dataChanged = len(promoted) > 0
	started, swapped, err := swapServices(spec, previousSpec(ledger), envFile, ledger.Salt, dataChanged)
	mutated = mutated || swapped
	entry.Services = started
	if err != nil {
		if mutated {
			err = fmt.Errorf("%w\n  this host is now running a mix of releases — `shunt rollback` restores the previous one%s",
				err, artifactRecovery(spec))
		} else {
			err = fmt.Errorf("%w%s", err, undoPromotions(promoted))
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
	if err := pruneImages(spec.Project, ledger, spec, keepImageRefs(ledger, keepIDs, spec)); err != nil {
		info("image prune: " + err.Error())
	}
	pruneEnvFiles(spec.Project, keepIDs)
	pruneSecretDirs(spec.Project, keepIDs)
	settleArtifacts(spec)

	// Success is announced only once it is on record.
	if err := finish(nil); err != nil {
		return err
	}
	emit(release.Event{Kind: release.KindResult, Release: spec.ID, Status: release.StatusActive})
	return nil
}
