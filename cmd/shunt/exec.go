package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/OAISP/shunt/internal/engine"
)

// cmdExec runs a command inside a service's running container.
//
// `shunt logs` existed but there was no way to get a shell or a console on the
// thing you just deployed, which is the first thing anyone wants at 2am. The
// container name is resolved from the labels rather than guessed, so it works
// for blue/green services whose names carry a release id.
func cmdExec(ctx context.Context, args []string) error {
	var c commonFlags
	fs := newFlagSet("exec", &c)
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return fmt.Errorf("usage: shunt exec <service> [--] <command>...")
	}
	service, command := rest[0], rest[1:]
	if len(command) == 0 {
		// A bare `shunt exec app` almost always means "give me a shell".
		command = []string{"sh"}
	}

	e, err := connect(ctx, c.file, c.target)
	if err != nil {
		return err
	}
	defer e.Close()

	container, err := resolveContainer(ctx, e, service)
	if err != nil {
		return err
	}
	return e.Exec(ctx, container, command)
}

// cmdRun runs a one-off command in a *new* container from the active release's
// image, with the release's secrets and network.
//
// The difference from exec matters: exec attaches to the container serving
// traffic, run gets a clean one. A migration, a rake task or a console that
// should not share a process table with production belongs here.
func cmdRun(ctx context.Context, args []string) error {
	var c commonFlags
	var image string
	fs := newFlagSet("run", &c)
	fs.StringVar(&image, "image", "", "image to run (defaults to the service's)")
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return fmt.Errorf("usage: shunt run <service> [--] <command>...")
	}
	service, command := rest[0], rest[1:]
	if len(command) == 0 {
		return fmt.Errorf("shunt run needs a command, e.g. `shunt run app -- bin/rails console`")
	}

	e, err := connect(ctx, c.file, c.target)
	if err != nil {
		return err
	}
	defer e.Close()

	svc, present := e.M.Services[service]
	if !present {
		if svc, present = e.M.Accessories[service]; !present {
			return fmt.Errorf("%q is not a service or accessory in shunt.toml", service)
		}
	}
	if image == "" {
		image = svc.Image
	}

	state, err := e.State(ctx)
	if err != nil {
		return err
	}
	if state.Ledger == nil || state.Ledger.Current == "" {
		return fmt.Errorf("nothing is deployed to %s yet — run `shunt up` first", e.M.Host)
	}
	cur := state.Ledger.Find(state.Ledger.Current)
	if cur == nil {
		return fmt.Errorf("the active release %s is not in this host's ledger", state.Ledger.Current)
	}
	ref, ok := cur.Images[image]
	if !ok || ref.Ref == "" {
		return fmt.Errorf("release %s did not record an image named %q", cur.ID, image)
	}

	return e.RunOneOff(ctx, cur.ID, ref.Ref, command)
}

// resolveContainer finds the container currently serving a service.
func resolveContainer(ctx context.Context, e *engine.Engine, service string) (string, error) {
	state, err := e.State(ctx)
	if err != nil {
		return "", err
	}
	found := state.ServiceContainers(service)
	if len(found) == 0 {
		known := knownServices(e)
		return "", fmt.Errorf("no container on %s for service %q%s", e.M.Host, service, known)
	}
	// During a blue/green overlap two containers can match; prefer the one from
	// the active release, then any running one.
	if state.Ledger != nil {
		for _, ct := range found {
			if ct.Release == state.Ledger.Current && ct.Running() {
				return ct.Name, nil
			}
		}
	}
	for _, ct := range found {
		if ct.Running() {
			return ct.Name, nil
		}
	}
	return "", fmt.Errorf("no running container for %q on %s (found %d stopped)", service, e.M.Host, len(found))
}

func knownServices(e *engine.Engine) string {
	var names []string
	for n := range e.M.Services {
		names = append(names, n)
	}
	for n := range e.M.Accessories {
		names = append(names, n)
	}
	if len(names) == 0 {
		return ""
	}
	b, _ := json.Marshal(names)
	return " — shunt.toml declares " + string(b)
}
