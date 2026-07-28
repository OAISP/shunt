package manifest

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

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

	if m.Secrets != nil {
		switch m.Secrets.Provider {
		case "file", "sops":
			if m.Secrets.Path == "" {
				add("secrets: provider %q requires path", m.Secrets.Provider)
			}
		case "env":
			if len(m.Secrets.Keys) == 0 {
				add("secrets: provider \"env\" requires keys")
			}
		case "":
			add("secrets: provider is required")
		default:
			add("secrets: unknown provider %q (want file, env, or sops)", m.Secrets.Provider)
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
