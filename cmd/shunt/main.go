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

const usage = `usage: shunt <command> [flags]

set up
  shunt init --host <user@host>  write shunt.toml, guessing the port
  shunt validate                 check shunt.toml offline: no ssh, no build
  shunt audit                    check everything a deploy needs, read-only

deploy
  shunt plan                     build, then show what a deploy would change
                                 --detailed-exitcode  exit 2 on changes
  shunt up                       build, ship, run stages, swap, health-check
                                 --rollback-on-failure --no-cache --no-plan -y
  shunt rollback [release]       restore the previous release, or a named one

inspect and reach
  shunt status                   what is running, and the release history
  shunt logs [service]           tail logs (--follow, --tail N)
  shunt exec <service> -- <cmd>  run a command in the serving container
  shunt run <service> -- <cmd>   run a one-off command in a fresh container
  shunt fetch [name|path]        pull an artifact or capture back (-o <path>)

lifecycle
  shunt boot <accessory>         recreate an accessory (destructive)
  shunt retire <service>         remove a service you deleted from shunt.toml
  shunt prune                    drop images and secrets beyond retain
  shunt down                     stop this project's services
                                 --all  also its accessories
                                 --purge  also network, images, history, secrets

bundles
  shunt bundle [-o <file>]       build a release into a portable file
  shunt bundle inspect <file>    show what a bundle contains
  shunt bundle verify <file>     rehash a bundle's blobs; needs no host
  shunt apply <file>             deploy a bundle (-t <target> to promote it)
                                 --plan  show what applying would change
  shunt version

flags for every command
  -f, --file <path>     shunt.toml to use (default: nearest one up the tree)
  -t, --target <name>   deploy to a target from [targets.*]
  -v, --verbose         build output and per-step detail
      --json            machine-readable output

exit codes: 0 ok, 1 failed, 64 bad usage, 2 changes (plan --detailed-exitcode)

environment: SHUNT_BUILD_CACHE=gha|<dir> keeps the build cache between CI
jobs; SHUNT_NO_BANNER=1 and NO_COLOR=1 quiet the output.

docs: https://oaisp.github.io/shunt/
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
	"down":     cmdDown,
	"bundle":   cmdBundle,
	"apply":    cmdApply,
	"fetch":    cmdFetch,
	"logs":     cmdLogs,
	"prune":    cmdPrune,
}

func main() {
	// Bare `shunt` is someone asking what this is, so greet them properly. Exit
	// non-zero all the same: no command ran.
	if len(os.Args) < 2 {
		help(os.Stderr)
		os.Exit(exitUsage)
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
		os.Exit(exitUsage)
	}
	exit(cmd(ctx, args))
}

// exit reports an error and terminates. errReported means the detail has already
// been shown, so it exits quietly rather than saying the same thing twice.
func exit(err error) {
	if err == nil {
		return
	}
	var usage usageError
	switch {
	case errors.Is(err, errHelp):
		os.Exit(0)
	case errors.Is(err, errExitChanges):
		os.Exit(exitChanges)
	case errors.Is(err, errReported):
		os.Exit(exitFailed)
	case errors.As(err, &usage):
		fmt.Fprintf(os.Stderr, "%s %v\n", ui.NewStyle(os.Stderr).Red("error:"), err)
		os.Exit(exitUsage)
	}
	s := ui.NewStyle(os.Stderr)
	fmt.Fprintf(os.Stderr, "\n%s %v\n", s.Red("error:"), err)
	os.Exit(exitFailed)
}

func printVersion() {
	if ui.BannerAllowed(os.Stdout, false) {
		ui.Banner(os.Stdout, ui.NewStyle(os.Stdout), version)
		fmt.Printf("protocol %d\n", protocolVersion)
		return
	}
	// Piped or scripted: emit one parseable line instead of artwork.
	fmt.Printf("shunt %s (protocol %d)\n", version, protocolVersion)
}

// help prints the banner, when the destination is a terminal, then usage.
func help(w *os.File) {
	if ui.BannerAllowed(w, false) {
		ui.Banner(w, ui.NewStyle(w), version)
		fmt.Fprintln(w)
	}
	fmt.Fprint(w, usage)
}
