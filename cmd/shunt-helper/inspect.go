package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"

	"github.com/OAISP/shunt/internal/oci"
	"github.com/OAISP/shunt/internal/release"
)

// hostStatus is what the host reports about a project.
type hostStatus struct {
	// The ledger's specs are returned intact — `shunt plan` diffs against the
	// previous service definitions. They are safe to send because secret values
	// were replaced by hashes before the ledger was ever written.
	Ledger     *release.Ledger `json:"ledger"`
	Containers []containerInfo `json:"containers"`

	// Store and Paths are filled for a plan only; see cmdPlan.
	Store map[string][]string          `json:"store,omitempty"`
	Paths map[string]release.PathState `json:"paths,omitempty"`
	Disk  *release.Disk                `json:"disk,omitempty"`
}

// containerInfo carries service, config and state so `shunt plan` can compare
// the manifest against what is actually running rather than against the
// ledger's account of it.
type containerInfo struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Image   string `json:"image"`
	Release string `json:"release"`
	Service string `json:"service"`
	Kind    string `json:"kind"`
	Config  string `json:"config"`
	State   string `json:"state"`
	Inputs  string `json:"inputs"`
}

func cmdStatus(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: shunt-helper status <project>")
	}
	st, err := projectStatus(args[0])
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(st)
}

// cmdPlan is status plus everything else a plan needs from the host, in one
// round trip: the blobs each image's store already holds, which is the transfer
// estimate, and for every artifact destination its current size and mtime and
// whether its directory can take a new copy — checked before the build, so a
// root-owned /opt fails in a second rather than after minutes of building.
func cmdPlan(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: shunt-helper plan <project> [artifact-dest...]")
	}
	st, err := projectStatus(args[0])
	if err != nil {
		return err
	}
	st.Store = storeBlobs(filepath.Join(projectDir(args[0]), "store"))
	st.Disk = disk(projectDir(args[0]))
	st.Paths = make(map[string]release.PathState, len(args)-1)
	for _, dest := range args[1:] {
		st.Paths[dest] = statPath(dest)
	}
	return json.NewEncoder(os.Stdout).Encode(st)
}

func projectStatus(project string) (*hostStatus, error) {
	ledger, err := loadLedger(project)
	if err != nil {
		return nil, err
	}
	out, _ := docker.Run("docker", "ps", "-a",
		"--filter", "label=shunt.project="+project,
		"--format", "{{.Names}}\t{{.Status}}\t{{.Image}}\t{{.Label \"shunt.release\"}}\t"+
			"{{.Label \"shunt.service\"}}\t{{.Label \"shunt.kind\"}}\t{{.Label \"shunt.config\"}}\t{{.State}}\t"+
			"{{.Label \"shunt.inputs\"}}")
	st := &hostStatus{Ledger: ledger}
	for _, ln := range splitLines(out) {
		f := strings.Split(ln, "\t")
		if len(f) < 9 {
			continue
		}
		st.Containers = append(st.Containers, containerInfo{f[0], f[1], f[2], f[3], f[4], f[5], f[6], f[7], f[8]})
	}
	return st, nil
}

// storeBlobs lists the blobs each image's layout in the store holds.
func storeBlobs(store string) map[string][]string {
	out := map[string][]string{}
	ents, _ := os.ReadDir(store)
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		blobs, err := oci.Blobs(filepath.Join(store, e.Name()))
		if err != nil {
			continue
		}
		out[e.Name()] = slices.Sorted(maps.Keys(blobs))
	}
	return out
}

// disk reports free space for the store and for the daemon's data root. Either
// figure is 0 when it cannot be read, which the CLI takes as "do not check".
func disk(store string) *release.Disk {
	d := &release.Disk{}
	storeDev, storeFree := fsFree(existingParent(store))
	d.StoreFree = storeFree
	root, err := docker.Run("docker", "info", "--format", "{{.DockerRootDir}}")
	if root = strings.TrimSpace(root); err == nil && root != "" {
		dev, free := fsFree(root)
		d.DockerFree = free
		d.Shared = free > 0 && dev == storeDev
	}
	return d
}

func fsFree(path string) (dev uint64, free int64) {
	var fs syscall.Statfs_t
	var st syscall.Stat_t
	if syscall.Statfs(path, &fs) != nil || syscall.Stat(path, &st) != nil {
		return 0, 0
	}
	return uint64(st.Dev), int64(fs.Bavail) * int64(fs.Bsize)
}

// existingParent walks up to the nearest directory that exists, so a project
// that has never deployed still measures the filesystem it will land on.
func existingParent(p string) string {
	for {
		if _, err := os.Stat(p); err == nil || p == filepath.Dir(p) {
			return p
		}
		p = filepath.Dir(p)
	}
}

// statPath summarises an artifact destination and checks its directory can
// receive the staged copy that lands beside it. It only looks: a plan must not
// change the host, so a missing directory is created by the transfer instead.
func statPath(dest string) release.PathState {
	st := release.PathState{Size: -1}
	if fi, err := os.Stat(dest); err == nil {
		st.Size, st.MTime = fi.Size(), fi.ModTime().Unix()
		if fi.IsDir() {
			st.Size, _, st.MTime = release.TreeSummary(dest)
		}
	}
	dir := filepath.Dir(dest)
	switch parent := existingParent(dir); {
	case syscall.Access(parent, 2 /* W_OK */) == nil:
	case parent == dir:
		st.Problem = dir + " is not writable"
	default:
		st.Problem = dir + " could not be created"
	}
	return st
}

// cmdLogs tails every container of a project, or of one service. Every one,
// prefixed: a project with a web service and a worker has two, and during a
// blue/green overlap one of them is the release being retired.
func cmdLogs(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: shunt-helper logs <project> [service] [--follow] [--tail N]")
	}
	project := args[0]

	filters := []string{"--filter", "label=shunt.project=" + project}
	if len(args) > 1 && !strings.HasPrefix(args[1], "-") {
		filters = append(filters, "--filter", "label=shunt.service="+args[1])
	}
	psArgs := append([]string{"ps"}, filters...)
	psArgs = append(psArgs, "--format", "{{.Names}}")
	out, err := exec.Command("docker", psArgs...).Output()
	if err != nil {
		return err
	}
	names := splitLines(string(out))
	if len(names) == 0 {
		return fmt.Errorf("no running containers for project %s", project)
	}
	sort.Strings(names) // stable order, so two runs are comparable

	tail := "100"
	follow := false
	for i, a := range args {
		if a == "--follow" || a == "-f" {
			follow = true
		}
		if a == "--tail" && i+1 < len(args) {
			tail = args[i+1]
		}
	}

	// One container needs no prefix; more than one does, or the output is an
	// unattributable interleaving.
	if len(names) == 1 {
		return streamLogs(names[0], tail, follow, "")
	}
	if !follow {
		for _, n := range names {
			streamLogs(n, tail, false, n+" | ")
		}
		return nil
	}

	var wg sync.WaitGroup
	for _, n := range names {
		wg.Add(1)
		go func(n string) {
			defer wg.Done()
			streamLogs(n, tail, true, n+" | ")
		}(n)
	}
	wg.Wait()
	return nil
}

// streamLogs pipes one container's logs out, optionally prefixed with its name.
func streamLogs(container, tail string, follow bool, prefix string) error {
	dargs := []string{"logs", "--tail", tail}
	if follow {
		dargs = append(dargs, "--follow")
	}
	dargs = append(dargs, container)
	cmd := exec.Command("docker", dargs...)
	if prefix == "" {
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		return cmd.Run()
	}
	cmd.Stdout = &prefixWriter{w: os.Stdout, prefix: prefix}
	cmd.Stderr = &prefixWriter{w: os.Stderr, prefix: prefix}
	return cmd.Run()
}

// prefixWriter tags each line with its container name. Writes are serialised
// globally so two containers logging at once cannot interleave mid-line.
type prefixWriter struct {
	w      io.Writer
	prefix string
	buf    []byte
}

var logMu sync.Mutex

func (p *prefixWriter) Write(b []byte) (int, error) {
	logMu.Lock()
	defer logMu.Unlock()
	p.buf = append(p.buf, b...)
	for {
		i := bytes.IndexByte(p.buf, '\n')
		if i < 0 {
			break
		}
		line := p.buf[:i+1]
		p.buf = p.buf[i+1:]
		if _, err := io.WriteString(p.w, p.prefix); err != nil {
			return 0, err
		}
		if _, err := p.w.Write(line); err != nil {
			return 0, err
		}
	}
	return len(b), nil
}

func cmdPrune(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: shunt-helper prune <project>")
	}
	project := args[0]
	return withLock(project, func() error {
		ledger, err := loadLedger(project)
		if err != nil {
			return err
		}
		retain := retainFor(ledger, nil)
		keepIDs := ledger.KeepIDs(retain)
		if err := pruneImages(project, ledger, nil, keepImageRefs(ledger, keepIDs, nil)); err != nil {
			return err
		}
		pruneEnvFiles(project, keepIDs)
		pruneSecretDirs(project, keepIDs)
		return nil
	})
}

// retainFor resolves how many restorable releases to keep: what the release
// being applied asks for, else what the active one asked for, else the default.
func retainFor(ledger *release.Ledger, current *release.Spec) int {
	if current != nil && current.Retain > 0 {
		return current.Retain
	}
	if cur := ledger.Find(ledger.Current); cur != nil && cur.Spec != nil && cur.Spec.Retain > 0 {
		return cur.Spec.Retain
	}
	return release.DefaultRetain
}

// keepImageRefs collects every image ref that must survive pruning.
//
// current is the release being applied, which is not in the ledger yet — its
// entry is only appended once the outcome is known — so its images have to be
// added explicitly or a deploy would prune the very images it just loaded.
func keepImageRefs(ledger *release.Ledger, keepIDs map[string]bool, current *release.Spec) map[string]bool {
	refs := map[string]bool{}
	for i := range ledger.Releases {
		if !keepIDs[ledger.Releases[i].ID] {
			continue
		}
		for _, img := range ledger.Releases[i].Images {
			refs[img.Ref] = true
		}
	}
	if current != nil {
		for _, img := range current.Images {
			refs[img.Ref] = true
		}
	}
	return refs
}

// pruneImages removes release-tagged images that no retained release
// references. Keeping the last N is what makes rollback instant — it is a
// deliberate disk-for-recovery-time trade, not an oversight.
func pruneImages(project string, ledger *release.Ledger, current *release.Spec, keep map[string]bool) error {
	refs, err := projectImages(project, ledger, current)
	if err != nil {
		return err
	}
	var removed int
	for _, ref := range refs {
		if keep[ref] {
			continue
		}
		if docker.Ok("docker", "rmi", ref) {
			removed++
		}
	}
	if removed > 0 {
		info(fmt.Sprintf("pruned %d superseded image(s)", removed))
	}
	return nil
}

// projectImages lists the release-tagged images on the host that belong to
// this project.
//
// The `shunt/<project>-*` glob alone is not ownership: it also matches
// `shunt/<project>-staging-app`, which is the default name of a staging target
// sharing the host. So the glob only narrows the listing, and a ref counts as
// ours only if its repository is one this project's own releases were tagged
// into.
func projectImages(project string, ledger *release.Ledger, current *release.Spec) ([]string, error) {
	repos := map[string]bool{}
	addRepos := func(imgs map[string]release.ImageRef) {
		for _, img := range imgs {
			if repo, _, ok := strings.Cut(img.Ref, ":"); ok && !img.External && strings.HasPrefix(repo, "shunt/") {
				repos[repo] = true
			}
		}
	}
	for i := range ledger.Releases {
		addRepos(ledger.Releases[i].Images)
	}
	if current != nil {
		addRepos(current.Images)
	}

	out, err := docker.Run("docker", "images",
		"--filter", "reference=shunt/"+project+"-*", "--format", "{{.Repository}}:{{.Tag}}")
	if err != nil {
		return nil, fmt.Errorf("list images: %s", strings.TrimSpace(out))
	}
	var refs []string
	for _, ref := range splitLines(out) {
		repo, tag, _ := strings.Cut(ref, ":")
		if repos[repo] && tag != "<none>" {
			refs = append(refs, ref)
		}
	}
	return refs, nil
}

// pruneSecretDirs drops the file-mode secret directories of releases that are
// no longer restorable, on the same keep set as the env-files and the images.
//
// Gated together deliberately, for the same reason: these are the only
// plaintext copy of a release's secrets, so dropping one while its images
// survive leaves a release that looks restorable and is not.
//
// A directory some container still mounts is kept whatever the ledger says: an
// accessory is never recreated, so it goes on mounting the directory of the
// release that booted it long after that release has left the keep set.
func pruneSecretDirs(project string, keepIDs map[string]bool) {
	dir := filepath.Join(projectDir(project), "secrets")
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	out, err := docker.Run("docker", "ps", "-a", "--filter", "label=shunt.project="+project,
		"--format", "{{.Label \"shunt.release\"}}")
	if err != nil {
		return // not knowing what is mounted is no time to delete
	}
	mounted := splitLines(out)
	for _, e := range ents {
		// "<id>" or, for a narrowed service, "<id>.<scope>".
		id, _, _ := strings.Cut(e.Name(), ".")
		if !keepIDs[id] && !slices.Contains(mounted, id) {
			os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
}

// pruneEnvFiles drops the env-files of releases that are no longer restorable.
//
// It is gated on the same keep set as the images deliberately: an env-file is
// the only plaintext copy of a release's secrets, so dropping one that still has
// images makes that release un-rollbackable in a way nothing reports until you
// try. The two have to expire together or not at all.
func pruneEnvFiles(project string, keepIDs map[string]bool) {
	dir := filepath.Join(projectDir(project), "env")
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		// A file is either "<id>.env" or, for a service that narrowed its
		// secrets, "<id>.<scope>.env". The release id is the leading segment in
		// both, and keying on anything else deletes every scoped file on sight.
		id, _, _ := strings.Cut(strings.TrimSuffix(e.Name(), ".env"), ".")
		if !keepIDs[id] {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
