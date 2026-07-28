---
title: Secrets
parent: Manifest reference
nav_order: 6
---

# Secrets

Declare a `[secrets]` block only if your app needs values you would not commit.

```toml
[secrets]
provider = "file"              # file | env | sops
path     = "secrets/prod.env"
mode     = "env"               # env (default) | file
```

| Provider | Source |
|:--|:--|
| `file` | A dotenv file. |
| `env` | Named variables from your environment, listed in `keys`. |
| `sops` | Shells out to `sops`, so your existing age/KMS/PGP setup works unchanged. |

```toml
[secrets]
provider = "env"
keys     = ["DATABASE_URL", "STRIPE_KEY"]
```

## How they reach the host

Secrets are resolved **on your machine**, streamed to the host inside the
release spec over the ssh channel, and written there with `0600` permissions.
They are never baked into an image, never written to a local temp file, and
never passed as arguments to the helper — so they cannot leak through your shell
history, a stray file in `/tmp`, `ps` on either machine, or a published image
layer.

See [Security]({% link security.md %}) for what that does and does not protect
against. The short version: the boundary is the host's Docker socket, and
nothing about delivery changes that.

## Delivery mode

`mode = "env"` (the default) passes secrets with `--env-file`. Docker expands
that into the container's own configuration, so the values come back out of
`docker inspect` — and therefore out of anything that captures it: a monitoring
agent, a bug report, a support ticket, an image made with `docker commit`.

`mode = "file"` writes each secret to its own `0600` file in a `0700` directory,
mounted read-only at `/run/secrets`:

```toml
[secrets]
provider = "file"
path     = "secrets/prod.env"
mode     = "file"
```

```sh
# in the app
DATABASE_URL="$(cat /run/secrets/DATABASE_URL)"
```

That is the same path Docker Swarm and Kubernetes use, so an app written for
either already looks in the right place. The file is exactly the value, with no
trailing newline to strip.

Measured on a live host: with `mode = "env"` the value appears in
`docker inspect`; with `mode = "file"` it does not. Per-service `secrets = [...]`
scoping works in both modes.

## Two things that follow

**`[secrets]` is the only path that gets this treatment.** A value written into
a service's `env` block — including via `${env:...}` — is passed to `docker run`
as `-e KEY=value` on the host, so it is briefly visible in `ps` there. Use `env`
for configuration and `[secrets]` for anything you would not commit.

**A rollback needs the old release's secrets.** They are the only plaintext copy
of that release's values, since the ledger holds only hashes — so a rollback
reads them back off the host rather than from the release description it is
replaying. Retention therefore bounds how far back you can roll back. Images and
secrets expire together on the same keep set, so a release never looks
restorable when it is not. See
[Rollback]({% link deploying/rollback.md %}#where-the-restored-secrets-come-from).

## What the plan shows

The host's ledger stores a salted hash of each value, never the value. That is
enough for `shunt plan` to report *which* secrets changed, with no plaintext
coming back over the wire:

```
secrets 4 key(s)
  ~ DATABASE_URL  (value changed)
  + STRIPE_KEY
```

The salt is per-project and generated on the host, so a truncated digest of a
low-entropy value is not recoverable from the ledger alone.
