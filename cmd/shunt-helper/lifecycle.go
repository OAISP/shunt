package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
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
			if err := exec.Command("docker", "image", "inspect", img.Ref).Run(); err != nil {
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
		if len(spec.Secrets) > 0 {
			if _, err := os.Stat(envFile); err != nil {
				return fmt.Errorf("the env-file for release %s has been pruned; roll back to a newer release or redeploy that commit", target.ID)
			}
		} else if _, err := os.Stat(envFile); err != nil {
			envFile = ""
		}
		if _, err := swapServices(&spec, previousSpec(ledger), envFile); err != nil {
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
		if err := saveLedger(ledger); err != nil {
			return err
		}
		emit(release.Event{Kind: release.KindResult, Release: target.ID, Status: release.StatusActive})
		return nil
	})
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
		if err := ensureNetwork(spec.Network); err != nil {
			return err
		}
		if img, present := spec.Images[acc.Image]; present && img.External {
			step("pull", "pulling "+img.Ref)
			if out, err := exec.Command("docker", "pull", "--quiet", img.Ref).CombinedOutput(); err != nil {
				return fmt.Errorf("pull %s: %s", img.Ref, strings.TrimSpace(string(out)))
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
		return waitHealthy(&spec, []string{name}, spec.Accessories)
	})
}
