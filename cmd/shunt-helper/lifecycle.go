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
		spec, err := replaySpec(target)
		if err != nil {
			return err
		}
		// The unscoped env-file is what a service that did not narrow its secrets
		// is started with. A missing one is not fatal here: replaySpec has already
		// established that every value this release needs is still on the host, and
		// a release with no secrets never had a file to begin with.
		envFile := ""
		if !spec.SecretsAsFiles() {
			if p := envFilePath(project, target.ID); fileExists(p) {
				envFile = p
			}
		}
		outgoing := previousSpec(ledger)
		if _, _, err := swapServices(spec, outgoing, envFile); err != nil {
			return err
		}
		if err := healthCheck(spec); err != nil {
			return err
		}
		// Only once the restore is proven good. Removing the newer release's
		// containers first would leave the host with neither if this failed.
		retireUndeclaredServices(spec, outgoing)
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
// A separate verb rather than something `up` does, for the same reason booting
// an accessory is: shunt does not stop containers it was not asked to stop.
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
		// Matching apply: in file mode the per-service directories are written as
		// the container starts, so writing an env-file here would only leave an
		// unread plaintext copy behind under a release id the ledger never records.
		envFile := ""
		if !spec.SecretsAsFiles() {
			var err error
			if envFile, err = writeEnvFile(&spec); err != nil {
				return err
			}
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

// replaySpec turns a ledger entry back into a spec that can be re-applied.
//
// Two things have to be undone. The stored spec's images are marked external
// only because that is how they were described at build time; they are already
// on this host, so a replay skips the load and goes straight to the swap. And
// its secret values are hashes — the ledger never holds plaintext — so they are
// restored from the retained env-file or secrets directory, which is the one
// copy on the host. Without that, every path that recreates a container would
// faithfully rewrite the hashes: the containers would come up holding "h:3f2a…"
// where the password should be, and in file mode the good copy would be
// overwritten with them on the way past.
//
// The entry is copied rather than adjusted in place. It is a pointer into the
// ledger about to be saved, and neither of those corrections belongs in the
// permanent record.
func replaySpec(entry *release.Entry) (*release.Spec, error) {
	spec := *entry.Spec
	spec.ID = entry.ID

	spec.Images = make(map[string]release.ImageRef, len(entry.Spec.Images))
	for n, img := range entry.Spec.Images {
		img.External = false
		spec.Images[n] = img
	}
	if err := recoverSecrets(&spec); err != nil {
		return nil, err
	}
	return &spec, nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
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
	prev, err := replaySpec(target)
	if err != nil {
		return err
	}
	// That release's own env-file, not this one's: its containers expect the
	// environment they were deployed with.
	prevEnv := ""
	if !prev.SecretsAsFiles() {
		if p := envFilePath(spec.Project, target.ID); fileExists(p) {
			prevEnv = p
		}
	}
	if _, _, err := swapServices(prev, spec, prevEnv); err != nil {
		return err
	}
	if err := healthCheck(prev); err != nil {
		return err
	}
	// A service this deploy introduced has no counterpart in the release being
	// restored, so nothing above has visited it and it is still running the code
	// that just failed. Clearing it is what lets apply() drop `mutated`.
	retireUndeclaredServices(prev, spec)
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
