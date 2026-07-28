package engine

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// Lock is a project lock held on the host for the whole of a deploy.
//
// The helper takes a flock around the part that mutates containers, which
// covers the ledger and the container swap but starts far too late: by then
// both CLIs have already rsync'd into the same store with --delete and staged
// their artifacts. Two deploys could therefore corrupt each other's layout
// before either helper ran.
//
// Holding it from the CLI closes that window without restructuring the store
// into a shared blob pool with a garbage collector — the store stays the plain
// --delete mirror it is documented to be. The lock is an ssh session running
// `flock` on a fd it keeps open; when the session ends, for any reason including
// the network dropping or the CLI being killed, the kernel releases it. There is
// no lease to expire and no stale lockfile to clean up.
type Lock struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	held  bool
}

// lockScript blocks until the lock is held, says so, and then waits.
//
// `flock` holds the lock for as long as the subshell owning fd 9 lives, so
// keeping the session open by reading stdin is what holds it. Releasing is
// simply closing stdin or dropping the connection.
const lockScript = `
mkdir -p "$(dirname "$1")" 2>/dev/null
exec 9>"$1" || exit 1
if ! flock -n 9; then
  echo "shunt-lock: waiting"
  flock 9 || exit 1
fi
echo "shunt-lock: held"
# Hold until the CLI closes stdin or the connection drops.
cat >/dev/null
`

// AcquireLock takes the project lock on the host and holds it until Release.
func (e *Engine) AcquireLock(ctx context.Context, notify func(string)) (*Lock, error) {
	path := e.lockPath()
	cmd, stdin, stdout, err := e.Client.StartSession(ctx, "sh", "-s", "--", path)
	if err != nil {
		return nil, fmt.Errorf("acquire deploy lock on %s: %w", e.M.Host, err)
	}
	if _, err := io.WriteString(stdin, lockScript); err != nil {
		cmd.Process.Kill()
		return nil, fmt.Errorf("acquire deploy lock on %s: %w", e.M.Host, err)
	}

	l := &Lock{cmd: cmd, stdin: stdin}
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		switch strings.TrimSpace(sc.Text()) {
		case "shunt-lock: waiting":
			if notify != nil {
				notify("another deploy is in progress on " + e.M.Host + " — waiting for it to finish")
			}
		case "shunt-lock: held":
			l.held = true
			// Drain anything further so the remote never blocks on a full pipe.
			go io.Copy(io.Discard, stdout)
			return l, nil
		}
	}
	l.Release()
	return nil, fmt.Errorf("could not acquire the deploy lock on %s (is flock available there?)", e.M.Host)
}

// Release drops the lock. Safe to call more than once.
func (l *Lock) Release() {
	if l == nil || l.cmd == nil {
		return
	}
	if l.stdin != nil {
		l.stdin.Close() // ends `cat`, which ends the shell, which drops the flock
	}
	done := make(chan struct{})
	go func() { l.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		// A wedged session must not keep the CLI from exiting; killing it drops
		// the lock just as effectively.
		l.cmd.Process.Kill()
		<-done
	}
	l.cmd = nil
	l.held = false
}

// Held reports whether the lock is currently held.
func (l *Lock) Held() bool { return l != nil && l.held }

// lockPath is deliberately NOT the helper's lock file.
//
// The helper flocks <project>/lock around host mutation. If the CLI held that
// same file for the whole deploy, the helper it then invokes would block
// forever waiting for a lock its own caller owns — a self-deadlock, and one
// that looks exactly like a stuck deploy.
//
// So the two locks nest instead of colliding: this one serialises whole deploys
// against each other, covering the transfer that the helper's lock starts too
// late to protect; the helper's still serialises container mutation against
// rollback, boot and prune. A rollback slipping between a deploy's transfer and
// its apply is caught by ExpectedCurrent rather than by this lock.
func (e *Engine) lockPath() string {
	return e.root + "/" + e.M.Project + "/deploy.lock"
}

// ExpectedCurrent is the release the host was serving when the plan was built.
//
// It is sent with the spec so the helper can refuse to apply a plan that was
// computed against a host somebody else has since changed. Holding the lock
// makes that rare, but the plan is built *before* the lock is taken — the build
// alone can take minutes — so the check is what makes the ordering safe rather
// than merely unlikely to matter.
func (s *RemoteState) ExpectedCurrent() string {
	if s == nil || s.Ledger == nil {
		return ""
	}
	return s.Ledger.Current
}
