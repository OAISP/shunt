// Package sshx runs commands on the deploy host by shelling out to the system
// ssh client.
//
// This is deliberate. Using the real ssh binary means the user's ~/.ssh/config,
// agent, jump hosts, hardware keys and known_hosts all work exactly as they
// already do — shunt never invents key management, never stores a credential,
// and never needs a Docker socket exposed over TCP. It also lets rsync reuse the
// very same multiplexed connection.
package sshx

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/OAISP/shunt/internal/ui"
)

type Client struct {
	Host string // user@host as written in the manifest

	ctlPath string
	started bool
}

func New(host string) *Client { return &Client{Host: host} }

// baseArgs are shared by ssh and (via -e) rsync. BatchMode keeps a misconfigured
// host from hanging on an interactive password prompt inside a deploy; there is
// deliberately no flag to disable host key checking.
func (c *Client) baseArgs() []string {
	args := []string{
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=15",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=6",
	}
	if c.ctlPath != "" {
		args = append(args,
			"-o", "ControlMaster=auto",
			"-o", "ControlPath="+c.ctlPath,
			"-o", "ControlPersist=120",
		)
	}
	return args
}

// Connect opens a multiplexed master connection. Every later ssh and rsync
// invocation rides it, so a deploy pays one handshake instead of a dozen.
func (c *Client) Connect(ctx context.Context) error {
	dir, err := os.MkdirTemp("", "shunt-ctl-")
	if err != nil {
		return err
	}
	// The socket path length is bounded by sockaddr_un (~104 bytes), so keep the
	// filename short rather than embedding the host.
	c.ctlPath = filepath.Join(dir, "s")

	cmd := exec.CommandContext(ctx, "ssh", append(c.baseArgs(), "-N", "-f", c.Host)...)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		c.ctlPath = ""
		os.RemoveAll(dir)
		return fmt.Errorf("ssh %s: %w (check `ssh %s` works non-interactively)", c.Host, err, c.Host)
	}
	c.started = true
	return nil
}

// Close tears the master connection down. Safe to call if Connect never ran.
func (c *Client) Close() {
	if !c.started || c.ctlPath == "" {
		return
	}
	exec.Command("ssh", "-o", "ControlPath="+c.ctlPath, "-O", "exit", c.Host).Run()
	os.RemoveAll(filepath.Dir(c.ctlPath))
	c.started = false
}

// SSHCommand is the -e value rsync needs to ride the multiplexed connection.
func (c *Client) SSHCommand() string {
	return "ssh " + strings.Join(c.baseArgs(), " ")
}

// Run executes argv on the host and returns combined output. argv is passed to
// ssh as separate arguments and quoted, so values never go through a shell on
// this side; the remote side still sees one command string, which is why callers
// must not pass untrusted data as a bare command.
func (c *Client) Run(ctx context.Context, argv ...string) (string, error) {
	args := append(c.baseArgs(), c.Host, "--")
	args = append(args, quoteAll(argv)...)
	cmd := exec.CommandContext(ctx, "ssh", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("remote %v: %w: %s", argv, err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// Stream executes argv with stdin piped from in, stdout to out and stderr to
// errw. This is how the helper is driven: the Spec (secrets included) goes in
// over the encrypted channel and NDJSON events come back.
func (c *Client) Stream(ctx context.Context, in io.Reader, out, errw io.Writer, argv ...string) error {
	args := append(c.baseArgs(), c.Host, "--")
	args = append(args, quoteAll(argv)...)
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdin = in
	cmd.Stdout = out
	cmd.Stderr = errw
	return cmd.Run()
}

// StartSession launches a long-running remote command and hands back its stdin
// and stdout without waiting for it.
//
// This is what lets the CLI hold a lock on the host for the duration of a
// deploy: the remote process lives as long as the session, and the kernel
// releases whatever it held the moment the session ends — including when the
// network drops or the CLI is killed. Callers own the returned Cmd.
func (c *Client) StartSession(ctx context.Context, argv ...string) (*exec.Cmd, io.WriteCloser, io.Reader, error) {
	args := append(c.baseArgs(), c.Host, "--")
	args = append(args, quoteAll(argv)...)
	cmd := exec.CommandContext(ctx, "ssh", args...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, nil, nil, err
	}
	return cmd, stdin, stdout, nil
}

// Interactive runs argv on the host with the local terminal attached,
// allocating a remote tty when there is a local one.
//
// This is what makes `shunt exec app -- sh` behave like a shell rather than a
// pipe: without -t the remote side has no tty, so readline, colour and Ctrl-C
// all misbehave.
func (c *Client) Interactive(ctx context.Context, argv ...string) error {
	args := c.baseArgs()
	if ui.IsTerminal(os.Stdin) {
		args = append(args, "-t")
	} else {
		args = append(args, "-T")
	}
	args = append(args, c.Host, "--")
	args = append(args, quoteAll(argv)...)

	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// Upload copies a local file to the host with the given mode, via a single
// `cat > file` over the multiplexed connection. Used for the helper binary and
// nothing else; bulk data goes through rsync.
func (c *Client) Upload(ctx context.Context, local, remote string, mode os.FileMode) error {
	f, err := os.Open(local)
	if err != nil {
		return err
	}
	defer f.Close()

	script := fmt.Sprintf("mkdir -p %s && cat > %s.tmp && chmod %o %s.tmp && mv -f %s.tmp %s",
		shellQuote(filepath.Dir(remote)), shellQuote(remote), mode.Perm(),
		shellQuote(remote), shellQuote(remote), shellQuote(remote))

	args := append(c.baseArgs(), c.Host, "--", script)
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdin = f
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// probeScript collects everything shunt needs to know about a host in one round
// trip, as key=value lines so a missing field cannot shift the meaning of the
// others the way positional output can.
//
// It covers the tools shunt shells out to on the host — not just the obvious
// ones. `tar` streams the image layout into `docker load` and `curl` runs url
// health checks; when either was missing the deploy got all the way past the
// container swap before failing, which is the worst possible moment to discover
// a missing package.
const probeScript = `
echo "arch=$(uname -m)"
echo "docker=$(docker version --format '{{.Server.Version}}' 2>&1 | head -1)"
echo "rsync=$(rsync --version 2>/dev/null | head -1 | awk '{print $3}')"
echo "rsync_zstd=$(rsync --version 2>/dev/null | grep -ci zstd)"
command -v curl >/dev/null && echo "curl=yes" || echo "curl=no"
command -v tar  >/dev/null && echo "tar=yes"  || echo "tar=no"
echo "free_kb=$(df -Pk "${SHUNT_ROOT:-$HOME}" 2>/dev/null | awk 'NR==2{print $4}')"
`

// Probe verifies the host is reachable and reports what shunt needs from it.
func (c *Client) Probe(ctx context.Context) (Facts, error) {
	var f Facts
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	out, err := c.Run(ctx, "sh", "-c", probeScript)
	if err != nil {
		return f, err
	}
	kv := map[string]string{}
	for _, ln := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(ln), "="); ok {
			kv[k] = v
		}
	}
	if kv["arch"] == "" {
		return f, fmt.Errorf("unexpected probe output: %q", out)
	}
	f.Arch = kv["arch"]
	f.DockerVersion = kv["docker"]
	f.RsyncVersion = kv["rsync"]
	f.HasRsync = f.RsyncVersion != ""
	f.RsyncZstd = kv["rsync_zstd"] != "" && kv["rsync_zstd"] != "0"
	f.HasCurl = kv["curl"] == "yes"
	f.HasTar = kv["tar"] == "yes"
	if n, err := strconv.ParseInt(kv["free_kb"], 10, 64); err == nil {
		f.FreeBytes = n * 1024
	}

	if strings.Contains(strings.ToLower(f.DockerVersion), "cannot connect") ||
		strings.Contains(strings.ToLower(f.DockerVersion), "permission denied") {
		return f, fmt.Errorf("docker is not usable as this user on %s: %s\n"+
			"  add the user to the docker group, or point host= at one that can reach the socket", c.Host, f.DockerVersion)
	}
	if missing := f.Missing(); len(missing) > 0 {
		return f, fmt.Errorf("%s is missing %s that shunt needs\n  install with: apt-get install -y %s",
			c.Host, strings.Join(missing, " and "), strings.Join(missing, " "))
	}
	return f, nil
}

// Missing names the required host tools that are absent.
func (f Facts) Missing() []string {
	var missing []string
	if !f.HasRsync {
		missing = append(missing, "rsync")
	}
	if !f.HasTar {
		missing = append(missing, "tar")
	}
	if !f.HasCurl {
		missing = append(missing, "curl")
	}
	return missing
}

type Facts struct {
	Arch          string
	DockerVersion string

	HasRsync     bool
	RsyncVersion string
	// RsyncZstd reports whether this rsync was built with zstd. Ubuntu 20.04
	// ships 3.1.3, which has no --compress-choice at all, and stock macOS is
	// older still — so the flag cannot simply be assumed.
	RsyncZstd bool

	HasCurl bool // url health checks shell out to it on the host
	HasTar  bool // the image layout is streamed into `docker load` through it

	// FreeBytes is free space where the store and ledger live. Running out
	// mid-transfer leaves a partial layout and a confusing error.
	FreeBytes int64
}

// GoArch maps uname -m to the GOARCH used to pick a helper binary.
func (f Facts) GoArch() string {
	switch f.Arch {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	default:
		return f.Arch
	}
}

func quoteAll(argv []string) []string {
	out := make([]string, len(argv))
	for i, a := range argv {
		out[i] = shellQuote(a)
	}
	return out
}

// shellQuote single-quotes a value for the remote shell that ssh always spawns.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '-' || r == '_' || r == '/' || r == '.' || r == ':' || r == '=' || r == '@' || r == ',' || r == '+') {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
