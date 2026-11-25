package main

import (
	"cmp"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/OAISP/shunt/internal/release"
)

// healthCheck gates the release on every service reporting healthy, skipping
// the ones the swap already gated.
//
// Two kinds were checked inline: proxied services, which must be proven healthy
// before the old container leaves rotation, and any service another one depends
// on, which is checked before its dependents start. Re-probing those is not
// merely redundant — a `grace` is a sleep, so a required service with a 30s
// grace made the deploy wait a full minute for one container.
func healthCheck(spec *release.Spec) error {
	// Only what this release started. A container it left running was probed
	// when the swap decided to keep it.
	out, _ := docker.Run("docker", "ps", "-a",
		"--filter", "label=shunt.project="+spec.Project,
		"--filter", "label=shunt.release="+spec.ID,
		"--format", `{{.Label "shunt.service"}}`)
	fresh := splitLines(out)

	gated := dependedOn(spec)
	var pending []string
	for _, name := range spec.Order {
		svc, present := spec.Services[name]
		if present && !svc.Proxied() && !gated[name] && slices.Contains(fresh, name) {
			pending = append(pending, name)
		}
	}
	return waitHealthy(spec, pending, spec.Services)
}

// waitHealthy probes the named containers in order and fails the deploy on the
// first one that never comes up. The ones with no health check are held to the
// weaker gate afterwards: see waitStable.
func waitHealthy(spec *release.Spec, order []string, set map[string]release.Service) error {
	var unprobed []string
	for _, name := range order {
		svc, present := set[name]
		if !present {
			continue
		}
		if svc.Health == nil {
			unprobed = append(unprobed, name)
			continue
		}
		h := svc.Health
		container := serviceContainer(spec, name, svc)
		step("health:"+name, "waiting for "+name+" to become healthy")

		if h.Grace != "" {
			if d, err := time.ParseDuration(h.Grace); err == nil {
				time.Sleep(d)
			}
		}
		interval := 3 * time.Second
		if d, err := time.ParseDuration(h.Interval); err == nil && d > 0 {
			interval = d
		}
		retries := h.Retries
		if retries <= 0 {
			retries = 10
		}

		var last string
		healthy := false
		for i := 0; i < retries; i++ {
			var err error
			last, err = probe(spec, container, svc, h)
			if err == nil {
				healthy = true
				break
			}
			last = err.Error()
			// Surface a container that already died rather than burning the full
			// retry budget waiting for a process that will never listen.
			if state, _ := docker.Run("docker", "inspect", "-f", "{{.State.Status}}", container); len(state) > 0 {
				if s := strings.TrimSpace(state); s == "exited" || s == "dead" {
					return fmt.Errorf("%s exited during startup:\n%s", container, tailLogs(container, 20))
				}
			}
			time.Sleep(interval)
		}
		if !healthy {
			return fmt.Errorf("%s did not become healthy after %d attempts (%s)\n%s",
				container, retries, last, tailLogs(container, 20))
		}
		ok("health:"+name, name+" healthy")
	}
	return waitStable(spec, unprobed, set)
}

// settle is how long a container with no health check has to stay up.
var settle = 5 * time.Second

// lastStart is when the helper last started a container, so time spent since
// then, probing other services, counts toward the settle.
var lastStart time.Time

// waitStable holds freshly started containers that have no health check to the
// one thing that can be asked of them: staying up. Without it a worker that
// dies on startup is restarted by docker forever under a release reported
// active. One shared wait, however many there are.
func waitStable(spec *release.Spec, names []string, set map[string]release.Service) error {
	if len(names) == 0 {
		return nil
	}
	time.Sleep(settle - time.Since(lastStart))
	for _, name := range names {
		container := serviceContainer(spec, name, set[name])
		out, err := docker.Run("docker", "inspect", "-f",
			"{{.State.Status}} {{.RestartCount}} {{.State.ExitCode}}", container)
		if err != nil {
			return fmt.Errorf("%s is gone: %s", container, strings.TrimSpace(out))
		}
		var status string
		var restarts, code int
		fmt.Sscan(out, &status, &restarts, &code)
		// Exiting 0 and staying exited is a one-shot doing its job; docker
		// restarting it, or a non-zero exit, is not.
		if restarts > 0 || status == "restarting" || code != 0 {
			return fmt.Errorf("%s is not staying up (%s, exit code %d, restarted %d times):\n%s",
				container, status, code, restarts, tailLogs(container, 20))
		}
	}
	return nil
}

// probeTimeout bounds one probe. Without it a health command that blocks would
// hold the deploy lock, and an in-place service's gap, open indefinitely.
const probeTimeout = 30 * time.Second

// probe runs one health check.
//
// An absolute url is fetched as-is, which matches a service with a published
// host port. A bare path is resolved against the container's own address on the
// deploy network — that is how a proxied service, which publishes nothing, is
// checked without needing anything inside the image or on the host.
// A command check runs inside the container.
func probe(spec *release.Spec, container string, svc release.Service, h *release.Health) (string, error) {
	if h.URL != "" {
		target := h.URL
		// A proxied service is always probed on its own address: an absolute url
		// goes through the proxy, where the container being replaced answers it.
		if svc.Proxied() {
			target = cmp.Or(release.HealthProbePath(svc), "/")
		}
		if strings.HasPrefix(target, "/") {
			base, err := probeBase(spec, container, svc)
			if err != nil {
				return "", err
			}
			target = base + target
		}
		return probeURL(target, h.Follow)
	}
	args := append([]string{"exec", container}, h.Command...)
	out, err := docker.RunFor(probeTimeout, "docker", args...)
	if err != nil {
		return "", fmt.Errorf("%v: %s", err, strings.TrimSpace(out))
	}
	return strings.TrimSpace(out), nil
}

// probeBase turns a service into the origin a bare health path is resolved
// against.
//
// A service that publishes a host port is reachable there, which is how it is
// actually served; one that only exposes a port is reachable on its own address
// on the deploy network. Supporting both means a health check can be written as
// "/health" regardless of which shape the service has.
func probeBase(spec *release.Spec, container string, svc release.Service) (string, error) {
	if host, port, ok := publishedHostPort(svc); ok {
		return "http://" + net.JoinHostPort(host, port), nil
	}
	port := svc.Expose
	if svc.Proxy != nil && svc.Proxy.Port != 0 {
		port = svc.Proxy.Port
	}
	if port == 0 {
		return "", fmt.Errorf("cannot resolve a health path for %s: it neither publishes nor exposes a port", container)
	}
	ip, err := containerIP(container, spec.Network)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("http://%s:%d", ip, port), nil
}

// publishedHostPort extracts the address a published service is reachable on
// from the first publish mapping, which takes the forms "port",
// "host:container" or "ip:host:container".
//
// The bind address matters: a service published as "10.0.0.5:9090:3000" is not
// listening on 127.0.0.1, and probing the wrong address fails the health check
// after the container has already been swapped in.
func publishedHostPort(svc release.Service) (host, port string, ok bool) {
	for _, p := range svc.Publish {
		// Strip any /tcp or /udp suffix before splitting.
		spec, _, _ := strings.Cut(p, "/")
		parts := strings.Split(spec, ":")
		switch len(parts) {
		case 2: // hostPort:containerPort
			return "127.0.0.1", parts[0], true
		case 3: // ip:hostPort:containerPort
			addr := parts[0]
			// 0.0.0.0 and :: mean "every interface", which includes loopback and
			// is the one address certain to be reachable from the host itself.
			if addr == "" || addr == "0.0.0.0" || addr == "::" {
				addr = "127.0.0.1"
			}
			return addr, parts[1], true
		}
		// A bare container port means Docker picks a random host port; there is
		// nothing stable to probe.
	}
	return "", "", false
}

// probeURL fetches a health url and judges the status code.
//
// A bare redirect only proves the server is listening. Accepting it is the
// historical default and stays for compatibility, but follow asks for the
// stronger gate: chase the redirect and require the page at the end of it.
func probeURL(target string, follow bool) (string, error) {
	client := &http.Client{Timeout: probeTimeout}
	if !follow {
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	resp, err := client.Get(target)
	if err != nil {
		return "", fmt.Errorf("GET %s: %v", target, err)
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()

	code := strconv.Itoa(resp.StatusCode)
	switch c := resp.StatusCode; {
	case c >= 200 && c < 300, c >= 300 && c < 400 && !follow:
		return code, nil
	case c >= 300 && c < 400:
		return "", fmt.Errorf("HTTP %s from %s after following redirects (a chain that never reaches 2xx)", code, target)
	}
	return "", fmt.Errorf("HTTP %s from %s", code, target)
}

func tailLogs(container string, n int) string {
	out, _ := docker.Run("docker", "logs", "--tail", fmt.Sprint(n), container)
	s := strings.TrimSpace(out)
	if s == "" {
		return "  (no container logs)"
	}
	return "  " + strings.ReplaceAll(s, "\n", "\n  ")
}
