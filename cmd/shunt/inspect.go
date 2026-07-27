package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/OAISP/shunt/internal/engine"
	"github.com/OAISP/shunt/internal/release"
	"github.com/OAISP/shunt/internal/ui"
)

// historyLimit is how many past releases `shunt status` lists. Enough to find
// the one you want to roll back to, short enough to read at a glance.
const historyLimit = 8

func cmdStatus(ctx context.Context, args []string) error {
	var c commonFlags
	fs := newFlagSet("status", &c)
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	e, err := connect(ctx, c.file)
	if err != nil {
		return err
	}
	defer e.Close()

	st, err := e.State(ctx)
	if err != nil {
		return err
	}
	if c.asJSON {
		return json.NewEncoder(os.Stdout).Encode(st)
	}

	s := c.out()
	fmt.Printf("\n%s on %s\n", s.Bold(e.M.Project), e.M.Host)
	if st.Ledger == nil || st.Ledger.Current == "" {
		fmt.Println("\n  nothing deployed yet — run `shunt up`")
		return nil
	}

	printCurrent(st.Ledger, s)
	printContainers(st, s)
	printHistory(st.Ledger, s)
	fmt.Println()
	return nil
}

func printCurrent(l *release.Ledger, s ui.Style) {
	fmt.Printf("\n  release  %s", s.Bold(l.Current))
	cur := l.Find(l.Current)
	if cur == nil {
		fmt.Println()
		return
	}
	mark := s.Bullet()
	if cur.Status != release.StatusActive {
		mark = s.Red("●")
	}
	fmt.Printf("  %s %s  %s\n", mark, cur.Status,
		s.Dim(cur.FinishedAt.Local().Format("2006-01-02 15:04:05")))
	if cur.Error != "" {
		fmt.Printf("  %s\n", s.Red(ui.FirstLine(cur.Error)))
	}
}

func printContainers(st *engine.RemoteState, s ui.Style) {
	if len(st.Containers) == 0 {
		return
	}
	fmt.Printf("\n  %s\n", s.Dim(fmt.Sprintf("%-30s %-28s %s", "CONTAINER", "STATUS", "RELEASE")))
	for _, ct := range st.Containers {
		fmt.Printf("  %-30s %-28s %s\n", ui.Truncate(ct.Name, 30), ui.Truncate(ct.Status, 28), ct.Release)
	}
}

func printHistory(l *release.Ledger, s ui.Style) {
	fmt.Printf("\n  %s\n", s.Dim("history"))
	shown := 0
	for i := len(l.Releases) - 1; i >= 0 && shown < historyLimit; i-- {
		r := l.Releases[i]
		mark := " "
		if r.ID == l.Current {
			mark = "*"
		}
		line := fmt.Sprintf("%s %-24s %-11s %s", mark, r.ID, r.Status,
			r.StartedAt.Local().Format("2006-01-02 15:04"))
		if r.Status == release.StatusFailed {
			line = s.Dim(line)
		}
		fmt.Printf("  %s\n", line)
		shown++
	}
}

func cmdLogs(ctx context.Context, args []string) error {
	var c commonFlags
	var follow bool
	var tail string
	fs := newFlagSet("logs", &c)
	// -f is already the manifest flag, so following is spelled out in full here.
	fs.BoolVar(&follow, "follow", false, "follow log output")
	fs.StringVar(&tail, "tail", "100", "number of lines to show")
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	e, err := connect(ctx, c.file)
	if err != nil {
		return err
	}
	defer e.Close()
	return e.Logs(ctx, fs.Arg(0), follow, tail)
}

func cmdPrune(ctx context.Context, args []string) error {
	var c commonFlags
	fs := newFlagSet("prune", &c)
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	e, err := connect(ctx, c.file)
	if err != nil {
		return err
	}
	defer e.Close()
	return e.Prune(ctx, c.renderer())
}
