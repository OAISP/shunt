```
█▀▀ █ █ █ █ █▄ █ ▀█▀
▄▄█ █▀█ █▄█ █ ▀█  █
```

[![ci](https://github.com/OAISP/shunt/actions/workflows/ci.yml/badge.svg)](https://github.com/OAISP/shunt/actions/workflows/ci.yml)
[![license](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

**Registry-free Docker deploys over ssh.** One app, one server, no orchestrator.

```sh
shunt up
```

Builds your image, ships **only the layers the host doesn't already have**, swaps
the containers, and fails the release if the app doesn't come up healthy.

A working manifest is about ten lines. Databases, migrations, data files and
zero-downtime swaps are all available and all optional — declare them when you
need them, and shunt skips them entirely when you don't.

---

## Contents

- [Why](#why) · [Install](#install) · [Quick start](#quick-start)
- [How a deploy runs](#how-a-deploy-runs) · [Configuration](#configuration) · [Commands](#commands)
- [Zero-downtime](#zero-downtime) · [Secrets](#secrets) · [Security](#security)
- [Non-goals](#non-goals) · [Internals](#internals) · [Development](#development)

---

## Why

Deploying a locally-built image usually means pushing it to a registry so the
server can pull it back down — your bytes cross the network twice, and you
maintain credentials for a warehouse you never wanted.

The usual workaround, `docker save | ssh docker load`, re-uploads the **entire
image every single deploy**, base layers and all.

shunt exports an OCI layout, where each blob's filename *is* its content hash,
and mirrors it with rsync. Files the host already has are skipped. That is
layer-level deduplication with no registry and no protocol to implement.

Measured on an 81 MB Node image, changing one source file:

| | `docker save \| zstd` | shunt |
|:--|--:|--:|
| first deploy | 81.1 MB | 81.3 MB |
| code-only redeploy | 81.1 MB | **5.3 KB** |

Real applications have a fatter top layer than that test app, so expect **30–100×**
on a typical Next.js or Rails image rather than four orders of magnitude. The
point isn't the ratio — it's that you stop paying for the base image on every
deploy.

## Install

**Installer** — detects your platform, verifies the checksum, needs no toolchain:

```sh
curl -sSL https://raw.githubusercontent.com/OAISP/shunt/main/install.sh | sh
```

It never invokes sudo: it installs to `/usr/local/bin` when that is writable and
`~/.local/bin` otherwise. Pin a release with `SHUNT_VERSION=v0.1.0`, choose a
directory with `PREFIX=...`, and read it first if you would rather not pipe a
script into a shell — that is a reasonable instinct for a tool that ends up
holding ssh access to your servers.

**Or grab the archive directly** — built for `linux_amd64`, `linux_arm64`,
`darwin_amd64`, `darwin_arm64`, with `checksums.txt` alongside:

```sh
curl -sSL https://github.com/OAISP/shunt/releases/latest/download/shunt_linux_amd64.tar.gz | tar xz
sudo install -m755 shunt /usr/local/bin/shunt
```

**With Go:**

```sh
go install github.com/OAISP/shunt/cmd/shunt@latest
```

Note the `/cmd/shunt` suffix — the module root is a library, not a command. A
`go install` build carries no prebuilt host helper, so shunt compiles one from
its own source the first time it connects to a host. That takes a second and
uses the Go toolchain you already have; the helper is cached on the host
afterwards.

**From source:**

```sh
git clone https://github.com/OAISP/shunt && cd shunt
make install          # builds ./shunt with helpers embedded, installs to ~/.local/bin
```

Requirements — **your machine:** docker (with buildx), rsync, ssh.
**The server:** docker, rsync, `tar`, `curl`, and an ssh account that can reach
the docker socket. Nothing else; shunt uploads its own helper. `tar` streams the
image layout into `docker load`; `curl` runs url health checks. Both are present
on essentially every distro image, but not on some minimal ones — `shunt audit`
checks for them, and so does every command that connects.

rsync 3.2 or newer on **both** ends gets zstd transfer compression. Older
versions (Ubuntu 20.04 ships 3.1.3; macOS ships 2.6.9 as `/usr/bin/rsync`) still
work — shunt detects them and falls back rather than failing.

Either image store works on either end. shunt writes a layout that both the
containerd snapshotter and the classic overlay2 store can load, so a laptop
running the newer store can deploy to a server running the older one without any
daemon configuration.

## Quick start

```sh
cd my-project
shunt init --host deploy@vps.example.com    # writes shunt.toml, guesses the port
shunt plan                                  # see exactly what would change
shunt up
```

`shunt plan` builds and diffs but changes nothing on the host. Read it before
your first `up`.

## How a deploy runs

```
build ─→ ship ─→ accessories ─→ stages ─→ artifacts ─→ services ─→ health
                 ╰──────── declare only if needed ────╯   ╰─── always ───╯
```

Most projects use the two ends and nothing in the middle: build an image, ship
it, run it, check it. The middle three steps do not exist unless the manifest
declares them, and a deploy that declares none skips straight from ship to
services.

Where they are used, the ordering *is* the safety property:

1. **Accessories** come up first, so anything after them has its dependencies
   available.
2. **Stages** are one-shot containers that must all succeed **before any running
   service is touched** — whatever they happen to do. If one fails, production is
   exactly as it was.
3. **Artifacts** are swapped in as late as possible: the rename is the point of
   no return for data, and it happens while the old container still holds the
   old inode, so its readers are unaffected until the restart.
4. **Services** are replaced only once everything before them has passed.
5. **Health** gates the release. Containers that never come up healthy mean a
   failed release, not a silent one.

## Configuration

`shunt.toml` is the committed description of production. Everything the host runs
derives from this file plus resolved secrets — there are no deploy-time flags
that change *what* gets deployed.

This is a complete, working manifest:

```toml
project = "acme"
host    = "deploy@vps.example.com"

[images.app]
context = "."

[services.app]
image   = "app"
publish = ["127.0.0.1:9090:3000"]

[services.app.health]
url = "/health"
```

`shunt init` writes roughly that for you. Everything below is optional — each
block does nothing at all unless you declare it, and plenty of projects never
need any of them.

Unknown keys are a hard error. A typo that silently does nothing is worse than a
failed load.

### Build options

```toml
[images.app]
context    = "."
dockerfile = "Dockerfile"
platform   = "linux/amd64"          # set this when building on an arm64 Mac
target     = "production"           # a specific multi-stage target
args       = { PUBLIC_URL = "${env:PUBLIC_URL}" }
```

Several images in one manifest is fine — declare `[images.<name>]` for each and
point services at them by name.

### More than one service

```toml
[services.app]
image   = "app"
publish = ["127.0.0.1:9090:3000"]

[services.worker]           # no port, no health check — it just runs
image   = "app"             # same image, different command
command = ["npm", "run", "worker"]
```

`requires = ["other"]` orders startup when it matters, and waits: a service that
something else requires is health-checked before its dependents start, so
`requires` is a readiness edge rather than only an ordering one.

### Secret values

Only if your app needs values you would not commit.

```toml
[secrets]
provider = "file"                   # file | env | sops
path     = "secrets/prod.env"
```

They reach every service and stage as an env-file on the host. See
[Secrets](#secrets) for how they get there.

### Accessories

Only if the host should run a stateful dependency for you — a database, a cache,
a model server. If yours already runs elsewhere (managed Postgres, another host,
an existing container), skip this entirely and point your app at it with a
secret.

```toml
[accessories.db]
image   = "postgres:18-alpine"
volumes = ["acme-pgdata:/var/lib/postgresql"]
[accessories.db.health]
command = ["pg_isready", "-U", "acme"]
```

### Stages

Only if something must run to completion *before* your services are replaced. A
stage is a one-shot container; what it does is up to you. Database migrations and
pre-deploy backups are the common cases, but a stage is just "run this, and stop
the deploy if it fails":

```toml
[[stages]]
name    = "migrate"
image   = "app"                     # any image in this manifest, or a pullable ref
command = ["npm", "run", "migrate"]
```

`capture` redirects a stage's stdout to a file on the host, and
`require_nonempty` fails the deploy if that file comes out empty — which is what
makes a pre-migration dump trustworthy rather than decorative:

```toml
[[stages]]
name             = "backup"
image            = "postgres:18-alpine"
command          = ["sh", "-c", "exec pg_dump --no-owner \"$DATABASE_URL\""]
capture          = "/opt/acme/backups/pre-migrate-{{.Release}}.sql"
require_nonempty = true
retain           = 10               # keep the 10 most recent
```

### Artifacts

Only if your app needs a large file the image does not carry — a database file,
model weights, a prebuilt index, a dataset. If your state lives in a database
server or object storage, you do not need this.

shunt ships artifacts over the same rsync transport as the image and swaps them
in atomically.

```toml
[[artifacts]]
name     = "index"
src      = "data/index.db"                 # local, relative to shunt.toml
dest     = "/opt/acme/data/index.db"       # on the host
magic    = "SQLite format 3"               # optional: refuse a file that is not one
retain   = 1                               # generations kept for recovery
required = true                            # absent locally fails the deploy

[services.app]
volumes = ["/opt/acme/data:/data:ro"]
```

An artifact may be a **directory** rather than a file — model weights, a
prebuilt index, an asset tree. Point `src` at the directory and `dest` at where
it should live; the whole tree is mirrored and swapped in as a unit. Unchanged
files inside it are not re-sent.

```toml
[[artifacts]]
name = "weights"
src  = "models/embed"                  # a directory
dest = "/opt/acme/models/embed"
```

`magic` is a literal prefix the file must start with. It is worth setting for any
format that has one, because it is what turns a truncated upload into a failed
deploy instead of an outage.

The new copy lands beside the old under a `.new` suffix, is checked for size and
`magic`, and only then renamed into place — a rename within a directory, so it is
atomic. The superseded copy is kept as `<dest>.prev.<release-id>`, and a failed
health check prints the exact `mv` that puts it back.

Transfers are incremental. Measured on a 5.5 MB file with a small change:

| | on the wire |
|:--|--:|
| whole file | 1.5 MB |
| rsync delta | **31 KB** |

That delta only works because shunt passes `--fuzzy`: the file is staged under a
name that does not exist yet, so rsync has nothing to diff against unless it is
told to find the current copy next door. Without it, every deploy resends the
whole artifact — which is easy to do by accident and hard to notice.

A data-only change is a deploy in its own right: regenerate the file, run
`shunt up`, and only the changed blocks move — no image is rebuilt.

**`shunt rollback` does not revert data.** It restores containers and images.
Silently rolling a database back as a side effect of rolling code back is the
kind of helpfulness that loses data — the `.prev` copy and the exact command are
right there instead.

### Deploy targets

Only if you deploy the same project to more than one server — staging and
production, typically.

```toml
project = "acme"
host    = "deploy@prod.example.com"

[targets.staging]
host = "deploy@staging.example.com"
# project = "acme-stg"          # defaults to "acme-staging"
# [targets.staging.secrets]     # staging should not hold production credentials
# provider = "file"
# path     = "secrets/staging.env"
```

```sh
shunt up                 # the host at the top of the file
shunt up -t staging      # the staging target
```

A target changes **where** a release goes, never **what** it contains: the
images, services, stages and artifacts are the ones declared above, whichever
target you select. That keeps the rule that no deploy-time flag alters what gets
deployed.

The project name is namespaced per target by default (`acme` → `acme-staging`),
so two targets can share one machine without colliding on container names, the
network, or the ledger.

### Scoping secrets to a service

By default every service and stage receives every resolved secret. A service can
narrow that:

```toml
[services.worker]
image   = "app"
command = ["npm", "run", "worker"]
secrets = ["DATABASE_URL"]        # not STRIPE_KEY
```

Each distinct scope gets its own `0600` env-file on the host, so the narrowing
holds against `docker inspect` too, not just against the process environment.

### Accessories vs services

A **service** is stateless and replaced on every release. An **accessory** is
stateful — a database, a cache, a model server — booted once and then left
alone. Shipping a one-line CSS fix must not restart Postgres.

Change an accessory's definition and `shunt plan` reports the drift without
acting on it. Applying it is the explicit, destructive `shunt boot db`.

### Getting data back

rsync does not care which end is the source, so the transport works in both
directions:

```sh
shunt fetch                       # list what can be fetched
shunt fetch index                 # pull the artifact back over its local source
shunt fetch index -o /tmp/prod.db # ...or somewhere else
shunt fetch /opt/acme/backups/pre-migrate-20260728.sql
```

Incrementally, like everything else: refreshing a stale local copy of a 500 MB
database moves only the blocks that changed. Useful for working against real
data, and for retrieving the pre-migration dump a `capture` stage produced.

## Bundles

Sometimes the machine that can build a release cannot reach the machine that
should run it — an air-gapped network, a change-approval queue, a laptop with
the source but no VPN.

```sh
shunt bundle                          # writes <project>-<release>.shuntpkg
scp acme-….shuntpkg ops:              # or a USB stick, or an approval queue

shunt bundle inspect acme-….shuntpkg  # what is in it — instant, whatever the size
shunt bundle verify  acme-….shuntpkg  # rehash every blob; needs no host
shunt apply --plan   acme-….shuntpkg  # what applying would change
shunt apply          acme-….shuntpkg
```

`shunt bundle inspect` is the command for someone handed a bundle to approve:
which release, from which commit and whose build, for which host, running what,
and which secrets it will want. It reads only the description — that is the
first entry in the archive — so it answers immediately on a bundle of any size,
and it says so loudly when the build came from a modified working tree, since
that is precisely when the commit id describes nothing.

`shunt bundle verify` rehashes every blob against its own filename. That is the
same check the host performs on load, run without a host, so a bundle can be
proven intact before being carried somewhere retrying is expensive. It proves
the bytes are intact and nothing more.

`shunt apply --plan` answers "what will this do to my host" before it does it,
with the same output and the same exit codes as `shunt plan`. It works even
where the secrets are not reachable — it says the secret diff was skipped rather
than reporting every key as removed.

`shunt bundle` does everything `shunt up` does up to the transfer and writes the
result to a file instead of a host. It never connects, so it works with the
target unreachable.

`shunt apply` needs **no Dockerfile, no shunt.toml and no source** — and no
Docker at all on the machine that runs it. It deploys through exactly the same
path as `shunt up`: the same lock held across the transfer, the same check that
the host has not moved on since the plan, the same helper. Layer deduplication
still applies, so re-applying a bundle to a host that already has most of the
image ships only the difference.

A bundle carries the release spec, the OCI layouts and any artifacts. It
deliberately does **not** carry:

- **Secret values.** A file that sits in a queue or on a stick is the last place
  production credentials belong, and encrypting them into it only moves the key
  problem. The provider block travels instead, and `shunt apply` resolves the
  values locally — so whoever applies a bundle needs access to the secrets,
  which is the correct requirement.
- **The host helper.** The CLI applying the bundle already embeds one.
- **A checksum of its own.** Every blob inside is content-addressed and rehashed
  on load, so a damaged bundle fails on the way in.

Bundles are always complete rather than a delta against a particular host: a
delta is only valid against the state it was computed from, and that is not a
promise a file sitting in a queue can keep.

Applying the same bundle twice is refused — release ids are immutable, and the
host has already recorded that one. Deploy elsewhere with `--host`.

## Commands

| command | |
|:--|:--|
| `shunt init` | scaffold a `shunt.toml`, guessing the port from `EXPOSE` |
| `shunt validate` | check the manifest offline — no ssh, no build |
| `shunt audit` | check everything a deploy needs, and change nothing |
| `shunt plan` | build, then diff the manifest against the host |
| `shunt up` | apply — the full pipeline above |
| `shunt status` | what's running, and the release history |
| `shunt rollback [id]` | restore the previous, or a named, release |
| `shunt boot <accessory>` | recreate a stateful accessory — destructive |
| `shunt exec <service> -- <cmd>` | run a command in the running container |
| `shunt run <service> -- <cmd>` | run a one-off command in a fresh container |
| `shunt retire <service>` | stop a service you removed from the manifest |
| `shunt logs [service]` | tail container logs, prefixed when there are several |
| `shunt prune` | drop superseded images on the host |

`--json` on any command gives machine-readable output; `-v` adds build output and
per-step detail. `SHUNT_NO_BANNER=1` and [`NO_COLOR=1`](https://no-color.org) are
respected, and both are implied when output isn't a terminal.

### Rollback

Each release is tagged immutably on the host as
`shunt/<project>-<image>:<release-id>`, and the last `retain` (default 5) are
kept. Rolling back re-runs the previous release's images — no rebuild, no
transfer, a couple of seconds.

```sh
shunt rollback                     # previous successful release
shunt rollback 20260726-225110     # a specific one
```

Releases that failed are skipped when picking "previous" — rolling onto one would
just repeat the outage.

## Zero-downtime

A service with a published host port must be stopped before its replacement can
bind the same port. Measured on a live deploy that gap was **0.29 s** for an app
booting in 200 ms — the gap is essentially your app's cold-boot time, so a
Next.js or Rails app will be seconds.

Give a service a `proxy` block and it goes blue/green instead: the new release
starts *alongside* the old one, is health-checked, and the old one drains only
once the new one is serving.

```toml
[services.app]
image  = "app"
expose = 3000        # no host port — that is what allows two releases at once
drain  = "10s"       # SIGTERM, then this long to finish in-flight work

[services.app.proxy]
kind        = "traefik"     # traefik | caddy
host        = "app.example.com"
entrypoints = ["web"]
# retry     = 2             # default; reissues requests the backend never answered

[services.app.health]
url = "/health"      # a bare path is probed against the container's own IP
```

shunt does not run a proxy. It emits the labels Traefik and caddy-docker-proxy
already watch for, so the switchover is the proxy's job and shunt gains no
long-lived component and no certificate management.

Measured through Traefik, hammering the app throughout a deploy:

| | requests | failures |
|:--|--:|--:|
| published port (stop-then-start) | 5648 | 23 — 0.29 s outage |
| blue/green, app ignores SIGTERM | 4031 | 3 |
| blue/green, graceful SIGTERM | 4018 | 2 |
| blue/green, graceful + retry | 4130 | **0** |

### When the proxy cannot gate readiness

A proxied service is put into rotation by the proxy, not by shunt, so the proxy
needs something it can poll to know the new container is ready. shunt emits an
active health check — for Traefik and for caddy — whenever the health block
gives it a path to poll, including an absolute url, from which it takes the path.

A **command** health check cannot be expressed to either proxy. Such a service
still overlaps, and `retry` still covers a backend that is not listening yet,
but nothing covers one that is listening and still warming up. `shunt plan` says
so on the affected service rather than leaving it implicit:

```
~ app            blue/green  (starts alongside; proxy cannot poll this health check)
```

Give the service a `url` health check to close that gap.

### Recovering from a partial failure

A deploy that fails *after* replacing a container leaves the host running a mix
of releases. `shunt status` reports that as `degraded` and prints the recovery
command. To have it recovered for you:

```sh
shunt up --rollback-on-failure
```

It restores the release that was serving, inside the same operation and under
the same lock, so nothing can land in between. Opt-in on purpose: it is right
for a stateless app and wrong for a release whose stages already migrated a
database — the code would go back and the schema would not.

### What your app has to do

Zero-downtime is a contract, not a flag. The last two rows above are entirely
app-side, and no deploy tool can supply them for you:

1. **Handle SIGTERM.** On signal, start failing your health check so the proxy
   takes you out of rotation, keep serving in-flight requests, then exit. A
   process that dies instantly on SIGTERM drops whatever was in flight.
2. **If you run schema migrations, make them expand/contract.** Stages run
   *before* the swap, so during the overlap both releases talk to the migrated
   schema. Dropping a column the old release still reads turns a clean outage
   into a stream of 500s. Projects without migrations can ignore this.

### Changing the proxy config

A label-discovered router is defined by *every* container carrying its labels,
and Traefik refuses to serve a router that two containers define differently — it
drops the route entirely, which is a total outage rather than a blip.

So whenever the `proxy` block changes, shunt detects it and degrades that one
deploy to stop-then-start. `shunt plan` says so, and you take a short gap instead
of a dropped route. Steady-state deploys, where the block is unchanged, overlap
normally.

Services without a `proxy` block — workers, anything with no HTTP port — are
stopped gracefully and restarted. They need none of this.

### Health checks

`url` may be a full URL or a bare path. A path is resolved against whatever the
service is actually reachable on — its published host port, or its address on
the deploy network when it only `expose`s a port.

By default a 3xx counts as healthy, which proves the server is listening. Set
`follow = true` to chase redirects and require a 2xx at the end. That matters
more than it sounds: an app whose `/` redirects to a locale prefix will pass the
default check while being completely broken behind the redirect. Verified on a
deliberately broken build — it passed with `follow = false` and failed with
`follow = true`.

## Secrets

Secrets are resolved **on your machine**, streamed to the host inside the release
spec over the ssh channel, and written to a `0600` env-file there. They are never
baked into an image, never written to a local temp file, and never passed as
arguments to the helper — so they cannot leak through your shell history, a
stray file in `/tmp`, `ps` on either machine, or a published image layer.

**The boundary is the host's Docker socket.** Docker expands `--env-file` into
the container's own configuration, so anyone who can reach the Docker API on that
host can read the values back with `docker inspect`. That is not a gap shunt can
close: as the [Security](#security) section notes, socket access is already root
on that host, and a process that can read the container config can equally read
the `0600` env-file or the container's memory. Treat "can talk to the Docker
socket" as "can read every secret in this project", and scope your deploy user
accordingly.

Two things follow from that, worth knowing before you rely on either:

- **`[secrets]` is the only path that gets the env-file treatment.** A value
  written into a service's `env` block — including via `${env:...}` — is passed
  to `docker run` as `-e KEY=value` on the host, so it is briefly visible in `ps`
  there. Use `env` for configuration and `[secrets]` for anything you would not
  commit.
- **A rollback needs the old release's env-file.** It is the only plaintext copy
  of that release's secrets — the ledger holds hashes — so retention bounds how
  far back you can roll back. See [Rollback](#rollback).

| provider | source |
|:--|:--|
| `file` | a dotenv file |
| `env` | named variables from your environment |
| `sops` | shells out to `sops`, so your existing age/KMS/PGP setup just works |

The host's ledger stores only a **hash** of each value. That is enough for
`shunt plan` to tell you *which* secrets changed, with no plaintext coming back
over the wire:

```
secrets 4 key(s)
  ~ DATABASE_URL  (value changed)
  + STRIPE_KEY
```

## Security

- **ssh access to a host in the `docker` group is root on that host.** That is
  inherent to deploying containers this way, not something shunt adds. Prefer
  rootless Docker or a dedicated deploy user.
- shunt shells out to the system `ssh`, so your `~/.ssh/config`, agent, jump
  hosts, hardware keys and `known_hosts` all apply unchanged. It stores no
  credentials and has **no flag to disable host key checking**.
- There is deliberately no `DOCKER_HOST=tcp://` mode. That is an unauthenticated
  root socket.
- Every blob is rehashed on the host and checked against its filename before
  loading, so a corrupted transfer fails loudly instead of deploying silently.
  (`SHUNT_NO_VERIFY=1` on the host skips it.)
- Concurrent deploys are serialised per project with `flock`.

## Non-goals

Multi-host orchestration, service discovery, autoscaling, a web UI, running its
own reverse proxy, terminating TLS. If you need those you need a real
orchestrator — and shunt is not trying to grow into one.

It exists for the case no orchestrator serves well: one app, one server, and a
Dockerfile. A static site with a single container and a small API with a
database, a migration step and a shipped index file are both squarely in scope —
the second just declares more.

## Internals

```
cmd/shunt/           the CLI you run
  cli.go               flags, arg parsing, shared setup
  deploy.go            plan, up
  lifecycle.go         rollback, boot, retire
  inspect.go           status, logs, prune
  audit.go             audit, validate
  exec.go              exec, run
  fetch.go             pulling artifacts and captures back down
  init.go              scaffolding

cmd/shunt-helper/    uploaded to the host, driven over ssh, emits NDJSON events
  apply.go             the deploy pipeline
  images.go            OCI layout verification and load
  containers.go        naming, start/stop, blue-green swap
  proxy.go             discovery labels for Traefik / caddy-docker-proxy
  stages.go            one-shot containers, captures
  health.go            probes and gating
  ledger.go            release history, outcome classification, redaction
  runner.go            the seam that makes the deploy path testable
  inspect.go           status, logs, pruning

internal/manifest    shunt.toml types, loading, validation
internal/build       buildx → OCI layout
internal/transport   rsync mirroring of the layout, with transfer stats
internal/engine      manifest → release spec → plan → apply
internal/release     the CLI ↔ helper wire contract and the on-host ledger
internal/secrets     secret providers and ${env:...} interpolation
internal/sshx        multiplexed ssh, honouring the user's own config
internal/ui          colour, banner, formatting — the only place ANSI exists
```

Four decisions worth knowing about:

**The helper is content-addressed.** It is embedded in the CLI and uploaded under
a filename derived from its own sha256. A rebuilt helper always lands at a new
path and an unchanged one is never re-uploaded, so version skew between the two
ends is impossible by construction. The wire protocol carries a version check as
a second line of defence.

**The store is a mirror, not an archive.** rsync runs with `--delete`, so the
host's OCI layout matches the latest build exactly and can never grow without
bound. Rollback doesn't depend on it — it re-uses the release-tagged images
Docker already holds.

**ssh is the real binary, not a library.** That is why your existing ssh config,
agent and hardware keys work with no configuration, and why shunt never has to
invent key management.

**Colour lives in exactly one package.** `internal/ui` is the only place an
escape code appears, so `NO_COLOR`, dumb terminals and redirected output are
handled once rather than per command. Piped output is plain text everywhere,
including the progress stream.

## Development

```sh
make build      # helpers + CLI
make test       # go test ./...
make helpers    # cross-compile the host helper for linux/amd64 and linux/arm64
make fmt vet
```

The helper is built first and embedded via `go:embed`. In a fresh checkout where
`make helpers` hasn't run, the CLI falls back to compiling one on demand, so
`go run ./cmd/shunt` works with no build step.

## Contributing

Issues and pull requests welcome. CI runs `gofmt`, `go vet`, `go test -race`,
a cross-compiled build of the embedded helper, and a smoke test of the binary —
`make test && make build` locally covers all but the last.

Changes to the deploy path are hard to prove correct with unit tests alone, so
CI runs `test/e2e.sh` against a real Docker daemon over real ssh: containers
swap, health checks probe, and a failing stage has to leave the running release
untouched. Run it yourself against any host you can ssh to:

```sh
make build && test/e2e.sh deploy@vps.example.com
```

Host-side logic reaches docker through the `runner` seam in
`cmd/shunt-helper/runner.go`, so swap, health and rollback failures are
assertable without a daemon. If you touch container swapping, health gating or
the transport, add a case there as well as saying in the PR what you exercised
against a real host.

## License

MIT © k3scat
