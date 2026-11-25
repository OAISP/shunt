package main

import (
	"cmp"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strconv"
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
// a published host port allows only one container at a time anyway. So does an
// accessory, proxied or not: it is booted once and found again by that name.
func serviceContainer(spec *release.Spec, name string, svc release.Service) string {
	if _, accessory := spec.Accessories[name]; svc.Proxied() && !accessory {
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
// It reports whether there was a container to remove.
func stopAndRemove(container string, drain int) bool {
	if !containerExists(container) {
		return false
	}
	start := time.Now()
	// -t, not --timeout: docker only learned the long name in 28.
	out, err := docker.Run("docker", "stop", "-t", fmt.Sprint(drain), container)
	took := time.Since(start)
	switch {
	case err != nil && !strings.Contains(out, "No such container"):
		// The removal below kills it either way: an in-place service has to give
		// up its port. Said out loud, because a stop that fails on every deploy
		// otherwise looks exactly like one that drains.
		info(fmt.Sprintf("%s could not be stopped gracefully and was killed without draining: %s",
			container, strings.TrimSpace(out)))
	case drain > 0 && took >= time.Duration(drain)*time.Second && exitCode(container) == "137":
		// Usually the largest cost in a deploy, and invisible unless reported.
		// Both conditions: 137 alone is also what an earlier OOM kill left behind,
		// and elapsed time alone is also a clean exit right at the deadline.
		info(fmt.Sprintf("%s was still running after its %ds drain and was killed — it either ignores SIGTERM "+
			"(handle it, or set `init = true` if it runs as PID 1) or had work in flight (a longer `drain` lets it finish)",
			container, drain))
	}
	docker.Ok("docker", "rm", "-f", container)
	return true
}

func exitCode(container string) string {
	out, _ := docker.Run("docker", "inspect", "-f", "{{.State.ExitCode}}", container)
	return strings.TrimSpace(out)
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
func retireOldContainers(spec *release.Spec, name string, svc release.Service, keep string) {
	out, err := docker.Run("docker", "ps", "-a",
		"--filter", "label=shunt.project="+spec.Project,
		"--filter", "label=shunt.service="+name,
		"--format", "{{.Names}}\t{{.Label \"shunt.release\"}}")
	if err != nil {
		return
	}
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

// inputsDigest fingerprints what a container is created from besides its own
// definition: the image, the release-wide settings and the secrets it is handed.
// Kept on the container as a label, it is how a later release recognises a
// container it would only recreate identically.
//
// Keyed by the ledger's salt, because the label is readable by anything that
// can inspect the container and a bare digest of a short secret is guessable.
func inputsDigest(spec *release.Spec, svc release.Service, salt string) (string, error) {
	secrets, err := narrow(spec.Secrets, svc.Secrets)
	if err != nil {
		return "", err
	}
	img := spec.Images[svc.Image]
	h := hmac.New(sha256.New, []byte(salt))
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00", cmp.Or(img.Fingerprint, imageID(img.Ref)), spec.Network, spec.SecretMode)
	for _, k := range sortedKeys(secrets) {
		fmt.Fprintf(h, "%s\x00%s\x00", k, secrets[k])
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

// imageID identifies an image shunt did not build, whose tag can move.
func imageID(ref string) string {
	out, _ := docker.Run("docker", "image", "inspect", "-f", "{{.Id}}", ref)
	return strings.TrimSpace(out)
}

// upToDate names the running container of a service that this release would
// only recreate identically, or "" when there is none. Found by label rather
// than by name: a proxied container is named after the release that started it.
func upToDate(spec *release.Spec, name string, svc release.Service, inputs string) string {
	out, err := docker.Run("docker", "ps", "--filter", "status=running",
		"--filter", "label=shunt.project="+spec.Project,
		"--filter", "label=shunt.service="+name,
		"--filter", "label=shunt.config="+release.HashService(svc),
		"--filter", "label=shunt.inputs="+inputs,
		"--format", "{{.Names}}")
	if names := splitLines(out); err == nil && len(names) > 0 {
		return names[0]
	}
	return ""
}

// stillHealthy probes a container that is about to be left running. It passed
// its gate when it started, which says nothing about now: a release must not be
// reported active, or a dependent started, over a service that is down. One
// attempt, since a container that has been up for a while has no startup to
// wait out.
func stillHealthy(spec *release.Spec, container string, svc release.Service) bool {
	if svc.Health == nil {
		return true
	}
	_, err := probe(spec, container, svc, svc.Health)
	return err == nil
}

// containerExists reports whether a container of that name is present in any
// state, running or stopped.
func containerExists(name string) bool {
	out, err := docker.Run("docker", "ps", "-aq", "--filter", "name=^/"+name+"$")
	return err == nil && strings.TrimSpace(out) != ""
}

// startContainer replaces any container of the same name with a fresh one, and
// reports whether there was one to replace.
//
// The replacement is created before the old container is stopped. Creating it
// is where docker checks the image, mounts and arguments, so a definition it
// rejects leaves the old container serving, and the gap an in-place swap cannot
// avoid shrinks to the stop and the start.
func startContainer(spec *release.Spec, name string, svc release.Service, envFile, kind, inputs string) (replaced bool, err error) {
	ref, err := imageRef(spec, svc.Image)
	if err != nil {
		return false, err
	}
	container := serviceContainer(spec, name, svc)
	staged := container + ".next"
	docker.Ok("docker", "rm", "-f", staged) // left by an interrupted deploy

	args := []string{"create", "--name", staged, "--restart", svc.Restart}
	if svc.Init {
		args = append(args, "--init")
	}
	args = append(args, limitArgs(svc.Limits)...)
	if spec.Network != "" {
		args = append(args, "--network", spec.Network, "--network-alias", name)
	}
	secretFlags, err := secretArgs(spec, name, svc, envFile)
	if err != nil {
		return false, err
	}
	args = append(args, secretFlags...)
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
	if inputs != "" {
		args = append(args, "--label", "shunt.inputs="+inputs)
	}
	args = append(args, proxyLabels(spec, name, svc)...)
	args = append(args, ref)
	args = append(args, svc.Command...)

	if out, err := docker.Run("docker", args...); err != nil {
		return false, fmt.Errorf("create %s: %s", container, strings.TrimSpace(out))
	}
	// Gracefully, not with rm -f: SIGKILL drops whatever is still in flight.
	replaced = stopAndRemove(container, drainSeconds(svc))
	if out, err := docker.Run("docker", "rename", staged, container); err != nil {
		return replaced, fmt.Errorf("rename %s: %s", staged, strings.TrimSpace(out))
	}
	if out, err := docker.Run("docker", "start", container); err != nil {
		return replaced, fmt.Errorf("start %s: %s", container, strings.TrimSpace(out))
	}
	lastStart = time.Now()
	return replaced, nil
}

// limitArgs turns a service's limits into create flags, so the container never
// runs without them.
func limitArgs(l release.Limits) []string {
	var args []string
	add := func(flag string, values ...string) {
		for _, v := range values {
			if v != "" {
				args = append(args, flag, v)
			}
		}
	}
	add("--memory", l.Memory)
	add("--memory-swap", l.MemorySwap)
	if l.CPUs > 0 {
		add("--cpus", strconv.FormatFloat(l.CPUs, 'f', -1, 64))
	}
	if l.PidsLimit != 0 {
		add("--pids-limit", strconv.Itoa(l.PidsLimit))
	}
	if l.ReadOnly {
		args = append(args, "--read-only")
	}
	add("--tmpfs", l.Tmpfs...)
	add("--cap-drop", l.CapDrop...)
	add("--cap-add", l.CapAdd...)
	if l.NoNewPrivileges {
		add("--security-opt", "no-new-privileges")
	}
	add("--log-driver", l.LogDriver)
	for _, k := range slices.Sorted(maps.Keys(l.LogOpts)) {
		add("--log-opt", k+"="+l.LogOpts[k])
	}
	return args
}

// secretArgs writes the secrets a service is handed and returns the flags that
// deliver them.
func secretArgs(spec *release.Spec, name string, svc release.Service, envFile string) ([]string, error) {
	if spec.SecretsAsFiles() {
		if len(spec.Secrets) == 0 {
			return nil, nil
		}
		// Values reach the container as files rather than environment, so they
		// never enter its configuration and never come back out of inspect.
		dir, err := writeSecretFiles(spec, svc.Secrets)
		if err != nil {
			return nil, fmt.Errorf("service %s: %w", name, err)
		}
		return []string{"-v", dir + ":" + release.SecretMountPath + ":ro"}, nil
	}
	// A service that narrowed its secrets gets a file holding only those.
	if len(svc.Secrets) > 0 {
		scoped, err := writeEnvFileScoped(spec, svc.Secrets)
		if err != nil {
			return nil, fmt.Errorf("service %s: %w", name, err)
		}
		envFile = scoped
	}
	if envFile == "" {
		return nil, nil
	}
	return []string{"--env-file", envFile}, nil
}

// ensureAccessories brings up stateful dependencies that are not already
// present, before any stage runs — a migration needs its database up.
//
// An accessory that already exists is left strictly alone. Recreating Postgres
// on every code deploy would be both pointless and, with the wrong volume
// config, destructive; changing one is the explicit `shunt boot` operation.
func ensureAccessories(spec *release.Spec, ledger *release.Ledger, envFile string) error {
	var gate []string
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
			// One that was already there is probed if it can be, and otherwise
			// trusted: its restart count says nothing about this deploy.
			if acc.Health != nil {
				gate = append(gate, name)
			}
			continue
		}
		step("accessory:"+name, "booting "+container)
		if _, err := startContainer(spec, name, acc, envFile, "accessory", ""); err != nil {
			return err
		}
		// Recorded here rather than from the release spec: a deploy records the
		// manifest it was handed, but only this branch actually applied one.
		ledger.RecordAccessory(name, acc)
		ok("accessory:"+name, container+" booted")
		gate = append(gate, name)
	}
	return waitHealthy(spec, gate, spec.Accessories)
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
// A service whose running container already matches and still passes its health
// check is left alone, unless replaceAll says otherwise. That is when the data
// under it changed — a container goes on holding the file or directory it
// mounted, not the one swapped into its place — and when an earlier attempt did
// not end cleanly, so a matching container proves nothing.
//
// The second return value separates "the deploy failed and production is
// exactly as it was" from "it failed halfway and the host is running a mix" —
// the distinction the ledger records as failed versus degraded.
func swapServices(spec, prev *release.Spec, envFile, salt string, replaceAll bool) (started []string, mutated bool, err error) {
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

		inputs, err := inputsDigest(spec, svc, salt)
		if err != nil {
			return started, mutated, fmt.Errorf("service %s: %w", name, err)
		}
		keep := ""
		if !replaceAll {
			keep = upToDate(spec, name, svc, inputs)
		}
		if keep != "" && !stillHealthy(spec, keep, svc) {
			info(keep + " is unchanged but failing its health check — replacing it")
			keep = ""
		}
		if keep != "" {
			// Written for this release all the same: a rollback to it, and
			// `shunt run`, read its secrets by release id.
			if _, err := secretArgs(spec, name, svc, envFile); err != nil {
				return started, mutated, err
			}
			retireOldContainers(spec, name, svc, keep)
			started = append(started, name)
			ok("swap:"+name, keep+" unchanged — left running")
			continue
		}

		if !svc.Proxied() {
			step("swap:"+name, "replacing "+container)
			replaced, err := startContainer(spec, name, svc, envFile, "service", inputs)
			// A replacement docker refused to create never touched the old
			// container; once that was stopped, the host has changed whether or
			// not the new one comes up.
			if err != nil {
				return started, mutated || replaced, err
			}
			mutated = true
			// A service that used to be proxied left per-release containers
			// behind, and those still carry their proxy labels — so without this
			// they would keep serving the old code indefinitely.
			retireOldContainers(spec, name, svc, container)
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
		overlap := canOverlap(prev, spec, name)
		if !overlap {
			info("proxy config for " + name + " changed — retiring the old container first to avoid a router conflict (brief gap)")
			mutated = true
			retireOldContainers(spec, name, svc, container)
		}

		step("swap:"+name, "starting "+container+" alongside the running release")
		if _, err := startContainer(spec, name, svc, envFile, "service", inputs); err != nil {
			return started, mutated, err
		}
		// The old container is still serving, so a broken new release must be
		// pulled back out of rotation immediately rather than left half-live.
		// Nothing was replaced, so this alone does not count as mutating.
		if err := waitHealthy(spec, []string{name}, spec.Services); err != nil {
			stopAndRemove(container, 0)
			left := "the previous release is still serving"
			if !overlap {
				left = "the previous one was already retired, so " + name + " is down"
			}
			return started, mutated, fmt.Errorf("%w\n  the new container was removed; %s", err, left)
		}
		started = append(started, name)
		ok("swap:"+name, container+" healthy and in rotation")

		mutated = true
		retireOldContainers(spec, name, svc, container)
		ok("swap:"+name, "previous "+name+" drained")
	}
	return started, mutated, nil
}
