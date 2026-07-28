package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/OAISP/shunt/internal/engine"
	"github.com/OAISP/shunt/internal/manifest"
	"github.com/OAISP/shunt/internal/ui"
)

// check is one thing that has to be true for a deploy to work.
type check struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok | warn | fail
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

const (
	statusOK   = "ok"
	statusWarn = "warn"
	statusFail = "fail"
)

// cmdAudit checks everything a deploy depends on, before a deploy depends on it.
//
// Every one of these was already checked somewhere in the deploy path — but
// spread across it, so the answer arrived at the worst possible moment: a
// missing `curl` on the host surfaced after the container swap, and a builder
// that cannot export OCI surfaced after waiting for a build. Collecting them
// into one command that changes nothing is what makes them useful before the
// first deploy rather than during it.
func cmdAudit(ctx context.Context, args []string) error {
	var c commonFlags
	fs := newFlagSet("audit", &c)
	if err := parseArgs(fs, args); err != nil {
		return err
	}

	var checks []check
	add := func(ch check) { checks = append(checks, ch) }

	m, manifestErr := loadManifest(c.file, c.target)
	if manifestErr != nil {
		add(check{"manifest", statusFail, manifestErr.Error(), "fix shunt.toml, or run `shunt init`"})
	} else {
		add(check{"manifest", statusOK, fmt.Sprintf("%s — %d service(s), %d image(s)",
			m.Project, len(m.Services), len(m.Images)), ""})
	}

	checks = append(checks, localChecks()...)

	// Host checks need a manifest to know which host to talk to.
	if manifestErr == nil {
		checks = append(checks, hostChecks(ctx, m)...)
	}

	if c.asJSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"checks": checks})
	}
	return renderAudit(checks, c.out())
}

// localChecks cover the machine running shunt.
func localChecks() []check {
	var out []check
	for _, tool := range []struct{ name, fix string }{
		{"docker", "install Docker, and make sure the daemon is running"},
		{"rsync", "install rsync (brew install rsync / apt-get install rsync)"},
		{"ssh", "install an ssh client"},
	} {
		if _, err := exec.LookPath(tool.name); err != nil {
			out = append(out, check{"local " + tool.name, statusFail, "not on PATH", tool.fix})
			continue
		}
		out = append(out, check{"local " + tool.name, statusOK, "present", ""})
	}

	// A builder without OCI export cannot produce the layout the whole design
	// rests on, and the failure otherwise appears only after a full build.
	if outb, err := exec.Command("docker", "buildx", "version").Output(); err != nil {
		out = append(out, check{"local buildx", statusFail, "not available",
			"install docker buildx; shunt exports an OCI layout with it"})
	} else {
		out = append(out, check{"local buildx", statusOK, strings.TrimSpace(string(outb)), ""})
	}

	// zstd on this side is half of the transfer-compression story; the host is
	// the other half and is reported separately.
	if outb, err := exec.Command("rsync", "--version").Output(); err == nil {
		if strings.Contains(strings.ToLower(string(outb)), "zstd") {
			out = append(out, check{"local rsync zstd", statusOK, "supported", ""})
		} else {
			out = append(out, check{"local rsync zstd", statusWarn,
				"this rsync has no zstd; transfers fall back to plain -z",
				"install rsync 3.2+ for cheaper compression (macOS ships 2.6.9)"})
		}
	}
	return out
}

// hostChecks cover the deploy target. They reuse the probe the deploy itself
// runs, so audit cannot drift from what a deploy actually requires.
func hostChecks(ctx context.Context, m *manifest.Manifest) []check {
	e := engine.New(m)
	if err := e.Connect(ctx); err != nil {
		return []check{{"host " + m.Host, statusFail, err.Error(),
			"check `ssh " + m.Host + "` works non-interactively"}}
	}
	defer e.Close()

	f := e.Facts()
	out := []check{
		{"host ssh", statusOK, "connected to " + m.Host, ""},
		{"host docker", statusOK, f.DockerVersion + " on " + f.Arch, ""},
		{"host rsync", statusOK, versionOr(f.RsyncVersion), ""},
		{"host curl", statusOK, "present", ""},
	}
	// Connect fails outright when rsync or curl is missing, so reaching here
	// means they are present; report them anyway so the list is a complete
	// account rather than a list of things that happened to be checked.

	// tar is not required — the helper writes the load archive itself — so its
	// absence is worth noting and never a failure.
	if f.HasTar {
		out = append(out, check{"host tar", statusOK, "present (not required)", ""})
	} else {
		out = append(out, check{"host tar", statusOK, "absent — not needed; shunt builds the archive itself", ""})
	}

	if f.RsyncZstd {
		out = append(out, check{"host rsync zstd", statusOK, "supported", ""})
	} else {
		out = append(out, check{"host rsync zstd", statusWarn,
			"host rsync has no zstd; transfers fall back to plain -z",
			"apt-get install rsync from a release with 3.2+ (Ubuntu 20.04 ships 3.1.3)"})
	}

	if f.FreeBytes > 0 {
		st := statusOK
		fix := ""
		if f.FreeBytes < 2<<30 {
			st, fix = statusWarn, "free some space, or run `shunt prune`"
		}
		out = append(out, check{"host disk", st, ui.Bytes(f.FreeBytes) + " free", fix})
	}

	// The image platform is the single most common first-deploy failure: an
	// arm64 laptop building for an amd64 server produces an image that loads
	// fine and then exits immediately.
	out = append(out, platformCheck(m, f.GoArch()))

	if err := e.PreflightArtifacts(ctx); err != nil {
		out = append(out, check{"host artifact paths", statusFail, ui.FirstLine(err.Error()),
			"create the destination directories, or point dest= somewhere writable"})
	} else if len(m.Artifacts) > 0 {
		out = append(out, check{"host artifact paths", statusOK, "writable", ""})
	}
	return out
}

// platformCheck compares what each image will be built for against the host.
func platformCheck(m *manifest.Manifest, hostArch string) check {
	var mismatched []string
	for name, img := range m.Images {
		want := img.Platform
		if want == "" {
			continue // buildx defaults to this machine; reported below
		}
		if !strings.HasSuffix(want, "/"+hostArch) {
			mismatched = append(mismatched, fmt.Sprintf("%s builds %s but the host is %s", name, want, hostArch))
		}
	}
	if len(mismatched) > 0 {
		return check{"image platform", statusFail, strings.Join(mismatched, "; "),
			"set platform = \"linux/" + hostArch + "\" in [images.*]"}
	}
	var unset []string
	for name, img := range m.Images {
		if img.Platform == "" {
			unset = append(unset, name)
		}
	}
	if len(unset) > 0 && localArch() != hostArch {
		return check{"image platform", statusWarn,
			fmt.Sprintf("%s set no platform; this machine is %s and the host is %s",
				strings.Join(unset, ", "), localArch(), hostArch),
			"set platform = \"linux/" + hostArch + "\" in [images.*]"}
	}
	return check{"image platform", statusOK, "matches the host (" + hostArch + ")", ""}
}

func localArch() string {
	out, err := exec.Command("docker", "version", "--format", "{{.Server.Arch}}").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func versionOr(v string) string {
	if v == "" {
		return "present"
	}
	return v
}

func renderAudit(checks []check, s ui.Style) error {
	fmt.Printf("\n%s\n\n", s.Bold("audit"))
	var failed, warned int
	for _, ch := range checks {
		var mark string
		switch ch.Status {
		case statusOK:
			mark = s.Tick()
		case statusWarn:
			mark = s.Warn()
			warned++
		default:
			mark = s.Cross()
			failed++
		}
		fmt.Printf("  %s %-22s %s\n", mark, ch.Name, ch.Detail)
		if ch.Fix != "" {
			fmt.Printf("      %s\n", s.Dim("→ "+ch.Fix))
		}
	}

	fmt.Println()
	switch {
	case failed > 0:
		fmt.Printf("  %s\n\n", s.Red(fmt.Sprintf("%d check(s) failed — a deploy would not succeed", failed)))
		// Non-zero so CI can gate on it.
		return errReported
	case warned > 0:
		fmt.Printf("  %s\n\n", s.Amber(fmt.Sprintf("%d warning(s); a deploy should still work", warned)))
	default:
		fmt.Printf("  %s\n\n", s.Dim("everything a deploy needs is in place"))
	}
	return nil
}

// cmdValidate loads and validates the manifest without touching the network.
//
// Deliberately offline: it is what an editor hook or a pull-request check can
// run, and neither should need ssh access to a production host to tell you that
// a key is misspelled.
func cmdValidate(_ context.Context, args []string) error {
	var c commonFlags
	fs := newFlagSet("validate", &c)
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	m, err := loadManifest(c.file, c.target)
	if err != nil {
		if c.asJSON {
			json.NewEncoder(os.Stdout).Encode(map[string]any{"valid": false, "error": err.Error()})
			return errReported
		}
		return err
	}
	if c.asJSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{
			"valid": true, "project": m.Project, "host": m.Host,
			"services": len(m.Services), "images": len(m.Images),
		})
	}
	s := c.out()
	fmt.Printf("\n  %s %s is valid\n", s.Tick(), s.Bold(m.Project))
	fmt.Printf("    %s\n\n", s.Dim(fmt.Sprintf("host %s · %d image(s) · %d service(s) · %d accessory(ies) · %d stage(s) · %d artifact(s)",
		m.Host, len(m.Images), len(m.Services), len(m.Accessories), len(m.Stages), len(m.Artifacts))))
	return nil
}
