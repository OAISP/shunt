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
| `shunt down` | Take this project off the host. `--all` and `--purge` go deeper. |
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

The secrets are the ones that service actually runs with, including its own
`secrets = [...]` narrowing — a console on a worker sees what the worker sees,
not the whole set.

```sh
shunt exec app -- sh
shunt run app -- bin/rails console
```

### `shunt down`

The inverse of `shunt up`, in three depths — because "stop the app for a minute"
and "I am finished with this server" are different requests, and conflating them
is how a rollback target or a database disappears by accident.

```sh
shunt down            # the services. Reversible: `shunt up` brings them back.
shunt down --all      # the accessories too
shunt down --purge    # everything shunt put on this host
```

`--purge` additionally removes the deploy network, every release-tagged image,
the release history and **this project's secrets on the host**. That last one is
the reason it exists rather than being left to `rm -rf`: the `0600` env-files and
`/run/secrets` directories under `~/.shunt/<project>/` are the only plaintext
copies of your credentials, and nothing else removes them — `shunt prune` only
expires what is already past `retain`.

It also removes every rollback target, which it says out loud before asking.

Two things it will never do:

- **Remove a volume.** Not at any depth. `docker compose down` needs `-v` for
  that and shunt has no equivalent on purpose — a flag that deletes a database
  is one tab-completion away from being pressed by mistake.
- **Purge without explicit consent.** Every other prompt in shunt proceeds when
  stdin is not a terminal, so CI never blocks on a question nobody can answer.
  `--purge` refuses instead, and asks for `-y`.

It works from the labels on the host rather than from the manifest, so it still
removes a container for a service you have already deleted from `shunt.toml` —
which `shunt retire` deliberately will not do.

### `shunt logs`

Tails every container of the project, or of one service. When there is more than
one, each line is prefixed with its container name and writes are serialised so
two containers logging at once cannot interleave mid-line.

### `shunt prune`

Drops release-tagged images the retention window no longer covers. Failed
releases do not consume a retain slot — counting them would evict the last good
release after a run of failed deploys, which is precisely when a rollback is the
thing you need.
