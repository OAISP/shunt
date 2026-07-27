package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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
		return pruneImages(project, ledger, nil)
	})
}

// pruneImages removes release-tagged images that no retained ledger entry
// references. Keeping the last N is what makes rollback instant — it is a
// deliberate disk-for-recovery-time trade, not an oversight.
func pruneImages(project string, ledger *release.Ledger, current *release.Spec) error {
	keep := map[string]bool{}
	retain := 5
	if current != nil && current.Retain > 0 {
		retain = current.Retain
	}
	seen := 0
	for i := len(ledger.Releases) - 1; i >= 0 && seen < retain; i-- {
		for _, img := range ledger.Releases[i].Images {
			keep[img.Ref] = true
		}
		seen++
	}
	if current != nil {
		for _, img := range current.Images {
			keep[img.Ref] = true
		}
	}

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

func pruneEnvFiles(project string, retain int) {
	if retain <= 0 {
		retain = 5
	}
	dir := filepath.Join(projectDir(project), "env")
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names))) // ids sort lexically by time
	for i := retain; i < len(names); i++ {
		os.Remove(filepath.Join(dir, names[i]))
	}
}
