// Package transport ships an OCI layout to the host over the existing ssh
// connection.
//
// The whole trick: blob filenames are content hashes, so rsync's "skip files
// that already match" behaviour *is* layer deduplication. --delete keeps the
// remote store an exact mirror of the latest build, which means the store never
// grows without bound and needs no separate garbage collector. Rollback does not
// depend on the store — it re-uses the release-tagged images docker already
// holds.
package transport

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/OAISP/shunt/internal/sshx"
)

// VerifiedSidecar is the host-side record of which blobs have already been
// integrity-checked. It lives inside the layout but is never transferred, so
// both the mirror and the helper have to agree on the name.
const VerifiedSidecar = ".shunt-verified.json*"

type Stats struct {
	Sent     int64 // bytes this end put on the wire
	Received int64 // bytes this end took off the wire
	Total    int64 // logical size of the transfer
	Matched  int64 // bytes the far end already had
	Literal  int64 // bytes that had to be transferred
}

// Moved is the payload in whichever direction this transfer ran. For a push
// that is what was sent; for a fetch, sent is just protocol chatter and the
// payload arrived the other way.
func (s Stats) Moved(pull bool) int64 {
	if pull {
		return s.Received
	}
	return s.Sent
}

// DedupPercent is the share of the transfer rsync did not have to send because
// the host already had it — the number that justifies the whole design.
//
// Counted from what was sent literally, not from "Matched data": that counts
// only delta matches inside files rsync transferred, and an unchanged blob is
// skipped outright by the quick check, so it appears in neither.
func (s Stats) DedupPercent() float64 {
	if s.Total == 0 {
		return 0
	}
	pct := float64(s.Total-s.Literal) / float64(s.Total) * 100
	// A flat 100% reads as "nothing sent" while bytes did cross the wire.
	if pct > 99.9 && s.Literal > 0 {
		return 99.9
	}
	return pct
}

type Options struct {
	Client    *sshx.Client
	LocalDir  string // a directory of OCI layouts, one per image
	RemoteDir string
	Verbose   bool
}

// localZstd reports whether the rsync on this machine understands zstd.
// Computed once: it costs a subprocess, and it cannot change mid-run.
var localZstd = sync.OnceValue(func() bool {
	out, err := exec.Command("rsync", "--version").Output()
	return err == nil && strings.Contains(strings.ToLower(string(out)), "zstd")
})

// compressArgs picks a compression mode both ends actually support.
//
// --compress-choice only exists from rsync 3.2, and plenty of live hosts are
// older: Ubuntu 20.04 ships 3.1.3, and macOS ships 2.6.9 as /usr/bin/rsync.
// Sending the flag to either end fails the transfer outright, so fall back to
// plain -z, which every rsync in circulation understands.
func compressArgs(remoteZstd bool) []string {
	if localZstd() && remoteZstd {
		return []string{"--compress", "--compress-choice=zstd", "--compress-level=1"}
	}
	return []string{"--compress"}
}

// Push mirrors a directory of OCI layouts into the host's store in a single
// transfer, and reports what crossed the wire.
//
// Uncompressed: layer blobs are already gzip or zstd inside, and rsync cannot
// tell, since a blob's filename is a bare digest with no extension its
// skip-compress list would recognise. Compressing them again burns CPU on both
// ends — badly so with the zlib fallback older rsyncs get — to save nothing;
// the JSON alongside is a few KB.
func Push(ctx context.Context, o Options) (*Stats, error) {
	if _, err := os.Stat(o.LocalDir); err != nil {
		return nil, fmt.Errorf("local layouts %s: %w", o.LocalDir, err)
	}
	remote := strings.TrimSuffix(o.RemoteDir, "/")
	args := []string{
		"-a",
		"--stats",
		"--delete",
		"--partial",
		// Deliberately NOT --human-readable: that abbreviates the stats to
		// "85.38M" and the byte counts below would be parsed wrong.
		//
		// The verification sidecar is written by the host and has no local
		// counterpart, so --delete would remove it on every push and the host
		// would rehash the whole image every time. Excluding it also protects it
		// from deletion, since --delete skips excluded files.
		"--exclude", VerifiedSidecar,
		// Creates the store on first use without a round trip of its own.
		"--rsync-path", "mkdir -p " + sshx.Quote(remote) + " && rsync",
		"-e", o.Client.SSHCommand(),
		strings.TrimSuffix(o.LocalDir, "/") + "/",
		o.Client.Host + ":" + remote + "/",
	}
	return runRsync(ctx, args, o.Verbose, func(err error) error {
		return fmt.Errorf("rsync to %s: %w", o.Client.Host, err)
	})
}

var numRE = regexp.MustCompile(`([0-9][0-9,\.]*)`)

func parseStat(line string, st *Stats) {
	get := func() int64 {
		m := numRE.FindString(line)
		if m == "" {
			return 0
		}
		v, _ := strconv.ParseInt(strings.ReplaceAll(strings.ReplaceAll(m, ",", ""), ".", ""), 10, 64)
		return v
	}
	switch {
	case strings.HasPrefix(line, "Total file size:"):
		st.Total = get()
	case strings.HasPrefix(line, "Literal data:"):
		st.Literal = get()
	case strings.HasPrefix(line, "Matched data:"):
		st.Matched = get()
	case strings.HasPrefix(line, "Total bytes sent:"):
		st.Sent = get()
	case strings.HasPrefix(line, "Total bytes received:"):
		st.Received = get()
	}
}

// FileOptions describes a single-file transfer.
type FileOptions struct {
	Client     *sshx.Client
	LocalPath  string
	RemotePath string
	Verbose    bool
	RemoteZstd bool

	// LinkDest is an existing tree on the receiving side to reuse unchanged
	// files from. Only meaningful for directory transfers.
	LinkDest string
}

// PushFile copies one file to the host, resumably and incrementally.
//
// This is what makes shipping a 500 MB database alongside the image affordable:
// --partial lets an interrupted transfer resume rather than restart, and rsync's
// delta algorithm sends only the changed blocks, so a database rebuilt by an ETL
// run usually moves a small fraction of its size.
func PushFile(ctx context.Context, o FileOptions) (*Stats, error) {
	fi, err := os.Stat(o.LocalPath)
	if err != nil {
		return nil, fmt.Errorf("artifact %s: %w", o.LocalPath, err)
	}

	local, remote := o.LocalPath, o.RemotePath
	args := []string{
		"--stats",
		"--partial",
		// --fuzzy is what makes the delta algorithm apply at all here. The file
		// is staged under a .new suffix so the final rename is atomic, but that
		// means the destination never exists beforehand and rsync would have
		// nothing to diff against — it would resend the whole artifact every
		// deploy. --fuzzy lets it find the current copy sitting next to the
		// staged path and use that as the basis instead.
		"--fuzzy",
		"--times",
	}
	if fi.IsDir() {
		// A directory artifact is a whole tree: model weights, a prebuilt index,
		// an asset bundle. -a to carry permissions and times, --delete so the
		// staged copy is the tree exactly rather than a merge with whatever an
		// earlier release left, and trailing slashes so rsync copies the
		// *contents* into the staged directory rather than nesting it.
		args = append(args, "-a", "--delete")
		// --fuzzy is what saves a single-file artifact from being re-sent every
		// deploy, but it cannot help here: it looks for a basis file inside the
		// destination directory, and the release-scoped staging directory is
		// empty, so every file in the tree reads as new. --link-dest points at
		// the live tree instead, so unchanged files become hard links to it and
		// never cross the wire.
		//
		// A missing --link-dest is a warning rather than an error, so the first
		// deploy — when there is no live tree yet — is unaffected.
		if o.LinkDest != "" {
			args = append(args, "--link-dest="+strings.TrimSuffix(o.LinkDest, "/"))
		}
		local = strings.TrimSuffix(local, "/") + "/"
		remote = strings.TrimSuffix(remote, "/") + "/"
	}
	args = append(args, compressArgs(o.RemoteZstd)...)
	args = append(args,
		// Creates the destination's directory on first use, as Push does its store.
		"--rsync-path", "mkdir -p "+sshx.Quote(filepath.Dir(o.RemotePath))+" && rsync",
		"-e", o.Client.SSHCommand(), local, o.Client.Host+":"+remote)
	return runRsync(ctx, args, o.Verbose, func(err error) error {
		return fmt.Errorf("rsync %s to %s: %w", o.LocalPath, o.Client.Host, err)
	})
}

// Fetch copies a path back down from the host, incrementally.
//
// The transport is symmetric — rsync does not care which end is the source — so
// pulling production data down for local work costs one function rather than a
// new subsystem. It is the natural other half of shipping artifacts up: the
// same delta algorithm, so refreshing a 500 MB database you already have a
// stale copy of moves only what changed.
func Fetch(ctx context.Context, o FileOptions, isDir bool) (*Stats, error) {
	args := []string{"--stats", "--partial", "--times", "--fuzzy"}
	local, remote := o.LocalPath, o.RemotePath
	if isDir {
		args = append(args, "-a")
		local = strings.TrimSuffix(local, "/") + "/"
		remote = strings.TrimSuffix(remote, "/") + "/"
	}
	args = append(args, compressArgs(o.RemoteZstd)...)
	args = append(args, "-e", o.Client.SSHCommand(), o.Client.Host+":"+remote, local)
	return runRsync(ctx, args, o.Verbose, func(err error) error {
		return fmt.Errorf("fetch %s from %s: %w", o.RemotePath, o.Client.Host, err)
	})
}

// scanRedraws splits on carriage returns as well as newlines. rsync redraws its
// progress line with bare \r, so read by lines a long transfer is one line that
// outgrows the scanner — which then stops reading, and rsync blocks for good.
func scanRedraws(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// runRsync executes rsync and collects its transfer statistics.
func runRsync(ctx context.Context, args []string, verbose bool, wrap func(error) error) (*Stats, error) {
	if verbose {
		args = append([]string{"--info=progress2"}, args...)
	}
	cmd := exec.CommandContext(ctx, "rsync", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("rsync: %w", err)
	}
	st := &Stats{}
	sc := bufio.NewScanner(stdout)
	sc.Split(scanRedraws)
	for sc.Scan() {
		line := sc.Text()
		if verbose && strings.Contains(line, "%") {
			fmt.Fprintf(os.Stderr, "\r%s", strings.TrimSpace(line))
		}
		parseStat(line, st)
	}
	// Whatever stopped the scan, rsync must not be left blocked on a full pipe.
	io.Copy(io.Discard, stdout)
	if err := cmd.Wait(); err != nil {
		return nil, wrap(err)
	}
	if verbose {
		fmt.Fprintln(os.Stderr)
	}
	return st, nil
}
