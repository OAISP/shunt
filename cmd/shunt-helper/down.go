package main

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/OAISP/shunt/internal/release"
)

// Tearing a project back down — the inverse of apply.
//
// Driven by the labels on the host, never by a manifest. The reason to tear a
// project down is usually that the manifest has already moved on or gone away,
// and a teardown that could only remove what the manifest still declares would
// strand exactly the containers you wanted rid of.
//
// Volumes are never touched, at any level. That is where the data is, and
// nothing in this file is worth losing a database over.

func cmdDown(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: shunt-helper down <project> <network> [--accessories] [--purge]")
	}
	project, network := args[0], args[1]

	var accessories, purge bool
	for _, a := range args[2:] {
		switch a {
		case "--accessories":
			accessories = true
		case "--purge":
			// Purging leaves nothing behind that an accessory container could
			// belong to, so it always implies removing them.
			accessories, purge = true, true
		default:
			return fmt.Errorf("unknown flag %q", a)
		}
	}
	return withLock(project, func() error { return down(project, network, accessories, purge) })
}

func down(project, network string, accessories, purge bool) error {
	ledger, err := loadLedger(project)
	if err != nil {
		return err
	}

	owned := projectContainers(project)
	rank := teardownRank(ledger)
	drain := drainFor(ledger)

	// Services first, dependents before their dependencies; accessories after,
	// because a service may still be talking to one on its way down.
	slices.SortStableFunc(owned, func(a, b ownedContainer) int {
		if a.accessory != b.accessory {
			if a.accessory {
				return 1
			}
			return -1
		}
		if r := rank[b.service] - rank[a.service]; r != 0 {
			return r
		}
		return strings.Compare(a.name, b.name)
	})

	var removed int
	for _, c := range owned {
		if c.accessory && !accessories {
			info("keeping accessory " + c.name + " — pass --all to remove it")
			continue
		}
		step("down", "removing "+c.name)
		stopAndRemove(c.name, drain)
		ok("down", c.name+" stopped and removed")
		removed++
	}
	if removed == 0 {
		ok("down", "nothing was running for "+project)
	}

	if !purge {
		// The ledger is deliberately left alone: `shunt up` brings this project
		// straight back, and every rollback target is still on the host.
		return nil
	}

	// The network only goes once its containers have, or docker refuses it as
	// still in use.
	if n := networkFor(ledger, network); n != "" {
		if out, err := docker.Run("docker", "network", "rm", n); err != nil {
			// An absent network is the expected case on a project that never got
			// past its first failed deploy.
			if !strings.Contains(out, "not found") && !strings.Contains(out, "No such network") {
				info("could not remove network " + n + ": " + strings.TrimSpace(out))
			}
		} else {
			ok("down", "network "+n+" removed")
		}
	}

	if n := removeProjectImages(project); n > 0 {
		ok("down", fmt.Sprintf("%d image(s) removed", n))
	}

	// Last, because it holds the ledger this function has been reading — and
	// because it holds the only plaintext copy of this project's secrets, which
	// is the whole reason --purge exists rather than being left to `rm -rf`.
	if err := os.RemoveAll(projectDir(project)); err != nil {
		return fmt.Errorf("removing %s: %w", projectDir(project), err)
	}
	ok("down", "release history and secrets removed from "+projectDir(project))
	info("named volumes were not touched; `docker volume ls` still shows your data")
	return nil
}

// ownedContainer is one container the host is running for this project, as the
// labels describe it.
type ownedContainer struct {
	name      string
	service   string
	accessory bool
}

func projectContainers(project string) []ownedContainer {
	out, err := docker.Run("docker", "ps", "-a",
		"--filter", "label=shunt.project="+project,
		"--format", "{{.Names}}\t{{.Label \"shunt.kind\"}}\t{{.Label \"shunt.service\"}}")
	if err != nil {
		return nil
	}
	var owned []ownedContainer
	for _, ln := range splitLines(out) {
		f := strings.SplitN(ln, "\t", 3)
		if len(f) < 3 || f[0] == "" {
			continue
		}
		owned = append(owned, ownedContainer{name: f[0], service: f[2], accessory: f[1] == "accessory"})
	}
	return owned
}

// teardownRank ranks services so that a dependent is removed before whatever it
// depends on — the start order, reversed.
//
// From the release that is actually serving, since that is what produced the
// containers being removed. A project with no usable ledger simply falls back to
// removing by name, which is unordered but never wrong: everything goes anyway.
func teardownRank(ledger *release.Ledger) map[string]int {
	rank := map[string]int{}
	cur := ledger.Find(ledger.Current)
	if cur == nil || cur.Spec == nil {
		return rank
	}
	for i, name := range cur.Spec.Order {
		rank[name] = i
	}
	return rank
}

// drainFor is how long a container gets to shut down cleanly. Taken from the
// serving release so a service that asked for a long drain still gets it on the
// way out, which is the moment it matters most.
func drainFor(ledger *release.Ledger) int {
	longest := 10
	cur := ledger.Find(ledger.Current)
	if cur == nil || cur.Spec == nil {
		return longest
	}
	for _, svc := range cur.Spec.Services {
		if d := drainSeconds(svc); d > longest {
			longest = d
		}
	}
	return longest
}

// networkFor prefers the network the serving release actually used over the one
// the caller's manifest currently names, because the manifest may have been
// edited since — and removing the wrong network would leave the real one behind.
func networkFor(ledger *release.Ledger, fallback string) string {
	if cur := ledger.Find(ledger.Current); cur != nil && cur.Spec != nil && cur.Spec.Network != "" {
		return cur.Spec.Network
	}
	return fallback
}

// removeProjectImages drops every release-tagged image of this project. Unlike
// pruneImages there is no keep set: purging means the rollback targets go too,
// which the CLI says out loud before asking.
func removeProjectImages(project string) int {
	out, err := docker.Run("docker", "images",
		"--filter", "reference=shunt/"+project+"-*", "--format", "{{.Repository}}:{{.Tag}}")
	if err != nil {
		return 0
	}
	var removed int
	for _, ref := range splitLines(out) {
		if ref == "" || strings.HasSuffix(ref, ":<none>") {
			continue
		}
		if _, err := docker.Run("docker", "rmi", "-f", ref); err == nil {
			removed++
		}
	}
	return removed
}
