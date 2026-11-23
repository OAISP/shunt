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
| `platform` | the builder's own | Target platform, such as `linux/amd64`. With `layout`, which image of a multi-platform layout to ship. |
| `target` | none | Multi-stage build target. |
| `args` | none | Build arguments. Supports [`${env:NAME}`]({% link manifest/index.md %}#environment-interpolation). |
| `layout` | none | Ship a [prebuilt image](#prebuilt-images) from this OCI layout directory instead of building. |

A service, accessory or stage that names an image not declared here — say
`postgres:18-alpine` — is pulled on the server instead.

Several images in one manifest are built in parallel. Declare
`[images.<name>]` for each and refer to them by name.

## Prebuilt images

When an image is built somewhere else — typically by an earlier CI job that also
scans and signs it — point `layout` at it instead of building:

```toml
[images.app]
layout = ".ci/oci/app"
```

shunt skips the build and ships that layout exactly as it would ship its own
export: the same transfer, the same deduplication, the same release tag. The
image keeps its digest, so the server runs byte for byte what the pipeline
checked. The machine running shunt needs no Docker for it.

Any registry tool can write the layout from an image reference, into an empty
directory:

```sh
regctl image copy registry.example.com/app@sha256:… ocidir://.ci/oci/app:release
```

The layout's `index.json` must name one image, which is what an export into an
empty directory gives; exporting into a layout that already holds one adds a
second entry, and shunt refuses to guess between them. That image may be a
multi-platform index:

- **Attestations** — the provenance and SBOM manifests buildx adds — are skipped.
- **One platform** left over is shipped as it is.
- **Several platforms** need `platform` to pick one, such as `linux/arm64`.

Only the chosen image's manifest, config and layers are shipped, never the other
platforms'. The layout itself is only read: shunt works on a copy.

`context`, `dockerfile`, `target` and `args` describe a build, so they are
rejected next to `layout`. `shunt audit` reads the platform from the layout, and
`shunt validate` does not need the layout to exist yet, so it still runs on a
pull request before anything is built.

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
