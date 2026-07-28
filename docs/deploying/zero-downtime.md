---
title: Zero-downtime
parent: Deploying
nav_order: 1
---

# Zero-downtime

A service with a published host port must be stopped before its replacement can
bind the same port. Measured on a live deploy, that gap was 0.29 s for an app
booting in 200 ms — the gap is essentially your app's cold-boot time, so a
Next.js or Rails app will be seconds.

Give a service a `proxy` block and it goes blue/green instead: the new release
starts alongside the old one, is health-checked, and the old one drains only
once the new one is serving.

```toml
[services.app]
image  = "app"
expose = 3000     # no host port — that is what allows two releases at once
drain  = "10s"    # SIGTERM, then this long to finish in-flight work

[services.app.proxy]
kind        = "traefik"          # traefik | caddy
host        = "app.example.com"
entrypoints = ["web"]
# path      = "/api"             # optional PathPrefix
# port      = 3000               # defaults to `expose`
# retry     = 2                  # default; reissues requests never answered

[services.app.health]
url = "/health"
```

shunt does not run a proxy. It emits the labels Traefik and caddy-docker-proxy
already watch for, so the switchover is the proxy's job and shunt gains no
long-lived component and no certificate management.

Measured through Traefik, hammering the app throughout a deploy:

| | Requests | Failures |
|:--|--:|--:|
| Published port (stop-then-start) | 5648 | 23 — 0.29 s outage |
| Blue/green, app ignores SIGTERM | 4031 | 3 |
| Blue/green, graceful SIGTERM | 4018 | 2 |
| Blue/green, graceful + retry | 4130 | **0** |

## What your app has to do

Zero-downtime is a contract, not a flag. The last two rows above are entirely
app-side, and no deploy tool can supply them for you.

1. **Handle SIGTERM.** On signal, start failing your health check so the proxy
   takes you out of rotation, keep serving in-flight requests, then exit. A
   process that dies instantly on SIGTERM drops whatever was in flight.
2. **If you run schema migrations, make them expand/contract.** Stages run
   before the swap, so during the overlap both releases talk to the migrated
   schema. Dropping a column the old release still reads turns a clean outage
   into a stream of 500s. Projects without migrations can ignore this.

A container that burns its whole drain window did not exit on SIGTERM and was
killed. shunt says so, because it is usually the largest single cost in a deploy
and is otherwise invisible:

```
shuntbig-app ignored SIGTERM and was killed after 10s — handling it would cut
that to near zero
```

## When the proxy cannot gate readiness

A proxied service is put into rotation by the proxy, not by shunt, so the proxy
needs something it can poll to know the new container is ready. shunt emits an
active health check — for Traefik and for caddy — whenever the health block
gives it a path to poll, taking the path from an absolute url if that is how it
is written.

A **command** health check cannot be expressed to either proxy. Such a service
still overlaps, and `retry` still covers a backend that is not listening yet,
but nothing covers one that is listening and still warming up. `shunt plan` says
so rather than leaving it implicit:

```
~ app            blue/green  (starts alongside; proxy cannot poll this health check)
```

Give the service a `url` health check to close that gap.

## Changing the proxy config

A label-discovered router is defined by *every* container carrying its labels,
and Traefik refuses to serve a router that two containers define differently. It
drops the route entirely, which is a total outage rather than a blip.

So whenever the `proxy` block changes, shunt detects it and degrades that one
deploy to stop-then-start. `shunt plan` says so, and you take a short gap
instead of a dropped route. Steady-state deploys, where the block is unchanged,
overlap normally.

Services without a `proxy` block — workers, anything with no HTTP port — are
stopped gracefully and restarted. They need none of this.
