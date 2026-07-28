package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/OAISP/shunt/internal/release"
)

func cmdStatus(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: shunt-helper status <project>")
	}
	ledger, err := loadLedger(args[0])
	if err != nil {
		return err
	}
	// Specs are returned intact — `shunt plan` needs the previous service
	// definitions to diff against. They are safe to send because secret values
	// were replaced by hashes before the ledger was ever written.
	type containerInfo struct {
		Name    string `json:"name"`
		Status  string `json:"status"`
		Image   string `json:"image"`
		Release string `json:"release"`
		Service string `json:"service"`
		Kind    string `json:"kind"`
		Config  string `json:"config"`
		State   string `json:"state"`
	}
	// Service, config and state come back so `shunt plan` can compare the
	// manifest against what is actually running rather than against the ledger's
	// account of it.
	out, _ := exec.Command("docker", "ps", "-a",
		"--filter", "label=shunt.project="+args[0],
		"--format", "{{.Names}}\t{{.Status}}\t{{.Image}}\t{{.Label \"shunt.release\"}}\t"+
			"{{.Label \"shunt.service\"}}\t{{.Label \"shunt.kind\"}}\t{{.Label \"shunt.config\"}}\t{{.State}}").Output()
	var cs []containerInfo
	for _, ln := range splitLines(string(out)) {
		f := strings.Split(ln, "\t")
		if len(f) < 8 {
			continue
		}
		cs = append(cs, containerInfo{f[0], f[1], f[2], f[3], f[4], f[5], f[6], f[7]})
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"ledger": ledger, "containers": cs})
}

// cmdLogs tails every container of a project, or of one service.
//
// It used to run `docker logs` against whichever container docker listed first
// and print it unlabelled — so a project with a web service and a worker showed
// one of them, arbitrarily, with nothing to say which. During a blue/green
// overlap it could equally well be the release being retired.
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
		if err := pruneImages(project, keepImageRefs(ledger, keepIDs, nil)); err != nil {
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
func pruneImages(project string, keep map[string]bool) error {
	out, err := exec.Command("docker", "images",
		"--filter", "reference=shunt/"+project+"-*", "--format", "{{.Repository}}:{{.Tag}}").Output()
	if err != nil {
		return err
	}
	var removed int
	for _, ref := range splitLines(string(out)) {
		if ref == "" || keep[ref] || strings.HasSuffix(ref, ":<none>") {
			continue
		}
		if err := exec.Command("docker", "rmi", ref).Run(); err == nil {
			removed++
		}
	}
	if removed > 0 {
		info(fmt.Sprintf("pruned %d superseded image(s)", removed))
	}
	return nil
}

// pruneSecretDirs drops the file-mode secret directories of releases that are
// no longer restorable, on the same keep set as the env-files and the images.
//
// Gated together deliberately, for the same reason: these are the only
// plaintext copy of a release's secrets, so dropping one while its images
// survive leaves a release that looks restorable and is not.
func pruneSecretDirs(project string, keepIDs map[string]bool) {
	dir := filepath.Join(projectDir(project), "secrets")
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		// "<id>" or, for a narrowed service, "<id>.<scope>".
		id, _, _ := strings.Cut(e.Name(), ".")
		if !keepIDs[id] {
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
