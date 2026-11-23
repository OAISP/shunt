---
title: Images
parent: Manifest reference
nav_order: 1
---

# Images

An image is something shunt builds on your machine and ships to the server.

```toml
[images.app]
context    = "."
dockerfile = "Dockerfile"
platform   = "linux/amd64"        # set this when building on an arm64 Mac
target     = "production"         # a stage of a multi-stage Dockerfile
args       = { PUBLIC_URL = "${env:PUBLIC_URL}" }
```

| Key | Default | Meaning |
|:--|:--|:--|
| `context` | `.` | Build context, relative to `shunt.toml`. |
| `dockerfile` | `<context>/Dockerfile` | Path to the Dockerfile, relative to `shunt.toml`. |
| `platform` | the builder's own | Target platform, such as `linux/amd64`. |
| `target` | none | Multi-stage build target. |
| `args` | none | Build arguments. Supports [`${env:NAME}`]({% link manifest/index.md %}#environment-interpolation). |

A service, accessory or stage that names an image not declared here — say
`postgres:18-alpine` — is pulled on the server instead.

Several images in one manifest are built in parallel. Declare
`[images.<name>]` for each and refer to them by name.

## Platform

The most common first-deploy failure is an architecture mismatch: an arm64
laptop builds an arm64 image, which loads on an amd64 server without complaint
and then exits immediately. `shunt audit` compares each image's platform with
the server's architecture and warns before you hit it. Setting `platform` is the
fix.

## How an image reaches the server

buildx exports an OCI layout — a directory in which every blob's filename is its
own content hash — and rsync mirrors it into a store on the server. A blob the
server already has keeps its name, so rsync skips it: layer-level deduplication
with no registry.

Two details make that hold in practice. Blob modification times are fixed at
export, because rsync's quick check compares size *and* mtime and a fresh
export would otherwise make every blob look new. And the layout carries the
small manifest the classic overlay2 image store needs alongside the OCI index
the containerd store reads, so either kind of server can load it.

On the server, only the blobs that actually arrived are rehashed and loaded;
see [Integrity]({% link security.md %}#integrity).

## Builds in CI

A CI runner starts with an empty build cache, so without one every layer after
the base image is rebuilt with a new digest and shipped again in full.
`SHUNT_BUILD_CACHE` keeps a cache between jobs; see
[Deploying from CI]({% link deploying/ci.md %}#give-the-build-a-cache).
