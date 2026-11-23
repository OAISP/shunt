---
title: Services
parent: Manifest reference
nav_order: 2
---

# Services

A service is a stateless container, replaced on every release.

```toml
[services.app]
image   = "app"
publish = ["127.0.0.1:9090:3000"]

[services.app.health]
url = "/health"

[services.worker]             # no port and no health check: it just runs
image   = "app"               # the same image with a different command
command = ["npm", "run", "worker"]
init    = true
```

| Key | Default | Meaning |
|:--|:--|:--|
| `image` | the service's name | An image from `[images.*]`, or a reference the server pulls. |
| `command` | the image's | Overrides the image's command. |
| `env` | none | Environment variables. Supports [`${env:NAME}`]({% link manifest/index.md %}#environment-interpolation). |
| `publish` | none | Host port mappings, in `docker run -p` form. |
| `expose` | none | A container port for a proxy to reach, with no host port. |
| `volumes` | none | Bind mounts and named volumes, in `docker run -v` form. |
| `restart` | `unless-stopped` | Docker restart policy. |
| `requires` | none | Services that must be healthy before this one starts. |
| `secrets` | all of them | The secret keys this service receives. |
| `drain` | `10s` | How long a stopped container gets to finish in-flight work. |
| `init` | `false` | Run a minimal init as PID 1. See [stopping cleanly](#stopping-cleanly). |
| `proxy` | none | Makes the service blue/green. See [Zero-downtime]({% link deploying/zero-downtime.md %}). |
| `health` | none | A readiness check that gates the release. |

## How a service is replaced

A deploy happens only when the plan finds a change, and then every service is
replaced and labelled with the new release.

Without a `proxy` block the replacement is in place. shunt creates the new
container first — that is where Docker rejects a bad mount, port or argument,
and at that point the old container is still serving. Then it stops the old one
with SIGTERM, waits up to `drain`, and starts the new one. The gap is the stop
plus your app's start-up time.

With a `proxy` block the new container starts alongside the old one, and the
old one is drained only after the new one is healthy. See
[Zero-downtime]({% link deploying/zero-downtime.md %}).

## Stopping cleanly

A container that does not exit on SIGTERM sits out its whole `drain` before it
is killed, and shunt says so:

```
acme-app ignored SIGTERM and was killed after 10s — handling it, or `init = true`
if it runs as PID 1, would cut that to near zero
```

The usual cause is a process running as PID 1 that never installed a SIGTERM
handler: the kernel gives PID 1 no default handlers, so the signal is ignored.
`init = true` runs a minimal init as PID 1 in front of your process (Docker's
`--init`), which forwards the signal so the default action applies. An app that
handles SIGTERM itself does not need it.

## Dependencies

`requires = ["api"]` makes a service wait for another: the required service is
started and health-checked first. Only services that something depends on are
gated this way; the rest start without waiting on each other, and the release's
final health check covers them all. Accessories are always up before any
service starts, so requiring one only documents the dependency.

A dependency cycle is rejected when the manifest loads.

## Health checks

```toml
[services.app.health]
url      = "/health"    # or a full URL; or `command` instead
retries  = 10
interval = "3s"
grace    = "2s"         # wait before the first probe
follow   = false
```

| Key | Default | Meaning |
|:--|:--|:--|
| `url` | — | Probed with an HTTP GET from the server. A 2xx is healthy. |
| `command` | — | Run inside the container instead; exit 0 is healthy. |
| `retries` | `10` | Attempts before the release fails. |
| `interval` | `3s` | Time between attempts. |
| `grace` | none | Time to wait before the first attempt. |
| `follow` | `false` | Follow redirects and require a 2xx at the end. |

A `url` that is a bare path is probed wherever the service is reachable: its
published host port — on the bind address you gave it, so `10.0.0.5:9090:3000`
is probed on `10.0.0.5` — or else its address on the deploy network, on the
`expose` port. A path therefore needs one of the two.

`command` suits services that speak no HTTP:

```toml
[accessories.db.health]
command = ["pg_isready", "-U", "acme"]
```

{: .warning }
Without `follow`, a 3xx counts as healthy, which proves only that something is
listening. An app whose `/` redirects to a locale prefix passes that check while
broken behind the redirect. Set `follow = true`, or point `url` at a page that
answers 2xx.

A container that exits during start-up fails the check at once rather than
using up its retries, and the error includes its last 20 log lines.

## Scoping secrets

Every service receives every secret unless it says otherwise:

```toml
[services.worker]
image   = "app"
secrets = ["DATABASE_URL"]   # not STRIPE_KEY
```

Each distinct set of keys is written to its own file on the server, so the
narrowing holds in `docker inspect` as well as inside the container.

## Removing a service

Deleting a service from the manifest does not stop its container, because a
deploy never stops a container it was not asked to replace. `shunt plan`
reports it as `orphaned`, and `shunt retire <service>` removes it. An orphan
does not count as a change, so it does not leave every plan dirty.
