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

`shunt up` builds your image, ships only the layers the host does not already
have, swaps the containers, and fails the release if the app does not come up
healthy.

A working manifest is about ten lines. Databases, migrations, data files and
zero-downtime swaps are all available and all optional: declare them when you
need them, and shunt skips them entirely when you do not.

## Why registry-free

Deploying a locally-built image usually means pushing it to a registry so the
server can pull it back down. Your bytes cross the network twice, and you
maintain credentials for a warehouse you never wanted.

The usual workaround, `docker save | ssh docker load`, re-uploads the entire
image every single deploy, base layers and all.

shunt exports an OCI layout, in which each blob's filename *is* its content
hash, and mirrors it with rsync. Files the host already has are skipped. That is
layer-level deduplication with no registry and no protocol to implement.

Measured on an 81 MB Node image, changing one source file:

| | `docker save \| zstd` | shunt |
|:--|--:|--:|
| First deploy | 81.1 MB | 81.3 MB |
| Code-only redeploy | 81.1 MB | **5.3 KB** |

Real applications have a fatter top layer than that test app, so expect 30–100x
on a typical Next.js or Rails image rather than four orders of magnitude. The
point is not the ratio. It is that you stop paying for the base image on every
deploy.

## What it is for

One app, one server, and a Dockerfile — the case no orchestrator serves well.
A static site with a single container is in scope. So is a small API with a
database, a migration step and a shipped index file; it just declares more.

Multi-host orchestration, service discovery, autoscaling, a web UI, running a
reverse proxy and terminating TLS are all explicit non-goals. If you need those,
you need a real orchestrator, and shunt is not trying to grow into one.

## Where to go next

| | |
|:--|:--|
| [Getting started]({% link getting-started.md %}) | Install, and a first deploy |
| [Manifest reference]({% link manifest/index.md %}) | Everything `shunt.toml` accepts |
| [Deploying]({% link deploying/index.md %}) | The pipeline, zero-downtime, rollback, bundles |
| [CLI reference]({% link cli.md %}) | Every command and flag |
| [Security]({% link security.md %}) | The trust boundary, stated plainly |
| [Internals]({% link internals/index.md %}) | How it works, and why it is built this way |
