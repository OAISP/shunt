---
title: Zero-downtime
parent: Deploying
nav_order: 1
---

# Zero-downtime

A service with a published host port is replaced in place: the old container
has to stop before the new one can bind the port. The gap is the stop plus your
app's start-up time — measured at 0.29 s for an app that boots in 200 ms, and
seconds for a typical Next.js or Rails app.

Give the service a `proxy` block and it goes blue/green instead. The new
release starts alongside the old one, is health-checked, and only then is the
old one drained.

```toml
[services.app]
image  = "app"
expose = 3000     # a container port, no host port: two releases can run at once
drain  = "10s"    # how long the old release gets to finish in-flight requests

[services.app.proxy]
kind        = "traefik"          # traefik (default) | caddy
host        = "app.example.com"
entrypoints = ["web"]
# path      = "/api"             # a path prefix; default "/"
# port      = 3000               # default: expose
# retry     = 2                  # default; reissues requests that got no answer

[services.app.health]
url = "/health"
```

A proxied service needs `expose` (or `proxy.port`) instead of `publish`, and a
health check — without one a broken release would go straight into rotation.
shunt probes the new container on its own address, using only the path of a
full `url`: through the proxy, the container being replaced would answer.

shunt does not run a proxy. It sets the container labels that Traefik and
caddy-docker-proxy already watch, so the proxy does the switchover and shunt
adds no long-running component and no certificates to manage.

Measured through Traefik, with the app under load for the whole deploy:

| | Requests | Failures |
|:--|--:|--:|
| Published port, replaced in place | 5648 | 23 — a 0.29 s outage |
| Blue/green, app ignores SIGTERM | 4031 | 3 |
| Blue/green, app handles SIGTERM | 4018 | 2 |
| Blue/green, handles SIGTERM, `retry` on | 4130 | **0** |

## What your app has to do

The last rows of that table are up to the app; no deploy tool can supply them.

1. **Handle SIGTERM.** On the signal, start failing the health check so the
   proxy takes you out of rotation, finish the requests in flight, then exit.
   A process running as PID 1 with no handler ignores SIGTERM entirely and is
   killed at the end of `drain`; [`init = true`]({% link manifest/services.md %}#stopping-cleanly)
   fixes that without touching the app.
2. **Make migrations expand-then-contract.** Stages run before the swap, so
   during the overlap the old release runs against the new schema. Dropping a
   column the old release still reads turns a clean switchover into a burst of
   errors.

## When the proxy cannot check readiness

The proxy, not shunt, decides when the new container receives traffic, so it
needs something to poll. shunt gives Traefik and caddy an active health check
whenever the service's health check is a `url` — the path of a full URL is used.

A `command` health check cannot be expressed to either proxy. Such a service
still overlaps, and `retry` covers a backend that is not listening yet, but
nothing holds back one that is listening and still warming up. `shunt plan`
says so:

```
~ app            blue/green  (starts alongside; proxy cannot poll this health check)
```

Use a `url` health check to close that gap.

## Changing the proxy block

A router discovered from labels is defined by every container that carries
them, and Traefik drops a router that two containers define differently — a
full outage, not a blip. So a deploy that changes the `proxy` block, or the path
of the health check the proxy polls, stops the old container before starting
the new one, once. `shunt plan` says so, and the next deploy overlaps normally
again.

## Behind a proxy that is not Traefik or caddy

Blue/green needs a proxy that finds containers by their labels, and a proxy
configured from a file — a host nginx, HAProxy, a cloud load balancer — cannot.
You do not have to replace it. Keep it, and put one of the two label-driven
proxies between it and your services, as an accessory:

```toml
[accessories.edge]
image   = "traefik:v3"
command = ["--providers.docker=true", "--providers.docker.exposedbydefault=false",
           "--entrypoints.web.address=:80"]
publish = ["127.0.0.1:8080:80"]
volumes = ["/var/run/docker.sock:/var/run/docker.sock:ro"]

[services.app]
image  = "app"
expose = 3000

[services.app.proxy]
host        = "app.example.com"
entrypoints = ["web"]

[services.app.health]
url = "/health"
```

The existing proxy keeps its TLS and its configuration and points at one
upstream that never changes, `127.0.0.1:8080`; make sure it passes the `Host`
header through. The accessory is booted once and left alone by deploys, and the
switchover happens behind it. Nothing has to reload the host proxy, so the
deploy user needs no extra privileges.
