package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/OAISP/shunt/internal/release"
	"github.com/OAISP/shunt/internal/ui"
)

// EventRenderer consumes the helper's NDJSON stream. The CLI has a human
// renderer and a JSON passthrough for CI.
type EventRenderer interface {
	Handle(release.Event)
	// Failed reports whether a failure event was seen. The helper exits
	// non-zero too, but reading it from the stream means the CLI can describe
	// what failed rather than just that something did.
	Failed() bool
}

// stream runs a helper command and decodes its NDJSON output. Anything that is
// not valid JSON is passed through verbatim — that is how a remote panic or a
// shell error still reaches the operator instead of being swallowed.
func (e *Engine) stream(ctx context.Context, in io.Reader, r EventRenderer, argv ...string) error {
	pr, pw := io.Pipe()
	done := make(chan struct{})

	go func() {
		defer close(done)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			var ev release.Event
			if err := json.Unmarshal([]byte(line), &ev); err != nil || ev.Kind == "" {
				fmt.Fprintln(os.Stderr, line)
				continue
			}
			r.Handle(ev)
		}
		io.Copy(io.Discard, pr)
	}()

	err := e.Client.Stream(ctx, in, pw, os.Stderr, argv...)
	pw.Close()
	<-done
	return err
}

// HumanRenderer prints a live, indented step list.
type HumanRenderer struct {
	Out     io.Writer
	Style   ui.Style
	Verbose bool

	failed bool
}

func (h *HumanRenderer) Failed() bool { return h.failed }

func (h *HumanRenderer) Handle(ev release.Event) {
	w := h.Out
	if w == nil {
		w = os.Stderr
	}
	s := h.Style
	switch ev.Kind {
	case release.KindStep:
		// Steps are the intent; the ok that follows is the outcome. Showing both
		// doubles the output for no gain unless you are debugging.
		if h.Verbose {
			fmt.Fprintf(w, "  %s\n", s.Dim("· "+ev.Message))
		}
	case release.KindOK:
		fmt.Fprintf(w, "  %s %s\n", s.Tick(), ev.Message)
	case release.KindInfo:
		fmt.Fprintf(w, "  %s\n", s.Dim(ev.Message))
	case release.KindLog:
		if h.Verbose {
			fmt.Fprintf(w, "    %s\n", s.Dim(ev.Message))
		}
	case release.KindFail:
		h.failed = true
		fmt.Fprintf(w, "  %s %s\n", s.Cross(), ui.Indent(ev.Message))
	case release.KindResult:
		fmt.Fprintf(w, "\n  %s release %s is %s\n", s.Bullet(), s.Bold(ev.Release), ev.Status)
	}
}

// JSONRenderer re-emits events unchanged for machine consumers.
type JSONRenderer struct {
	Out io.Writer

	failed bool
}

func (j *JSONRenderer) Failed() bool { return j.failed }

func (j *JSONRenderer) Handle(ev release.Event) {
	w := j.Out
	if w == nil {
		w = os.Stdout
	}
	if ev.Kind == release.KindFail {
		j.failed = true
	}
	json.NewEncoder(w).Encode(ev)
}
