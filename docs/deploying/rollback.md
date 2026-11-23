---
title: Rollback and recovery
parent: Deploying
nav_order: 2
---

# Rollback and recovery

Every release tags its images on the server as
`shunt/<project>-<image>:<release-id>`, and the newest `retain` restorable
releases (default 5) are kept. Rolling back re-runs a previous release's
containers from those images: no build, no transfer, a few seconds.

```sh
shunt rollback                         # the release before the serving one
shunt rollback 20260726-225110-3fa9c1  # a specific release
```

`shunt status` lists the release ids. A release that `failed` or was left
`degraded` is never picked as "previous" — rolling onto it would repeat the
problem.

## Recovering from a partial failure

A deploy that fails *after* replacing a container leaves the server running a
mix of releases, recorded as `degraded`; `shunt status` shows it and the
command that recovers. To have it recovered automatically:

```sh
shunt up --rollback-on-failure
```

The release that was serving is restored in the same operation, under the same
lock, so nothing can land in between.

{: .warning }
This is opt-in on purpose. It is right for a stateless app and wrong for a
release whose stages already migrated a database: the code goes back, the
schema does not.

## What rollback restores

The containers of the target release, with the images, definitions and secrets
they were deployed with.

A service the newer release added has nothing to go back to, so its container
is removed as part of the rollback; otherwise the server would keep running
code from the release being undone. That is the only case in which shunt stops
a container nobody asked it to stop. A service you delete from `shunt.toml` is
different: `shunt plan` reports it as orphaned, and only
[`shunt retire`]({% link manifest/services.md %}#removing-a-service) removes it.

Accessories are never touched by a rollback.

## What rollback does not restore

Data. An artifact's superseded copy is kept as `<dest>.prev.<release-id>`, and a
release that fails after swapping one prints the `mv` that puts it back. A
database migrated by a stage stays migrated.

## How far back you can go

A release can be restored while its images and its secrets are both still on
the server. They expire together, when the release falls out of `retain`.
Failed attempts do not use up a slot — a run of failed deploys must not push
out the last good release, since that is exactly when you need it.

`shunt status` marks the releases it can no longer restore:

```
history
  * 20260728-093012-a1b2c3  active       2026-07-28 09:30
    20260728-091133-0d4e7f  superseded   2026-07-28 09:11
    20260727-224510-9c2b18  superseded   2026-07-27 22:45  (beyond retention — images pruned)
```

### Where the restored secrets come from

The release history stores only a salted hash of each secret, so it cannot
restart a container by itself. The values come from the `0600` copy the server
keeps for each retained release — its env-file, or its `/run/secrets` files in
`mode = "file"` — with each service's narrowed set intact. A release whose copy
is gone is refused by name rather than started without its secrets:

```
✗ the secrets for release 20260727-224510-9c2b18 are no longer on this host
  (DATABASE_URL, STRIPE_KEY); roll back to a newer release, or redeploy that commit
```
