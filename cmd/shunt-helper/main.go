// Command shunt-helper runs on the deploy host. The CLI uploads it under a name
// derived from its own content hash — so a rebuilt helper is always re-uploaded
// and an unchanged one never is — and then drives it over ssh.
//
// It exists so the host-side logic is real, testable code instead of a bash
// heredoc: it gets structured errors, a lock, an on-disk ledger, and it reports
// progress as NDJSON events rather than interleaved shell output.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/OAISP/shunt/internal/release"
)

// Version is informational only. What identifies this binary on the host is its
// content hash, not this string; see internal/engine/helperbin.go.
const Version = "0.1.0"

func main() {
	if len(os.Args) < 2 {
		fatal(errors.New("usage: shunt-helper <apply|rollback|boot|retire|status|logs|prune|version>"))
	}
	var err error
	switch os.Args[1] {
	case "version":
		fmt.Println(Version)
		return
	case "apply":
		err = cmdApply(os.Stdin)
	case "rollback":
		err = cmdRollback(os.Args[2:])
	case "status":
		err = cmdStatus(os.Args[2:])
	case "logs":
		err = cmdLogs(os.Args[2:])
	case "prune":
		err = cmdPrune(os.Args[2:])
	case "boot":
		err = cmdBoot(os.Stdin, os.Args[2:])
	case "retire":
		err = cmdRetire(os.Args[2:])
	default:
		err = fmt.Errorf("unknown subcommand %q", os.Args[1])
	}
	if err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	emit(release.Event{Kind: release.KindFail, Message: err.Error()})
	os.Exit(1)
}

func root() string {
	if v := os.Getenv("SHUNT_ROOT"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "/var/lib/shunt"
	}
	return filepath.Join(home, ".shunt")
}

func projectDir(project string) string { return filepath.Join(root(), project) }

func ledgerPath(project string) string { return filepath.Join(projectDir(project), "releases.json") }

func envFilePath(project, id string) string {
	return filepath.Join(projectDir(project), "env", id+".env")
}

var enc = json.NewEncoder(os.Stdout)

func emit(e release.Event) {
	enc.Encode(e)
	os.Stdout.Sync()
}

func step(name, msg string) { emit(release.Event{Kind: release.KindStep, Step: name, Message: msg}) }

func ok(name, msg string) { emit(release.Event{Kind: release.KindOK, Step: name, Message: msg}) }

func info(msg string) { emit(release.Event{Kind: release.KindInfo, Message: msg}) }

func logline(s string) { emit(release.Event{Kind: release.KindLog, Message: s}) }

// withLock serialises everything that mutates a project. Two engineers deploying
// at the same moment would otherwise interleave container swaps and corrupt the
// ledger; the second one waits instead.
func withLock(project string, fn func() error) error {
	dir := projectDir(project)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	lf, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer lf.Close()

	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		info("another shunt operation is in progress on this host — waiting for the lock")
		if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
			return fmt.Errorf("acquire lock: %w", err)
		}
	}
	defer syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
	return fn()
}
