---
title: Home
layout: home
nav_order: 1
---

# shunt

Registry-free Docker deploys over ssh. One app, one server, no orchestrator.
{: .fs-6 .fw-300 }

[Get started]({% link getting-started.md %}){: .btn .btn-primary .mr-2 }
[Source](https://github.com/OAISP/shunt){: .btn }

---

`shunt up` builds your image, ships only the layers the server does not already
have, swaps the containers, and fails the release if the app does not come up
healthy.

A working manifest is about ten lines. Databases, migrations, data files and
zero-downtime swaps are there when you need them, and a deploy that declares
none of them never runs those steps.

## Why registry-free

Deploying a locally built image usually means pushing it to a registry so the
server can pull it back down: your bytes cross the network twice, and you keep
credentials for a warehouse you never wanted. The usual workaround,
`docker save | ssh docker load`, uploads the entire image on every deploy.

shunt exports the image as an OCI layout, in which every file is named by its
own content hash, and mirrors it with rsync. Files the server already has are
skipped — layer-level deduplication with no registry in between.

A Node app on `node:22-slim`, changing one source file:

| | `docker save \| zstd` | shunt |
|:--|--:|--:|
| First deploy | 75.9 MB | 76.2 MB |
| Code-only redeploy | 75.9 MB | **11.6 KB** |

Most apps have a bigger top layer than this one, so the saving on a real
project is smaller. What does not change is that the base image never crosses
the wire again.

## What it is for

One app, one server and a Dockerfile — the case no orchestrator serves well. A
static site in a single container is in scope; so is an API with a database, a
migration step and a shipped data file. The second just declares more.

Multi-host orchestration, service discovery, autoscaling, a web UI, running a
reverse proxy and terminating TLS are non-goals. If you need those you need an
orchestrator, and shunt is not trying to become one.

## Where to go next

| | |
|:--|:--|
| [Getting started]({% link getting-started.md %}) | Install, requirements, a first deploy |
| [Manifest reference]({% link manifest/index.md %}) | Everything `shunt.toml` accepts |
| [Deploying]({% link deploying/index.md %}) | The pipeline, zero-downtime, rollback, bundles, CI |
| [CLI reference]({% link cli.md %}) | Every command, flag, exit code and output format |
| [Security]({% link security.md %}) | The trust boundary, stated plainly |
| [Internals]({% link internals/index.md %}) | How it works, and why it is built this way |
