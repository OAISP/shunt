// Running commands on the host: an interactive shell in a serving container, a
// one-off container from a release's image, and log streaming.
package engine

import (
	"context"
	"os"
	"path/filepath"

	"github.com/OAISP/shunt/internal/ui"
)

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
	secretsDir := filepath.Join(e.root, e.M.Project, "secrets", releaseID)
	argv := []string{"sh", "-c", oneOffScript()}
	argv = append(argv, "--", e.M.Network, envFile, secretsDir, imageRef)
	argv = append(argv, command...)
	return e.Client.Interactive(ctx, argv...)
}

// oneOffScript exists because the env-file may have been pruned, and docker
// refuses to start at all when --env-file names a missing path. Deciding that
// on the host keeps it to a single round trip.
func oneOffScript() string {
	// Whichever form the release used is what a one-off gets, decided by what is
	// actually on disk rather than by threading the mode through.
	return `net="$1"; env="$2"; sec="$3"; img="$4"; shift 4
args="--rm -i"
[ -t 0 ] && args="$args -t"
[ -n "$net" ] && args="$args --network $net"
[ -f "$env" ] && args="$args --env-file $env"
[ -d "$sec" ] && args="$args -v $sec:/run/secrets:ro"
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
