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

Builds your image, ships **only the layers the host doesn't already have**, swaps
the containers, and fails the release if the app doesn't come up healthy.

A working manifest is about ten lines. Databases, migrations, data files and
zero-downtime swaps are all available and all optional — declare them when you
need them, and shunt skips them entirely when you don't.

**[Documentation →](https://oaisp.github.io/shunt/)**

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

```sh
curl -sSL https://raw.githubusercontent.com/OAISP/shunt/main/install.sh | sh
```

Or `go install github.com/OAISP/shunt/cmd/shunt@latest`, or grab a release
archive. See [Getting started](https://oaisp.github.io/shunt/getting-started/)
for the other options and the full requirements.

**Your machine:** docker (with a buildx builder that can export an OCI layout),
rsync, ssh.
**The server:** docker, rsync, curl, and an ssh account that can reach the docker
socket. Nothing else — shunt uploads its own helper.

## Quick start

```sh
cd my-project
shunt init --host deploy@vps.example.com   # writes shunt.toml, guesses the port
shunt audit                                # check the host can accept a deploy
shunt plan                                 # see exactly what would change
shunt up
```

A complete, working manifest:

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

Everything else — accessories, stages, artifacts, secrets, deploy targets,
zero-downtime proxying, portable bundles — is optional and documented at
**[oaisp.github.io/shunt](https://oaisp.github.io/shunt/)**.

## Commands

| | |
|:--|:--|
| `shunt init` · `validate` · `audit` | scaffold, check offline, check the host |
| `shunt plan` · `up` | diff, then apply |
| `shunt status` · `logs` · `exec` · `run` | see and reach what's running |
| `shunt rollback` · `boot` · `retire` · `prune` | lifecycle |
| `shunt fetch` | pull artifacts and captures back down |
| `shunt bundle` · `apply` | deploy where the builder can't reach |

Full reference: [oaisp.github.io/shunt/cli](https://oaisp.github.io/shunt/cli/)

## Non-goals

Multi-host orchestration, service discovery, autoscaling, a web UI, running its
own reverse proxy, terminating TLS. If you need those you need a real
orchestrator — and shunt is not trying to grow into one.

It exists for the case no orchestrator serves well: one app, one server, and a
Dockerfile. A static site with a single container and a small API with a
database, a migration step and a shipped index file are both squarely in scope —
the second just declares more.

## Contributing

Issues and pull requests welcome. `make test && make build` covers most of CI;
`test/e2e.sh <host>` runs a real deploy against any host you can ssh to. See
[Contributing](https://oaisp.github.io/shunt/contributing/).

## License

MIT © k3scat
