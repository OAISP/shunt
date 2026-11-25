---
title: Accessories
parent: Manifest reference
nav_order: 3
---

# Accessories

An accessory is a stateful container the server runs for you: a database, a
cache, a model server. Declare one only if the server should run it. If yours
lives elsewhere — managed Postgres, another machine — leave this out and give
your app its address as a secret.

```toml
[accessories.db]
image   = "postgres:18-alpine"
volumes = ["acme-pgdata:/var/lib/postgresql"]
env     = { POSTGRES_USER = "acme" }

[accessories.db.health]
command = ["pg_isready", "-U", "acme"]
```

An accessory takes the same keys as a [service]({% link manifest/services.md %}).
A `proxy` block gives it the proxy's labels, but not blue/green: an accessory
is always one container under one name.

## Booted once, then left alone

A service is replaced whenever a release changes it. An accessory is created on the first
deploy that declares it and never replaced by a deploy afterwards: shipping a
CSS fix must not restart Postgres. If the container exists but is stopped — a
server reboot leaves it that way — a deploy starts it, and one that has been
removed is booted again. `shunt plan` reports either as work to do.

Accessories come up, and pass their health checks, before any stage runs, so a
migration always has its database.

## Changing one

Editing an accessory does not change the running container. `shunt plan`
reports the difference and the command that applies it:

```
accessories (booted once; never replaced by a deploy)
  ! db             drifted from shunt.toml
      image postgres:17-alpine → postgres:18-alpine
      not applied by `shunt up` — run `shunt boot db` to recreate
```

The comparison is against the definition the running container was actually
created with, which the server records, so an unrelated deploy does not make
the difference disappear.

`shunt boot db` recreates the container from the current manifest and records
it as applied.

{: .warning }
`shunt boot` destroys the container and creates a new one. Only data in a
volume survives — a named volume, as above, or a bind mount.

Accessories are never removed by a rollback or by a plain `shunt down`; see
[`shunt down`]({% link cli.md %}#shunt-down).
