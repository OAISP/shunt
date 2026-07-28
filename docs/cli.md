---
title: CLI reference
nav_order: 5
---

# CLI reference

| Command | |
|:--|:--|
| `shunt init` | Scaffold a `shunt.toml`, guessing the port from `EXPOSE`. |
| `shunt validate` | Check the manifest offline. No ssh, no build. |
| `shunt audit` | Check everything a deploy needs, and change nothing. |
| `shunt plan` | Build, then diff the manifest against the host. |
| `shunt up` | Apply: build, ship, stages, swap, health-check. |
| `shunt status` | What is running, and the release history. |
| `shunt logs [service]` | Tail container logs, prefixed when there are several. |
| `shunt exec <service> -- <cmd>` | Run a command in the running container. |
| `shunt run <service> -- <cmd>` | Run a one-off command in a fresh container. |
| `shunt rollback [id]` | Restore the previous, or a named, release. |
| `shunt boot <accessory>` | Recreate a stateful accessory. Destructive. |
| `shunt retire <service>` | Stop a service you removed from the manifest. |
| `shunt fetch [name\|path]` | Pull an artifact or capture back down. |
| `shunt prune` | Drop superseded images on the host. |
| `shunt bundle` | Build a release into a portable file. |
| `shunt bundle inspect <file>` | Show what a bundle contains. |
| `shunt bundle verify <file>` | Rehash a bundle's blobs. Needs no host. |
| `shunt apply <file>` | Deploy a bundle. |

## Common flags

| Flag | |
|:--|:--|
| `-f`, `--file <path>` | Path to `shunt.toml`. Defaults to the nearest one up the tree. |
| `-t`, `--target <name>` | Deploy target from `[targets.*]`. |
| `-v`, `--verbose` | Build output and per-step detail. |
| `--json` | Machine-readable output. |

`SHUNT_NO_BANNER=1` and [`NO_COLOR=1`](https://no-color.org) are respected, and
both are implied when output is not a terminal.

Flags are accepted anywhere, including after a positional argument, so
`shunt logs app --follow` and `shunt boot db -f path` both do what they look
like they do.

## Exit codes

| Code | Meaning |
|:--|:--|
| 0 | Success, or `shunt plan` found nothing to do. |
| 1 | The command failed. |
| 2 | `shunt plan` found changes to apply. Also bad usage. |

The distinct code for "there are changes" lets CI branch on it without parsing
output.

## Machine-readable output

`shunt plan --json` emits a versioned document with stable field names:

```json
{
  "schema": 1,
  "release_id": "20260728-093012-a1b2c3",
  "has_changes": true,
  "images": [{"name": "app", "action": "update", "old_digest": "...", "new_digest": "..."}],
  "services": [{"name": "app", "action": "update", "reasons": ["image app rebuilt"]}],
  "transfer": {"total": 85383168, "missing": 5312, "blobs": 3}
}
```

`has_changes` is materialised rather than left for the consumer to re-derive.
`shunt status --json` and `shunt audit --json` are similarly structured.

## Selected commands

### `shunt audit`

Connects and checks everything a deploy depends on: docker, buildx and rsync
locally; ssh, docker, rsync and curl on the host; free disk; artifact
destinations; and whether each image's platform matches the host architecture.
It changes nothing and exits non-zero if any check fails, so CI can gate on it.

Every one of these is checked somewhere in the deploy path anyway, but spread
across it — a missing `curl` otherwise surfaces after the container swap.

### `shunt exec` and `shunt run`

`exec` attaches to the container currently serving traffic. During a blue/green
overlap two containers carry the same service label, and `exec` resolves to the
one belonging to the active release rather than the one being retired.

`run` starts a throwaway container from the active release's image, on the
deploy network and with that release's secrets, and removes it when it exits.
A migration, a rake task or a console that should not share a process table with
production belongs here.

```sh
shunt exec app -- sh
shunt run app -- bin/rails console
```

### `shunt logs`

Tails every container of the project, or of one service. When there is more than
one, each line is prefixed with its container name and writes are serialised so
two containers logging at once cannot interleave mid-line.

### `shunt prune`

Drops release-tagged images the retention window no longer covers. Failed
releases do not consume a retain slot — counting them would evict the last good
release after a run of failed deploys, which is precisely when a rollback is the
thing you need.
