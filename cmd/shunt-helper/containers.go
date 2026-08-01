package main

import (
	"fmt"
	"maps"
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
	start := time.Now()
	docker.Ok("docker", "stop", "--timeout", fmt.Sprint(drain), container)
	took := time.Since(start)
	docker.Ok("docker", "rm", "-f", container)

	// A container that burns the whole drain window did not exit on SIGTERM and
	// was killed. That is the single largest cost in a typical deploy — far
	// larger than loading the image — and it is invisible unless said out loud.
	// It is also the app-side contract the zero-downtime section describes, so
	// point at the fix rather than just reporting a number.
	if drain > 0 && took >= time.Duration(drain)*time.Second {
		info(fmt.Sprintf("%s ignored SIGTERM and was killed after %ds — handling it would cut that to near zero",
			container, drain))
	}
}

// containerIP resolves a container's address on the deploy network. It lets
// shunt health-check a service that publishes no host port, straight from the
// host, with no extra tooling in the image.
func containerIP(container, network string) (string, error) {
	format := "{{.NetworkSettings.IPAddress}}"
	if network != "" {
		format = fmt.Sprintf(`{{with index .NetworkSettings.Networks %q}}{{.IPAddress}}{{end}}`, network)
	}
	out, err := docker.Run("docker", "inspect", "-f", format, container)
	if err != nil {
		return "", fmt.Errorf("inspect %s: %w", container, err)
	}
	ip := strings.TrimSpace(out)
	if ip == "" {
		return "", fmt.Errorf("%s has no address on network %s", container, network)
	}
	return ip, nil
}

// retireOldContainers removes containers of a service that belong to any release
// other than the current one. This is what completes a blue/green swap, and it
// also cleans up leftovers from a deploy that failed midway.
func retireOldContainers(spec *release.Spec, name string, svc release.Service) {
	out, err := docker.Run("docker", "ps", "-a",
		"--filter", "label=shunt.project="+spec.Project,
		"--filter", "label=shunt.service="+name,
		"--format", "{{.Names}}\t{{.Label \"shunt.release\"}}")
	if err != nil {
		return
	}
	keep := serviceContainer(spec, name, svc)
	drain := drainSeconds(svc)
	for _, ln := range splitLines(out) {
		f := strings.SplitN(ln, "\t", 2)
		if len(f) < 2 || f[0] == keep {
			continue
		}
		info(fmt.Sprintf("draining %s (%ds)", f[0], drain))
		stopAndRemove(f[0], drain)
	}
}

// retireUndeclaredServices removes service containers for services the restored
// release does not have.
//
// Only rollback calls this, never a deploy. The two look alike and are not: a
// service dropped from the manifest is an orphan, which shunt reports and
// refuses to act on, because a human deleted it and only a human should stop it.
// A service that exists solely because the release being undone introduced it is
// shunt's own leftover — nobody asked for it, the release that created it is
// being taken back, and leaving it means the host keeps serving the code the
// rollback exists to remove.
//
// That also makes the ledger honest. A restored release that leaves one
// container of the failed one behind is still a mix, and `shunt status` would
// call it `failed` — production untouched — while a container from the bad
// release was in rotation.
//
// Accessories are exempt, as everywhere else: they are stateful, and no rollback
// should destroy a database because the release that booted it went away.
func retireUndeclaredServices(spec, outgoing *release.Spec) {
	out, err := docker.Run("docker", "ps", "-a",
		"--filter", "label=shunt.project="+spec.Project,
		"--filter", "label=shunt.kind=service",
		"--format", "{{.Names}}\t{{.Label \"shunt.service\"}}")
	if err != nil {
		return
	}
	for _, ln := range splitLines(out) {
		f := strings.SplitN(ln, "\t", 2)
		// An unlabelled container predates this and cannot be attributed to a
		// service, so it is left alone rather than guessed at.
		if len(f) < 2 || f[1] == "" {
			continue
		}
		if _, declared := spec.Services[f[1]]; declared {
			continue // its own swap has already dealt with it
		}
		drain := 10
		if outgoing != nil {
			if svc, ok := outgoing.Services[f[1]]; ok {
				drain = drainSeconds(svc)
			}
		}
		info(fmt.Sprintf("%s is not part of %s — removing it", f[0], spec.ID))
		stopAndRemove(f[0], drain)
		ok("rollback", f[0]+" removed; it belonged to the release being undone")
	}
}

// containerExists reports whether a container of that name is present in any
// state, running or stopped.
func containerExists(name string) bool {
	out, err := docker.Run("docker", "ps", "-aq", "--filter", "name=^/"+name+"$")
	return err == nil && strings.TrimSpace(out) != ""
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
	if spec.SecretsAsFiles() {
		// Values reach the container as files rather than environment, so they
		// never enter its configuration and never come back out of inspect.
		if len(spec.Secrets) > 0 {
			dir, err := writeSecretFiles(spec, svc.Secrets)
			if err != nil {
				return fmt.Errorf("service %s: %w", name, err)
			}
			args = append(args, "-v", dir+":"+release.SecretMountPath+":ro")
		}
	} else {
		// A service that narrowed its secrets gets a file holding only those.
		if len(svc.Secrets) > 0 {
			scoped, err := writeEnvFileScoped(spec, svc.Secrets)
			if err != nil {
				return fmt.Errorf("service %s: %w", name, err)
			}
			envFile = scoped
		}
		if envFile != "" {
			args = append(args, "--env-file", envFile)
		}
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
		"--label", "shunt.release="+spec.ID,
		// The definition this container was actually started with. `shunt plan`
		// compares it against the manifest, which is what lets a plan describe
		// the host rather than merely replaying the ledger.
		"--label", "shunt.config="+release.HashService(svc))
	// The commit on the container itself, so `docker inspect` answers "what is
	// this running" without going through shunt at all.
	if spec.Provenance.Short != "" {
		args = append(args, "--label", "shunt.commit="+spec.Provenance.Short)
	}
	args = append(args, proxyLabels(spec, name, svc)...)
	args = append(args, ref)
	args = append(args, svc.Command...)

	if out, err := docker.Run("docker", args...); err != nil {
		return fmt.Errorf("start %s: %s", container, strings.TrimSpace(out))
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
			docker.Ok("docker", "start", container)
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

// dependedOn returns the services that at least one other service requires.
//
// Accessories are excluded: they are already up and health-checked before any
// service starts, so nothing here needs to wait on them again.
func dependedOn(spec *release.Spec) map[string]bool {
	out := map[string]bool{}
	for _, svc := range spec.Services {
		for _, dep := range svc.Requires {
			if _, isService := spec.Services[dep]; isService {
				out[dep] = true
			}
		}
	}
	return out
}

// swapServices brings each service onto the new release in dependency order.
//
// Proxied services go blue/green: the new container starts alongside the old
// one, is health-checked before the old is retired, and never leaves a gap. The
// rest are stopped gracefully and restarted, which does have a gap — a published
// host port can only belong to one container at a time.
//
// The second return value separates "the deploy failed and production is
// exactly as it was" from "it failed halfway and the host is running a mix" —
// the distinction the ledger records as failed versus degraded.
func swapServices(spec, prev *release.Spec, envFile string) (started []string, mutated bool, err error) {
	// Services something else depends on are health-checked before their
	// dependents start, so `requires` is a readiness edge rather than only an
	// ordering one.
	depended := dependedOn(spec)

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

			// Only gate on services something actually depends on. Checking every
			// service here would serialise unrelated startups behind each other's
			// boot time for no benefit; the final health gate still covers them.
			if depended[name] {
				if err := waitHealthy(spec, []string{name}, spec.Services); err != nil {
					return started, mutated, fmt.Errorf("%w\n  %s is required by another service, which was not started", err, name)
				}
			}
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
