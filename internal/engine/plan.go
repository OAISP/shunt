package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/OAISP/shunt/internal/build"
	"github.com/OAISP/shunt/internal/release"
	"github.com/OAISP/shunt/internal/transport"
)

// Plan is the diff between what the host is running and what the manifest says
// it should run. Printing this before mutating anything is the difference
// between a deploy tool and a script you hope works.
type Plan struct {
	ReleaseID   string
	Current     *release.Entry
	Images      []ImageChange
	Accessories []ServiceChange
	Services    []ServiceChange
	Artifacts   []ArtifactChange
	Stages      []string
	Secrets     SecretChange
	Transfer    TransferEstimate
}

type ImageChange struct {
	Name     string
	Action   string // create | update | unchanged | pull
	OldDgst  string
	NewDgst  string
	External bool
}

type ServiceChange struct {
	Name    string
	Action  string // create | update | unchanged | remove | drift
	Reasons []string

	// ZeroDowntime is true for proxied services, which start alongside the
	// running release instead of replacing it in place.
	ZeroDowntime bool
}

type SecretChange struct {
	Added, Removed, Changed []string
	Total                   int
}

// ArtifactChange is one data file the deploy would ship.
type ArtifactChange struct {
	Name       string
	Dest       string
	LocalBytes int64
	LocalMTime int64
	HostBytes  int64 // -1 when the host has no copy yet
	HostMTime  int64
}

// Differs reports whether the host's copy needs replacing, using size and mtime
// — the same test rsync applies before deciding to transfer anything.
func (a ArtifactChange) Differs() bool {
	return a.HostBytes < 0 || a.HostBytes != a.LocalBytes || a.HostMTime != a.LocalMTime
}

// Action describes what shipping this artifact would do.
func (a ArtifactChange) Action() string {
	switch {
	case a.HostBytes < 0:
		return "create"
	case a.Differs():
		return "replace"
	default:
		return "unchanged"
	}
}

type TransferEstimate struct {
	Total   int64 // logical size of all layouts
	Missing int64 // bytes the host does not have yet
	Blobs   int   // number of blobs to send
}

// CachedPercent is the share of the image the host already holds.
//
// It never reports a flat 100% while bytes still need sending: that reads as
// "nothing to transfer" and undermines trust in the number right when it is
// most impressive.
func (t TransferEstimate) CachedPercent() float64 {
	if t.Total == 0 {
		return 0
	}
	pct := float64(t.Total-t.Missing) / float64(t.Total) * 100
	if pct >= 99.95 && t.Missing > 0 {
		return 99.9
	}
	return pct
}

// Changed reports whether applying this plan would alter anything.
func (p *Plan) Changed() bool {
	// A previous release that failed left the host in an unknown state, so
	// re-applying an identical manifest is real work, not a no-op.
	if p.Current == nil || p.Current.Status != "active" {
		return true
	}
	for _, i := range p.Images {
		if i.Action != "unchanged" && i.Action != "pull" {
			return true
		}
	}
	for _, s := range p.Services {
		if s.Action != "unchanged" {
			return true
		}
	}
	for _, a := range p.Accessories {
		if a.Action == "create" {
			return true
		}
	}
	// Data is work in its own right: re-running an ETL and shipping only the
	// new database is a deploy, even though no image or service changed.
	for _, a := range p.Artifacts {
		if a.Differs() {
			return true
		}
	}
	return false
}

// BuildPlan diffs a freshly-built spec against the host's ledger.
func (e *Engine) BuildPlan(ctx context.Context, spec *release.Spec, built map[string]*build.Result, state *RemoteState) (*Plan, error) {
	p := &Plan{ReleaseID: spec.ID}
	if state != nil && state.Ledger != nil && state.Ledger.Current != "" {
		p.Current = state.Ledger.Find(state.Ledger.Current)
	}

	for _, name := range sortedKeys(spec.Images) {
		img := spec.Images[name]
		if img.External {
			p.Images = append(p.Images, ImageChange{Name: name, Action: "pull", External: true})
			continue
		}
		ic := ImageChange{Name: name, NewDgst: img.Digest, Action: "create"}
		if p.Current != nil {
			if old, ok := p.Current.Images[name]; ok {
				ic.OldDgst = old.Digest
				if old.Digest == img.Digest {
					ic.Action = "unchanged"
				} else {
					ic.Action = "update"
				}
			}
		}
		p.Images = append(p.Images, ic)
	}

	var oldSpec *release.Spec
	if p.Current != nil {
		oldSpec = p.Current.Spec
	}

	for _, name := range sortedKeys(spec.Services) {
		svc := spec.Services[name]
		sc := ServiceChange{Name: name, Action: "create", ZeroDowntime: svc.Proxied()}
		if oldSpec != nil {
			if old, ok := oldSpec.Services[name]; ok {
				sc.Reasons = diffService(old, svc)
				// An image whose digest moved forces a restart even when the
				// service definition itself is byte-identical.
				if imgChanged(p.Images, svc.Image) {
					sc.Reasons = append(sc.Reasons, "image "+svc.Image+" rebuilt")
				}
				if len(sc.Reasons) == 0 {
					sc.Action = "unchanged"
				} else {
					sc.Action = "update"
				}
				// An overlap is only safe when the proxy labels are unchanged;
				// see sameProxy in the helper.
				if !reflect.DeepEqual(old.Proxy, svc.Proxy) {
					sc.ZeroDowntime = false
					sc.Reasons = append(sc.Reasons,
						"proxy config changed — this one deploy stops the old container first")
				}
			}
		}
		p.Services = append(p.Services, sc)
	}
	if oldSpec != nil {
		for _, name := range sortedKeys(oldSpec.Services) {
			if _, ok := spec.Services[name]; !ok {
				p.Services = append(p.Services, ServiceChange{Name: name, Action: "remove",
					Reasons: []string{"no longer in shunt.toml — shunt will not stop it automatically"}})
			}
		}
	}

	// Accessories are only ever created, never replaced by a deploy. When the
	// manifest drifts from what is running, say so loudly rather than silently
	// ignoring the change — the operator has to run `shunt boot` deliberately.
	for _, name := range sortedKeys(spec.Accessories) {
		acc := spec.Accessories[name]
		ac := ServiceChange{Name: name, Action: "create"}
		if oldSpec != nil {
			if old, present := oldSpec.Accessories[name]; present {
				ac.Action = "unchanged"
				if reasons := diffService(old, acc); len(reasons) > 0 {
					ac.Action = "drift"
					ac.Reasons = append(reasons, "not applied by `shunt up` — run `shunt boot "+name+"` to recreate")
				}
			}
		}
		p.Accessories = append(p.Accessories, ac)
	}

	for _, a := range spec.Artifacts {
		// One stat over the existing connection; see RemoteFileStat for why this
		// is the right test rather than hashing.
		host := transport.RemoteFileStat(ctx, e.Client, a.Dest)
		p.Artifacts = append(p.Artifacts, ArtifactChange{
			Name:       a.Name,
			Dest:       a.Dest,
			LocalBytes: a.Bytes,
			LocalMTime: a.MTime,
			HostBytes:  host.Size,
			HostMTime:  host.MTime,
		})
	}

	for _, st := range spec.Stages {
		p.Stages = append(p.Stages, st.Name)
	}

	p.Secrets = diffSecrets(oldSpec, spec)

	est, err := e.estimate(ctx, built)
	if err != nil {
		// A failed estimate must not block a deploy; it is informational.
		fmt.Fprintf(os.Stderr, "warning: could not estimate transfer size: %v\n", err)
	} else {
		p.Transfer = *est
	}
	return p, nil
}

func imgChanged(imgs []ImageChange, name string) bool {
	for _, i := range imgs {
		if i.Name == name {
			return i.Action == "update" || i.Action == "create"
		}
	}
	return false
}

func diffService(old, nw release.Service) []string {
	var r []string
	if old.Image != nw.Image {
		r = append(r, fmt.Sprintf("image %s → %s", old.Image, nw.Image))
	}
	if !reflect.DeepEqual(old.Command, nw.Command) {
		r = append(r, "command changed")
	}
	if !reflect.DeepEqual(old.Publish, nw.Publish) {
		r = append(r, fmt.Sprintf("publish %v → %v", old.Publish, nw.Publish))
	}
	if !reflect.DeepEqual(old.Volumes, nw.Volumes) {
		r = append(r, "volumes changed")
	}
	if old.Expose != nw.Expose {
		r = append(r, fmt.Sprintf("expose %d → %d", old.Expose, nw.Expose))
	}
	if old.Drain != nw.Drain {
		r = append(r, fmt.Sprintf("drain %s → %s", old.Drain, nw.Drain))
	}
	if !reflect.DeepEqual(old.Proxy, nw.Proxy) {
		switch {
		case old.Proxy == nil:
			r = append(r, "proxy added — this service becomes zero-downtime")
		case nw.Proxy == nil:
			r = append(r, "proxy removed — this service goes back to restart-in-place")
		default:
			r = append(r, fmt.Sprintf("proxy %s %s → %s %s",
				old.Proxy.Kind, old.Proxy.Host, nw.Proxy.Kind, nw.Proxy.Host))
		}
	}
	if old.Restart != nw.Restart {
		r = append(r, fmt.Sprintf("restart %s → %s", old.Restart, nw.Restart))
	}
	for _, k := range mapKeys(old.Env, nw.Env) {
		ov, oo := old.Env[k]
		nv, no := nw.Env[k]
		switch {
		case oo && !no:
			r = append(r, "- env "+k)
		case !oo && no:
			r = append(r, "+ env "+k+"="+nv)
		case ov != nv:
			r = append(r, fmt.Sprintf("~ env %s=%s → %s", k, ov, nv))
		}
	}
	return r
}

// diffSecrets compares key sets and whether values moved — never the values.
func diffSecrets(old, nw *release.Spec) SecretChange {
	sc := SecretChange{Total: len(nw.Secrets)}
	// The ledger stores hashes, never values, so hash the freshly-resolved
	// secrets before comparing — otherwise every key looks changed every time.
	hashed := make(map[string]string, len(nw.Secrets))
	for k, v := range nw.Secrets {
		hashed[k] = release.HashSecret(v)
	}
	nw = &release.Spec{Secrets: hashed}
	if old == nil {
		for k := range nw.Secrets {
			sc.Added = append(sc.Added, k)
		}
		slices.Sort(sc.Added)
		return sc
	}
	for _, k := range mapKeys(old.Secrets, nw.Secrets) {
		ov, oo := old.Secrets[k]
		nv, no := nw.Secrets[k]
		switch {
		case oo && !no:
			sc.Removed = append(sc.Removed, k)
		case !oo && no:
			sc.Added = append(sc.Added, k)
		case ov != nv:
			sc.Changed = append(sc.Changed, k)
		}
	}
	return sc
}

// estimate asks the host which blobs it already holds and sums the rest. This is
// the number that makes the registry-free approach legible: it is the actual
// wire cost of the deploy, before it happens.
func (e *Engine) estimate(ctx context.Context, built map[string]*build.Result) (*TransferEstimate, error) {
	est := &TransferEstimate{}
	for name, r := range built {
		local, err := layoutBlobs(r.Dir)
		if err != nil {
			return nil, err
		}
		for _, sz := range local {
			est.Total += sz
		}
		remoteDir := filepath.Join(e.StorePath(), name, "blobs", "sha256")
		out, err := e.Client.Run(ctx, "sh", "-c", "ls -1 "+shellArg(remoteDir)+" 2>/dev/null || true")
		if err != nil {
			return nil, err
		}
		have := map[string]bool{}
		for _, ln := range strings.Split(strings.TrimSpace(out), "\n") {
			if ln != "" {
				have[strings.TrimSpace(ln)] = true
			}
		}
		for digest, sz := range local {
			if !have[digest] {
				est.Missing += sz
				est.Blobs++
			}
		}
	}
	return est, nil
}

func layoutBlobs(dir string) (map[string]int64, error) {
	blobs := filepath.Join(dir, "blobs", "sha256")
	ents, err := os.ReadDir(blobs)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(ents))
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			return nil, err
		}
		out[e.Name()] = fi.Size()
	}
	return out, nil
}

func mapKeys(a, b map[string]string) []string {
	set := map[string]bool{}
	for k := range a {
		set[k] = true
	}
	for k := range b {
		set[k] = true
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func shellArg(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
