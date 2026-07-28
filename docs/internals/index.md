---
title: Internals
nav_order: 7
has_children: true
---

# Internals

shunt is two binaries and a wire contract.

The **CLI** you run resolves the manifest, builds images, transfers them, and
renders what the host reports. The **helper** runs on the host, is driven over
ssh, and does everything that mutates the machine. Between them is a single JSON
document — the release spec — carried on stdin, and a stream of NDJSON events
carried back on stdout.

```
cmd/shunt/           the CLI you run
  cli.go               flags, arg parsing, shared setup
  deploy.go            plan, up
  lifecycle.go         rollback, boot, retire
  inspect.go           status, logs, prune
  audit.go             audit, validate
  exec.go              exec, run
  fetch.go             pulling artifacts and captures back down
  bundle.go            bundle, apply
  bundle_inspect.go    bundle inspect, bundle verify
  init.go              scaffolding

cmd/shunt-helper/    uploaded to the host, driven over ssh, emits NDJSON events
  apply.go             the deploy pipeline
  images.go            OCI layout verification and load
  containers.go        naming, start/stop, blue-green swap
  proxy.go             discovery labels for Traefik / caddy-docker-proxy
  stages.go            one-shot containers, captures, secret materialisation
  artifacts.go         transactional promotion of data files and trees
  health.go            probes and gating
  ledger.go            release history, outcome classification, redaction
  exchange.go          RENAME_EXCHANGE, for atomic directory swaps
  runner.go            the seam that makes the deploy path testable
  inspect.go           status, logs, pruning

internal/manifest    shunt.toml types, loading, validation
internal/build       buildx to OCI layout
internal/oci         the content-addressed blob store inside a layout
internal/transport   rsync mirroring, in both directions, with transfer stats
internal/engine      manifest to release spec to plan to apply
internal/release     the CLI/helper wire contract and the on-host ledger
internal/bundle      the portable release archive
internal/secrets     secret providers and ${env:...} interpolation
internal/sshx        multiplexed ssh, honouring the user's own config
internal/ui          colour, banner, formatting — the only place ANSI exists
```

## On-host layout

```
~/.shunt/
  bin/shunt-helper-<sha256>          content-addressed helper binaries
  <project>/
    releases.json                    the ledger: history, salt, applied state
    lock                             helper-side flock
    deploy.lock                      CLI-side flock, held across the transfer
    env/<release>.env                secrets, mode = "env"
    secrets/<release>/<KEY>          secrets, mode = "file"
    store/<image>/                   the mirrored OCI layout
```

`SHUNT_ROOT` overrides `~/.shunt`.
