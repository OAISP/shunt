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
	"sort"
	"strings"
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
}

// SetArtifactSources points artifact transfers at explicit local paths instead
// of the manifest's src fields.
func (e *Engine) SetArtifactSources(paths map[string]string) { e.artifactSrc = paths }

func New(m *manifest.Manifest) *Engine {
	return &Engine{M: m, Client: sshx.New(m.Host)}
}

// Connect opens the multiplexed ssh connection, checks the host has what shunt
// needs, and makes sure a matching helper binary is in place.
func (e *Engine) Connect(ctx context.Context) error {
	if err := e.Client.Connect(ctx); err != nil {
		return err
	}
	facts, err := e.Client.Probe(ctx)
	if err != nil {
		return err
	}
	e.facts = facts

	out, err := e.Client.Run(ctx, "sh", "-c", `echo "${SHUNT_ROOT:-$HOME/.shunt}"`)
	if err != nil {
		return err
	}
	e.root = strings.TrimSpace(out)
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
		"ls -1t "+shellArg(filepath.Join(e.root, "bin"))+"/shunt-helper-* 2>/dev/null | tail -n +4 | xargs -r rm -f")
	return nil
}

type BuildOptions struct {
	NoCache bool
	Verbose bool
}

// Build compiles every image in the manifest into a local OCI layout.
func (e *Engine) Build(ctx context.Context, id string, o BuildOptions) (map[string]*build.Result, error) {
	res := map[string]*build.Result{}
	// buildx writes the image id to stdout even under --progress quiet; only a
	// verbose run wants to see it.
	var logSink io.Writer = io.Discard
	progress := "quiet"
	if o.Verbose {
		logSink, progress = os.Stderr, "auto"
	}
	cacheDir, err := e.cacheDir(id)
	if err != nil {
		return nil, err
	}
	// Old export directories are dropped here rather than at the end, so a run
	// that fails partway still tidies up after its predecessors.
	e.pruneCacheDirs(3)
	for _, name := range sortedKeys(e.M.Images) {
		img := e.M.Images[name]
		args, err := secrets.InterpolateMap(img.Args)
		if err != nil {
			return nil, fmt.Errorf("image %s build args: %w", name, err)
		}
		r, err := build.Build(ctx, build.Options{
			Name:       name,
			Context:    e.M.Abs(img.Context),
			Dockerfile: e.M.Abs(img.Dockerfile),
			Platform:   img.Platform,
			Target:     img.Target,
			Args:       args,
			OutDir:     filepath.Join(cacheDir, name),
			NoCache:    o.NoCache,
			Progress:   progress,
			Stdout:     logSink,
		})
		if err != nil {
			return nil, err
		}
		ref := e.ImageRef(name, id)
		if err := build.Retag(r.Dir, ref); err != nil {
			return nil, fmt.Errorf("image %s: %w", name, err)
		}
		// Makes the layout loadable by hosts without the containerd image store,
		// which is most of them.
		if err := build.WriteDockerArchiveManifest(r.Dir, ref); err != nil {
			return nil, fmt.Errorf("image %s: %w", name, err)
		}
		res[name] = r
	}
	return res, nil
}

// Push mirrors each built layout to the host's store.
func (e *Engine) Push(ctx context.Context, built map[string]*build.Result, verbose bool) (map[string]*transport.Stats, error) {
	stats := map[string]*transport.Stats{}
	for _, name := range sortedResultKeys(built) {
		st, err := transport.Push(ctx, transport.Options{
			Client:     e.Client,
			LocalDir:   built[name].Dir,
			RemoteDir:  filepath.Join(e.StorePath(), name),
			Verbose:    verbose,
			RemoteZstd: e.facts.RsyncZstd,
		})
		if err != nil {
			return nil, fmt.Errorf("push image %s: %w", name, err)
		}
		stats[name] = st
	}
	return stats, nil
}

// ImageRef is the immutable, release-addressable tag a built image gets on the
// host. Rollback works by re-running a previous release's refs, so these must
// never be reused.
func (e *Engine) ImageRef(image, releaseID string) string {
	return fmt.Sprintf("shunt/%s-%s:%s", e.M.Project, image, releaseID)
}

// Spec assembles the complete release description sent to the helper.
func (e *Engine) Spec(ctx context.Context, id string, built map[string]*build.Result) (*release.Spec, error) {
	sec, err := secrets.Resolve(e.M)
	if err != nil {
		return nil, err
	}

	images := map[string]release.ImageRef{}
	for name, r := range built {
		images[name] = release.ImageRef{Ref: e.ImageRef(name, id), Digest: r.Digest}
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
	}
	return spec, nil
}

// StagedPath is where an artifact is transferred to before being swapped into
// place.
//
// Beside the destination, so the final rename is on the same filesystem and
// therefore atomic. Scoped by release id, so two deploys cannot land on each
// other's half-transferred file — and so a fragment left by an abandoned deploy
// can never be mistaken for this one's.
//
// The suffix does not cost anything at transfer time: rsync's --fuzzy still
// finds the current copy next door as its delta basis, because the destination
// name is still the staged name's longest common prefix.
func StagedPath(dest, releaseID string) string { return dest + ".new." + releaseID }

// artifacts resolves the manifest's artifact list, dropping any whose local file
// is missing unless it is marked required.
func (e *Engine) artifacts(releaseID string) ([]release.Artifact, error) {
	out := make([]release.Artifact, 0, len(e.M.Artifacts))
	for _, a := range e.M.Artifacts {
		src := e.M.Abs(a.Src)
		fi, err := os.Stat(src)
		if err != nil {
			if a.Required {
				return nil, fmt.Errorf("artifact %q: %s is required but missing", a.Name, src)
			}
			// Not an error: the host keeps whatever it already has, which is the
			// right default for a file produced by an occasional ETL run.
			fmt.Fprintf(os.Stderr, "warning: artifact %q: %s not found, the host keeps its current copy\n", a.Name, src)
			continue
		}
		size, mtime := fi.Size(), fi.ModTime().Unix()
		if fi.IsDir() {
			// A tree has no single size or mtime, so summarise it: the plan needs
			// something to compare, and the helper needs something to validate
			// the staged copy against.
			size, mtime = treeSummary(src)
			if a.Magic != "" {
				return nil, fmt.Errorf("artifact %q: magic cannot apply to a directory", a.Name)
			}
		}
		out = append(out, release.Artifact{
			Name:   a.Name,
			Dest:   a.Dest,
			Staged: StagedPath(a.Dest, releaseID),
			Magic:  a.Magic,
			Retain: a.Retain,
			Bytes:  size,
			MTime:  mtime,
			Dir:    fi.IsDir(),
		})
	}
	return out, nil
}

// treeSummary totals a directory's file sizes and takes its newest mtime.
//
// Not a hash: hashing a directory of model weights means reading every byte on
// both sides, which is most of the cost of simply shipping it. Size and mtime
// are the same heuristic rsync itself uses to decide what to transfer, which
// makes them the right basis for deciding whether the tree counts as changed.
func treeSummary(root string) (bytes, mtime int64) {
	filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		bytes += fi.Size()
		if m := fi.ModTime().Unix(); m > mtime {
			mtime = m
		}
		return nil
	})
	return bytes, mtime
}

// linkDestFor names the live tree to reuse unchanged files from, or "" when the
// host has nothing there yet.
func linkDestFor(a release.Artifact, present map[string]transport.FileStat) string {
	if !a.Dir {
		return "" // single files use --fuzzy instead
	}
	if st, ok := present[a.Dest]; ok && st.Size >= 0 {
		return a.Dest
	}
	return ""
}

// LocalArtifactPath is the source file for a named artifact.
func (e *Engine) LocalArtifactPath(name string) string {
	if p, ok := e.artifactSrc[name]; ok {
		return p
	}
	for _, a := range e.M.Artifacts {
		if a.Name == name {
			return e.M.Abs(a.Src)
		}
	}
	return ""
}

// PushArtifacts transfers each artifact to its staged path on the host.
func (e *Engine) PushArtifacts(ctx context.Context, spec *release.Spec, verbose bool) (map[string]*transport.Stats, error) {
	stats := map[string]*transport.Stats{}

	// Which destinations already exist, in one round trip. A directory transfer
	// reuses the live tree via --link-dest, and pointing that at a path the host
	// does not have yet makes rsync print a warning on a first deploy — accurate
	// but alarming, and avoidable now that stats come back batched.
	dests := make([]string, 0, len(spec.Artifacts))
	for _, a := range spec.Artifacts {
		dests = append(dests, a.Dest)
	}
	present := transport.RemoteFileStats(ctx, e.Client, dests)

	for _, a := range spec.Artifacts {
		st, err := transport.PushFile(ctx, transport.FileOptions{
			Client:     e.Client,
			LocalPath:  e.LocalArtifactPath(a.Name),
			RemotePath: a.Staged,
			Verbose:    verbose,
			RemoteZstd: e.facts.RsyncZstd,
			// The live tree is what an unchanged file should be linked from
			// rather than re-sent; ignored for single-file artifacts, and
			// omitted entirely until the host actually has a tree to link from.
			LinkDest: linkDestFor(a, present),
		})
		if err != nil {
			return nil, err
		}
		stats[a.Name] = st
	}
	return stats, nil
}

// RemoteKind reports whether a host path is a directory and how large it is.
func (e *Engine) RemoteKind(ctx context.Context, path string) (isDir bool, size int64, err error) {
	out, err := e.Client.Run(ctx, "sh", "-c",
		"p="+shellArg(path)+`; if [ -d "$p" ]; then echo -n "dir "; du -sb "$p" | cut -f1; `+
			`elif [ -f "$p" ]; then echo -n "file "; stat -c %s "$p"; else echo "missing 0"; fi`)
	if err != nil {
		return false, 0, err
	}
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) < 2 {
		return false, 0, fmt.Errorf("could not stat %s on %s", path, e.M.Host)
	}
	if fields[0] == "missing" {
		return false, 0, fmt.Errorf("%s does not exist on %s", path, e.M.Host)
	}
	fmt.Sscanf(fields[1], "%d", &size)
	return fields[0] == "dir", size, nil
}

// Fetch pulls a host path down to a local one.
func (e *Engine) Fetch(ctx context.Context, remote, local string, isDir, verbose bool) (*transport.Stats, error) {
	return transport.Fetch(ctx, transport.FileOptions{
		Client:     e.Client,
		LocalPath:  local,
		RemotePath: remote,
		Verbose:    verbose,
		RemoteZstd: e.facts.RsyncZstd,
	}, isDir)
}

// Captures lists stage capture files on the host, newest first.
//
// A pre-migration dump that cannot be retrieved is decorative, and working out
// where the helper put it is not something an operator should have to do from
// the manifest.
func (e *Engine) Captures(ctx context.Context) ([]string, error) {
	dirs := map[string]bool{}
	for _, st := range e.M.Stages {
		if st.Capture != "" {
			dirs[filepath.Dir(st.Capture)] = true
		}
	}
	if len(dirs) == 0 {
		return nil, nil
	}
	var script strings.Builder
	for _, d := range slices.Sorted(maps.Keys(dirs)) {
		fmt.Fprintf(&script, "ls -1t %s/* 2>/dev/null | head -20\n", shellArg(d))
	}
	out, err := e.Client.Run(ctx, "sh", "-c", script.String())
	if err != nil {
		return nil, err
	}
	var found []string
	for _, ln := range strings.Split(strings.TrimSpace(out), "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			found = append(found, ln)
		}
	}
	return found, nil
}

// PreflightSpace refuses a transfer the host has no room for.
//
// rsync running out of space partway leaves a truncated blob and an error about
// a write failure, which tells the operator nothing about the actual problem.
// Checking first costs nothing — the free-space figure already came back with
// the probe. The headroom multiplier covers `docker load` unpacking the layout
// alongside the copy rsync just wrote.
func (e *Engine) PreflightSpace(built map[string]*build.Result) error {
	if e.facts.FreeBytes <= 0 {
		return nil // df was unavailable; not a reason to refuse to deploy
	}
	var need int64
	for _, r := range built {
		need += r.Bytes
	}
	if need == 0 {
		return nil
	}
	const headroom = 3
	if want := need * headroom; e.facts.FreeBytes < want {
		return fmt.Errorf("%s has %s free, and this deploy needs roughly %s (%s of images, plus room to unpack them)\n"+
			"  free some space, or run `shunt prune` to drop superseded images",
			e.M.Host, ui.Bytes(e.facts.FreeBytes), ui.Bytes(want), ui.Bytes(need))
	}
	return nil
}

// PreflightArtifacts checks the host can actually receive every artifact, before
// anything expensive happens.
//
// Building and shipping a 400 MB image only to fail on mkdir wastes minutes and
// tells the operator nothing useful, so every destination directory is probed in
// a single round trip and each failure names its own fix.
func (e *Engine) PreflightArtifacts(ctx context.Context) error {
	dests := make([]string, 0, len(e.M.Artifacts))
	for _, a := range e.M.Artifacts {
		dests = append(dests, a.Dest)
	}
	return e.PreflightArtifactDests(ctx, dests)
}

// PreflightArtifactDests is the same check driven by explicit destinations,
// which is what a bundle has: it carries a release, not a manifest, so the
// paths come from the spec. Skipping this was why applying a bundle with an
// artifact failed at rsync with a bare "No such file or directory".
func (e *Engine) PreflightArtifactDests(ctx context.Context, dests []string) error {
	if len(dests) == 0 {
		return nil
	}
	dirs := map[string]bool{}
	for _, d := range dests {
		if d != "" {
			dirs[filepath.Dir(d)] = true
		}
	}
	var script strings.Builder
	for _, d := range slices.Sorted(maps.Keys(dirs)) {
		fmt.Fprintf(&script, "if ! mkdir -p %s 2>/dev/null; then echo \"NODIR %s\"; elif [ ! -w %s ]; then echo \"NOWRITE %s\"; fi\n",
			shellArg(d), d, shellArg(d), d)
	}
	out, err := e.Client.Run(ctx, "sh", "-c", script.String())
	if err != nil {
		return err
	}

	var problems []string
	for _, ln := range strings.Split(strings.TrimSpace(out), "\n") {
		if ln == "" {
			continue
		}
		kind, dir, _ := strings.Cut(ln, " ")
		switch kind {
		case "NODIR":
			problems = append(problems, fmt.Sprintf("  %s could not be created", dir))
		case "NOWRITE":
			problems = append(problems, fmt.Sprintf("  %s is not writable", dir))
		}
	}
	if len(problems) == 0 {
		return nil
	}

	user := e.M.Host
	if u, _, ok := strings.Cut(e.M.Host, "@"); ok {
		user = u
	}
	return fmt.Errorf("artifact destinations are not usable on %s:\n%s\n\n"+
		"  This is a one-time setup step — /opt is root-owned by default. On the host run:\n"+
		"    sudo mkdir -p <dir> && sudo chown -R %s <dir>\n"+
		"  Or point dest= somewhere you already own, e.g. $HOME.",
		e.M.Host, strings.Join(problems, "\n"), user)
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
		spec.Images[name] = release.ImageRef{Ref: deployed.Ref, Digest: deployed.Digest}
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
}

// Running reports whether docker considers this container up.
func (c Container) Running() bool { return c.State == "running" }

// RemoteState is what the host currently believes it is running.
type RemoteState struct {
	Ledger     *release.Ledger `json:"ledger"`
	Containers []Container     `json:"containers"`
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
	out, err := e.Client.Run(ctx, e.helperPath, "status", e.M.Project)
	if err != nil {
		return nil, err
	}
	var st RemoteState
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		return nil, fmt.Errorf("parse remote status: %w (%s)", err, truncate(out, 200))
	}
	return &st, nil
}

// Apply streams the spec to the helper and renders the event stream.
func (e *Engine) Apply(ctx context.Context, spec *release.Spec, r EventRenderer) error {
	b, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	return e.stream(ctx, strings.NewReader(string(b)), r, e.helperPath, "apply")
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

func (e *Engine) Rollback(ctx context.Context, id string, r EventRenderer) error {
	args := []string{e.helperPath, "rollback", e.M.Project}
	if id != "" {
		args = append(args, id)
	}
	return e.stream(ctx, nil, r, args...)
}

// Exec runs a command inside an existing container, wired to the local
// terminal so an interactive shell or console works.
func (e *Engine) Exec(ctx context.Context, container string, command []string) error {
	argv := append([]string{"docker", "exec"}, execTTYFlags()...)
	argv = append(argv, container)
	argv = append(argv, command...)
	return e.Client.Interactive(ctx, argv...)
}

// RunOneOff starts a throwaway container from a release's image, on the deploy
// network and with that release's secrets, and removes it when it exits.
//
// The env-file is the release's own, which is the only plaintext copy of its
// secrets on the host — so a console started this way sees exactly what the
// running service sees, without secrets being re-sent or re-resolved.
func (e *Engine) RunOneOff(ctx context.Context, releaseID, imageRef string, command []string) error {
	envFile := filepath.Join(e.root, e.M.Project, "env", releaseID+".env")
	argv := []string{"sh", "-c", oneOffScript()}
	argv = append(argv, "--", e.M.Network, envFile, imageRef)
	argv = append(argv, command...)
	return e.Client.Interactive(ctx, argv...)
}

// oneOffScript exists because the env-file may have been pruned, and docker
// refuses to start at all when --env-file names a missing path. Deciding that
// on the host keeps it to a single round trip.
func oneOffScript() string {
	return `net="$1"; env="$2"; img="$3"; shift 3
args="--rm -i"
[ -t 0 ] && args="$args -t"
[ -n "$net" ] && args="$args --network $net"
[ -f "$env" ] && args="$args --env-file $env"
exec docker run $args "$img" "$@"`
}

// execTTYFlags asks for a tty only when there is one to attach, so piping into
// `shunt exec` still works.
func execTTYFlags() []string {
	if ui.IsTerminal(os.Stdin) {
		return []string{"-it"}
	}
	return []string{"-i"}
}

func (e *Engine) Logs(ctx context.Context, service string, follow bool, tail string) error {
	args := []string{e.helperPath, "logs", e.M.Project}
	if service != "" {
		args = append(args, service)
	}
	if follow {
		args = append(args, "--follow")
	}
	args = append(args, "--tail", tail)
	return e.Client.Stream(ctx, nil, os.Stdout, os.Stderr, args...)
}

func (e *Engine) HelperPath() string { return e.helperPath }
func (e *Engine) Facts() sshx.Facts  { return e.facts }

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

func sortedKeys[T any](m map[string]T) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func sortedResultKeys(m map[string]*build.Result) []string { return sortedKeys(m) }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
