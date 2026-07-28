package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

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
	}
	out, _ := exec.Command("docker", "ps", "-a",
		"--filter", "label=shunt.project="+args[0],
		"--format", "{{.Names}}\t{{.Status}}\t{{.Image}}\t{{.Label \"shunt.release\"}}").Output()
	var cs []containerInfo
	for _, ln := range splitLines(string(out)) {
		f := strings.Split(ln, "\t")
		if len(f) < 4 {
			continue
		}
		cs = append(cs, containerInfo{f[0], f[1], f[2], f[3]})
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"ledger": ledger, "containers": cs})
}

func cmdLogs(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: shunt-helper logs <project> [service] [--follow] [--tail N]")
	}
	project := args[0]
	filter := "label=shunt.project=" + project
	if len(args) > 1 && !strings.HasPrefix(args[1], "-") {
		filter = "label=shunt.service=" + args[1]
	}
	out, err := exec.Command("docker", "ps", "-q", "--filter", filter,
		"--filter", "label=shunt.project="+project).Output()
	if err != nil {
		return err
	}
	ids := splitLines(string(out))
	if len(ids) == 0 {
		return fmt.Errorf("no running containers for project %s", project)
	}
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
	dargs := []string{"logs", "--tail", tail}
	if follow {
		dargs = append(dargs, "--follow")
	}
	dargs = append(dargs, ids[0])
	cmd := exec.Command("docker", dargs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
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
		if id := strings.TrimSuffix(e.Name(), ".env"); !keepIDs[id] {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
