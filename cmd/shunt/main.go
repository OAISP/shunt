// Command shunt deploys a locally-built Docker image to a single remote host
// over ssh, without a registry.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/OAISP/shunt/internal/engine"
	"github.com/OAISP/shunt/internal/ui"
)

// version is stamped at build time by the release workflow and the Makefile:
//
//	-ldflags "-X main.version=1.2.3"
//
// A `go install` build gets no ldflags, so it falls back to the module version
// the toolchain recorded — otherwise every installed binary would report
// whatever number happened to be hardcoded here when the tag was cut.
var version = ""

func init() {
	if version != "" {
		return
	}
	version = "dev"
	bi, ok := debug.ReadBuildInfo()
	if !ok || bi.Main.Version == "" || bi.Main.Version == "(devel)" {
		return
	}
	// Either a tag (v0.1.0) or a pseudo-version identifying the exact commit,
	// which is precisely what a bug report needs.
	version = strings.TrimPrefix(bi.Main.Version, "v")
}

const usage = `usage:
  shunt init                 scaffold a shunt.toml for this project
  shunt validate             check shunt.toml without touching the network
  shunt audit                check everything a deploy needs, and change nothing
  shunt plan                 build, then show what a deploy would change
  shunt up                   build, ship, run stages, swap containers, health-check
  shunt status               what the host is running right now
  shunt exec <service> ...   run a command in the running container
  shunt run <service> ...    run a one-off command in a fresh container
  shunt rollback [release]   restore the previous (or a named) release
  shunt boot <accessory>     (re)create a stateful accessory — destructive
  shunt retire <service>     stop a service you removed from shunt.toml
  shunt fetch [name|path]    pull an artifact or capture back down
  shunt logs [service]       tail logs from the host
  shunt prune                drop superseded images on the host
  shunt version

common flags:
  -f, --file <path>   path to shunt.toml (default: nearest one up the tree)
  -v, --verbose       show build output and per-step detail
      --json          emit machine-readable events instead of prose

Set SHUNT_NO_BANNER=1 to suppress the banner, NO_COLOR=1 to suppress colour.
`

// commands is the dispatch table. Keeping it as data means a command listed in
// the usage text above and one main actually routes cannot drift apart
// unnoticed — there is a test that checks they match.
var commands = map[string]func(context.Context, []string) error{
	// init needs no host, but it lives in the table anyway: a command handled by
	// its own switch case can fall through into "unknown command" if the case
	// forgets to return, which is exactly the bug this shape makes impossible.
	"init": func(_ context.Context, args []string) error { return cmdInit(args, os.Stdout) },

	// validate needs no host either, for the same reason as init.
	"validate": cmdValidate,

	"audit":    cmdAudit,
	"plan":     cmdPlan,
	"up":       cmdUp,
	"deploy":   cmdUp, // alias
	"status":   cmdStatus,
	"exec":     cmdExec,
	"run":      cmdRun,
	"rollback": cmdRollback,
	"boot":     cmdBoot,
	"retire":   cmdRetire,
	"fetch":    cmdFetch,
	"logs":     cmdLogs,
	"prune":    cmdPrune,
}

func main() {
	// Bare `shunt` is someone asking what this is, so greet them properly. Exit
	// non-zero all the same: no command ran.
	if len(os.Args) < 2 {
		help(os.Stderr)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	name, args := os.Args[1], os.Args[2:]
	switch name {
	case "version", "--version", "-V":
		printVersion()
		return
	case "help", "--help", "-h":
		help(os.Stdout)
		return
	}

	cmd, ok := commands[name]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", name)
		help(os.Stderr)
		os.Exit(2)
	}
	exit(cmd(ctx, args))
}

// exit reports an error and terminates. errReported means the detail has already
// been shown, so it exits quietly rather than saying the same thing twice.
func exit(err error) {
	if err == nil {
		return
	}
	if errors.Is(err, errReported) {
		os.Exit(1)
	}
	s := ui.NewStyle(os.Stderr)
	fmt.Fprintf(os.Stderr, "\n%s %v\n", s.Red("error:"), err)
	os.Exit(1)
}

func printVersion() {
	if ui.BannerAllowed(os.Stdout, false) {
		ui.Banner(os.Stdout, ui.NewStyle(os.Stdout), version)
		fmt.Printf("helper %s · protocol %d\n", engine.HelperVersion, protocolVersion)
		return
	}
	// Piped or scripted: emit one parseable line instead of artwork.
	fmt.Printf("shunt %s (helper %s, protocol %d)\n", version, engine.HelperVersion, protocolVersion)
}

// help prints the banner, when the destination is a terminal, then usage.
func help(w *os.File) {
	if ui.BannerAllowed(w, false) {
		ui.Banner(w, ui.NewStyle(w), version)
		fmt.Fprintln(w)
	}
	fmt.Fprint(w, usage)
}
