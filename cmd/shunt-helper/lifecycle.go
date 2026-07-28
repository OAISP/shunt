package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/OAISP/shunt/internal/release"
)

func cmdRollback(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: shunt-helper rollback <project> [release-id]")
	}
	project := args[0]
	var want string
	if len(args) > 1 {
		want = args[1]
	}
	return withLock(project, func() error {
		ledger, err := loadLedger(project)
		if err != nil {
			return err
		}
		var target *release.Entry
		if want != "" {
			if target = ledger.Find(want); target == nil {
				return fmt.Errorf("release %s is not in this host's ledger", want)
			}
		} else if target = ledger.Previous(); target == nil {
			return errors.New("no previous successful release to roll back to")
		}
		if target.Spec == nil {
			return fmt.Errorf("release %s predates spec retention and cannot be replayed", target.ID)
		}

		// Every image the old release needs must still exist locally; the store
		// only ever holds the newest build, so this is the real constraint on how
		// far back a rollback can go.
		for _, img := range target.Images {
			if !docker.Ok("docker", "image", "inspect", img.Ref) {
				return fmt.Errorf("image %s for release %s is no longer on this host (pruned) — redeploy that commit instead",
					img.Ref, target.ID)
			}
		}

		info("rolling back to " + target.ID)
		spec := *target.Spec
		spec.ID = target.ID
		// Images are already present; skip load and go straight to the swap.
		for n, img := range spec.Images {
			img.External = false
			spec.Images[n] = img
		}
		// The retained 0600 env-file is the only plaintext copy of that release's
		// secrets — the ledger holds hashes — so it cannot be reconstructed here.
		// If retention already dropped it, say so instead of silently starting
		// containers with no environment at all.
		envFile := envFilePath(project, target.ID)
		if spec.SecretsAsFiles() {
			// File mode writes a directory per release; the containers are
			// recreated from it, so it has to still be there.
			if len(spec.Secrets) > 0 {
				if _, err := os.Stat(secretsDir(project, target.ID, nil)); err != nil {
					return fmt.Errorf("the secrets for release %s have been pruned; roll back to a newer release or redeploy that commit", target.ID)
				}
			}
			envFile = ""
		} else if len(spec.Secrets) > 0 {
			if _, err := os.Stat(envFile); err != nil {
				return fmt.Errorf("the env-file for release %s has been pruned; roll back to a newer release or redeploy that commit", target.ID)
			}
		} else if _, err := os.Stat(envFile); err != nil {
			envFile = ""
		}
		if _, _, err := swapServices(&spec, previousSpec(ledger), envFile); err != nil {
			return err
		}
		if err := healthCheck(&spec); err != nil {
			return err
		}
		for i := range ledger.Releases {
			if ledger.Releases[i].Status == release.StatusActive {
				ledger.Releases[i].Status = release.StatusSuperseded
			}
			if ledger.Releases[i].ID == target.ID {
				ledger.Releases[i].Status = release.StatusActive
				ledger.Releases[i].Error = ""
			}
		}
		ledger.Current = target.ID
		ledger.LastAttempt = target.ID
		if err := saveLedger(ledger); err != nil {
			return err
		}
		emit(release.Event{Kind: release.KindResult, Release: target.ID, Status: release.StatusActive})
		return nil
	})
}

// cmdRetire stops and removes every container belonging to one service.
//
// It exists because dropping a service from shunt.toml previously did nothing:
// the plan reported it forever and the container ran forever. Retiring is a
// separate verb rather than something `up` does, for the same reason booting an
// accessory is — shunt does not stop containers it was not explicitly asked to.
func cmdRetire(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: shunt-helper retire <project> <service>")
	}
	project, service := args[0], args[1]
	return withLock(project, func() error {
		if err := retireContainers(project, service); err != nil {
			return err
		}
		// Drop the applied-accessory record too, so a service that comes back
		// later is not compared against state that no longer exists.
		ledger, err := loadLedger(project)
		if err != nil {
			return err
		}
		if _, present := ledger.Accessories[service]; present {
			delete(ledger.Accessories, service)
			return saveLedger(ledger)
		}
		return nil
	})
}

// retireContainers stops and removes every container belonging to one service.
func retireContainers(project, service string) error {
	out, err := docker.Run("docker", "ps", "-a",
		"--filter", "label=shunt.project="+project,
		"--filter", "label=shunt.service="+service,
		"--format", "{{.Names}}")
	if err != nil {
		return fmt.Errorf("list containers for %s: %w", service, err)
	}
	names := splitLines(out)
	if len(names) == 0 {
		ok("retire:"+service, "nothing running for "+service)
		return nil
	}
	for _, n := range names {
		step("retire:"+service, "stopping "+n)
		// A drain rather than an immediate kill: an orphan is still serving
		// something until it stops.
		stopAndRemove(n, 10)
		ok("retire:"+service, n+" stopped and removed")
	}
	return nil
}

// cmdBoot force-recreates one accessory from a spec supplied on stdin. It is a
// separate verb precisely because it is destructive to a stateful container and
// must never happen as a side effect of shipping code.
func cmdBoot(in io.Reader, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: shunt-helper boot <accessory> < spec.json")
	}
	name := args[0]
	var spec release.Spec
	if err := json.NewDecoder(in).Decode(&spec); err != nil {
		return fmt.Errorf("decode release spec: %w", err)
	}
	if spec.Protocol != release.Protocol {
		return fmt.Errorf("protocol mismatch: helper speaks v%d, CLI sent v%d", release.Protocol, spec.Protocol)
	}
	acc, present := spec.Accessories[name]
	if !present {
		return fmt.Errorf("%q is not an accessory in this manifest", name)
	}
	return withLock(spec.Project, func() error {
		ledger, err := loadLedger(spec.Project)
		if err != nil {
			return err
		}
		if err := ensureNetwork(spec.Network); err != nil {
			return err
		}
		if img, present := spec.Images[acc.Image]; present && img.External {
			step("pull", "pulling "+img.Ref)
			if out, err := docker.Run("docker", "pull", "--quiet", img.Ref); err != nil {
				return fmt.Errorf("pull %s: %s", img.Ref, strings.TrimSpace(out))
			}
		}
		envFile, err := writeEnvFile(&spec)
		if err != nil {
			return err
		}
		step("boot:"+name, "recreating "+containerName(spec.Project, name))
		if err := startContainer(&spec, name, acc, envFile, "accessory"); err != nil {
			return err
		}
		ok("boot:"+name, containerName(spec.Project, name)+" recreated")
		if err := waitHealthy(&spec, []string{name}, spec.Accessories); err != nil {
			return err
		}
		// Only now is the drift actually resolved. Without recording it, `shunt
		// plan` would keep reporting the accessory as drifted after the very
		// command that fixed it.
		ledger.RecordAccessory(name, acc)
		return saveLedger(ledger)
	})
}

// autoRollback restores the previous release after a deploy has already
// replaced running containers.
//
// It runs inside the same helper invocation, under the same lock, so nothing
// can slip between the failure and the recovery. Deliberately narrow: it
// restores containers only, exactly as `shunt rollback` does, and says so —
// data that stages or artifacts already changed is not reverted, and pretending
// otherwise is how a "helpful" rollback loses a database.
func autoRollback(spec *release.Spec, ledger *release.Ledger) error {
	// The release to restore is the one that was *serving* when this deploy
	// started — which is still ledger.Current, because the failing attempt is
	// only appended to the ledger afterwards.
	//
	// Not Previous(): that answers "the release before the one serving", which is
	// what `shunt rollback` wants and is one step too far here. Getting this
	// wrong restores a release older than the one the operator was running,
	// silently reverting work that had nothing to do with the failure.
	target := ledger.Find(ledger.Current)
	if target == nil || !target.Healthy() {
		target = ledger.Previous()
	}
	if target == nil {
		return errors.New("no previous successful release to restore")
	}
	if target.Spec == nil {
		return fmt.Errorf("release %s predates spec retention and cannot be replayed", target.ID)
	}
	for _, img := range target.Images {
		if !docker.Ok("docker", "image", "inspect", img.Ref) {
			return fmt.Errorf("image %s for release %s is no longer on this host", img.Ref, target.ID)
		}
	}

	info("restoring " + target.ID)
	prev := *target.Spec
	prev.ID = target.ID
	for n, img := range prev.Images {
		img.External = false
		prev.Images[n] = img
	}
	// That release's own env-file, not this one's: its containers expect the
	// environment they were deployed with.
	prevEnv := envFilePath(spec.Project, target.ID)
	if _, err := os.Stat(prevEnv); err != nil {
		if len(prev.Secrets) > 0 {
			return fmt.Errorf("the env-file for %s has been pruned", target.ID)
		}
		prevEnv = ""
	}
	if _, _, err := swapServices(&prev, spec, prevEnv); err != nil {
		return err
	}
	if err := healthCheck(&prev); err != nil {
		return err
	}
	for i := range ledger.Releases {
		if ledger.Releases[i].ID == target.ID {
			ledger.Releases[i].Status = release.StatusActive
			ledger.Releases[i].Error = ""
		}
	}
	ledger.Current = target.ID
	ok("rollback", "restored "+target.ID)
	return nil
}
