---
title: Images
parent: Manifest reference
nav_order: 1
---

# Images

An image is something shunt builds locally and ships. Images referenced by a
service but absent from `[images.*]` are treated as external and pulled on the
host instead.

```toml
[images.app]
context    = "."
dockerfile = "Dockerfile"
platform   = "linux/amd64"        # set this when building on an arm64 Mac
target     = "production"         # a specific multi-stage target
args       = { PUBLIC_URL = "${env:PUBLIC_URL}" }
```

| Key | Default | Meaning |
|:--|:--|:--|
| `context` | `.` | Build context, relative to `shunt.toml`. |
| `dockerfile` | `<context>/Dockerfile` | Path to the Dockerfile. |
| `platform` | buildx default | Target platform, e.g. `linux/amd64`. |
| `target` | none | Multi-stage build target. |
| `args` | none | Build arguments. `${env:NAME}` interpolates from your environment. |

Several images in one manifest is fine. Declare `[images.<name>]` for each and
point services at them by name.

## Platform

The single most common first-deploy failure is an architecture mismatch: an
arm64 laptop building for an amd64 server produces an image that loads without
complaint and then exits immediately.

`shunt audit` compares each image's platform against the host architecture and
warns before you hit it. Setting `platform` explicitly is the fix.

## How the build reaches the host

buildx exports an OCI layout — a directory in which every blob's filename is its
own content hash — and rsync mirrors that directory to the host. Files the host
already has are skipped, which is layer-level deduplication with no registry
involved.

Blob modification times are normalised at export, because a blob's filename
already identifies its contents and rsync's quick check compares size *and*
mtime. Without that, a re-export would make every blob look changed and the
whole image would be re-sent on every deploy.

The layout is written so that both the containerd snapshotter and the classic
overlay2 store can load it, so the two ends do not have to match.
