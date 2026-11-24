---
title: Internals
nav_order: 7
has_children: true
---

# Internals

shunt is two binaries and a wire contract.

The **CLI** runs on your machine. It reads the manifest, builds the images,
transfers them, and shows what the server reports. The **helper** runs on the
server, driven over ssh, and does everything that changes the server. Between
them is one JSON document — the release spec — sent on the helper's stdin, and a
stream of JSON events, one per line, coming back on its stdout.

## A deploy, end to end

`shunt up` makes four remote calls after the ssh connection is open, all over
one multiplexed connection:

1. **Probe.** A short shell script reports the server's architecture, Docker
   and rsync versions, whether the helper for this exact CLI is already
   installed, and where shunt's files live. If the helper is missing, it is
   uploaded now.
2. **`helper plan`.** The release history, the project's containers, which image
   blobs the store already holds, the state of every artifact destination, and
   free disk space. The build runs after this — so a destination that cannot be
   written fails in seconds — and the plan, the transfer estimate and the space
   check all come from this one answer.
3. **`helper apply`.** Opened before anything is shipped. The helper takes the
   project lock and reports the serving release; the CLI checks its plan still
   holds, then runs the transfer, then sends the spec on this session's stdin.
   The helper applies it and streams events back. During the transfer the CLI
   sends a heartbeat on the same stdin, and a helper that stops hearing it gives
   the lock up.
4. **rsync**, between 3's lock and its spec: every image layout in one
   transfer, then each artifact.

## Source map

```
cmd/shunt/             the CLI
  main.go                dispatch, usage, exit codes
  cli.go                 flags, argument parsing, shared setup
  deploy.go              plan, up, and the deploy sequence apply shares
  bundle.go              bundle, apply
  bundle_inspect.go      bundle inspect, bundle verify
  lifecycle.go           rollback, boot, retire
  down.go                down
  inspect.go             status, logs, prune
  exec.go                exec, run
  fetch.go               fetch
  audit.go               audit, validate
  init.go                init

cmd/shunt-helper/      runs on the server
  apply.go               the deploy session: lock, spec, pipeline, outcome
  images.go              blob verification and docker load
  containers.go          naming, creating, swapping and retiring containers
  proxy.go               Traefik and caddy-docker-proxy labels
  health.go              health probes
  stages.go              one-shot containers and captures
  artifacts.go           checking and swapping in artifacts
  exchange*.go           RENAME_EXCHANGE, for atomic directory swaps
  secrets.go             writing secrets for containers, reading them back
  ledger.go              the release history and outcome classification
  lifecycle.go           rollback, boot, retire
  down.go                down
  inspect.go             status, plan, logs, prune
  runner.go              the seam that lets tests stand in for docker

internal/manifest      shunt.toml: types, loading, targets, validation
internal/build         buildx to an OCI layout, and the build cache
internal/oci           the content-addressed blob store inside a layout
internal/transport     rsync, in both directions
internal/engine        manifest → spec → plan → deploy; the helper binaries
internal/release       the CLI/helper contract and the release history format
internal/bundle        the bundle file format
internal/secrets       secret providers and ${env:...}
internal/sshx          the multiplexed ssh connection and the probe
internal/ui            colour and formatting — the only place escape codes appear
```

## On the server

```
~/.shunt/                              SHUNT_ROOT overrides this
  bin/shunt-helper-<hash>              helpers, named by their own content hash
  <project>/
    releases.json                      release history, secret-hash salt, applied accessories
    lock                               the deploy lock
    store/<image>/                     the mirrored OCI layout of each image
    env/<release>[.<scope>].env        secrets, mode = "env"
    secrets/<release>[.<scope>]/<KEY>  secrets, mode = "file"
```

`<scope>` is a digest of the keys a service narrowed to; an unnarrowed service
uses the file without one. Images are in Docker as
`shunt/<project>-<image>:<release>`, and artifacts and captures are wherever the
manifest puts them.
