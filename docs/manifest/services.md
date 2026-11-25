---
title: Services
parent: Manifest reference
nav_order: 2
---

# Services

A service is a stateless container, replaced whenever a release changes it.

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
| `restart` | `unless-stopped` | Docker restart policy: `no`, `always`, `unless-stopped` or `on-failure[:N]`. |
| `requires` | none | Services that must be healthy before this one starts. |
| `secrets` | all of them | The secret keys this service receives. |
| `drain` | `10s` | How long a stopped container gets to finish in-flight work. |
| `init` | `false` | Run a minimal init as PID 1. See [stopping cleanly](#stopping-cleanly). |
| `memory` | unlimited | A memory limit, such as `512m` or `1g`. See [limits](#limits). |
| `memory_swap` | twice `memory` | Memory plus swap, such as `1536m`; `-1` for unlimited swap. |
| `cpus` | unlimited | How many CPUs the container may use, such as `1.5`. |
| `pids_limit` | unlimited | The most processes the container may hold. |
| `log_driver`, `log_opts` | the daemon's | Where logs go, and how they rotate. See [limits](#limits). |
| `read_only`, `tmpfs` | `false`, none | A read-only root filesystem, and the paths that stay writable in memory. |
| `cap_drop`, `cap_add` | Docker's set | Linux capabilities to take away or grant. |
| `no_new_privileges` | `false` | Stop processes gaining privileges through setuid binaries. |
| `proxy` | none | Makes the service blue/green. See [Zero-downtime]({% link deploying/zero-downtime.md %}). |
| `health` | none | A readiness check that gates the release. |

## How a service is replaced

A deploy happens only when the plan finds a change, and it replaces only the
services that change reaches. A service keeps its container when all of these
are as they were:

- its own definition in `shunt.toml`;
- what its image runs — the layers and runtime settings, not the labels or
  timestamps a rebuild stamps on it;
- the secrets it receives, the deploy network and the secret delivery mode.

A replaced [artifact]({% link manifest/artifacts.md %}) replaces every service,
because a container keeps the file or directory it mounted, not the one swapped
into its place. `shunt plan` lists each service as `unchanged` or with the
reasons it will be replaced, and `shunt up` does what the plan said.

Two things override that, because matching what the release asks for is not the
same as working:

- A service with a `health` check is probed once before its container is kept.
  If the probe fails, the container is replaced and the new one has to pass the
  check like any other, so a release is never reported active over a service
  that is down.
- After a deploy or rollback that did not end cleanly — it failed after
  replacing a container, or was cut off by a reboot or a killed process — the
  next one replaces every service. A container the failed attempt started looks
  exactly like a healthy one, and a service it never reached may still hold an
  artifact that was replaced under it. `shunt status` and `shunt plan` both say
  when this applies.

{: .note }
A service on an image the server pulls is also replaced when the pull brings a
different image under the same tag, which a plan cannot know in advance. And the
first deploy after upgrading from shunt 0.4 or older replaces every service
once: their containers predate the label this comparison reads.

`shunt status` shows, for each container, the release that started it, so an
unchanged service lists an older release than the one serving.

Without a `proxy` block the replacement is in place. shunt creates the new
container first — that is where Docker rejects a bad mount, port or argument,
and at that point the old container is still serving. Then it stops the old one
with SIGTERM, waits up to `drain`, and starts the new one. The gap is the stop
plus your app's start-up time.

With a `proxy` block the new container starts alongside the old one, and the
old one is drained only after the new one is healthy. See
[Zero-downtime]({% link deploying/zero-downtime.md %}).

## Stopping cleanly

A container still running at the end of its `drain` is killed, and shunt says
so:

```
acme-app was still running after its 10s drain and was killed — it either ignores
SIGTERM (handle it, or set `init = true` if it runs as PID 1) or had work in
flight (a longer `drain` lets it finish)
```

There are two causes, and shunt cannot tell them apart. One is a process
running as PID 1 that never installed a SIGTERM handler: the kernel gives PID 1
no default handlers, so the signal is ignored. `init = true` runs a minimal init
as PID 1 in front of your process (Docker's `--init`), which forwards the signal
so the default action applies. The other is an app that handles SIGTERM and was
waiting for a long request — a long poll, an upload, a server-sent event stream.
That one needs a longer `drain`, or to end such requests when it is told to stop.

If Docker cannot stop the container at all, shunt reports Docker's error and
that the container was killed without draining.

## Limits

Each of these is the Docker flag of the same name, set when the container is
created, so it never runs without them. A changed limit shows in `shunt plan`
and applies on the next deploy; a rollback restores the limits of the release it
returns to. On an [accessory]({% link manifest/accessories.md %}#changing-one) a
changed limit needs `shunt boot`, like any other change to one.

```toml
[services.app]
image      = "app"
cpus       = 1.5
pids_limit = 200
log_opts   = { max-size = "10m", max-file = "3" }

read_only         = true
tmpfs             = ["/tmp"]
cap_drop          = ["ALL"]
cap_add           = ["NET_BIND_SERVICE"]
no_new_privileges = true
```

**Logs.** Docker's default `json-file` driver never rotates, so a chatty
service fills the disk in the end. `log_opts` are the driver's own options:
`max-size` and `max-file` cap `json-file` and `local`. `log_driver` picks
another driver for this service. An option the driver does not know is rejected
by Docker when the container is created, which leaves the running one serving.

**Hardening.** `read_only` mounts the root filesystem read-only; list the paths
the app writes to under `tmpfs` (optionally with options, `"/tmp:size=64m"`), or
mount a volume. `cap_drop = ["ALL"]` with the few capabilities the app needs in
`cap_add` is least privilege, and `no_new_privileges` stops a setuid binary
handing them back.

### Memory

A container with no limit can take the whole server into swap, and its
neighbours with it. `memory` caps one:

```toml
[services.app]
image       = "app"
memory      = "1g"
memory_swap = "1536m"    # 1g of memory plus 512m of swap
```

Both are Docker's `--memory` and `--memory-swap`. A size is a whole number with `b`, `k`,
`m` or `g`, and at least `6m`. `memory_swap` is the total of memory and swap,
not the swap alone: set it equal to `memory` to allow no swap, or to `-1` to
allow as much as the server has. Left out, Docker allows as much swap again as
`memory`.

A process that exceeds its limit is killed by the kernel, and the container's
`restart` policy starts it again.

## Dependencies

`requires = ["api"]` makes a service wait for another: the required service is
started and health-checked first. Only services that something depends on are
gated this way; the rest start without waiting on each other, and are checked
together once they are all started. Accessories are always up before any
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

One probe is given up after 30 seconds and counts as a failed attempt, so a
check that hangs cannot hold a deploy open.

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

### Without a health check

A service with no `health` block — a worker with no port, typically — is still
held to staying up. Five seconds after the last container starts, each one must
be running and never restarted, or have exited 0 and stayed exited, as a
one-shot does. One that crashed or is restarting fails the release with its last
20 log lines, and `--rollback-on-failure` applies as for a failed health check.
The wait is shared, not per service, and time spent probing other services
counts toward it.

It is a floor, not a health check: a process that takes longer than that to fail
passes it.

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
