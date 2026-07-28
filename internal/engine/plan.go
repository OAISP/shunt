package engine

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/OAISP/shunt/internal/build"
	"github.com/OAISP/shunt/internal/oci"
	"github.com/OAISP/shunt/internal/release"
	"github.com/OAISP/shunt/internal/sshx"
	"github.com/OAISP/shunt/internal/transport"
)

// PlanSchema versions the machine-readable plan.
//
// `--json` previously emitted whatever Go field names happened to be, which is
// not a contract: renaming a field would have silently broken every consumer.
// The version is what lets a consumer notice a change instead of misreading one.
const PlanSchema = 1

// Plan is the diff between what the host is running and what the manifest says
// it should run. Printing this before mutating anything is the difference
// between a deploy tool and a script you hope works.
type Plan struct {
	// Schema is the version of this document's shape.
	Schema      int              `json:"schema"`
	ReleaseID   string           `json:"release_id"`
	Current     *release.Entry   `json:"current"`
	Images      []ImageChange    `json:"images"`
	Accessories []ServiceChange  `json:"accessories"`
	Services    []ServiceChange  `json:"services"`
	Artifacts   []ArtifactChange `json:"artifacts"`
	Stages      []StageChange    `json:"stages"`
	Secrets     SecretChange     `json:"secrets"`
	Transfer    TransferEstimate `json:"transfer"`

	// Changed is materialised into the document so a consumer does not have to
	// re-derive shunt's own notion of "is there work to do".
	HasChanges bool `json:"has_changes"`

	// Release lists release-wide settings that changed — things that belong to
	// no individual service but alter how all of them are created.
	Release []string `json:"release,omitempty"`

	// SecretsUnknown means the values could not be resolved here, so the secret
	// diff is absent rather than empty. Applying a bundle on a machine without
	// the provider is the case: reporting every key as removed would be worse
	// than admitting the comparison did not happen.
	SecretsUnknown bool `json:"secrets_unknown,omitempty"`
}

type ImageChange struct {
	Name     string `json:"name"`
	Action   string `json:"action"` // create | update | unchanged | pull
	OldDgst  string `json:"old_digest,omitempty"`
	NewDgst  string `json:"new_digest,omitempty"`
	External bool   `json:"external,omitempty"`
}

type ServiceChange struct {
	Name    string   `json:"name"`
	Action  string   `json:"action"` // create | update | unchanged | orphaned | drift
	Reasons []string `json:"reasons,omitempty"`

	// ZeroDowntime is true for proxied services, which start alongside the
	// running release instead of replacing it in place.
	ZeroDowntime bool `json:"zero_downtime,omitempty"`

	// ProxyGated is false for a proxied service whose health check the proxy
	// cannot poll — a command check. Such a service overlaps, but the proxy has
	// no way to keep a still-warming container out of rotation, so the swap
	// leans on retry alone. Reported rather than left implicit.
	ProxyGated bool `json:"proxy_gated,omitempty"`
}

// StageChange is one one-shot container the deploy would run, and whether the
// manifest changed it.
type StageChange struct {
	Name   string `json:"name"`
	Action string `json:"action"` // run | create | update | remove
}

type SecretChange struct {
	Added   []string `json:"added,omitempty"`
	Removed []string `json:"removed,omitempty"`
	Changed []string `json:"changed,omitempty"`
	Total   int      `json:"total"`
}

// ArtifactChange is one data file the deploy would ship.
type ArtifactChange struct {
	Name       string `json:"name"`
	Dest       string `json:"dest"`
	LocalBytes int64  `json:"local_bytes"`
	LocalMTime int64  `json:"local_mtime"`
	HostBytes  int64  `json:"host_bytes"` // -1 when the host has no copy yet
	HostMTime  int64  `json:"host_mtime"`
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
	Total   int64 `json:"total"`   // logical size of all layouts
	Missing int64 `json:"missing"` // bytes the host does not have yet
	Blobs   int   `json:"blobs"`   // number of blobs to send
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
		// An orphan is reported, never acted on — counting it would leave the
		// plan permanently dirty with a change `shunt up` will never make.
		if s.Action != "unchanged" && s.Action != "orphaned" {
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
	// Adding a migration to the manifest is a deploy. Without this the stage was
	// invisible to change detection, `shunt up` said "nothing to do", and the
	// migration silently never ran.
	for _, st := range p.Stages {
		if st.Action != "run" {
			return true
		}
	}
	// Settings that belong to the release rather than to any one service —
	// switching secrets from environment to files, renaming the network —
	// change how every container is created but appear in no service diff.
	// Without this they were silently ignored until something else forced a
	// deploy, which is the same shape of bug stages had.
	return len(p.Release) > 0
}

// BuildPlan diffs a freshly-built spec against the host's ledger.
func (e *Engine) BuildPlan(ctx context.Context, spec *release.Spec, built map[string]*build.Result, state *RemoteState) (*Plan, error) {
	p := &Plan{Schema: PlanSchema, ReleaseID: spec.ID}
	if state != nil && state.Ledger != nil && state.Ledger.Current != "" {
		p.Current = state.Ledger.Find(state.Ledger.Current)
	}

	for _, name := range slices.Sorted(maps.Keys(spec.Images)) {
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

	for _, name := range slices.Sorted(maps.Keys(spec.Services)) {
		svc := spec.Services[name]
		sc := ServiceChange{
			Name: name, Action: "create",
			ZeroDowntime: svc.Proxied(),
			ProxyGated:   release.ProxyGatesReadiness(svc),
		}
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
		// The ledger says what shunt last deployed; only the containers say what
		// the host is running now. Without this a service deleted, stopped or
		// replaced by hand read as "unchanged" and `shunt up` refused to fix it.
		if drift := reconcileService(state, name, svc); len(drift) > 0 {
			if sc.Action == "unchanged" {
				sc.Action = "update"
			}
			sc.Reasons = append(sc.Reasons, drift...)
		}
		p.Services = append(p.Services, sc)
	}
	if oldSpec != nil {
		for _, name := range slices.Sorted(maps.Keys(oldSpec.Services)) {
			if _, ok := spec.Services[name]; !ok {
				p.Services = append(p.Services, orphanChange(state, name))
			}
		}
	}

	p.Accessories = e.planAccessories(p, spec, state, oldSpec)

	// Every artifact stat in one round trip rather than one apiece; see
	// RemoteFileStat for why size and mtime are the right test rather than
	// hashing.
	dests := make([]string, 0, len(spec.Artifacts))
	for _, a := range spec.Artifacts {
		dests = append(dests, a.Dest)
	}
	hostStats := transport.RemoteFileStats(ctx, e.Client, dests)
	for _, a := range spec.Artifacts {
		host := hostStats[a.Dest]
		p.Artifacts = append(p.Artifacts, ArtifactChange{
			Name:       a.Name,
			Dest:       a.Dest,
			LocalBytes: a.Bytes,
			LocalMTime: a.MTime,
			HostBytes:  host.Size,
			HostMTime:  host.MTime,
		})
	}

	p.Stages = diffStages(oldSpec, spec)

	p.Release = diffRelease(oldSpec, spec)

	var salt string
	if state != nil && state.Ledger != nil {
		salt = state.Ledger.Salt
	}
	p.Secrets = diffSecrets(oldSpec, spec, salt)

	defer func() { p.HasChanges = p.Changed() }()

	est, err := e.estimate(ctx, built)
	if err != nil {
		// A failed estimate must not block a deploy; it is informational.
		fmt.Fprintf(os.Stderr, "warning: could not estimate transfer size: %v\n", err)
	} else {
		p.Transfer = *est
	}
	return p, nil
}

// planAccessories diffs the manifest's accessories against what the host
// recorded as applied.
//
// Accessories are only ever created by a deploy, never replaced. When the
// manifest drifts from what is actually running, say so loudly rather than
// silently ignoring it — recreating one is the explicit `shunt boot`.
//
// The comparison is against the *applied* hash, not the last release's spec.
// A deploy records the manifest it was handed even though it deliberately left
// the accessory alone, so diffing against that made drift vanish after any
// unrelated deploy while the container kept running the old config.
func (e *Engine) planAccessories(p *Plan, spec *release.Spec, state *RemoteState, oldSpec *release.Spec) []ServiceChange {
	var applied map[string]string
	if state != nil && state.Ledger != nil {
		applied = state.Ledger.Accessories
	}

	out := make([]ServiceChange, 0, len(spec.Accessories))
	for _, name := range slices.Sorted(maps.Keys(spec.Accessories)) {
		acc := spec.Accessories[name]
		ac := ServiceChange{Name: name, Action: "create"}
		const hint = "not applied by `shunt up` — run `shunt boot "

		switch got, recorded := applied[name]; {
		case recorded:
			ac.Action = "unchanged"
			if got != release.HashService(acc) {
				ac.Action = "drift"
				ac.Reasons = append(driftReasons(oldSpec, name, acc), hint+name+"` to recreate")
			}
		case oldSpec != nil:
			// A host deployed before applied-state tracking existed. Fall back to
			// the old comparison rather than reporting every accessory as new.
			if old, present := oldSpec.Accessories[name]; present {
				ac.Action = "unchanged"
				if reasons := diffService(old, acc); len(reasons) > 0 {
					ac.Action = "drift"
					ac.Reasons = append(reasons, hint+name+"` to recreate")
				}
			}
		}
		out = append(out, ac)
	}
	return out
}

// estimate asks the host which blobs it already holds and sums the rest. This is
// the number that makes the registry-free approach legible: it is the actual
// wire cost of the deploy, before it happens.
//
// One round trip for every image, not one per image: each listing is fenced by
// a marker line so the replies can be told apart. A manifest with several images
// otherwise paid a full ssh round trip each, before any work had started.
func (e *Engine) estimate(ctx context.Context, built map[string]*build.Result) (*TransferEstimate, error) {
	est := &TransferEstimate{}
	if len(built) == 0 {
		return est, nil
	}
	names := slices.Sorted(maps.Keys(built))

	var script strings.Builder
	for _, name := range names {
		dir := filepath.Join(e.StorePath(), name, "blobs", "sha256")
		fmt.Fprintf(&script, "echo %s\nls -1 %s 2>/dev/null || true\n",
			sshx.Quote(blobFence+name), sshx.Quote(dir))
	}
	out, err := e.Client.Run(ctx, "sh", "-c", script.String())
	if err != nil {
		return nil, err
	}
	have := parseBlobListing(out)

	for _, name := range names {
		local, err := oci.Blobs(built[name].Dir)
		if err != nil {
			return nil, err
		}
		for digest, sz := range local {
			est.Total += sz
			if !have[name][digest] {
				est.Missing += sz
				est.Blobs++
			}
		}
	}
	return est, nil
}

// blobFence separates one image's blob listing from the next in a batched reply.
// A digest is hex, so this cannot collide with one.
const blobFence = "--shunt-image:"

func parseBlobListing(out string) map[string]map[string]bool {
	have := map[string]map[string]bool{}
	var current string
	for _, ln := range strings.Split(out, "\n") {
		ln = strings.TrimSpace(ln)
		switch {
		case ln == "":
		case strings.HasPrefix(ln, blobFence):
			current = strings.TrimPrefix(ln, blobFence)
			have[current] = map[string]bool{}
		case current != "":
			have[current][ln] = true
		}
	}
	return have
}
