// Package manifest defines shunt.toml — the committed description of a production
// deployment. It is the source of truth: everything the remote host ends up running
// is derived from this file plus resolved secrets, never from ad-hoc flags.
package manifest

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

type Manifest struct {
	Project string `toml:"project"`
	Host    string `toml:"host"` // user@host, resolved through the user's ssh config

	Network string           `toml:"network"`
	Images  map[string]Image `toml:"images"`

	// Accessories are stateful dependencies — a database, a cache, a model
	// server. They are booted once and then left alone: a code deploy must not
	// tear down Postgres. Recreating one is an explicit `shunt boot` operation.
	Accessories map[string]Service `toml:"accessories"`

	// Services are the stateless containers a release replaces every time.
	Services map[string]Service `toml:"services"`

	// Artifacts are large files the app needs but the image does not carry — a
	// SQLite database, model weights, a prebuilt index. They are rsync'd
	// incrementally and swapped into place atomically.
	Artifacts []Artifact `toml:"artifacts"`

	Stages  []Stage  `toml:"stages"`
	Secrets *Secrets `toml:"secrets"`
	Retain  int      `toml:"retain"` // releases kept on the host for rollback

	// Targets are alternative hosts this same manifest can deploy to —
	// staging and production, typically.
	//
	// A target changes *where* a release goes, never *what* it contains: the
	// images, services, stages and artifacts are the ones declared above,
	// whichever target is selected. That keeps the rule that no deploy-time flag
	// alters what gets deployed, while removing the need to maintain two
	// near-identical manifests that drift apart in exactly the ways that matter.
	Targets map[string]Target `toml:"targets"`

	// Target is the selected target's name, empty when deploying to the
	// manifest's own host.
	Target string `toml:"-"`

	// dir is the directory shunt.toml was loaded from; all relative paths resolve
	// against it, not against the process working directory.
	dir string

	// projectBase and networkExplicit remember what the file said, so selecting
	// a target can re-derive the values that depend on the project name without
	// mistaking an earlier default for an explicit choice.
	projectBase     string
	networkExplicit string
}

// Image is something shunt builds locally and ships. Images referenced by a service
// but absent from this map are treated as external and pulled on the host.
type Image struct {
	Context    string            `toml:"context"`
	Dockerfile string            `toml:"dockerfile"`
	Platform   string            `toml:"platform"`
	Target     string            `toml:"target"`
	Args       map[string]string `toml:"args"`
}

// Target is a named deploy destination.
type Target struct {
	Host string `toml:"host"`

	// Project renames the deployment on the host, so staging and production can
	// share one machine without colliding on container names, networks or the
	// ledger. Defaults to "<project>-<target>" rather than the bare project,
	// because sharing a host is the case that silently corrupts state.
	Project string `toml:"project"`

	// Secrets may differ per target — staging must not hold production
	// credentials. Unset means the manifest's own [secrets] block.
	Secrets *Secrets `toml:"secrets"`
}

type Service struct {
	Image   string            `toml:"image"`
	Command []string          `toml:"command"`
	Env     map[string]string `toml:"env"`
	Publish []string          `toml:"publish"`
	Volumes []string          `toml:"volumes"`
	Restart string            `toml:"restart"`
	Health  *Health           `toml:"health"`

	// Expose is the container port a proxy should reach. Unlike publish it takes
	// no host port, which is what allows two releases to run side by side.
	Expose int `toml:"expose"`

	// Drain is how long a replaced container gets to finish in-flight work
	// before it is killed. Containers are stopped with SIGTERM and this timeout
	// rather than SIGKILLed outright.
	Drain Duration `toml:"drain"`

	// Proxy turns this service blue/green: each release runs under its own
	// container name and an external proxy is told about it via labels.
	Proxy *Proxy `toml:"proxy"`

	// Requires orders service startup. Cycles are rejected at validation time.
	Requires []string `toml:"requires"`

	// Secrets narrows which resolved secrets this service receives. Empty means
	// all of them, which is the historical behaviour and the right default for a
	// single-service project — but a worker has no business holding the payment
	// credentials just because the web app needs them.
	Secrets []string `toml:"secrets"`
}

// Proxy describes how an already-running reverse proxy should find this service.
// shunt does not run the proxy — it only emits the labels Traefik or
// caddy-docker-proxy already watch for, so the switchover is the proxy's job and
// shunt gains no long-lived component of its own.
type Proxy struct {
	Kind        string   `toml:"kind"` // traefik | caddy
	Host        string   `toml:"host"`
	Path        string   `toml:"path"`
	Port        int      `toml:"port"` // defaults to the service's expose
	EntryPoints []string `toml:"entrypoints"`

	// Retry reissues a request when the backend never answered — which is what
	// a connection lost to a container shutting down looks like. Traefik stops
	// retrying as soon as a server responds, so a non-idempotent request is
	// never sent twice to a backend that actually received it. Set 0 to disable.
	Retry *int `toml:"retry"`
}

// RetryAttempts is how many times the proxy should reissue an unanswered
// request. Defaulted rather than zero because the last errors in a blue/green
// swap are almost always a keep-alive connection closing mid-flight.
func (p Proxy) RetryAttempts() int {
	if p.Retry == nil {
		return 2
	}
	return *p.Retry
}

// Proxied reports whether this service gets blue/green treatment.
func (s Service) Proxied() bool { return s.Proxy != nil }

// HasPublishedPort reports whether any publish mapping names a host port, which
// is what a bare health path can be resolved against. A mapping of just the
// container port lets Docker choose a random host port, so there is nothing
// stable to probe.
func (s Service) HasPublishedPort() bool {
	for _, p := range s.Publish {
		spec, _, _ := strings.Cut(p, "/")
		if n := strings.Count(spec, ":"); n == 1 || n == 2 {
			return true
		}
	}
	return false
}

// ProxyPort is the container port the proxy should target.
func (s Service) ProxyPort() int {
	if s.Proxy != nil && s.Proxy.Port != 0 {
		return s.Proxy.Port
	}
	return s.Expose
}

// Health gates a deploy: a service that never reports healthy fails the release
// rather than silently leaving a broken container running.
type Health struct {
	URL      string   `toml:"url"`
	Command  []string `toml:"command"`
	Retries  int      `toml:"retries"`
	Interval Duration `toml:"interval"`
	Grace    Duration `toml:"grace"` // wait before the first probe

	// Follow makes the probe chase redirects and require a 2xx at the end.
	//
	// Without it a 3xx counts as healthy, which proves the server is listening
	// but not that it works — an app whose "/" redirects to a locale prefix
	// would pass while being completely broken behind the redirect.
	Follow bool `toml:"follow"`
}

// Stage is a one-shot container run before any running service is replaced.
// This ordering is the core safety property: a failed backup or migration leaves
// production completely untouched instead of half-swapped.
type Stage struct {
	Name            string            `toml:"name"`
	Image           string            `toml:"image"`
	Command         []string          `toml:"command"`
	Env             map[string]string `toml:"env"`
	Capture         string            `toml:"capture"`          // redirect stdout to this path on the host
	RequireNonEmpty bool              `toml:"require_nonempty"` // fail if the capture file is empty
	Retain          int               `toml:"retain"`           // keep N most recent capture files
}

// Artifact is a file shipped alongside the image.
//
// The swap is deliberately careful: a half-transferred 500 MB database promoted
// over a good one is unrecoverable without another 500 MB upload, so the new
// copy lands beside the old under a .new suffix, is validated, and only then
// renamed into place.
type Artifact struct {
	Name string `toml:"name"`
	Src  string `toml:"src"`  // local, relative to shunt.toml
	Dest string `toml:"dest"` // absolute path on the host

	// Magic is a literal string the file must begin with. "SQLite format 3"
	// catches a truncated upload that would otherwise take the app down with a
	// database it cannot open.
	Magic string `toml:"magic"`

	// Retain is how many superseded generations to keep for manual recovery.
	Retain int `toml:"retain"`

	// Required fails the deploy when the local file is absent, instead of
	// leaving whatever the host already has.
	Required bool `toml:"required"`
}

type Secrets struct {
	Provider string   `toml:"provider"` // file | env | sops
	Path     string   `toml:"path"`
	Keys     []string `toml:"keys"` // provider=env: which vars to forward
}

// Duration wraps time.Duration so TOML can carry "3s" as a string.
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// Dir returns the manifest's directory. Relative paths in the manifest are
// interpreted against it.
func (m *Manifest) Dir() string { return m.dir }

// Abs resolves a manifest-relative path.
func (m *Manifest) Abs(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(m.dir, p)
}

// SelectTarget points the manifest at a named target, or leaves it alone when
// name is empty. It is applied after loading and before anything reads Host.
func (m *Manifest) SelectTarget(name string) error {
	if name == "" {
		return nil
	}
	t, ok := m.Targets[name]
	if !ok {
		known := make([]string, 0, len(m.Targets))
		for k := range m.Targets {
			known = append(known, k)
		}
		slices.Sort(known)
		if len(known) == 0 {
			return fmt.Errorf("shunt.toml declares no [targets.*], so there is no target %q", name)
		}
		return fmt.Errorf("no target %q in shunt.toml (declared: %s)", name, strings.Join(known, ", "))
	}
	m.Host = t.Host
	if t.Secrets != nil {
		m.Secrets = t.Secrets
	}
	m.Project = t.Project
	if m.Project == "" {
		m.Project = m.projectBase + "-" + name
	}
	// The network is derived from the project, so it has to be re-derived here
	// or staging and production would share one network on a shared host.
	if m.networkExplicit == "" {
		m.Network = m.Project + "-net"
	}
	m.Target = name
	return nil
}

// Find walks up from start looking for shunt.toml, so the CLI works from any
// subdirectory of a project the way git does.
func Find(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		p := filepath.Join(dir, "shunt.toml")
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no shunt.toml found in %s or any parent directory (run `shunt init`)", start)
		}
		dir = parent
	}
}

func Load(path string) (*Manifest, error) {
	var m Manifest
	md, err := toml.DecodeFile(path, &m)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if undec := md.Undecoded(); len(undec) > 0 {
		// Unknown keys are almost always typos in a field that silently does
		// nothing — refuse rather than deploy something the user didn't mean.
		keys := make([]string, len(undec))
		for i, k := range undec {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("%s: unknown key(s): %s", filepath.Base(path), strings.Join(keys, ", "))
	}
	m.dir = filepath.Dir(path)
	m.projectBase = m.Project
	m.networkExplicit = m.Network
	m.applyDefaults()
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

func (m *Manifest) applyDefaults() {
	if m.Network == "" {
		m.Network = m.Project + "-net"
	}
	if m.Retain == 0 {
		m.Retain = 5
	}
	for name, img := range m.Images {
		if img.Context == "" {
			img.Context = "."
		}
		if img.Dockerfile == "" {
			img.Dockerfile = filepath.Join(img.Context, "Dockerfile")
		}
		m.Images[name] = img
	}
	for _, set := range []map[string]Service{m.Services, m.Accessories} {
		for name, svc := range set {
			if svc.Image == "" {
				svc.Image = name // a service named "app" defaults to the image named "app"
			}
			if svc.Restart == "" {
				svc.Restart = "unless-stopped"
			}
			if svc.Health != nil {
				if svc.Health.Retries == 0 {
					svc.Health.Retries = 10
				}
				if svc.Health.Interval.Duration == 0 {
					svc.Health.Interval = Duration{3 * time.Second}
				}
			}
			if svc.Drain.Duration == 0 {
				svc.Drain = Duration{10 * time.Second}
			}
			if svc.Proxy != nil {
				if svc.Proxy.Kind == "" {
					svc.Proxy.Kind = "traefik"
				}
				if svc.Proxy.Path == "" {
					svc.Proxy.Path = "/"
				}
			}
			set[name] = svc
		}
	}
	for i := range m.Stages {
		if m.Stages[i].Retain == 0 {
			m.Stages[i].Retain = 10
		}
	}
	for i := range m.Artifacts {
		if m.Artifacts[i].Retain == 0 {
			m.Artifacts[i].Retain = 1
		}
	}
}

// all merges service maps for checks that apply to both kinds. Accessories win
// no precedence; a name declared in both is rejected separately.
func all(sets ...map[string]Service) map[string]Service {
	out := map[string]Service{}
	for _, s := range sets {
		for k, v := range s {
			out[k] = v
		}
	}
	return out
}

// AccessoryOrder lists accessories in a stable order. They have no dependency
// edges between them — a stateful dependency that needs another one is a sign
// the manifest wants a real orchestrator, not shunt.
func (m *Manifest) AccessoryOrder() []string {
	ks := make([]string, 0, len(m.Accessories))
	for k := range m.Accessories {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return ks
}

// StartOrder topologically sorts services by their requires edges. Ties break
// alphabetically so a given manifest always produces an identical plan.
func (m *Manifest) StartOrder() []string {
	names := make([]string, 0, len(m.Services))
	for n := range m.Services {
		names = append(names, n)
	}
	slices.Sort(names)

	state := map[string]int{} // 0 unvisited, 1 visiting, 2 done
	var out []string
	var visit func(string)
	visit = func(n string) {
		if state[n] != 0 {
			return
		}
		state[n] = 1
		deps := append([]string(nil), m.Services[n].Requires...)
		slices.Sort(deps)
		for _, d := range deps {
			// Accessories are already up before services start, so they are not
			// part of this ordering.
			if _, ok := m.Services[d]; ok {
				visit(d)
			}
		}
		state[n] = 2
		out = append(out, n)
	}
	for _, n := range names {
		visit(n)
	}
	return out
}

func (m *Manifest) detectCycle() string {
	state := map[string]int{}
	var path []string
	var found string
	var visit func(string)
	visit = func(n string) {
		if found != "" || state[n] == 2 {
			return
		}
		if state[n] == 1 {
			found = strings.Join(append(path, n), " -> ")
			return
		}
		state[n] = 1
		path = append(path, n)
		for _, d := range m.Services[n].Requires {
			if _, ok := m.Services[d]; ok {
				visit(d)
			}
		}
		path = path[:len(path)-1]
		state[n] = 2
	}
	names := make([]string, 0, len(m.Services))
	for n := range m.Services {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		visit(n)
	}
	return found
}
