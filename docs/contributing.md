---
title: Contributing
nav_order: 8
---

# Contributing

Issues and pull requests are welcome.

## Building and testing

```sh
make build      # cross-compile the helpers, then build ./shunt with them embedded
make test       # go test ./...
make fmt vet    # gofmt and go vet
make install    # build, then copy to ~/.local/bin
```

The helper is built first because the CLI embeds it. Without it — a fresh
checkout where `make helpers` has not run — the CLI compiles a helper from the
source on demand, so `go run ./cmd/shunt` works too.

CI runs `gofmt`, `go vet`, `go test -race`, shellcheck on the shell scripts, a
full `make build`, and a smoke test of the binary. `make fmt vet test build`
covers the same ground locally, apart from the race detector and shellcheck.

## Tests against real servers

Two scripts exercise what unit tests cannot, and CI runs both:

```sh
make build && test/e2e.sh deploy@vps.example.com   # any host you can ssh to
test/compat.sh                                     # needs local docker
test/compat.sh docker 27                           # the e2e suite on that Docker
```

- **`test/e2e.sh`** deploys a small project to the host and checks the result:
  first deploy, no-op, code change, a replaced container receiving SIGTERM, an
  unchanged service keeping its container, a failing stage leaving production
  serving, limits, rollback, drift after a container deleted or an accessory
  stopped by hand, exec and logs, automatic rollback, a service that cannot stay
  up, an irreversible stage, a deploy cancelled midway, a deploy queued behind
  another, bundles, a rotated secret, secrets surviving a rollback, retire, and
  `down`. It cleans up after itself. CI points it at the runner itself.
- **`test/compat.sh`** builds a throwaway server in a container with rsync 3.1.3
  and neither `curl` nor `tar`, and deploys to it. With `docker <version>` it
  instead starts a server running that Docker release and runs the whole e2e
  suite against it; CI does so for each supported major.

## Changes to the deploy path

Changes here are hard to prove with unit tests alone, and they are the ones that
can take someone's site down. The helper reaches Docker through the `runner`
interface in `cmd/shunt-helper/runner.go`; tests substitute a fake that records
calls and scripts failures, which is how swaps, health gating and rollback are
tested without a daemon. If you touch container swapping, health checks, the
lock or the transport, add a case there, and say in the pull request what you
ran against a real server.

## Releases

Pushing a `v*` tag runs the same jobs as a push to `main` — tests, `e2e`,
`compat` and the Docker versions — and publishes the release only if they pass.

## Documentation

This site is [just-the-docs](https://just-the-docs.com) in `docs/`, published by
GitHub Actions on every push to `main` that changes it. To preview:

```sh
cd docs
bundle install
bundle exec jekyll serve
```

Every page links to its own source, so a wrong sentence is one click from the
file to fix.
