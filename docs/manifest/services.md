---
title: Services
parent: Manifest reference
nav_order: 2
---

# Services

A service is a stateless container replaced on every release.

```toml
[services.app]
image   = "app"
publish = ["127.0.0.1:9090:3000"]
restart = "unless-stopped"

[services.app.health]
url = "/health"

[services.worker]           # no port, no health check — it just runs
image   = "app"             # same image, different command
command = ["npm", "run", "worker"]
```

| Key | Default | Meaning |
|:--|:--|:--|
| `image` | the service name | An image from `[images.*]`, or a pullable reference. |
| `command` | image default | Overrides the image's command. |
| `env` | none | Environment variables. `${env:NAME}` interpolates locally. |
| `publish` | none | Host port mappings, as `docker run -p` accepts them. |
| `expose` | none | Container port for a proxy to reach. No host port. |
| `volumes` | none | Bind mounts and named volumes. |
| `restart` | `unless-stopped` | Docker restart policy. |
| `requires` | none | Services or accessories that must be ready first. |
| `secrets` | all of them | Narrows which secrets this service receives. |
| `drain` | `10s` | SIGTERM, then this long to finish in-flight work. |
| `proxy` | none | Makes the service blue/green. See [zero-downtime]({% link deploying/zero-downtime.md %}). |
| `health` | none | Readiness probe. Gates the release. |

## Dependencies

`requires = ["db"]` is a readiness edge, not only an ordering one. A service
that something else requires is health-checked before its dependents start, so a
worker declaring `requires = ["db"]` will not be running and failing against a
database that has not finished booting.

Only services with dependents are gated this way. Gating every service would
serialise unrelated startups behind each other's boot time for no benefit, and
the final health gate covers them anyway.

Cycles are rejected at validation time, naming the cycle.

## Health checks

```toml
[services.app.health]
url      = "/health"    # or a full URL, or use `command` instead
retries  = 10
interval = "3s"
grace    = "2s"         # wait before the first probe
follow   = false
```

`url` may be a full URL or a bare path. A path is resolved against whatever the
service is actually reachable on: its published host port, or its address on the
deploy network when it only exposes a port. A published mapping that names a
bind address is honoured — a service published on `10.0.0.5:9090:3000` is probed
there, not on loopback.

`command` runs inside the container instead, for services that speak no HTTP:

```toml
[accessories.db.health]
command = ["pg_isready", "-U", "acme"]
```

{: .warning }
By default a 3xx counts as healthy, which proves only that the server is
listening. Set `follow = true` to chase redirects and require a 2xx at the end.
An app whose `/` redirects to a locale prefix passes the default check while
being completely broken behind the redirect.

A container that exits during startup fails immediately rather than burning the
whole retry budget, and the error carries the container's own last 20 log lines.

## Scoping secrets

By default every service and stage receives every resolved secret. A service can
narrow that:

```toml
[services.worker]
image   = "app"
secrets = ["DATABASE_URL"]   # not STRIPE_KEY
```

Each distinct scope gets its own file on the host, so the narrowing holds
against `docker inspect` as well as against the process environment.

## Removing a service

Deleting a service from the manifest does not stop its container: shunt does not
stop containers it was not asked to stop. `shunt plan` reports it as `orphaned`
and names the fix, and `shunt retire <service>` acts on it.

An orphan is deliberately not counted as work, so it does not leave the plan
permanently dirty with a change no `shunt up` would ever make.
