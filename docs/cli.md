---
title: CLI reference
nav_order: 5
---

# CLI reference

```
shunt <command> [arguments] [flags]
```

Flags can go anywhere, including after an argument: `shunt logs app --follow`
and `shunt boot db -f path/shunt.toml` both work. `--` ends the flags, which is
how `exec` and `run` take a command.

## Flags for every command

| Flag | |
|:--|:--|
| `-f`, `--file <path>` | The `shunt.toml` to use. Default: the nearest one in this directory or above. |
| `-t`, `--target <name>` | Deploy to a [target]({% link manifest/targets.md %}) instead of the manifest's own host. |
| `-v`, `--verbose` | Build output and every step, not just outcomes. |
| `--json` | Machine-readable output; see [below](#machine-readable-output). |

Commands that ask before acting take `-y`, `--yes` to skip the question. With no
terminal to ask at — in CI — they proceed without asking, except `down --purge`.

## Commands

### Set up

| Command | |
|:--|:--|
| `shunt init --host <user@host>` | Write a `shunt.toml` for the Dockerfile in this directory, taking the port from its first `EXPOSE`. `--force` overwrites an existing one. |
| `shunt validate` | Check `shunt.toml` with no ssh and no build. |
| `shunt audit` | Check this machine and the server can deploy. Exits 1 if any check fails, with or without `--json`. |

### Deploy

| Command | |
|:--|:--|
| `shunt plan` | Build, then show what a deploy would change. |
| `shunt up` | Build, ship, run stages, swap containers, health-check. Does nothing when the server already matches. |
| `shunt rollback [release]` | Restore the release before the serving one, or the one named. |

| Flag | On | |
|:--|:--|:--|
| `--detailed-exitcode` | `plan` | Exit 2 when there are changes to apply. |
| `--no-cache` | `plan`, `up` | Build without buildx's layer cache. |
| `--no-plan` | `up` | Do not print the plan before applying. |
| `--rollback-on-failure` | `up` | If the release fails after replacing a container, restore the one that was serving. See [Rollback]({% link deploying/rollback.md %}#recovering-from-a-partial-failure). |

### Inspect and reach

| Command | |
|:--|:--|
| `shunt status` | The serving release, its containers, the last attempt if it failed, and the history. |
| `shunt logs [service]` | Logs of every container, or one service's. `--follow` streams; `--tail N` (default 100). |
| `shunt exec <service> -- <cmd>` | Run a command in the container serving that service. |
| `shunt run <service> -- <cmd>` | Run a command in a new container from the serving release's image, then remove it. `--image <name>` picks another image of that release. |
| `shunt fetch [name\|path]` | Copy an artifact, by name, or any file on the server, by absolute path, back to this machine. With no argument, list artifacts and captures. `-o <path>` chooses where. |

### Lifecycle

| Command | |
|:--|:--|
| `shunt boot <accessory>` | Recreate an accessory from the manifest. Destructive. |
| `shunt retire <service>` | Remove a service you deleted from the manifest. |
| `shunt prune` | Remove images and stored secrets of releases beyond `retain`. |
| `shunt down` | Take the project off the server. See [below](#shunt-down). |

### Bundles

| Command | |
|:--|:--|
| `shunt bundle` | Build a release into a file, without connecting to a server. `-o <file>` names it; `--no-cache` as for `up`. |
| `shunt bundle inspect <file>` | Show what a bundle contains. |
| `shunt bundle verify <file>` | Rehash a bundle's blobs. Needs no server. |
| `shunt apply <file>` | Deploy a bundle. `-t <target>` sends it to that target; `--host <user@host>` to any server; `--plan` shows what it would change; `--rollback-on-failure` and `--detailed-exitcode` as above. |

See [Bundles]({% link deploying/bundles.md %}).

### Other

`shunt version` prints the version and the helper protocol; `shunt help` prints
the command summary. `deploy` is an alias for `up`.

## Environment variables

| Variable | Where | |
|:--|:--|:--|
| `SHUNT_BUILD_CACHE` | your machine | `gha`, or a directory, to keep the build cache between CI jobs. See [Deploying from CI]({% link deploying/ci.md %}#give-the-build-a-cache). |
| `SHUNT_NO_BANNER=1` | your machine | No banner. |
| `NO_COLOR=1` | your machine | No colour. Both are implied when output is not a terminal. |
| `SHUNT_ROOT` | the server | Where shunt keeps its files. Default `~/.shunt`. |
| `SHUNT_NO_VERIFY_CACHE=1` | the server | Rehash every blob on every deploy. See [Integrity]({% link security.md %}#integrity). |
| `SHUNT_NO_VERIFY=1` | the server | Skip blob verification. |

The server-side ones are read by the helper, so they must be in the
environment ssh commands get on that server.

## Exit codes

| Code | |
|:--|:--|
| 0 | Success. `plan` exits 0 whether or not it found changes. |
| 1 | The command failed. |
| 2 | `plan --detailed-exitcode` or `apply --plan --detailed-exitcode` found changes to apply. |
| 64 | Bad usage: an unknown command or flag, or a missing argument. |

The changes code is opt-in, as in Terraform, so a plan step on a pull request
does not fail just because there is something to deploy — and bad usage has a
code of its own, so a typo can never read as "deploy needed".

## Machine-readable output

**`plan --json`** (and `apply --plan --json`) prints one versioned document.
`schema` changes only when the shape does:

```json
{
  "schema": 1,
  "release_id": "20260728-093012-a1b2c3",
  "has_changes": true,
  "images": [{"name": "app", "action": "update", "old_digest": "sha256:…", "new_digest": "sha256:…"}],
  "services": [{"name": "app", "action": "update", "reasons": ["image app rebuilt"]}],
  "transfer": {"total": 85383168, "missing": 5312, "blobs": 3}
}
```

**`up`, `rollback`, `boot`, `retire`, `down`, `prune` and `apply` with `--json`**
print the server's progress as one JSON event per line, each with a `kind`:
`step` (work starting), `ok` (work done), `info`, `log` (container output),
`fail` (with a `message`), and — when a deploy or rollback succeeds — a final
`result` naming the `release` and its `status`. Build and transfer progress,
and what a command says before it asks to proceed, go to stderr, so stdout
carries only events.

**`status --json`** prints the release history and the project's containers as
the server reports them, without the salt its secret hashes are keyed with. **`audit --json`** prints a list of checks, each with
`name`, `status` (`ok`, `warn` or `fail`), `detail` and, where there is one, a
`fix`. **`validate --json`** prints `valid` and, if it is not, the `error`.
Only the plan document is versioned.

## Notes on some commands

### `shunt exec` and `shunt run`

```sh
shunt exec app -- sh
shunt run app -- bin/rails console
```

`exec` uses the container of the serving release. During a blue/green overlap
two containers carry the service's label, and `exec` picks the serving one.

`run` starts a throwaway container on the deploy network, with the secrets that
service was deployed with — a console on a worker sees what the worker sees.
Use it for a migration, a task or a console that should not share a process
table with production.

### `shunt down`

```sh
shunt down            # the services; `shunt up` brings them back
shunt down --all      # and the accessories
shunt down --purge    # everything shunt put on this server for the project
```

`--purge` also removes the network, every release-tagged image, the release
history and the project's stored secrets — the `0600` copies kept for rollback,
which nothing else removes. It removes every rollback target, and says so
before asking.

Two things `down` never does:

- **Remove a volume**, at any depth. A flag that deletes a database is one
  tab-completion away from being used by mistake.
- **Purge without being told to.** With no terminal to ask at, `--purge`
  refuses unless given `-y`.

It works from the labels on the server's containers, not from the manifest, so
it also removes containers of services you have since deleted from
`shunt.toml`.

### `shunt logs`

With more than one container, each line is prefixed with the container's name,
and lines from different containers are never interleaved mid-line.

### `shunt prune`

Each successful deploy already prunes; `shunt prune` does it on demand. Failed
releases do not count toward `retain`, so a run of failed deploys never prunes
the last good release.
