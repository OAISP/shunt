package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"time"

	"github.com/OAISP/shunt/internal/release"
)

// Deploy is one deploy session on the host. The helper takes the project lock
// when the session opens and holds it until the release is applied, so the
// transfer in between is covered — and because the lock belongs to the helper
// rather than to this process, a CLI that dies after handing the spec over
// leaves the helper to finish the deploy under it, not a half-swapped host.
type Deploy struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	events *events
	done   bool
	// stopBeat ends the heartbeat and waits for it, so it cannot write into the
	// middle of the spec.
	stopBeat func()

	// Serving is the release the host was serving when the lock was taken.
	Serving string
}

// BeginDeploy opens a deploy session and waits until the helper holds the lock.
func (e *Engine) BeginDeploy(ctx context.Context, notify func(string)) (*Deploy, error) {
	cmd, stdin, stdout, err := e.Client.StartSession(ctx, e.helperPath, "apply", e.M.Project)
	if err != nil {
		return nil, fmt.Errorf("open deploy session on %s: %w", e.M.Host, err)
	}
	d := &Deploy{cmd: cmd, stdin: stdin, events: newEvents(stdout)}
	d.stopBeat = heartbeat(stdin)
	for {
		ev, ok := d.events.next()
		if !ok {
			d.Close()
			return nil, fmt.Errorf("the helper on %s exited before taking the deploy lock", e.M.Host)
		}
		switch ev.Kind {
		case release.KindHeld:
			d.Serving = ev.Release
			return d, nil
		case release.KindFail:
			d.Close()
			return nil, errors.New(ev.Message)
		case release.KindInfo:
			if notify != nil {
				notify(ev.Message)
			}
		}
	}
}

// heartbeat writes a newline to the helper every interval — whitespace its JSON
// decoder skips — until the returned function is called.
func heartbeat(w io.Writer) (stop func()) {
	quit, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(release.HeartbeatInterval)
		defer t.Stop()
		for {
			select {
			case <-quit:
				return
			case <-t.C:
				w.Write([]byte("\n"))
			}
		}
	}()
	return func() { close(quit); <-done }
}

// Apply hands the spec to the helper and renders the deploy as it happens.
func (d *Deploy) Apply(ctx context.Context, spec *release.Spec, r EventRenderer) error {
	b, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	// Cancelled before the hand-over: nothing has been sent, so nothing changes.
	if err := ctx.Err(); err != nil {
		d.Close()
		return err
	}
	d.done = true
	d.stopBeat()
	_, werr := d.stdin.Write(b)
	d.stdin.Close()
	for ev, ok := d.events.next(); ok; ev, ok = d.events.next() {
		r.Handle(ev)
	}
	err = d.cmd.Wait()
	if ctx.Err() != nil && werr == nil {
		return errors.New("interrupted, but the release had already been handed over — the host finishes it " +
			"under the lock; `shunt status` shows how it ended")
	}
	return errors.Join(werr, err)
}

// Close ends a session that never reached Apply, releasing the lock without
// changing anything. Safe to call more than once.
func (d *Deploy) Close() {
	if d == nil || d.done {
		return
	}
	d.done = true
	d.stopBeat()
	d.stdin.Close()
	d.events.drain()
	d.cmd.Wait()
}

// ExpectedCurrent is the release the host was serving when the plan was built.
func (s *RemoteState) ExpectedCurrent() string {
	if s == nil || s.Ledger == nil {
		return ""
	}
	return s.Ledger.Current
}
