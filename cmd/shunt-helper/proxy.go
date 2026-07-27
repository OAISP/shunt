package main

import (
	"fmt"
	"strings"

	"github.com/OAISP/shunt/internal/release"
)

// proxyLabels emits the discovery labels an external reverse proxy already
// watches for. shunt runs no proxy itself — it just makes the new container
// visible and lets Traefik or caddy-docker-proxy do the switchover.
func proxyLabels(spec *release.Spec, name string, svc release.Service) []string {
	p := svc.Proxy
	if p == nil {
		return nil
	}
	id := spec.Project + "-" + name
	var out []string
	add := func(k, v string) { out = append(out, "--label", k+"="+v) }

	switch p.Kind {
	case "caddy":
		add("caddy", p.Host)
		add("caddy.reverse_proxy", fmt.Sprintf("{{upstreams %d}}", p.Port))
		if p.Path != "" && p.Path != "/" {
			add("caddy.route", p.Path+"*")
		}
	default: // traefik
		add("traefik.enable", "true")
		rule := fmt.Sprintf("Host(`%s`)", p.Host)
		if p.Path != "" && p.Path != "/" {
			rule += fmt.Sprintf(" && PathPrefix(`%s`)", p.Path)
		}
		add("traefik.http.routers."+id+".rule", rule)
		if len(p.EntryPoints) > 0 {
			add("traefik.http.routers."+id+".entrypoints", strings.Join(p.EntryPoints, ","))
		}
		// Closes the last gap in a blue/green swap: a keep-alive connection the
		// old container tears down mid-request is a connection failure, not a
		// response, so reissuing it is safe and turns a 502 into a served request.
		if p.Retry > 0 {
			add("traefik.http.middlewares."+id+"-retry.retry.attempts", fmt.Sprint(p.Retry))
			add("traefik.http.middlewares."+id+"-retry.retry.initialinterval", "100ms")
			add("traefik.http.routers."+id+".middlewares", id+"-retry")
		}
		add("traefik.http.services."+id+".loadbalancer.server.port", fmt.Sprint(p.Port))
		if spec.Network != "" {
			add("traefik.docker.network", spec.Network)
		}
		// Router and service names are stable across releases on purpose: during
		// the overlap both containers register as backends of the same load
		// balancer, so traffic is served throughout the switch.
		//
		// The proxy-side health check is what keeps a container that is still
		// booting out of rotation — without it the overlap would serve errors.
		if svc.Health != nil && strings.HasPrefix(svc.Health.URL, "/") {
			add("traefik.http.services."+id+".loadbalancer.healthcheck.path", svc.Health.URL)
			add("traefik.http.services."+id+".loadbalancer.healthcheck.interval", "2s")
			add("traefik.http.services."+id+".loadbalancer.healthcheck.timeout", "2s")
		}
	}
	return out
}

// sameProxy reports whether two releases would emit identical proxy labels.
//
// This matters more than it looks: a label-discovered router is defined by every
// container carrying its labels, and Traefik refuses to serve a router that two
// containers define differently — it drops the route entirely, which is a total
// outage rather than a blip. So an overlap is only safe when the proxy config is
// unchanged.
func sameProxy(a, b *release.Proxy) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Kind != b.Kind || a.Host != b.Host || a.Path != b.Path || a.Port != b.Port || a.Retry != b.Retry {
		return false
	}
	if len(a.EntryPoints) != len(b.EntryPoints) {
		return false
	}
	for i := range a.EntryPoints {
		if a.EntryPoints[i] != b.EntryPoints[i] {
			return false
		}
	}
	return true
}

// canOverlap reports whether the new container for a proxied service may run
// alongside the old one.
func canOverlap(prev *release.Spec, name string, svc release.Service) bool {
	if prev == nil {
		return true
	}
	old, present := prev.Services[name]
	if !present {
		return true
	}
	return sameProxy(old.Proxy, svc.Proxy)
}
