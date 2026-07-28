package main

import (
	"fmt"
	"maps"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/OAISP/shunt/internal/release"
)

func containerName(project, service string) string { return project + "-" + service }

// releaseContainerName is the per-release name a proxied service runs under, so
// the old and new releases can coexist while the proxy switches over.
func releaseContainerName(project, service, releaseID string) string {
	return project + "-" + service + "-" + releaseID
}

// serviceContainer picks the naming scheme: proxied services are blue/green and
// need a unique name per release; everything else keeps a stable name, because
// a published host port allows only one container at a time anyway.
func serviceContainer(spec *release.Spec, name string, svc release.Service) string {
	if svc.Proxied() {
		return releaseContainerName(spec.Project, name, spec.ID)
	}
	return containerName(spec.Project, name)
}

// drainSeconds is how long a container gets to shut down cleanly.
func drainSeconds(svc release.Service) int {
	if d, err := time.ParseDuration(svc.Drain); err == nil && d > 0 {
		return int(d.Seconds())
	}
	return 10
}

// stopAndRemove stops a container with SIGTERM and a grace period, then removes
// it. `docker rm -f` SIGKILLs immediately, which drops every in-flight request;
// this gives the process a chance to finish what it was doing.
func stopAndRemove(container string, drain int) {
	if !containerExists(container) {
		return
	}
	exec.Command("docker", "stop", "--timeout", fmt.Sprint(drain), container).Run()
	exec.Command("docker", "rm", "-f", container).Run()
}

// containerIP resolves a container's address on the deploy network. It lets
// shunt health-check a service that publishes no host port, straight from the
// host, with no extra tooling in the image.
func containerIP(container, network string) (string, error) {
	format := "{{.NetworkSettings.IPAddress}}"
	if network != "" {
		format = fmt.Sprintf(`{{with index .NetworkSettings.Networks %q}}{{.IPAddress}}{{end}}`, network)
	}
	out, err := exec.Command("docker", "inspect", "-f", format, container).Output()
	if err != nil {
		return "", fmt.Errorf("inspect %s: %w", container, err)
	}
	ip := strings.TrimSpace(string(out))
	if ip == "" {
		return "", fmt.Errorf("%s has no address on network %s", container, network)
	}
	return ip, nil
}

// retireOldContainers removes containers of a service that belong to any release
// other than the current one. This is what completes a blue/green swap, and it
// also cleans up leftovers from a deploy that failed midway.
func retireOldContainers(spec *release.Spec, name string, svc release.Service) {
	out, err := exec.Command("docker", "ps", "-a",
		"--filter", "label=shunt.project="+spec.Project,
		"--filter", "label=shunt.service="+name,
		"--format", "{{.Names}}\t{{.Label \"shunt.release\"}}").Output()
	if err != nil {
		return
	}
	keep := serviceContainer(spec, name, svc)
	drain := drainSeconds(svc)
	for _, ln := range splitLines(string(out)) {
		f := strings.SplitN(ln, "\t", 2)
		if len(f) < 2 || f[0] == keep {
			continue
		}
		info(fmt.Sprintf("draining %s (%ds)", f[0], drain))
		stopAndRemove(f[0], drain)
	}
}

// containerExists reports whether a container of that name is present in any
// state, running or stopped.
func containerExists(name string) bool {
	out, err := exec.Command("docker", "ps", "-aq", "--filter", "name=^/"+name+"$").Output()
	return err == nil && strings.TrimSpace(string(out)) != ""
}

// startContainer removes any container of the same name and runs a fresh one.
func startContainer(spec *release.Spec, name string, svc release.Service, envFile, kind string) error {
	ref, err := imageRef(spec, svc.Image)
	if err != nil {
		return err
	}
	container := serviceContainer(spec, name, svc)

	// A same-named container may linger from an interrupted deploy. Stop it
	// gracefully rather than SIGKILLing whatever is still in flight.
	stopAndRemove(container, drainSeconds(svc))

	args := []string{"run", "-d", "--name", container, "--restart", svc.Restart}
	if spec.Network != "" {
		args = append(args, "--network", spec.Network, "--network-alias", name)
	}
	if envFile != "" {
		args = append(args, "--env-file", envFile)
	}
	for _, k := range slices.Sorted(maps.Keys(svc.Env)) {
		args = append(args, "-e", k+"="+svc.Env[k])
	}
	for _, p := range svc.Publish {
		args = append(args, "-p", p)
	}
	if svc.Expose > 0 {
		args = append(args, "--expose", fmt.Sprint(svc.Expose))
	}
	for _, v := range svc.Volumes {
		args = append(args, "-v", v)
	}
	args = append(args, "--label", "shunt.project="+spec.Project,
		"--label", "shunt.service="+name,
		"--label", "shunt.kind="+kind,
		"--label", "shunt.release="+spec.ID)
	args = append(args, proxyLabels(spec, name, svc)...)
	args = append(args, ref)
	args = append(args, svc.Command...)

	if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("start %s: %s", container, strings.TrimSpace(string(out)))
	}
	return nil
}

// ensureAccessories brings up stateful dependencies that are not already
// present, before any stage runs — a migration needs its database up.
//
// An accessory that already exists is left strictly alone. Recreating Postgres
// on every code deploy would be both pointless and, with the wrong volume
// config, destructive; changing one is the explicit `shunt boot` operation.
func ensureAccessories(spec *release.Spec, ledger *release.Ledger, envFile string) error {
	for _, name := range spec.AccessoryOrder {
		acc, present := spec.Accessories[name]
		if !present {
			continue
		}
		container := containerName(spec.Project, name)
		if containerExists(container) {
			// It may be stopped after a host reboot; start it, but do not replace it.
			exec.Command("docker", "start", container).Run()
			ok("accessory:"+name, container+" already present")
			continue
		}
		step("accessory:"+name, "booting "+container)
		if err := startContainer(spec, name, acc, envFile, "accessory"); err != nil {
			return err
		}
		// Recorded here rather than from the release spec: a deploy records the
		// manifest it was handed, but only this branch actually applied one.
		ledger.RecordAccessory(name, acc)
		ok("accessory:"+name, container+" booted")
	}
	return waitHealthy(spec, spec.AccessoryOrder, spec.Accessories)
}

// swapServices brings each service onto the new release in dependency order.
//
// Proxied services go blue/green: the new container starts alongside the old
// one, is health-checked before the old is retired, and never leaves a gap. The
// rest are stopped gracefully and restarted, which does have a gap — a published
// host port can only belong to one container at a time.
//
// Returns the services it started, and whether it replaced any running
// container at all.
//
// That second value is what separates "the deploy failed and production is
// exactly as it was" from "the deploy failed halfway and the host is now
// running a mix". Only the caller can record that distinction, and without it
// the ledger would claim a clean failure after having already taken a service
// down.
func swapServices(spec, prev *release.Spec, envFile string) (started []string, mutated bool, err error) {
	for _, name := range spec.Order {
		svc, present := spec.Services[name]
		if !present {
			continue
		}
		container := serviceContainer(spec, name, svc)

		if !svc.Proxied() {
			// Replacing in place stops the old container first, so the moment
			// this begins the host is no longer as it was — whether or not the
			// new container comes up.
			mutated = true
			step("swap:"+name, "replacing "+container)
			if err := startContainer(spec, name, svc, envFile, "service"); err != nil {
				return started, mutated, err
			}
			// A service that used to be proxied left per-release containers
			// behind, and those still carry their proxy labels — so without this
			// they would keep serving the old code indefinitely.
			retireOldContainers(spec, name, svc)
			started = append(started, name)
			ok("swap:"+name, container+" started")
			continue
		}

		// Changing the proxy config makes an overlap unsafe, so this one deploy
		// degrades to stop-then-start: a short gap instead of a dropped route.
		if !canOverlap(prev, name, svc) {
			info("proxy config for " + name + " changed — retiring the old container first to avoid a router conflict (brief gap)")
			mutated = true
			retireOldContainers(spec, name, svc)
		}

		step("swap:"+name, "starting "+container+" alongside the running release")
		if err := startContainer(spec, name, svc, envFile, "service"); err != nil {
			return started, mutated, err
		}
		// The old container is still serving, so a broken new release must be
		// pulled back out of rotation immediately rather than left half-live.
		// Nothing was replaced, so this alone does not count as mutating.
		if err := waitHealthy(spec, []string{name}, spec.Services); err != nil {
			stopAndRemove(container, 0)
			return started, mutated, fmt.Errorf("%w\n  the new container was removed; the previous release is still serving", err)
		}
		started = append(started, name)
		ok("swap:"+name, container+" healthy and in rotation")

		mutated = true
		retireOldContainers(spec, name, svc)
		ok("swap:"+name, "previous "+name+" drained")
	}
	return started, mutated, nil
}
