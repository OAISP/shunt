```
█▀▀ █ █ █ █ █▄ █ ▀█▀
▄▄█ █▀█ █▄█ █ ▀█  █
```

[![ci](https://github.com/OAISP/shunt/actions/workflows/ci.yml/badge.svg)](https://github.com/OAISP/shunt/actions/workflows/ci.yml)
[![docs](https://img.shields.io/badge/docs-oaisp.github.io%2Fshunt-blue)](https://oaisp.github.io/shunt/)
[![license](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

**Registry-free Docker deploys over ssh.** One app, one server, no orchestrator.

```sh
shunt up
```

Builds your image, ships **only the layers the server doesn't already have**,
swaps the containers, and fails the release if the app doesn't come up healthy.

A working manifest is about ten lines. Databases, migrations, data files and
zero-downtime swaps are there when you need them and cost nothing when you
don't.

**[Documentation →](https://oaisp.github.io/shunt/)**

---

## Why

Deploying a locally built image usually means pushing it to a registry so the
server can pull it back down: your bytes cross the network twice, and you keep
credentials for a warehouse you never wanted. The usual workaround,
`docker save | ssh docker load`, uploads the **entire image on every deploy**.

shunt exports the image as an OCI layout, in which every file is named by its
own content hash, and mirrors it with rsync. Files the server already has are
skipped — layer-level deduplication with no registry in between.

A Node app on `node:22-slim`, changing one source file:

| | `docker save \| zstd` | shunt |
|:--|--:|--:|
| first deploy | 75.9 MB | 76.2 MB |
| code-only redeploy | 75.9 MB | **11.6 KB** |

Most apps have a bigger top layer than this one — dependencies, compiled
assets — so the saving on a real project is smaller. What does not change is
that the base image never crosses the wire again.

## Install

```sh
curl -sSL https://raw.githubusercontent.com/OAISP/shunt/main/install.sh | sh
```

Or `go install github.com/OAISP/shunt/cmd/shunt@latest`, or a release archive.
[Getting started](https://oaisp.github.io/shunt/getting-started/) has the
details.

- **Your machine:** docker with a buildx builder that can export an OCI layout,
  rsync, ssh.
- **The server:** docker, rsync, and an ssh user that can reach the docker
  socket. shunt uploads its own helper.

## Quick start

```sh
cd my-project
shunt init --host deploy@vps.example.com   # writes shunt.toml
shunt audit                                # checks both machines
shunt plan                                 # shows what a deploy would change
shunt up                                   # deploys
```

`shunt init` writes a complete manifest:

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

## What else it does

All optional, and each a block in the same file:

- **Accessories** — a database or cache, booted once and never restarted by a
  code deploy.
- **Stages** — migrations or backups that must succeed before anything running
  is replaced.
- **Artifacts** — large files and directories, shipped incrementally and swapped
  in atomically.
- **Secrets** — from a dotenv file, your environment or sops; delivered as
  environment variables or as files.
- **Limits** — memory, CPU, processes, log rotation and privileges per
  service, set when the container is created.
- **Minimal restarts** — a deploy replaces only the services it changes; a
  rebuild that only restamps labels deploys nothing.
- **Zero-downtime** — blue/green through Traefik or caddy-docker-proxy labels.
- **Rollback** — to any retained release in seconds, optionally automatic.
- **Targets and bundles** — staging and production from one manifest; build
  once and promote, or carry a release to a host the builder cannot reach.
- **Prebuilt images** — ship the image your pipeline already built, scanned and
  signed, straight from an OCI layout, with no Docker on the deploy runner.
- **CI** — a build cache for fresh runners, and deploys that finish even when
  the job is cancelled. See
  [Deploying from CI](https://oaisp.github.io/shunt/deploying/ci/).

Every command: [CLI reference](https://oaisp.github.io/shunt/cli/).

## Non-goals

Multi-host orchestration, service discovery, autoscaling, a web UI, running its
own reverse proxy, terminating TLS. shunt is for one app on one server, and is
not trying to grow into an orchestrator.

## Contributing

Issues and pull requests are welcome. `make fmt vet test build` runs the unit
checks CI runs, and `test/e2e.sh <host>` runs a real deploy against any host you
can ssh to. See [Contributing](https://oaisp.github.io/shunt/contributing/).

## License

MIT © k3scat
