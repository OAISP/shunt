// The comparisons a plan is made of: manifest against the last release, and
// manifest against the containers actually running.
package engine

import (
	"fmt"
	"reflect"
	"slices"

	"github.com/OAISP/shunt/internal/release"
)

// diffStages compares the stage pipeline against the one last deployed. Without
// it, adding a migration to the manifest is a silent no-op — nothing else moved,
// so `shunt up` finds nothing to do and the migration never runs.
func diffStages(old, nw *release.Spec) []StageChange {
	prev := map[string]release.Stage{}
	if old != nil {
		for _, st := range old.Stages {
			prev[st.Name] = st
		}
	}
	seen := map[string]bool{}
	out := make([]StageChange, 0, len(nw.Stages))
	for _, st := range nw.Stages {
		seen[st.Name] = true
		sc := StageChange{Name: st.Name, Action: "create"}
		if was, ok := prev[st.Name]; ok {
			sc.Action = "run"
			if !reflect.DeepEqual(was, st) {
				sc.Action = "update"
			}
		} else if old == nil {
			// Nothing to compare against on a first deploy; every stage simply runs.
			sc.Action = "run"
		}
		out = append(out, sc)
	}
	// A stage dropped from the manifest will not run again, which is a change
	// worth showing even though there is nothing on the host to clean up.
	if old != nil {
		for _, st := range old.Stages {
			if !seen[st.Name] {
				out = append(out, StageChange{Name: st.Name, Action: "remove"})
			}
		}
	}
	return out
}

// reconcileService compares the manifest against the containers actually on the
// host. A plan built from the ledger alone describes what shunt last did, not
// what the host is doing — a container deleted, stopped or replaced by hand
// would otherwise read as unchanged.
func reconcileService(state *RemoteState, name string, svc release.Service) []string {
	if state == nil {
		return nil
	}
	found := state.ServiceContainers(name)
	if len(found) == 0 {
		// Nothing deployed yet is not drift; the service diff already calls that
		// a create.
		if state.Ledger == nil || state.Ledger.Current == "" {
			return nil
		}
		return []string{"no container on the host — it was removed outside shunt"}
	}

	want := release.HashService(svc)
	for _, c := range found {
		// An empty config label means the container predates this check. Treat it
		// as satisfied rather than reporting drift on every pre-existing host.
		if c.Running() && (c.Config == "" || c.Config == want) {
			return nil
		}
	}

	c := found[0]
	switch {
	case !c.Running():
		return []string{fmt.Sprintf("container %s is %s, not running", c.Name, orUnknown(c.State))}
	default:
		return []string{fmt.Sprintf("container %s is running a different configuration than shunt.toml describes", c.Name)}
	}
}

func orUnknown(s string) string {
	if s == "" {
		return "in an unknown state"
	}
	return s
}

// orphanChange describes a service the manifest dropped but the host may still
// be running. Deliberately not counted as work — shunt will not stop a container
// it was not asked to, so treating it as actionable would leave the plan
// permanently dirty. `shunt retire` is the explicit way to act on it.
func orphanChange(state *RemoteState, name string) ServiceChange {
	sc := ServiceChange{Name: name, Action: "orphaned"}
	running := 0
	for _, c := range state.ServiceContainers(name) {
		if c.Running() {
			running++
		}
	}
	if running > 0 {
		sc.Reasons = []string{fmt.Sprintf("no longer in shunt.toml, but %d container(s) still running — `shunt retire %s` stops them", running, name)}
	} else {
		sc.Reasons = []string{"no longer in shunt.toml; nothing is running for it"}
	}
	return sc
}

// driftReasons explains an accessory drift in field-level terms when the last
// recorded spec makes that possible. The hash proves *that* it drifted; this is
// only there to say how, so a bare "drifted" is still correct without it.
func driftReasons(oldSpec *release.Spec, name string, acc release.Service) []string {
	if oldSpec == nil {
		return nil
	}
	old, present := oldSpec.Accessories[name]
	if !present {
		return nil
	}
	return diffService(old, acc)
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

// diffRelease reports settings that apply to the whole release rather than to
// any one service.
func diffRelease(old, nw *release.Spec) []string {
	if old == nil {
		return nil
	}
	var out []string
	if old.Network != nw.Network {
		out = append(out, fmt.Sprintf("network %s → %s", old.Network, nw.Network))
	}
	if old.SecretMode != nw.SecretMode {
		out = append(out, fmt.Sprintf("secret delivery %s → %s",
			secretModeName(old.SecretMode), secretModeName(nw.SecretMode)))
	}
	return out
}

func secretModeName(m string) string {
	if m == "file" {
		return "files under /run/secrets"
	}
	return "environment"
}

// diffSecrets compares key sets and whether values moved — never the values.
//
// salt comes back from the host with the ledger, because that is what its stored
// hashes are keyed by. An empty salt only happens on a host that has never
// deployed, where there is nothing to compare against anyway.
func diffSecrets(old, nw *release.Spec, salt string) SecretChange {
	sc := SecretChange{Total: len(nw.Secrets)}
	// The ledger stores hashes, never values, so hash the freshly-resolved
	// secrets before comparing — otherwise every key looks changed every time.
	hashed := make(map[string]string, len(nw.Secrets))
	for k, v := range nw.Secrets {
		hashed[k] = release.HashSecret(salt, v)
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
