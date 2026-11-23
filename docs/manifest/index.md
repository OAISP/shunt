---
title: Manifest reference
nav_order: 3
has_children: true
---

# Manifest reference

`shunt.toml` describes production. Everything the server runs comes from this
file plus your resolved secrets. Command-line flags choose *where* a release
goes — which file, which target — never *what* it contains.

Only `project`, `host` and one service are required. Every other block is
optional, and a deploy that does not declare one never runs its step.

| Block | What it is | Page |
|:--|:--|:--|
| `[images.<name>]` | An image shunt builds and ships | [Images]({% link manifest/images.md %}) |
| `[services.<name>]` | A stateless container replaced on every release | [Services]({% link manifest/services.md %}) |
| `[accessories.<name>]` | A stateful container booted once: a database, a cache | [Accessories]({% link manifest/accessories.md %}) |
| `[[stages]]` | A one-shot container that must succeed before anything is replaced | [Stages]({% link manifest/stages.md %}) |
| `[[artifacts]]` | A large file or directory shipped beside the image | [Artifacts]({% link manifest/artifacts.md %}) |
| `[secrets]` | Values you would not commit | [Secrets]({% link manifest/secrets.md %}) |
| `[targets.<name>]` | Another server for the same release: staging, typically | [Targets]({% link manifest/targets.md %}) |

## Top-level keys

| Key | Default | Meaning |
|:--|:--|:--|
| `project` | — | Required. Names the containers, the network and the release history on the server. Lowercase letters, digits, `-` and `_`. |
| `host` | — | Required. `user@host`, or any alias in your `~/.ssh/config`. |
| `network` | `<project>-net` | The Docker network every container joins. |
| `retain` | `5` | How many releases to keep restorable for rollback. |

## Environment interpolation

`${env:NAME}` is replaced with the value of `NAME` from the environment of the
machine building the release. It works in image `args`, and in the `env` of
services, accessories and stages:

```toml
[images.app]
args = { PUBLIC_URL = "${env:PUBLIC_URL}" }

[services.app]
env = { LOG_LEVEL = "${env:LOG_LEVEL}" }
```

A referenced variable that is not set is an error, not an empty string. This is
for configuration: values in `env` are visible on the server's process list
while containers are created, so anything you would not commit belongs in
[`[secrets]`]({% link manifest/secrets.md %}).

## Validation

Unknown keys are an error: a typo in a field that silently does nothing is
worse than a manifest that fails to load. Every problem is reported at once,
so a broken manifest takes one round of fixes rather than five:

```
error: invalid shunt.toml:
  - service "app": proxy needs a host
  - service "worker": requires unknown service or accessory "cache"
  - stage "backup": require_nonempty has no meaning without capture
```

`shunt validate` runs these checks with no ssh and no build, which makes it a
good editor hook or pull-request check.
