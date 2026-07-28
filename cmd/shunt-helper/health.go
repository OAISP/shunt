package main

import (
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"

	"github.com/OAISP/shunt/internal/release"
)

// healthCheck gates the release on every service reporting healthy. Proxied
// services were already checked inline during the swap — re-probing them here
// would just repeat work.
func healthCheck(spec *release.Spec) error {
	var pending []string
	for _, name := range spec.Order {
		if svc, present := spec.Services[name]; present && !svc.Proxied() {
			pending = append(pending, name)
		}
	}
	return waitHealthy(spec, pending, spec.Services)
}

// waitHealthy probes the named containers in order and fails the deploy on the
// first one that never comes up.
func waitHealthy(spec *release.Spec, order []string, set map[string]release.Service) error {
	for _, name := range order {
		svc, present := set[name]
		if !present || svc.Health == nil {
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
			if state, _ := exec.Command("docker", "inspect", "-f", "{{.State.Status}}", container).Output(); len(state) > 0 {
				if s := strings.TrimSpace(string(state)); s == "exited" || s == "dead" {
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
	return nil
}

// probe runs one health check.
//
// An absolute url is fetched as-is, which matches a service with a published
// host port. A bare path is resolved against the container's own address on the
// deploy network — that is how a proxied service, which publishes nothing, is
// checked without needing curl inside the image or a second container.
// A command check runs inside the container.
func probe(spec *release.Spec, container string, svc release.Service, h *release.Health) (string, error) {
	if h.URL != "" {
		target := h.URL
		if strings.HasPrefix(target, "/") {
			base, err := probeBase(spec, container, svc)
			if err != nil {
				return "", err
			}
			target = base + h.URL
		}
		args := []string{"-sS", "-o", "/dev/null", "-w", "%{http_code}", "--max-time", "10"}
		if h.Follow {
			args = append(args, "-L")
		}
		out, err := exec.Command("curl", append(args, target)...).Output()
		if err != nil {
			return "", fmt.Errorf("curl %s: %v", target, err)
		}
		code := strings.TrimSpace(string(out))
		if strings.HasPrefix(code, "2") {
			return code, nil
		}
		// A bare redirect only proves the server is listening. Accepting it is
		// the historical default and stays for compatibility, but a service that
		// sets follow has asked for the stronger gate: chase the redirect and
		// require the page at the end of it.
		if !h.Follow && strings.HasPrefix(code, "3") {
			return code, nil
		}
		if h.Follow && strings.HasPrefix(code, "3") {
			return "", fmt.Errorf("HTTP %s from %s after following redirects (a redirect loop, or a chain that never reaches 2xx)", code, target)
		}
		return "", fmt.Errorf("HTTP %s from %s", code, target)
	}
	args := append([]string{"exec", container}, h.Command...)
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
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
// The bind address matters and used to be discarded. A service published as
// "10.0.0.5:9090:3000" is not listening on 127.0.0.1, so probing there failed
// the health check after the container had already been swapped in — a healthy
// release reported broken because shunt looked in the wrong place.
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

func tailLogs(container string, n int) string {
	out, _ := exec.Command("docker", "logs", "--tail", fmt.Sprint(n), container).CombinedOutput()
	s := strings.TrimSpace(string(out))
	if s == "" {
		return "  (no container logs)"
	}
	return "  " + strings.ReplaceAll(s, "\n", "\n  ")
}
