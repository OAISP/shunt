// Package release defines the wire contract between the shunt CLI and the
// shunt-helper binary that runs on the host. The CLI resolves a manifest plus
// secrets into a Spec, streams it over stdin (never argv, never a temp file),
// and the helper applies it and emits Events on stdout as NDJSON.
package release

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// Protocol is bumped when Spec or Event change shape incompatibly. The helper
// refuses a Spec whose protocol it does not understand, which turns a silent
// version-skew misdeploy into a clean error.
const Protocol = 1

// Spec is a complete, self-contained description of one release. Everything the
// helper needs is here; it never reads the manifest and never phones home.
type Spec struct {
	Protocol int    `json:"protocol"`
	Project  string `json:"project"`
	ID       string `json:"id"` // release id, e.g. 20260726-175612-a1b2c3
	Network  string `json:"network"`
	Retain   int    `json:"retain"`

	// StorePath is the OCI layout directory on the host that the CLI rsync'd
	// into before invoking the helper.
	StorePath string `json:"store_path"`

	// Images maps a manifest image name to the digest that must be present after
	// load. The helper verifies this rather than trusting the transfer.
	Images map[string]ImageRef `json:"images"`

	// Accessories are ensured (created only if absent) before stages run, so a
	// migration has its database available. They are never recreated by a normal
	// deploy — see `shunt boot`.
	Accessories    map[string]Service `json:"accessories,omitempty"`
	AccessoryOrder []string           `json:"accessory_order,omitempty"`

	// Artifacts have already been transferred to their staged path by the time
	// the helper runs; it validates and swaps them.
	Artifacts []Artifact `json:"artifacts,omitempty"`

	Stages   []Stage            `json:"stages"`
	Services map[string]Service `json:"services"`
	Order    []string           `json:"order"` // service start order, precomputed by the CLI

	// Secrets are applied to every service and stage via an --env-file written
	// 0600 on the host. Values never appear in argv or in `docker inspect`.
	Secrets map[string]string `json:"secrets,omitempty"`

	// Provenance records where this release came from. It is carried on the
	// spec so the host can store it with the release, which is what lets
	// `shunt status` answer "which commit is production running" without the
	// operator holding a mapping in their head.
	Provenance Provenance `json:"provenance,omitzero"`

	// ExpectedCurrent is the release the host was serving when this plan was
	// built. The helper refuses to apply a spec whose assumption no longer
	// holds, so a plan computed against one state cannot be applied to another.
	// Empty means "no expectation" — a first deploy, or a caller that did not
	// read the host first.
	ExpectedCurrent string `json:"expected_current,omitempty"`
}

// Provenance is the origin of a release: which commit, built by whom, with
// which shunt. Every field is best-effort — a project deployed from a tarball
// has no git metadata and must still deploy.
type Provenance struct {
	Commit   string `json:"commit,omitempty"`
	Short    string `json:"short,omitempty"`
	Branch   string `json:"branch,omitempty"`
	Dirty    bool   `json:"dirty,omitempty"`
	CLI      string `json:"cli,omitempty"`
	Deployer string `json:"deployer,omitempty"`
}

// Describe renders provenance for a human, or "" when nothing is known.
func (p Provenance) Describe() string {
	if p.Short == "" {
		if p.Deployer == "" {
			return ""
		}
		return "by " + p.Deployer
	}
	s := p.Short
	if p.Branch != "" {
		s = p.Branch + "@" + s
	}
	if p.Dirty {
		s += " (dirty tree)"
	}
	if p.Deployer != "" {
		s += " by " + p.Deployer
	}
	return s
}

type ImageRef struct {
	// Ref is the tag the helper applies locally after load, e.g. shunt/latent-app:20260726-175612.
	Ref string `json:"ref"`
	// Digest is the OCI manifest digest exported by buildx.
	Digest string `json:"digest"`
	// External images are pulled on the host instead of loaded from the store.
	External bool `json:"external"`
}

type Service struct {
	Image    string            `json:"image"`
	Command  []string          `json:"command,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	Publish  []string          `json:"publish,omitempty"`
	Volumes  []string          `json:"volumes,omitempty"`
	Restart  string            `json:"restart"`
	Health   *Health           `json:"health,omitempty"`
	Requires []string          `json:"requires,omitempty"`

	Expose int    `json:"expose,omitempty"`
	Drain  string `json:"drain,omitempty"`
	Proxy  *Proxy `json:"proxy,omitempty"`

	// Secrets narrows which of the release's secrets this service receives.
	// Empty means all of them.
	Secrets []string `json:"secrets,omitempty"`
}

// Proxy carries what the helper needs to emit discovery labels for an external
// reverse proxy.
type Proxy struct {
	Kind        string   `json:"kind"`
	Host        string   `json:"host"`
	Path        string   `json:"path,omitempty"`
	Port        int      `json:"port"`
	EntryPoints []string `json:"entrypoints,omitempty"`
	Retry       int      `json:"retry,omitempty"`
}

// Proxied services run blue/green: a new release starts alongside the old one
// under its own container name, and the old one is retired only after the new
// one is healthy.
func (s Service) Proxied() bool { return s.Proxy != nil }

type Health struct {
	URL      string   `json:"url,omitempty"`
	Command  []string `json:"command,omitempty"`
	Retries  int      `json:"retries"`
	Interval string   `json:"interval"`
	Grace    string   `json:"grace,omitempty"`
	Follow   bool     `json:"follow,omitempty"`
}

// Artifact is one file to swap into place on the host.
type Artifact struct {
	Name   string `json:"name"`
	Dest   string `json:"dest"`
	Staged string `json:"staged"` // where the CLI rsync'd it; renamed onto Dest
	Magic  string `json:"magic,omitempty"`
	Retain int    `json:"retain"`
	Bytes  int64  `json:"bytes"`
	MTime  int64  `json:"mtime"` // unix seconds; rsync --times preserves it

	// Dir marks an artifact that is a directory tree rather than a single file.
	// The swap is the same rename, because renaming a directory within its
	// parent is just as atomic as renaming a file.
	Dir bool `json:"dir,omitempty"`
}

type Stage struct {
	Name            string            `json:"name"`
	Image           string            `json:"image"`
	Command         []string          `json:"command"`
	Env             map[string]string `json:"env,omitempty"`
	Capture         string            `json:"capture,omitempty"`
	RequireNonEmpty bool              `json:"require_nonempty,omitempty"`
	Retain          int               `json:"retain,omitempty"`
}

// Ledger is the host-side record of what has been deployed. It lives at
// <root>/<project>/releases.json and is the authority for status and rollback.
type Ledger struct {
	Project string `json:"project"`

	// Current is the release believed to be *serving*. A deploy that fails
	// before replacing any running container does not move it — the previous
	// release is still up, and reporting otherwise would contradict the error
	// the operator was just shown.
	Current string `json:"current"`

	// LastAttempt is the most recent deploy regardless of outcome. It differs
	// from Current exactly when the last deploy failed without taking over.
	LastAttempt string `json:"last_attempt,omitempty"`

	// Accessories records the definition hash of each accessory as it was
	// actually applied — when its container was created or recreated — keyed by
	// name.
	//
	// It has to be tracked separately from the release spec because a deploy
	// records the manifest it was *given*, not the accessory state it *applied*:
	// `up` deliberately leaves an existing accessory alone. Diffing the manifest
	// against the last recorded spec therefore made drift disappear after any
	// unrelated deploy, while the container kept running the old config.
	Accessories map[string]string `json:"accessories,omitempty"`

	// Salt keys the secret hashes stored in this ledger. Generated once per
	// project on the host, so a truncated digest of a low-entropy value is not
	// brute-forceable from the ledger alone.
	Salt string `json:"salt,omitempty"`

	Releases []Entry `json:"releases"`
}

// RecordAccessory notes the definition an accessory container was created with.
func (l *Ledger) RecordAccessory(name string, svc Service) {
	if l.Accessories == nil {
		l.Accessories = map[string]string{}
	}
	l.Accessories[name] = HashService(svc)
}

// HashService fingerprints a service or accessory definition. encoding/json
// sorts map keys, so the same definition always produces the same digest.
func HashService(svc Service) string {
	b, err := json.Marshal(svc)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return "h:" + hex.EncodeToString(sum[:])[:16]
}

// DefaultRetain is how many restorable releases a host keeps when the manifest
// does not say. Both ends fall back to it, so a spec that predates the field
// prunes the same way a current one does.
const DefaultRetain = 5

// Release statuses recorded in the ledger.
//
// The distinction between failed and degraded is the whole point of recording a
// status at all: one means the host is exactly as it was, the other means it is
// not, and an operator reading `shunt status` after a bad night needs to know
// which without inferring it from an error message.
const (
	StatusActive     = "active"     // currently serving
	StatusSuperseded = "superseded" // replaced by a later release
	StatusFailed     = "failed"     // failed before any running container was replaced
	StatusDegraded   = "degraded"   // failed partway; the host is running a mix
)

// Healthy reports whether an entry represents a release that took over cleanly.
// Failed and degraded releases are neither serving nor safe to roll onto.
func (e *Entry) Healthy() bool {
	return e.Status != StatusFailed && e.Status != StatusDegraded
}

type Entry struct {
	ID         string              `json:"id"`
	Status     string              `json:"status"`
	StartedAt  time.Time           `json:"started_at"`
	FinishedAt time.Time           `json:"finished_at,omitempty"`
	Images     map[string]ImageRef `json:"images"`
	Services   []string            `json:"services"`
	Error      string              `json:"error,omitempty"`

	// Spec is retained so a rollback can re-apply the exact previous release
	// without needing the manifest that produced it.
	Spec *Spec `json:"spec,omitempty"`

	// Provenance and the transfer cost are lifted out of the spec so `status`
	// and the JSON contract can read them without unpacking a whole release.
	Provenance Provenance `json:"provenance,omitzero"`
	Bytes      int64      `json:"bytes,omitempty"`
}

// HashSecret is the one-way form a secret value takes once it is written to the
// host's ledger. Both ends use it: the helper redacts with it before persisting,
// and the CLI applies it to freshly-resolved values so `shunt plan` can compare
// like with like without a plaintext secret ever crossing back.
//
// Keyed by a per-project salt rather than a bare digest. Plenty of real secrets
// are low-entropy or drawn from a known format — a six-digit pin, a postcode, an
// api key with a fixed prefix — and an unsalted truncated sha256 of those is
// recoverable by anyone who reads the ledger. The salt makes the stored digests
// useless off that host while still comparing equal for equal values.
func HashSecret(salt, v string) string {
	m := hmac.New(sha256.New, []byte(salt))
	m.Write([]byte(v))
	return "h:" + hex.EncodeToString(m.Sum(nil))[:16]
}

// NewSalt generates a project's secret-hash salt.
func NewSalt() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// Find returns the entry with the given id, or nil.
func (l *Ledger) Find(id string) *Entry {
	for i := range l.Releases {
		if l.Releases[i].ID == id {
			return &l.Releases[i]
		}
	}
	return nil
}

// Restorable reports whether this entry can serve as a rollback target: it
// reached a healthy state, and it retained the spec needed to replay it.
func (e *Entry) Restorable() bool { return e.Healthy() && e.Spec != nil }

// KeepIDs returns the releases whose images and env-files must survive pruning:
// the newest `retain` restorable ones, plus whatever is currently active.
//
// Counting failed attempts toward `retain` is the subtle way to lose a rollback.
// A run of failed deploys — exactly the situation where you most want to go back
// — would otherwise push the last good release out of the keep set, and the next
// successful deploy would delete its images and its env-file. So failures are
// skipped rather than counted, and the history stays as deep as it claims.
func (l *Ledger) KeepIDs(retain int) map[string]bool {
	if retain <= 0 {
		retain = DefaultRetain
	}
	keep := map[string]bool{}
	if l.Current != "" {
		keep[l.Current] = true
	}
	seen := 0
	for i := len(l.Releases) - 1; i >= 0 && seen < retain; i-- {
		if !l.Releases[i].Restorable() {
			continue
		}
		keep[l.Releases[i].ID] = true
		seen++
	}
	return keep
}

// Trim bounds the ledger's length without dropping releases that are still
// rollback targets.
//
// A plain "keep the last N entries" would let a run of failed deploys evict the
// last good release from the history altogether, which is the same bug KeepIDs
// exists to prevent, one layer up.
func (l *Ledger) Trim(retain int) {
	if retain <= 0 {
		retain = DefaultRetain
	}
	window := retain * 2
	if len(l.Releases) <= window {
		return
	}
	keep := l.KeepIDs(retain)
	cutoff := len(l.Releases) - window
	out := make([]Entry, 0, window+len(keep))
	for i := range l.Releases {
		if i >= cutoff || keep[l.Releases[i].ID] {
			out = append(out, l.Releases[i])
		}
	}
	l.Releases = out
}

// Previous returns the most recent successfully-activated release before the
// current one — the target of `shunt rollback` with no argument.
func (l *Ledger) Previous() *Entry {
	seenCurrent := false
	for i := len(l.Releases) - 1; i >= 0; i-- {
		e := &l.Releases[i]
		if e.ID == l.Current {
			seenCurrent = true
			continue
		}
		// A degraded release is no more a rollback target than a failed one:
		// rolling onto a release that only half took over repeats the problem.
		if !seenCurrent || !e.Healthy() {
			continue
		}
		return e
	}
	return nil
}

// Event kinds. Constants rather than bare strings so the two binaries cannot
// drift on a typo that would silently stop rendering a whole class of event.
const (
	KindStep   = "step"   // work starting; shown only in verbose mode
	KindOK     = "ok"     // work completed
	KindInfo   = "info"   // noteworthy but not a step outcome
	KindLog    = "log"    // passthrough output from a container
	KindFail   = "fail"   // the operation failed; message explains why
	KindResult = "result" // terminal summary of a successful operation
)

// Event is one NDJSON line emitted by the helper on stdout. The CLI renders
// these; anything the helper writes to stderr is passed through as raw output.
type Event struct {
	Kind    string `json:"kind"`
	Step    string `json:"step,omitempty"`
	Message string `json:"message,omitempty"`

	// Result payload, set when Kind == KindResult.
	Release string `json:"release,omitempty"`
	Status  string `json:"status,omitempty"`
}
