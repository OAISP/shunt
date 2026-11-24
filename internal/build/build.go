// Package build drives buildx to produce an OCI layout directory per image.
//
// The OCI layout is what makes registry-free deploys cheap: every blob is stored
// under blobs/sha256/<digest>, so its filename *is* its content hash. rsync then
// gives layer-level deduplication for free — a rebuild that only changes the app
// layer transfers only the app layer, because every other filename already
// exists on the host with an identical size.
package build

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

type Result struct {
	Name   string // manifest image name
	Dir    string // local OCI layout directory
	Digest string // OCI manifest digest, e.g. sha256:73c1...
	Bytes  int64  // on-disk size of the layout
}

type Options struct {
	Name       string
	Context    string // absolute
	Dockerfile string // absolute
	Platform   string
	Target     string
	Args       map[string]string
	OutDir     string // absolute; overwritten on each build
	NoCache    bool
	Progress   string    // buildx --progress value
	Stdout     io.Writer // build log sink; discard it when not verbose
	Cache      *Cache    // nil builds with only the builder's own cache
}

// Cache is where buildx keeps layer cache between builds.
//
// A CI runner starts with an empty builder, so without one every layer after
// the base image is rebuilt, gets a new digest and is shipped again in full,
// and every pipeline reads as a change. With it, unchanged layers keep their
// digests and the transfer and the no-op check both work as they do locally.
type Cache struct {
	From, To string // --cache-from / --cache-to values

	// rotate moves a freshly exported local cache over the old one. buildx
	// never garbage-collects a local cache it exports into, so writing beside
	// it and swapping is what keeps it from growing on every build.
	rotate func() error
}

// CacheFor resolves a SHUNT_BUILD_CACHE value for one image. "gha" uses GitHub
// Actions' cache service; anything else is a directory the CI system persists
// between jobs. key separates images, so it must be stable across builds.
func CacheFor(spec, key string) *Cache {
	if spec == "" {
		return nil
	}
	if spec == "gha" {
		scope := "shunt-" + key
		// ignore-error: a cache service outage should cost a slower build, not
		// a failed deploy.
		return &Cache{From: "type=gha,scope=" + scope, To: "type=gha,mode=max,ignore-error=true,scope=" + scope}
	}
	dir := filepath.Join(spec, key)
	c := &Cache{To: "type=local,mode=max,dest=" + dir + ".next"}
	if _, err := os.Stat(filepath.Join(dir, "index.json")); err == nil {
		c.From = "type=local,src=" + dir
	}
	c.rotate = func() error {
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
		return os.Rename(dir+".next", dir)
	}
	return c
}

// Build exports one image to an OCI layout directory and returns its digest.
func Build(ctx context.Context, o Options) (*Result, error) {
	// buildx refuses to write into a non-empty directory that is not already a
	// valid layout, and stale index.json entries from a previous build would
	// otherwise linger. Start clean; the *remote* store is what accumulates.
	if err := os.RemoveAll(o.OutDir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(o.OutDir, 0o755); err != nil {
		return nil, err
	}

	args := []string{
		"buildx", "build",
		"--file", o.Dockerfile,
		"--output", fmt.Sprintf("type=oci,dest=%s,tar=false", o.OutDir),
		"--tag", "shunt-build/" + o.Name + ":latest",
	}
	if o.Platform != "" {
		args = append(args, "--platform", o.Platform)
	}
	if o.Target != "" {
		args = append(args, "--target", o.Target)
	}
	if o.NoCache {
		args = append(args, "--no-cache")
	}
	if o.Progress != "" {
		args = append(args, "--progress", o.Progress)
	}
	if c := o.Cache; c != nil {
		if c.From != "" {
			args = append(args, "--cache-from", c.From)
		}
		args = append(args, "--cache-to", c.To)
	}
	for _, k := range slices.Sorted(maps.Keys(o.Args)) {
		args = append(args, "--build-arg", k+"="+o.Args[k])
	}
	args = append(args, o.Context)

	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout = o.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("build image %q: %w%s", o.Name, err, buildxHint())
	}

	if o.Cache != nil && o.Cache.rotate != nil {
		if err := o.Cache.rotate(); err != nil {
			return nil, fmt.Errorf("build image %q: rotate build cache: %w", o.Name, err)
		}
	}

	dg, err := readIndexDigest(o.OutDir)
	if err != nil {
		return nil, fmt.Errorf("build image %q: %w", o.Name, err)
	}

	// buildx leaves an empty ingest/ scratch directory behind; it is not part of
	// the layout and must not be shipped.
	os.RemoveAll(filepath.Join(o.OutDir, "ingest"))

	if err := normalizeBlobTimes(o.OutDir); err != nil {
		return nil, fmt.Errorf("build image %q: %w", o.Name, err)
	}

	return &Result{Name: o.Name, Dir: o.OutDir, Digest: dg, Bytes: DirSize(o.OutDir)}, nil
}

// blobEpoch is the fixed timestamp every blob is stamped with. The value is
// arbitrary; only its stability matters.
var blobEpoch = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// Import writes a single-image layout holding just the image a prebuilt layout
// names, so it ships, deduplicates and loads like a fresh export. Attestation
// manifests are skipped, and from a multi-platform index only the requested
// platform is taken, so blobs the server would never run are never shipped.
// Blobs are copied, not linked: pinning their times must not touch the source.
func Import(src, outDir, name, platform string) (*Result, error) {
	d, man, err := selectImage(src, platform)
	if err != nil {
		return nil, fmt.Errorf("import image %q: %w", name, err)
	}
	if err := os.RemoveAll(outDir); err != nil {
		return nil, err
	}
	blobs := append([]string{d.Digest, man.Config.Digest}, layerDigests(man)...)
	for _, dg := range blobs {
		// A digest becomes a path on both sides of this copy.
		if !digestRE.MatchString(dg) {
			return nil, fmt.Errorf("import image %q: %q is not a digest", name, dg)
		}
		if err := copyFile(filepath.Join(src, blobPath(dg)), filepath.Join(outDir, blobPath(dg))); err != nil {
			return nil, fmt.Errorf("import image %q: %w", name, err)
		}
	}
	idx, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.index.v1+json",
		"manifests":     []descriptor{{MediaType: d.MediaType, Digest: d.Digest, Size: d.Size}},
	})
	if err != nil {
		return nil, err
	}
	for file, body := range map[string][]byte{"index.json": idx, "oci-layout": []byte(`{"imageLayoutVersion":"1.0.0"}`)} {
		if err := os.WriteFile(filepath.Join(outDir, file), body, 0o644); err != nil {
			return nil, err
		}
	}
	if err := normalizeBlobTimes(outDir); err != nil {
		return nil, fmt.Errorf("import image %q: %w", name, err)
	}
	return &Result{Name: name, Dir: outDir, Digest: d.Digest, Bytes: DirSize(outDir)}, nil
}

var digestRE = regexp.MustCompile(`^[a-z0-9]+:[A-Za-z0-9=_-]+$`)

// LayoutPlatform reads the os/arch of the image Import would take from a layout.
func LayoutPlatform(dir, platform string) (string, error) {
	_, man, err := selectImage(dir, platform)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(filepath.Join(dir, blobPath(man.Config.Digest)))
	if err != nil {
		return "", fmt.Errorf("read image config: %w", err)
	}
	var cfg ociPlatform
	if err := json.Unmarshal(b, &cfg); err != nil {
		return "", fmt.Errorf("parse image config: %w", err)
	}
	return cfg.String(), nil
}

type descriptor struct {
	MediaType   string            `json:"mediaType,omitempty"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size,omitempty"`
	Platform    *ociPlatform      `json:"platform,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type ociPlatform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant,omitempty"`
}

func (p ociPlatform) String() string {
	s := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		s += "/" + p.Variant
	}
	return s
}

// isAttestation recognises the provenance and SBOM manifests buildx adds to an
// index: annotated as such, and given the platform unknown/unknown.
func isAttestation(d descriptor) bool {
	return d.Annotations["vnd.docker.reference.type"] == "attestation-manifest" ||
		(d.Platform != nil && d.Platform.OS == "unknown")
}

// selectImage finds the image manifest a layout means: its one index.json entry,
// descending through nested indexes, minus attestations, narrowed to platform.
func selectImage(dir, platform string) (descriptor, *ociManifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if err != nil {
		return descriptor{}, nil, fmt.Errorf("%s is not an OCI image layout (no index.json)", dir)
	}
	var top struct {
		Manifests []descriptor `json:"manifests"`
	}
	if err := json.Unmarshal(b, &top); err != nil {
		return descriptor{}, nil, fmt.Errorf("parse index.json: %w", err)
	}
	if len(top.Manifests) != 1 {
		return descriptor{}, nil, fmt.Errorf("%s lists %d images in index.json; export exactly one image into an empty directory", dir, len(top.Manifests))
	}
	d := top.Manifests[0]
	for depth := 0; depth <= 4; depth++ {
		if !digestRE.MatchString(d.Digest) {
			return descriptor{}, nil, fmt.Errorf("%q is not a digest", d.Digest)
		}
		mb, err := os.ReadFile(filepath.Join(dir, blobPath(d.Digest)))
		if err != nil {
			return descriptor{}, nil, fmt.Errorf("read blob %s: %w", d.Digest, err)
		}
		var m struct {
			ociManifest
			Manifests []descriptor `json:"manifests"`
		}
		if err := json.Unmarshal(mb, &m); err != nil {
			return descriptor{}, nil, fmt.Errorf("parse blob %s: %w", d.Digest, err)
		}
		if m.Config.Digest != "" {
			if d.MediaType == "" {
				d.MediaType = m.MediaType
			}
			if d.Size == 0 {
				d.Size = int64(len(mb))
			}
			return d, &m.ociManifest, nil
		}
		var images []descriptor
		for _, c := range m.Manifests {
			if !isAttestation(c) && (platform == "" || c.Platform == nil || matchesPlatform(*c.Platform, platform)) {
				images = append(images, c)
			}
		}
		switch {
		case len(images) == 1:
			d = images[0]
		case len(images) == 0 && platform != "":
			return descriptor{}, nil, fmt.Errorf("layout has no image for %s (it has %s)", platform, platformList(m.Manifests))
		case len(images) == 0:
			return descriptor{}, nil, fmt.Errorf("index %s holds no image manifest", d.Digest)
		default:
			return descriptor{}, nil, fmt.Errorf("layout holds %d platforms (%s); set platform in [images.*] to pick one", len(images), platformList(images))
		}
	}
	return descriptor{}, nil, fmt.Errorf("image index nests more than 4 levels deep")
}

// matchesPlatform compares os/arch, and the variant only when one is asked for.
func matchesPlatform(p ociPlatform, want string) bool {
	parts := strings.Split(want, "/")
	if len(parts) < 2 || p.OS != parts[0] || p.Architecture != parts[1] {
		return false
	}
	return len(parts) < 3 || p.Variant == parts[2]
}

func platformList(ds []descriptor) string {
	var out []string
	for _, d := range ds {
		if d.Platform != nil && !isAttestation(d) {
			out = append(out, d.Platform.String())
		}
	}
	return strings.Join(out, ", ")
}

func layerDigests(m *ociManifest) []string {
	out := make([]string, 0, len(m.Layers))
	for _, l := range m.Layers {
		out = append(out, l.Digest)
	}
	return out
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// normalizeBlobTimes gives every blob the same modification time.
//
// A blob's filename is its content hash, so its mtime carries no information
// whatsoever — but rsync's quick check compares size *and* mtime, and this
// directory is deleted and re-exported on every build. Fresh mtimes on
// byte-identical blobs therefore made rsync consider all of them changed: it
// delta'd the entire image every deploy to transfer almost nothing, and rewrote
// every file on the host in the process.
//
// Pinning the mtime lets the quick check do its job — unchanged blobs are
// skipped outright rather than delta'd — and keeps the host's mtimes stable,
// which is what makes the host-side verification cache able to hit at all.
func normalizeBlobTimes(dir string) error {
	blobs := filepath.Join(dir, "blobs")
	if _, err := os.Stat(blobs); err != nil {
		return nil // no blobs directory is not this function's problem
	}
	return filepath.WalkDir(blobs, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		return os.Chtimes(path, blobEpoch, blobEpoch)
	})
}

// ociIndex is the subset of the OCI image index we care about.
type ociIndex struct {
	Manifests []struct {
		MediaType   string            `json:"mediaType"`
		Digest      string            `json:"digest"`
		Size        int64             `json:"size"`
		Annotations map[string]string `json:"annotations"`
	} `json:"manifests"`
}

func readIndexDigest(dir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if err != nil {
		return "", fmt.Errorf("read index.json: %w", err)
	}
	var idx ociIndex
	if err := json.Unmarshal(b, &idx); err != nil {
		return "", fmt.Errorf("parse index.json: %w", err)
	}
	if len(idx.Manifests) == 0 {
		return "", fmt.Errorf("index.json contains no manifests")
	}
	return idx.Manifests[0].Digest, nil
}

// Retag rewrites index.json so the layout carries the release-specific reference
// the host should end up with. docker load reads this annotation to name the
// image, which is how a release gets an immutable, rollback-addressable tag.
func Retag(dir, ref string) error {
	p := filepath.Join(dir, "index.json")
	b, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	ms, _ := raw["manifests"].([]any)
	if len(ms) == 0 {
		return fmt.Errorf("index.json contains no manifests")
	}
	for _, m := range ms {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		ann, _ := mm["annotations"].(map[string]any)
		if ann == nil {
			ann = map[string]any{}
		}
		ann["org.opencontainers.image.ref.name"] = ref
		// containerd's importer keys off io.containerd.image.name; setting both
		// keeps `docker load` naming consistent across storage drivers.
		ann["io.containerd.image.name"] = "docker.io/" + ref
		mm["annotations"] = ann
	}
	out, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	return os.WriteFile(p, out, 0o644)
}

// OCIExport describes whether the local builder can write the OCI layout every
// part of shunt's transport depends on.
//
// The rule is not "which driver": the classic `docker` driver exports a layout
// perfectly well once the daemon uses the containerd image store, and refuses
// outright when it does not. So both are read, and only that one combination
// is a problem.
type OCIExport struct {
	Driver     string // buildx driver: docker, docker-container, remote, kubernetes
	Containerd bool   // whether the daemon uses the containerd image store
	OK         bool
}

// CheckOCIExport reads the active builder and the daemon's image store.
//
// Anything it cannot determine is reported as OK. This gates a deploy and sits
// in `shunt audit`, and refusing to build because a `docker buildx inspect`
// output could not be parsed would be far worse than the error the build itself
// would print.
func CheckOCIExport() OCIExport {
	c := OCIExport{OK: true}

	out, err := exec.Command("docker", "buildx", "inspect").Output()
	if err != nil {
		return c
	}
	c.Driver = parseBuildxDriver(string(out))

	// The containerd image store reports itself in the daemon's driver status,
	// not as a storage driver name — overlayfs backed by containerd and
	// overlay2 backed by the classic store are both called "overlay"-something.
	if status, err := exec.Command("docker", "info", "-f", "{{json .DriverStatus}}").Output(); err == nil {
		c.Containerd = strings.Contains(string(status), "io.containerd.snapshotter.v1")
	}

	c.OK = c.supported()
	return c
}

// supported is the rule, kept apart from the two subprocesses that feed it so
// it can be exercised for combinations this machine does not have.
//
// An undeterminable driver reads as supported for the same reason the whole
// check defaults open: this gates a deploy.
func (c OCIExport) supported() bool { return c.Driver != "docker" || c.Containerd }

// parseBuildxDriver picks the driver out of `docker buildx inspect`.
//
// buildx pads its labels into a column, so this matches the label rather than a
// fixed "Driver: docker" string; it is split out to be tested against the real
// output.
func parseBuildxDriver(out string) string {
	for _, ln := range strings.Split(out, "\n") {
		if rest, found := strings.CutPrefix(strings.TrimSpace(ln), "Driver:"); found {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// Fix names the two ways out, in the order most people should take them. One
// line, because both callers render it into their own indentation.
func (c OCIExport) Fix() string {
	return "`docker buildx create --use --name shunt --driver docker-container`, " +
		"or turn on the containerd image store in the Docker daemon"
}

func buildxHint() string {
	if c := CheckOCIExport(); !c.OK {
		return "\n  hint: the " + c.Driver + " buildx driver cannot export an OCI layout, which is how shunt ships images" +
			"\n  " + c.Fix()
	}
	return ""
}

// DirSize totals the regular files under dir.
func DirSize(dir string) int64 {
	var n int64
	filepath.Walk(dir, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			n += fi.Size()
		}
		return nil
	})
	return n
}

// dockerArchiveEntry is one element of the manifest.json that `docker save`
// writes alongside the OCI layout.
type dockerArchiveEntry struct {
	Config   string   `json:"Config"`
	RepoTags []string `json:"RepoTags"`
	Layers   []string `json:"Layers"`
}

// ociManifest is the subset of an OCI image manifest we need.
type ociManifest struct {
	MediaType string `json:"mediaType"`
	Config    struct {
		Digest string `json:"digest"`
	} `json:"config"`
	Layers []struct {
		Digest string `json:"digest"`
	} `json:"layers"`
}

// WriteDockerArchiveManifest adds the manifest.json that a Docker daemon without
// the containerd image store needs in order to load this layout.
//
// Such a daemon only understands the docker-archive format. Handed a bare OCI
// layout it falls back to the legacy importer, treats blobs/ as a layer
// directory and fails on a missing blobs/json — which is most servers, since the
// containerd snapshotter is increasingly the default locally while hosts
// overwhelmingly still run the classic store.
//
// `docker save` solves this by emitting both formats at once: the OCI layout
// plus a manifest.json whose Config and Layers point at the very same blobs by
// path. Doing the same here costs one small JSON file and no additional
// transfer, so layer deduplication is untouched and both loaders are satisfied
// without shunt having to probe the host or care which one it is talking to.
func WriteDockerArchiveManifest(dir, ref string) error {
	_, man, err := selectImage(dir, "")
	if err != nil {
		return err
	}

	entry := dockerArchiveEntry{
		Config:   blobPath(man.Config.Digest),
		RepoTags: []string{ref},
		Layers:   make([]string, 0, len(man.Layers)),
	}
	for _, l := range man.Layers {
		entry.Layers = append(entry.Layers, blobPath(l.Digest))
	}

	out, err := json.Marshal([]dockerArchiveEntry{entry})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "manifest.json"), out, 0o644)
}

func blobPath(digest string) string {
	return filepath.Join("blobs", strings.ReplaceAll(digest, ":", string(filepath.Separator)))
}
