// Package engine turns a manifest into a release and applies it to the host.
// The CLI commands are thin wrappers over the steps here.
package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/OAISP/shunt/internal/build"
	"github.com/OAISP/shunt/internal/manifest"
	"github.com/OAISP/shunt/internal/release"
	"github.com/OAISP/shunt/internal/secrets"
	"github.com/OAISP/shunt/internal/sshx"
	"github.com/OAISP/shunt/internal/transport"
	"github.com/OAISP/shunt/internal/ui"
)

type Engine struct {
	M      *manifest.Manifest
	Client *sshx.Client

	root       string // resolved SHUNT_ROOT on the host
	helperPath string
	facts      sshx.Facts

	// artifactSrc overrides where an artifact's local copy is read from. A
	// bundle supplies its own extracted paths, having no manifest to resolve
	// them against.
	artifactSrc map[string]string

	// SecretsUnknown says the spec being planned carries no secret values
	// because they could not be resolved here, so a plan must not diff them.
	SecretsUnknown bool
}

func New(m *manifest.Manifest) *Engine {
	return &Engine{M: m, Client: sshx.New(m.Host)}
}

// Connect opens the multiplexed ssh connection, checks the host has what shunt
// needs, and makes sure a matching helper binary is in place.
func (e *Engine) Connect(ctx context.Context) error {
	if err := e.Client.Connect(ctx); err != nil {
		return err
	}
	facts, err := e.Client.Probe(ctx, embeddedHelperNames()...)
	if err != nil {
		return err
	}
	e.facts = facts

	e.root = strings.TrimSpace(facts.Root)
	if e.root == "" || e.root == "/.shunt" {
		return fmt.Errorf("could not resolve a home directory on %s; set SHUNT_ROOT there", e.M.Host)
	}
	return e.ensureHelper(ctx)
}

func (e *Engine) Close() { e.Client.Close() }

func (e *Engine) StorePath() string {
	return filepath.Join(e.root, e.M.Project, "store")
}

// ensureHelper puts a helper matching this CLI on the host. The remote filename
// is the helper's own content hash, so a rebuilt helper always lands at a new
// path and an outdated one can never be silently reused; an unchanged helper
// costs one `test -x` and no upload.
func (e *Engine) ensureHelper(ctx context.Context) error {
	h, err := helperBinary(e.facts.GoArch())
	if err != nil {
		return err
	}
	remote := filepath.Join(e.root, "bin", h.remoteName())
	e.helperPath = remote

	if e.facts.Helpers[h.remoteName()] {
		return nil
	}
	// A helper compiled on demand was not known before the probe.
	if _, err := e.Client.Run(ctx, "test", "-x", remote); err == nil {
		return nil
	}

	local, cleanup, err := h.write()
	if err != nil {
		return err
	}
	defer cleanup()

	if err := e.Client.Upload(ctx, local, remote, 0o755); err != nil {
		return fmt.Errorf("upload helper to %s: %w", e.M.Host, err)
	}
	if out, err := e.Client.Run(ctx, remote, "version"); err != nil {
		return fmt.Errorf("helper does not run on %s (arch %s): %s", e.M.Host, e.facts.Arch, out)
	}
	// Old content-addressed helpers accumulate otherwise, one per CLI build.
	e.Client.Run(ctx, "sh", "-c",
		"ls -1t "+sshx.Quote(filepath.Join(e.root, "bin"))+"/shunt-helper-* 2>/dev/null | tail -n +4 | xargs -r rm -f")
	return nil
}

type BuildOptions struct {
	NoCache bool
	Verbose bool
}

// Build compiles every image in the manifest into a local OCI layout, all at
// once: BuildKit schedules concurrent builds itself and shares the work they
// have in common, such as a base image.
func (e *Engine) Build(ctx context.Context, id string, o BuildOptions) (map[string]*build.Result, error) {
	// buildx writes the image id to stdout even under --progress quiet; only a
	// verbose run wants to see it.
	var logSink io.Writer = io.Discard
	progress := "quiet"
	if o.Verbose {
		logSink, progress = os.Stderr, "auto"
		if len(e.M.Images) > 1 {
			progress = "plain" // interactive progress displays cannot share a terminal
		}
	}
	cacheDir, err := e.cacheDir(id)
	if err != nil {
		return nil, err
	}
	// Old export directories are dropped here rather than at the end, so a run
	// that fails partway still tidies up after its predecessors.
	e.pruneCacheDirs(3)
	// Keyed on the manifest's own project rather than the target's: staging and
	// production build the same images, so they should share one cache.
	cacheKey := e.M.Project
	if own, _ := e.M.Destination(""); own.Project != "" {
		cacheKey = own.Project
	}
	cacheSpec := os.Getenv("SHUNT_BUILD_CACHE")

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		res      = map[string]*build.Result{}
		firstErr error
	)
	for _, name := range slices.Sorted(maps.Keys(e.M.Images)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := e.buildOne(ctx, name, id, build.Options{
				OutDir:   filepath.Join(cacheDir, name),
				NoCache:  o.NoCache,
				Progress: progress,
				Stdout:   logSink,
				Cache:    build.CacheFor(cacheSpec, cacheKey+"-"+name),
			})
			mu.Lock()
			defer mu.Unlock()
			if err != nil && firstErr == nil {
				firstErr = err
				cancel() // one failed image fails the release; stop the rest
			}
			res[name] = r
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return res, nil
}

// buildOne builds or imports one image and tags its layout for this release.
func (e *Engine) buildOne(ctx context.Context, name, id string, o build.Options) (*build.Result, error) {
	img := e.M.Images[name]
	var r *build.Result
	if img.Layout != "" {
		var err error
		if r, err = build.Import(e.M.Abs(img.Layout), o.OutDir, name, img.Platform); err != nil {
			return nil, err
		}
	} else {
		args, err := secrets.InterpolateMap(img.Args)
		if err != nil {
			return nil, fmt.Errorf("image %s build args: %w", name, err)
		}
		o.Name, o.Args, o.Platform, o.Target = name, args, img.Platform, img.Target
		o.Context, o.Dockerfile = e.M.Abs(img.Context), e.M.Abs(img.Dockerfile)
		if r, err = build.Build(ctx, o); err != nil {
			return nil, err
		}
	}
	ref := e.ImageRef(name, id)
	if err := build.Retag(r.Dir, ref); err != nil {
		return nil, fmt.Errorf("image %s: %w", name, err)
	}
	// Makes the layout loadable by hosts without the containerd image store.
	if err := build.WriteDockerArchiveManifest(r.Dir, ref); err != nil {
		return nil, fmt.Errorf("image %s: %w", name, err)
	}
	fp, err := build.Fingerprint(r.Dir)
	if err != nil {
		return nil, fmt.Errorf("image %s: %w", name, err)
	}
	r.Fingerprint = fp
	return r, nil
}

// Push mirrors the built layouts into the host's store in one transfer. They
// are exported side by side, one directory per image, which is exactly the
// store's own shape — so mirroring their parent also drops the layout of an
// image the manifest no longer builds.
func (e *Engine) Push(ctx context.Context, built map[string]*build.Result, verbose bool) (*transport.Stats, error) {
	if len(built) == 0 {
		return &transport.Stats{}, nil
	}
	parent := ""
	for name, r := range built {
		dir := filepath.Dir(r.Dir)
		if filepath.Base(r.Dir) != name || (parent != "" && dir != parent) {
			return nil, fmt.Errorf("image layouts must sit side by side, named after their images: %s", r.Dir)
		}
		parent = dir
	}
	st, err := transport.Push(ctx, transport.Options{
		Client:    e.Client,
		LocalDir:  parent,
		RemoteDir: e.StorePath(),
		Verbose:   verbose,
	})
	if err != nil {
		return nil, fmt.Errorf("push images: %w", err)
	}
	return st, nil
}

// ImageRef is the immutable, release-addressable tag a built image gets on the
// host. Rollback works by re-running a previous release's refs, so these must
// never be reused.
func (e *Engine) ImageRef(image, releaseID string) string {
	return ImageTag(e.M.Project, image, releaseID)
}

// ImageTag is the tag a project's built image carries on the host.
func ImageTag(project, image, releaseID string) string {
	return fmt.Sprintf("shunt/%s-%s:%s", project, image, releaseID)
}

// Spec assembles the complete release description sent to the helper.
func (e *Engine) Spec(ctx context.Context, id string, built map[string]*build.Result) (*release.Spec, error) {
	sec, err := secrets.Resolve(e.M)
	if err != nil {
		return nil, err
	}
	return e.SpecWith(id, built, sec)
}

// SpecWith assembles a spec around secrets the caller already resolved — or
// none, for a bundle, which never carries values.
func (e *Engine) SpecWith(id string, built map[string]*build.Result, sec map[string]string) (*release.Spec, error) {
	images := map[string]release.ImageRef{}
	for name, r := range built {
		images[name] = release.ImageRef{Ref: e.ImageRef(name, id), Digest: r.Digest, Fingerprint: r.Fingerprint}
	}
	// Services and accessories may reference images shunt does not build; those
	// are pulled on the host.
	for _, set := range []map[string]manifest.Service{e.M.Services, e.M.Accessories} {
		for _, svc := range set {
			if _, ok := images[svc.Image]; !ok {
				images[svc.Image] = release.ImageRef{Ref: svc.Image, External: true}
			}
		}
	}
	for _, st := range e.M.Stages {
		if _, ok := images[st.Image]; !ok {
			images[st.Image] = release.ImageRef{Ref: st.Image, External: true}
		}
	}

	svcs, err := convertServices(e.M.Services)
	if err != nil {
		return nil, err
	}
	accs, err := convertServices(e.M.Accessories)
	if err != nil {
		return nil, err
	}

	stages := make([]release.Stage, 0, len(e.M.Stages))
	for _, st := range e.M.Stages {
		env, err := secrets.InterpolateMap(st.Env)
		if err != nil {
			return nil, fmt.Errorf("stage %s env: %w", st.Name, err)
		}
		stages = append(stages, release.Stage{
			Name:            st.Name,
			Image:           st.Image,
			Command:         st.Command,
			Env:             env,
			Capture:         st.Capture,
			RequireNonEmpty: st.RequireNonEmpty,
			Retain:          st.Retain,
		})
	}

	arts, err := e.artifacts(id)
	if err != nil {
		return nil, err
	}

	spec := &release.Spec{
		Protocol:       release.Protocol,
		Project:        e.M.Project,
		ID:             id,
		Network:        e.M.Network,
		Retain:         e.M.Retain,
		StorePath:      e.StorePath(),
		Images:         images,
		Accessories:    accs,
		AccessoryOrder: e.M.AccessoryOrder(),
		Artifacts:      arts,
		Stages:         stages,
		Services:       svcs,
		Order:          e.M.StartOrder(),
		Secrets:        sec,
		SecretMode:     secretMode(e.M),
	}
	return spec, nil
}

// secretMode reports how this manifest wants secrets delivered.
func secretMode(m *manifest.Manifest) string { return SecretMode(m.Secrets) }

// SecretMode is the spec's delivery mode for a secrets block.
func SecretMode(s *manifest.Secrets) string {
	if s != nil && s.Mode == "file" {
		return "file"
	}
	return ""
}

// PreflightSpace refuses a transfer the host has no room for.
//
// rsync running out of space partway leaves a truncated blob and an error about
// a write failure, which tells the operator nothing about the actual problem.
// Sized from what is actually missing, not the whole image: a 5 KB deploy of a
// 2 GB image needs 5 KB of store. The daemon side gets headroom for the archive
// it copies in plus the layers it unpacks, which are typically two or three
// times their compressed size.
func (e *Engine) PreflightSpace(est *TransferEstimate, d *release.Disk) error {
	if est == nil || d == nil || est.Missing == 0 {
		return nil
	}
	storeNeed, dockerNeed := est.Missing, 3*est.Missing
	check := func(free, need int64, where string) error {
		if free <= 0 || free >= need {
			return nil
		}
		return fmt.Errorf("%s has %s free %s, and this deploy needs roughly %s there (%s of new image data)\n"+
			"  free some space, or run `shunt prune` to drop superseded images",
			e.M.Host, ui.Bytes(free), where, ui.Bytes(need), ui.Bytes(est.Missing))
	}
	if d.Shared {
		return check(d.StoreFree, storeNeed+dockerNeed, "for the store and the docker daemon")
	}
	if err := check(d.StoreFree, storeNeed, "for the store"); err != nil {
		return err
	}
	return check(d.DockerFree, dockerNeed, "in docker's data root")
}

// BootSpec assembles a spec for `shunt boot`, which recreates an accessory
// without building anything.
//
// Any image shunt normally builds is therefore absent, and Spec would mark it
// external and try to pull a name no registry knows. Resolving those refs from
// the release currently on the host is what makes booting an accessory that
// uses a locally-built image work at all.
func (e *Engine) BootSpec(ctx context.Context, state *RemoteState) (*release.Spec, error) {
	spec, err := e.Spec(ctx, NewReleaseID(), nil)
	if err != nil {
		return nil, err
	}
	var current *release.Entry
	if state != nil && state.Ledger != nil {
		current = state.Ledger.Find(state.Ledger.Current)
	}
	for name := range e.M.Images {
		ref, ok := spec.Images[name]
		if !ok || !ref.External {
			continue // already resolved
		}
		if current == nil {
			return nil, fmt.Errorf("image %q is built locally and nothing is deployed yet — run `shunt up` first", name)
		}
		deployed, ok := current.Images[name]
		if !ok || deployed.Ref == "" {
			return nil, fmt.Errorf("image %q is built locally but release %s did not record it — run `shunt up` first",
				name, current.ID)
		}
		spec.Images[name] = deployed
	}
	return spec, nil
}

// Prune drops superseded images on the host.
func (e *Engine) Prune(ctx context.Context, r EventRenderer) error {
	return e.stream(ctx, nil, r, e.helperPath, "prune", e.M.Project)
}

// convertServices maps manifest services (or accessories) onto the wire type,
// expanding ${env:...} references as it goes.
func convertServices(in map[string]manifest.Service) (map[string]release.Service, error) {
	out := map[string]release.Service{}
	for name, s := range in {
		env, err := secrets.InterpolateMap(s.Env)
		if err != nil {
			return nil, fmt.Errorf("service %s env: %w", name, err)
		}
		rs := release.Service{
			Image:    s.Image,
			Secrets:  s.Secrets,
			Command:  s.Command,
			Env:      env,
			Publish:  s.Publish,
			Volumes:  s.Volumes,
			Restart:  s.Restart,
			Requires: s.Requires,
			Expose:   s.Expose,
			Drain:    s.Drain.String(),
			Init:     s.Init,

			Limits: s.Limits,
		}
		if s.Proxy != nil {
			rs.Proxy = &release.Proxy{
				Kind:        s.Proxy.Kind,
				Host:        s.Proxy.Host,
				Path:        s.Proxy.Path,
				Port:        s.ProxyPort(),
				EntryPoints: s.Proxy.EntryPoints,
				Retry:       s.Proxy.RetryAttempts(),
			}
		}
		if s.Health != nil {
			rs.Health = &release.Health{
				URL:      s.Health.URL,
				Command:  s.Health.Command,
				Retries:  s.Health.Retries,
				Interval: s.Health.Interval.String(),
				Follow:   s.Health.Follow,
			}
			if s.Health.Grace.Duration > 0 {
				rs.Health.Grace = s.Health.Grace.String()
			}
		}
		out[name] = rs
	}
	return out, nil
}

// NewReleaseID is a sortable, human-readable identifier.
//
// The timestamp is only second-granular, and a release id becomes a container
// name and an immutable image tag — so two deploys in the same second would
// collide on both. The random suffix makes that impossible while keeping ids
// lexically sortable by time, which the ledger and env-file pruning rely on.
func NewReleaseID() string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A failing CSPRNG is not a reason to refuse to deploy; the timestamp
		// alone still orders correctly and collides only within one second.
		return time.Now().UTC().Format("20060102-150405")
	}
	return time.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b[:])
}

// Container is one container the host is running for this project, as reported
// by the helper.
type Container struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Image   string `json:"image"`
	Release string `json:"release"`
	Service string `json:"service"`
	Kind    string `json:"kind"`

	// Config is the hash of the definition this container was started with.
	// Empty on containers created before shunt labelled them, which is why a
	// missing value never counts as drift.
	Config string `json:"config"`
	State  string `json:"state"`

	// Inputs is the digest of what the container was created from besides its
	// definition. Empty on one started before shunt recorded it, which a deploy
	// therefore cannot recognise as unchanged.
	Inputs string `json:"inputs"`
}

// Running reports whether docker considers this container up.
func (c Container) Running() bool { return c.State == "running" }

// RemoteState is what the host currently believes it is running.
type RemoteState struct {
	Ledger     *release.Ledger `json:"ledger"`
	Containers []Container     `json:"containers"`

	// Store and Paths come back only from PlanState: the blobs each image's
	// store holds, and the state of every artifact destination.
	Store map[string][]string          `json:"store,omitempty"`
	Paths map[string]release.PathState `json:"paths,omitempty"`
	Disk  *release.Disk                `json:"disk,omitempty"`
}

// ServiceContainers returns the containers belonging to one service.
func (s *RemoteState) ServiceContainers(name string) []Container {
	var out []Container
	for _, c := range s.Containers {
		if c.Service == name {
			out = append(out, c)
		}
	}
	return out
}

func (e *Engine) State(ctx context.Context) (*RemoteState, error) {
	return e.remoteState(ctx, "status")
}

// PlanState is State plus everything a plan needs from the host, in one round
// trip: what each image's store already holds, and the state of every artifact
// destination. It fails if a destination cannot receive its artifact, which is
// why it runs before the build: building and shipping a 400 MB image only to
// fail on mkdir wastes minutes and tells the operator nothing useful.
func (e *Engine) PlanState(ctx context.Context, dests []string) (*RemoteState, error) {
	st, err := e.remoteState(ctx, append([]string{"plan"}, dests...)...)
	if err != nil {
		return nil, err
	}
	return st, e.unusableDests(st.Paths)
}

func (e *Engine) remoteState(ctx context.Context, args ...string) (*RemoteState, error) {
	argv := append([]string{e.helperPath, args[0], e.M.Project}, args[1:]...)
	out, err := e.Client.Run(ctx, argv...)
	if err != nil {
		return nil, err
	}
	var st RemoteState
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		return nil, fmt.Errorf("parse remote status: %w (%s)", err, truncate(out, 200))
	}
	return &st, nil
}

// Boot force-recreates a single accessory. The spec is streamed the same way a
// deploy is, so the accessory gets the current manifest and current secrets.
func (e *Engine) Boot(ctx context.Context, name string, spec *release.Spec, r EventRenderer) error {
	b, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	return e.stream(ctx, strings.NewReader(string(b)), r, e.helperPath, "boot", name)
}

// Retire stops and removes the containers of a service the manifest no longer
// declares.
func (e *Engine) Retire(ctx context.Context, service string, r EventRenderer) error {
	return e.stream(ctx, nil, r, e.helperPath, "retire", e.M.Project, service)
}

// DownOptions says how much of a project to take away.
type DownOptions struct {
	// Accessories removes the stateful containers too. Opt-in for the same
	// reason `shunt boot` is its own verb: that is the database.
	Accessories bool

	// Purge additionally removes the network, the release-tagged images, the
	// ledger and the on-host secrets. Never volumes.
	Purge bool
}

// Down stops and removes a project's containers, and with Purge everything else
// shunt put on the host.
func (e *Engine) Down(ctx context.Context, o DownOptions, r EventRenderer) error {
	args := []string{e.helperPath, "down", e.M.Project, e.M.Network}
	if o.Accessories || o.Purge {
		args = append(args, "--accessories")
	}
	if o.Purge {
		args = append(args, "--purge")
	}
	return e.stream(ctx, nil, r, args...)
}

func (e *Engine) Rollback(ctx context.Context, id string, r EventRenderer) error {
	args := []string{e.helperPath, "rollback", e.M.Project}
	if id != "" {
		args = append(args, id)
	}
	return e.stream(ctx, nil, r, args...)
}

func (e *Engine) Facts() sshx.Facts { return e.facts }

// cacheDir is where this release's OCI layouts are exported.
//
// Scoped by release id because two deploys of the same project on one machine
// otherwise share it, and Build starts by deleting the directory — so one
// process wipes the layout another is still exporting into, and buildx fails
// with a rename error naming a blob that vanished underneath it. The layout is
// re-exported from scratch on every build regardless, so a per-release
// directory costs nothing beyond the name.
func (e *Engine) cacheDir(releaseID string) (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		base = filepath.Join(os.TempDir(), "shunt-cache")
	}
	d := filepath.Join(base, "shunt", e.M.Project, releaseID)
	return d, os.MkdirAll(d, 0o755)
}

// pruneCacheDirs keeps the most recent export directories and drops the rest.
//
// A few are kept rather than one, so a concurrent deploy's layout is never
// deleted out from under it while it is still transferring.
func (e *Engine) pruneCacheDirs(keep int) {
	base, err := os.UserCacheDir()
	if err != nil {
		base = filepath.Join(os.TempDir(), "shunt-cache")
	}
	root := filepath.Join(base, "shunt", e.M.Project)
	ents, err := os.ReadDir(root)
	if err != nil {
		return
	}
	var dirs []string
	for _, d := range ents {
		if d.IsDir() {
			dirs = append(dirs, d.Name())
		}
	}
	// Release ids sort lexically by time.
	slices.Sort(dirs)
	slices.Reverse(dirs)
	for i := keep; i < len(dirs); i++ {
		os.RemoveAll(filepath.Join(root, dirs[i]))
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
