package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// cmdInit writes a shunt.toml seeded from whatever it can infer about the
// project. It guesses rather than interrogates: a wrong guess in a file the user
// is about to edit is cheaper than a wizard.
func cmdInit(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	host := fs.String("host", "", "deploy target, e.g. deploy@vps.example.com")
	force := fs.Bool("force", false, "overwrite an existing shunt.toml")
	if err := fs.Parse(args); err != nil {
		return err
	}

	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	path := filepath.Join(wd, "shunt.toml")
	if _, err := os.Stat(path); err == nil && !*force {
		return errors.New("shunt.toml already exists (use --force to overwrite)")
	}

	project := sanitizeName(filepath.Base(wd))
	dockerfile := "Dockerfile"
	if _, err := os.Stat(filepath.Join(wd, dockerfile)); err != nil {
		return fmt.Errorf("no Dockerfile in %s — shunt deploys images it builds from one", wd)
	}
	port := sniffPort(filepath.Join(wd, dockerfile))

	h := *host
	if h == "" {
		h = "deploy@example.com"
	}

	tmpl := `# shunt.toml — the committed description of this project's production deploy.
# Everything the host runs is derived from this file plus resolved secrets.
#   shunt plan   see what would change
#   shunt up     apply it

project = "%s"
host    = "%s"

[images.app]
context    = "."
dockerfile = "Dockerfile"
# Uncomment when building on an arm64 machine for an amd64 host — a silent
# architecture mismatch is the single most common first-deploy failure.
# platform = "linux/amd64"

# Secrets are resolved on your machine and streamed to the host over ssh into a
# 0600 env-file. They never enter the image, argv, or ` + "`docker inspect`" + `.
# [secrets]
# provider = "file"     # file | env | sops
# path     = "secrets/prod.env"

# A stage is a one-shot container that must succeed BEFORE any running container
# is replaced — a migration, a backup, a smoke test. If one fails, production is
# left exactly as it was. Most projects need none.
# [[stages]]
# name    = "migrate"
# image   = "app"
# command = ["npm", "run", "migrate"]

# A large file the image does not carry, swapped in atomically. Most projects
# need none.
# [[artifacts]]
# name = "index"
# src  = "data/index.db"
# dest = "/opt/PROJECT/data/index.db"

[services.app]
image   = "app"
publish = ["127.0.0.1:%d:%d"]
restart = "unless-stopped"

[services.app.health]
# A bare path is resolved against the published port above.
url      = "/"
retries  = 10
interval = "3s"
# follow = true   # chase redirects and require a 2xx at the end
`
	content := fmt.Sprintf(tmpl, project, h, port, port)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return err
	}

	fmt.Fprintf(out, "wrote %s\n\n", path)
	fmt.Fprintf(out, "  project  %s\n  host     %s\n  port     %d (guessed from the Dockerfile EXPOSE)\n\n", project, h, port)
	if *host == "" {
		fmt.Fprintln(out, "  next: set `host` to your server, then run `shunt plan`")
	} else {
		fmt.Fprintln(out, "  next: run `shunt plan`")
	}
	return nil
}

var exposeRE = regexp.MustCompile(`(?i)^\s*EXPOSE\s+(\d+)`)

// sniffPort reads the first EXPOSE in the Dockerfile so the scaffold is usually
// correct without asking.
func sniffPort(dockerfile string) int {
	f, err := os.Open(dockerfile)
	if err != nil {
		return 3000
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if m := exposeRE.FindStringSubmatch(sc.Text()); m != nil {
			var p int
			if _, err := fmt.Sscanf(m[1], "%d", &p); err == nil && p > 0 && p < 65536 {
				return p
			}
		}
	}
	return 3000
}

func sanitizeName(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-_")
	if out == "" {
		return "app"
	}
	return out
}
