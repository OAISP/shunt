---
title: Accessories
parent: Manifest reference
nav_order: 3
---

# Accessories

An accessory is a stateful dependency the host runs for you: a database, a
cache, a model server. Declare one only if the host should run it. If yours
already lives elsewhere — managed Postgres, another host, an existing container
— skip this entirely and point your app at it with a secret.

```toml
[accessories.db]
image   = "postgres:18-alpine"
volumes = ["acme-pgdata:/var/lib/postgresql"]

[accessories.db.health]
command = ["pg_isready", "-U", "acme"]
```

Accessories take the same keys as [services]({% link manifest/services.md %}).

## Accessories versus services

A **service** is stateless and replaced on every release. An **accessory** is
booted once and then left alone. Shipping a one-line CSS fix must not restart
Postgres.

Accessories come up before stages run, so a migration has its database
available. An accessory that already exists is started if stopped — a host
reboot leaves it that way — but never replaced.

## Drift

Changing an accessory's definition does not change the running container.
`shunt plan` reports the difference and names the fix:

```
accessories (booted once; never replaced by a deploy)
  ! db             drifted from shunt.toml
      image postgres:17-alpine → postgres:18-alpine
      not applied by `shunt up` — run `shunt boot db` to recreate
```

Drift is measured against what the host recorded as *applied* — the definition a
container was actually created with — rather than against the last release's
manifest. A deploy records the manifest it was handed even though it
deliberately left the accessory alone, so comparing against that would make a
reported drift disappear after any unrelated deploy while the container went on
running the old configuration.

Applying it is the explicit, destructive `shunt boot db`, which recreates the
container. Data survives only in named volumes.

{: .warning }
`shunt boot` destroys and recreates the container. Anything not in a named
volume is lost.
