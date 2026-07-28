---
title: Contributing
nav_order: 8
---

# Contributing

Issues and pull requests are welcome.

## Building

```sh
make build      # helpers + CLI
make test       # go test ./...
make helpers    # cross-compile the host helper for linux/amd64 and linux/arm64
make fmt vet
```

The helper is built first and embedded via `go:embed`. In a fresh checkout where
`make helpers` has not run, the CLI falls back to compiling one on demand, so
`go run ./cmd/shunt` works with no build step.

## What CI runs

`gofmt`, `go vet`, `go test -race`, shellcheck over the shell scripts, a
cross-compiled build of the embedded helper, and a smoke test of the binary.
`make test && make build` locally covers all but the last.

Two further jobs exercise what unit tests cannot:

- **e2e** runs a real deploy over real ssh against a real daemon, with the
  runner deploying to itself. It covers first deploy, no-op, code change,
  rollback, secrets surviving a rollback, drift detection after a hand-deleted
  container, exec, multi-service logs, bundles, orphan detection and retire —
  and the invariant the whole tool rests on, that a failing stage leaves the
  running release serving.
- **compat** builds hosts that actually have the compatibility problems a
  healthy runner cannot reproduce: rsync 3.1.3 with no `--compress-choice`, and
  a host with neither `curl` nor `tar`.

Run either against any host you can ssh to:

```sh
make build && test/e2e.sh deploy@vps.example.com
test/compat.sh
```

## Changes to the deploy path

These are hard to prove correct with unit tests alone, and they are the ones
that can cost someone their site.

Host-side logic reaches docker through the `runner` seam in
`cmd/shunt-helper/runner.go`, which is what makes swap, health and rollback
failures assertable without a daemon. If you touch container swapping, health
gating or the transport, add a case there — and say in the pull request what you
exercised against a real host.

## Documentation

This site is [just-the-docs](https://just-the-docs.com) under `docs/`, built and
published by GitHub Actions on every push to `main` that touches it. Preview it
locally:

```sh
cd docs
bundle install
bundle exec jekyll serve
```

Every page has an "Edit this page on GitHub" link, so a wrong sentence is one
click from the file that needs correcting.
