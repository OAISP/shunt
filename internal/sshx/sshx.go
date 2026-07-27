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
	"strings"
	"time"
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

// Probe verifies the host is reachable and reports what shunt needs from it.
func (c *Client) Probe(ctx context.Context) (Facts, error) {
	var f Facts
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	out, err := c.Run(ctx, "sh", "-c",
		`printf '%s\n' "$(uname -m)"; docker version --format '{{.Server.Version}}' 2>&1 | head -1; `+
			`command -v rsync >/dev/null && echo yes || echo no`)
	if err != nil {
		return f, err
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 3 {
		return f, fmt.Errorf("unexpected probe output: %q", out)
	}
	f.Arch = strings.TrimSpace(lines[0])
	f.DockerVersion = strings.TrimSpace(lines[1])
	f.HasRsync = strings.TrimSpace(lines[2]) == "yes"

	if strings.Contains(strings.ToLower(f.DockerVersion), "cannot connect") ||
		strings.Contains(strings.ToLower(f.DockerVersion), "permission denied") {
		return f, fmt.Errorf("docker is not usable as this user on %s: %s\n"+
			"  add the user to the docker group, or point host= at one that can reach the socket", c.Host, f.DockerVersion)
	}
	if !f.HasRsync {
		return f, fmt.Errorf("rsync is not installed on %s — shunt needs it for incremental image transfer\n"+
			"  install it with: apt-get install -y rsync", c.Host)
	}
	return f, nil
}

type Facts struct {
	Arch          string
	DockerVersion string
	HasRsync      bool
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
