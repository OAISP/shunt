package manifest

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/OAISP/shunt/internal/release"
)

var (
	nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	sizeRE = regexp.MustCompile(`^([0-9]+)([bkmg]?)$`)
)

// sizeBytes parses a docker memory size: an integer with an optional b, k, m or g.
func sizeBytes(s string) (int64, bool) {
	m := sizeRE.FindStringSubmatch(strings.ToLower(s))
	if m == nil {
		return 0, false
	}
	shift := 10 * strings.Index("bkmg", m[2])
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil || n > math.MaxInt64>>shift {
		return 0, false
	}
	return n << shift, true
}

// restartRE is docker's own set of restart policies. One it does not know fails
// at create, which is after stages have run and artifacts have been swapped.
var restartRE = regexp.MustCompile(`^(no|always|unless-stopped|on-failure(:[0-9]+)?)$`)

// secretsProblems checks one [secrets] block, the manifest's own or a target's.
func secretsProblems(s *Secrets) []string {
	if s == nil {
		return nil
	}
	var out []string
	switch s.Provider {
	case "file", "sops":
		if s.Path == "" {
			out = append(out, fmt.Sprintf("provider %q requires path", s.Provider))
		}
	case "env":
		if len(s.Keys) == 0 {
			out = append(out, `provider "env" requires keys`)
		}
	case "":
		out = append(out, "provider is required")
	default:
		out = append(out, fmt.Sprintf("unknown provider %q (want file, env, or sops)", s.Provider))
	}
	// An unknown mode would fall back to environment delivery without a word.
	if s.Mode != "" && s.Mode != "env" && s.Mode != "file" {
		out = append(out, fmt.Sprintf("unknown mode %q (want env or file)", s.Mode))
	}
	return out
}

var (
	tmpfsRE = regexp.MustCompile(`^/[^:]*(:.+)?$`)
	capRE   = regexp.MustCompile(`^[A-Za-z_]+$`)
)

// limitProblems catches at validate time what docker would only reject on the
// host, at create — which is after stages have run.
func limitProblems(svc release.Limits) []string {
	var out []string
	if svc.CPUs != 0 && svc.CPUs < 0.01 {
		out = append(out, fmt.Sprintf("cpus %v must be at least 0.01", svc.CPUs))
	}
	if svc.PidsLimit < -1 {
		out = append(out, fmt.Sprintf("pids_limit %d must be a count, or -1 for unlimited", svc.PidsLimit))
	}
	for _, t := range svc.Tmpfs {
		if !tmpfsRE.MatchString(t) {
			out = append(out, fmt.Sprintf("tmpfs %q must be an absolute path, optionally followed by :options", t))
		}
	}
	for _, c := range slices.Concat(svc.CapDrop, svc.CapAdd) {
		if !capRE.MatchString(c) {
			out = append(out, fmt.Sprintf("%q is not a capability name such as ALL or NET_BIND_SERVICE", c))
		}
	}
	for k := range svc.LogOpts {
		if k == "" || strings.Contains(k, "=") {
			out = append(out, fmt.Sprintf("log_opts key %q is not an option name such as max-size", k))
		}
	}
	mem, memOK := sizeBytes(svc.Memory)
	switch {
	case svc.Memory == "":
	case !memOK:
		out = append(out, fmt.Sprintf("memory %q is not a size such as 512m or 1g", svc.Memory))
	case mem < 6<<20:
		out = append(out, fmt.Sprintf("memory %q is below docker's 6m minimum", svc.Memory))
	}
	if svc.MemorySwap == "" || svc.MemorySwap == "-1" {
		return out
	}
	swap, ok := sizeBytes(svc.MemorySwap)
	switch {
	case !ok:
		out = append(out, fmt.Sprintf("memory_swap %q is not a size such as 1536m, or -1 for unlimited swap", svc.MemorySwap))
	case svc.Memory == "":
		out = append(out, "memory_swap is memory plus swap, so it needs memory set as well")
	case memOK && swap < mem:
		out = append(out, fmt.Sprintf("memory_swap %q is memory plus swap, so it cannot be less than memory %q", svc.MemorySwap, svc.Memory))
	}
	return out
}

func (m *Manifest) Validate() error {
	var errs []string
	add := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }

	if m.Project == "" {
		add("project is required")
	} else if !nameRE.MatchString(m.Project) {
		add("project %q must be lowercase alphanumeric with - or _", m.Project)
	}
	if m.Host == "" {
		add("host is required (e.g. host = \"deploy@vps.example.com\")")
	}
	if len(m.Services) == 0 {
		add("at least one [services.*] is required")
	}

	for name := range m.Accessories {
		if _, clash := m.Services[name]; clash {
			add("%q is declared as both a service and an accessory", name)
		}
	}

	for name, svc := range all(m.Services, m.Accessories) {
		if !nameRE.MatchString(name) {
			add("service %q: name must be lowercase alphanumeric with - or _", name)
		}
		// An image is either built by us or pulled — both are fine, but a service
		// pointing at a built image that doesn't exist is a typo.
		if _, built := m.Images[svc.Image]; !built && !strings.ContainsAny(svc.Image, ":/.") {
			add("service %q: image %q is not defined in [images.%s] and does not look like a pullable reference",
				name, svc.Image, svc.Image)
		}
		for _, dep := range svc.Requires {
			_, isSvc := m.Services[dep]
			_, isAcc := m.Accessories[dep]
			if !isSvc && !isAcc {
				add("service %q: requires unknown service or accessory %q", name, dep)
			}
		}
		if len(svc.Secrets) > 0 && m.Secrets == nil {
			add("service %q: lists secrets but the manifest declares no [secrets] block", name)
		}
		if svc.Health != nil && svc.Health.URL == "" && len(svc.Health.Command) == 0 {
			add("service %q: health block needs either url or command", name)
		}
		// A health url written as a bare path is resolved against the container's
		// own IP, which requires knowing which port to hit.
		if svc.Health != nil && strings.HasPrefix(svc.Health.URL, "/") &&
			svc.Expose == 0 && svc.ProxyPort() == 0 && !svc.HasPublishedPort() {
			add("service %q: health url %q is a path, so the service needs `expose` or a `publish` mapping with a host port",
				name, svc.Health.URL)
		}
		if !restartRE.MatchString(svc.Restart) {
			add("service %q: restart %q is not a docker restart policy (no, always, unless-stopped, on-failure[:N])", name, svc.Restart)
		}
		for _, problem := range limitProblems(svc.Limits) {
			add("service %q: %s", name, problem)
		}
		if p := svc.Proxy; p != nil {
			switch p.Kind {
			case "traefik", "caddy":
			default:
				add("service %q: proxy kind %q is not supported (want traefik or caddy)", name, p.Kind)
			}
			if p.Host == "" {
				add("service %q: proxy needs a host", name)
			}
			if svc.ProxyPort() == 0 {
				add("service %q: proxy needs `expose` or `proxy.port`", name)
			}
			// Publishing a host port pins the service to one container at a time,
			// which is exactly what proxying exists to avoid.
			if len(svc.Publish) > 0 {
				add("service %q: cannot combine `publish` with `proxy` — a published host port prevents two releases running side by side; use `expose`", name)
			}
			if svc.Health == nil {
				add("service %q: proxy requires a health block, or a broken release would be put into rotation", name)
			}
		}
	}

	for name, img := range m.Images {
		if !nameRE.MatchString(name) {
			add("image %q: name must be lowercase alphanumeric with - or _", name)
		}
		// A layout is usually produced by an earlier CI step, so its absence here
		// is normal; Import reports it when a deploy actually needs it.
		if img.Layout != "" {
			if img.Context != "" || img.Dockerfile != "" || img.Target != "" || len(img.Args) > 0 {
				add("image %q: layout ships a prebuilt image, so context, dockerfile, target and args do not apply", name)
			}
			continue
		}
		if _, err := os.Stat(m.Abs(img.Dockerfile)); err != nil {
			add("image %q: dockerfile %s not found", name, img.Dockerfile)
		}
	}

	seenStage := map[string]bool{}
	for i, st := range m.Stages {
		if st.Name == "" {
			add("stages[%d]: name is required", i)
			continue
		}
		if seenStage[st.Name] {
			add("stage %q: duplicate name", st.Name)
		}
		seenStage[st.Name] = true
		if st.Image == "" {
			add("stage %q: image is required", st.Name)
		} else if _, built := m.Images[st.Image]; !built && !strings.ContainsAny(st.Image, ":/.") {
			add("stage %q: image %q is not defined in [images.%s] and does not look like a pullable reference",
				st.Name, st.Image, st.Image)
		}
		if len(st.Command) == 0 {
			add("stage %q: command is required", st.Name)
		}
		if st.RequireNonEmpty && st.Capture == "" {
			add("stage %q: require_nonempty has no meaning without capture", st.Name)
		}
	}

	seenArtifact := map[string]bool{}
	for i, a := range m.Artifacts {
		switch {
		case a.Name == "":
			add("artifacts[%d]: name is required", i)
		case !nameRE.MatchString(a.Name):
			add("artifact %q: name must be lowercase alphanumeric with - or _", a.Name)
		case seenArtifact[a.Name]:
			add("artifact %q: duplicate name", a.Name)
		}
		seenArtifact[a.Name] = true

		if a.Src == "" {
			add("artifact %q: src is required", a.Name)
		}
		if a.Dest == "" {
			add("artifact %q: dest is required", a.Name)
		} else if !filepath.IsAbs(a.Dest) {
			add("artifact %q: dest %q must be an absolute path on the host", a.Name, a.Dest)
		} else if strings.HasSuffix(a.Dest, "/") {
			add("artifact %q: dest %q must not end in a slash", a.Name, a.Dest)
		}
		if a.Retain < 0 {
			add("artifact %q: retain cannot be negative", a.Name)
		}
	}

	for _, problem := range secretsProblems(m.Secrets) {
		add("secrets: %s", problem)
	}

	for name, t := range m.Targets {
		if !nameRE.MatchString(name) {
			add("target %q: name must be lowercase alphanumeric with - or _", name)
		}
		if t.Host == "" {
			add("target %q: host is required", name)
		}
		if t.Project != "" && !nameRE.MatchString(t.Project) {
			add("target %q: project %q must be lowercase alphanumeric with - or _", name, t.Project)
		}
		for _, problem := range secretsProblems(t.Secrets) {
			add("target %q: secrets: %s", name, problem)
		}
	}

	if cyc := m.detectCycle(); cyc != "" {
		add("service dependency cycle: %s", cyc)
	}

	if len(errs) > 0 {
		slices.Sort(errs)
		return fmt.Errorf("invalid shunt.toml:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}
