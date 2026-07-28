// The pull direction. rsync does not care which end is the source, so bringing
// production data back down costs one function rather than a subsystem.
package engine

import (
	"context"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/OAISP/shunt/internal/sshx"
	"github.com/OAISP/shunt/internal/transport"
)

// RemoteKind reports whether a host path is a directory and how large it is.
func (e *Engine) RemoteKind(ctx context.Context, path string) (isDir bool, size int64, err error) {
	out, err := e.Client.Run(ctx, "sh", "-c",
		"p="+sshx.Quote(path)+`; if [ -d "$p" ]; then echo -n "dir "; du -sb "$p" | cut -f1; `+
			`elif [ -f "$p" ]; then echo -n "file "; stat -c %s "$p"; else echo "missing 0"; fi`)
	if err != nil {
		return false, 0, err
	}
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) < 2 {
		return false, 0, fmt.Errorf("could not stat %s on %s", path, e.M.Host)
	}
	if fields[0] == "missing" {
		return false, 0, fmt.Errorf("%s does not exist on %s", path, e.M.Host)
	}
	fmt.Sscanf(fields[1], "%d", &size)
	return fields[0] == "dir", size, nil
}

// Fetch pulls a host path down to a local one.
func (e *Engine) Fetch(ctx context.Context, remote, local string, isDir, verbose bool) (*transport.Stats, error) {
	return transport.Fetch(ctx, transport.FileOptions{
		Client:     e.Client,
		LocalPath:  local,
		RemotePath: remote,
		Verbose:    verbose,
		RemoteZstd: e.facts.RsyncZstd,
	}, isDir)
}

// Captures lists stage capture files on the host, newest first.
//
// A pre-migration dump that cannot be retrieved is decorative, and working out
// where the helper put it is not something an operator should have to do from
// the manifest.
func (e *Engine) Captures(ctx context.Context) ([]string, error) {
	dirs := map[string]bool{}
	for _, st := range e.M.Stages {
		if st.Capture != "" {
			dirs[filepath.Dir(st.Capture)] = true
		}
	}
	if len(dirs) == 0 {
		return nil, nil
	}
	var script strings.Builder
	for _, d := range slices.Sorted(maps.Keys(dirs)) {
		fmt.Fprintf(&script, "ls -1t %s/* 2>/dev/null | head -20\n", sshx.Quote(d))
	}
	out, err := e.Client.Run(ctx, "sh", "-c", script.String())
	if err != nil {
		return nil, err
	}
	var found []string
	for _, ln := range strings.Split(strings.TrimSpace(out), "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			found = append(found, ln)
		}
	}
	return found, nil
}
