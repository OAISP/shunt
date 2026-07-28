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
	"context"
	"fmt"
	"os"
	"os/exec"
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

// DedupPercent is the share of the image rsync did not have to send because the
// host already had those blobs — the number that justifies the whole design.
func (s Stats) DedupPercent() float64 {
	if s.Total == 0 {
		return 0
	}
	return float64(s.Matched) / float64(s.Total) * 100
}

type Options struct {
	Client    *sshx.Client
	LocalDir  string // OCI layout directory
	RemoteDir string
	Verbose   bool

	// RemoteZstd reports whether the host's rsync was built with zstd. Both ends
	// have to support it or the transfer fails outright.
	RemoteZstd bool
}

// localZstd reports whether the rsync on this machine understands zstd.
// Computed once: it costs a subprocess, and it cannot change mid-run.
var localZstd = sync.OnceValue(func() bool {
	out, err := exec.Command("rsync", "--version").Output()
	return err == nil && strings.Contains(strings.ToLower(string(out)), "zstd")
})

// compressArgs picks a compression mode both ends actually support.
//
// zstd at level 1 is the right default — layer blobs are already compressed
// inside, so this is about the cheap win on the small JSON files rather than the
// bulk. But --compress-choice only exists from rsync 3.2, and plenty of live
// hosts are older: Ubuntu 20.04 ships 3.1.3, and macOS ships 2.6.9 as
// /usr/bin/rsync. Sending the flag to either end fails the deploy outright, so
// fall back to plain -z, which every rsync in circulation understands.
func compressArgs(remoteZstd bool) []string {
	if localZstd() && remoteZstd {
		return []string{"--compress", "--compress-choice=zstd", "--compress-level=1"}
	}
	return []string{"--compress"}
}

// Push mirrors LocalDir to RemoteDir on the host and reports transfer stats.
func Push(ctx context.Context, o Options) (*Stats, error) {
	if _, err := os.Stat(o.LocalDir); err != nil {
		return nil, fmt.Errorf("local layout %s: %w", o.LocalDir, err)
	}
	if _, err := o.Client.Run(ctx, "mkdir", "-p", o.RemoteDir); err != nil {
		return nil, err
	}

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
	}
	args = append(args, compressArgs(o.RemoteZstd)...)
	args = append(args,
		"-e", o.Client.SSHCommand(),
		strings.TrimSuffix(o.LocalDir, "/")+"/",
		o.Client.Host+":"+strings.TrimSuffix(o.RemoteDir, "/")+"/",
	)
	if o.Verbose {
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
	for sc.Scan() {
		line := sc.Text()
		if o.Verbose && strings.Contains(line, "%") {
			fmt.Fprintf(os.Stderr, "\r%s", strings.TrimSpace(line))
		}
		parseStat(line, st)
	}
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("rsync to %s: %w", o.Client.Host, err)
	}
	if o.Verbose {
		fmt.Fprintln(os.Stderr)
	}
	return st, nil
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
		// staged path and use that as the basis instead. On a 5.6 MB database
		// with a small change that is the difference between 1.5 MB and 15 KB.
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
		// never cross the wire. Measured on a 195 KB tree with one small file
		// changed: 195 KB re-sent becomes 158 bytes.
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
		"-e", o.Client.SSHCommand(),
		local,
		o.Client.Host+":"+remote,
	)
	if o.Verbose {
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
	for sc.Scan() {
		line := sc.Text()
		if o.Verbose && strings.Contains(line, "%") {
			fmt.Fprintf(os.Stderr, "\r%s", strings.TrimSpace(line))
		}
		parseStat(line, st)
	}
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("rsync %s to %s: %w", o.LocalPath, o.Client.Host, err)
	}
	if o.Verbose {
		fmt.Fprintln(os.Stderr)
	}
	return st, nil
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
	args = append(args,
		"-e", o.Client.SSHCommand(),
		o.Client.Host+":"+remote,
		local,
	)
	if o.Verbose {
		args = append([]string{"--info=progress2"}, args...)
	}
	return runRsync(ctx, args, o.Verbose, func(err error) error {
		return fmt.Errorf("fetch %s from %s: %w", o.RemotePath, o.Client.Host, err)
	})
}

// runRsync executes rsync and collects its transfer statistics.
func runRsync(ctx context.Context, args []string, verbose bool, wrap func(error) error) (*Stats, error) {
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
	for sc.Scan() {
		line := sc.Text()
		if verbose && strings.Contains(line, "%") {
			fmt.Fprintf(os.Stderr, "\r%s", strings.TrimSpace(line))
		}
		parseStat(line, st)
	}
	if err := cmd.Wait(); err != nil {
		return nil, wrap(err)
	}
	if verbose {
		fmt.Fprintln(os.Stderr)
	}
	return st, nil
}

// FileStat is what the plan knows about a file on the host without moving any
// of it. Size is -1 when the file is absent.
type FileStat struct {
	Size  int64
	MTime int64 // unix seconds
}

// RemoteFileStat reports a host file's size and modification time in one round
// trip.
//
// Size and mtime together are exactly the heuristic rsync itself uses to decide
// whether a file needs transferring, which makes it the right basis for deciding
// whether an artifact counts as a change. Hashing would be exact but means
// reading the whole file on both sides — most of the cost of just shipping it.
// RemoteFileStats stats several host paths in a single round trip.
//
// Replies are keyed by index rather than by echoing the path back, so a path
// containing whitespace cannot corrupt the parse.
//
// A directory is summarised as the sum of its regular files' sizes and their
// newest mtime — deliberately the same computation the CLI does locally, and
// deliberately not `du`, which counts directory inodes too and would make every
// directory artifact differ from itself on every deploy.
func RemoteFileStats(ctx context.Context, c *sshx.Client, paths []string) map[string]FileStat {
	out := make(map[string]FileStat, len(paths))
	if len(paths) == 0 {
		return out
	}
	for _, p := range paths {
		out[p] = FileStat{Size: -1}
	}

	var script strings.Builder
	for i, p := range paths {
		q := shellArg(p)
		fmt.Fprintf(&script,
			"if [ -d %s ]; then find %s -type f -exec stat -c '%%s %%Y' {} + 2>/dev/null | "+
				"awk -v i=%d 'BEGIN{b=0;m=0} {b+=$1; if ($2>m) m=$2} END{print i, b, m}'; "+
				"elif [ -f %s ]; then echo \"%d $(stat -c '%%s %%Y' %s)\"; "+
				"else echo '%d -1 0'; fi\n",
			q, q, i, q, i, q, i)
	}
	reply, err := c.Run(ctx, "sh", "-c", script.String())
	if err != nil {
		return out
	}
	for _, ln := range strings.Split(reply, "\n") {
		var idx int
		var st FileStat
		if _, err := fmt.Sscanf(strings.TrimSpace(ln), "%d %d %d", &idx, &st.Size, &st.MTime); err != nil {
			continue
		}
		if idx >= 0 && idx < len(paths) {
			out[paths[idx]] = st
		}
	}
	return out
}

func RemoteFileStat(ctx context.Context, c *sshx.Client, path string) FileStat {
	out, err := c.Run(ctx, "sh", "-c",
		"if [ -f "+shellArg(path)+" ]; then stat -c '%s %Y' "+shellArg(path)+"; else echo '-1 0'; fi")
	if err != nil {
		return FileStat{Size: -1}
	}
	var st FileStat
	if _, err := fmt.Sscanf(strings.TrimSpace(out), "%d %d", &st.Size, &st.MTime); err != nil {
		return FileStat{Size: -1}
	}
	return st
}

func shellArg(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
