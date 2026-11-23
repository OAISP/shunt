---
title: Secrets
parent: Manifest reference
nav_order: 6
---

# Secrets

Declare a `[secrets]` block if your app needs values you would not commit.

```toml
[secrets]
provider = "file"              # file | env | sops
path     = "secrets/prod.env"
mode     = "env"               # env (default) | file
```

| Provider | Reads |
|:--|:--|
| `file` | A dotenv file at `path`. |
| `env` | The variables named in `keys`, from your environment — the usual choice in CI. |
| `sops` | A sops-encrypted file at `path`, decrypted by the `sops` binary with whatever age, KMS or PGP setup you already have. |

```toml
[secrets]
provider = "env"
keys     = ["DATABASE_URL", "STRIPE_KEY"]
```

Every service, accessory and stage receives every secret, unless a service or
accessory [narrows its set]({% link manifest/services.md %}#scoping-secrets)
with `secrets = [...]`.

## How they reach the server

Secrets are resolved on your machine and sent inside the release description
over the ssh connection, on stdin. On the server they are written to `0600`
files. They are never baked into an image, written to a local temporary file,
or passed as command-line arguments, so they do not show up in shell history,
in `/tmp`, in `ps` on either machine, or in an image layer.

See [Security]({% link security.md %}) for what that does and does not protect
against. In short: anyone who can use the server's Docker socket can read them,
and no delivery mechanism changes that.

## Delivery mode

**`mode = "env"`** (the default) passes secrets as environment variables
through `--env-file`. Docker copies them into the container's configuration, so
they come back out of `docker inspect` — and out of anything that captures it,
such as a monitoring agent or a bug report.

**`mode = "file"`** writes each secret to its own `0600` file and mounts the
directory read-only at `/run/secrets`:

```sh
# in the app
DATABASE_URL="$(cat /run/secrets/DATABASE_URL)"
```

That is the path Docker Swarm and Kubernetes use, so an app written for either
already looks there. Each file holds exactly the value, with no trailing
newline. The values do not appear in `docker inspect`.

## Secrets and `env`

Only `[secrets]` gets this handling. A value in a service's `env` — including
one filled in from `${env:...}` — is passed to Docker on the command line, so it
is briefly visible in the server's process list. Use `env` for configuration
and `[secrets]` for credentials.

## What the plan shows

The server keeps a salted hash of each value, never the value, which is enough
for `shunt plan` to say which secrets changed without sending any back:

```
secrets 4 key(s)
  ~ DATABASE_URL  (value changed)
  + STRIPE_KEY
```

The salt is generated on the server, per project.

## Secrets and rollback

Because the release history holds only hashes, a rollback reads the old
release's values back from the `0600` copy kept for it on the server. Those
copies expire with the release's images, so how far back you can roll back is
bounded by `retain`. See
[Rollback]({% link deploying/rollback.md %}#where-the-restored-secrets-come-from).

To remove every copy when you are done with a server, use
`shunt down --purge`.
