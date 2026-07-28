---
title: Rollback and recovery
parent: Deploying
nav_order: 2
---

# Rollback and recovery

Each release is tagged immutably on the host as
`shunt/<project>-<image>:<release-id>`, and the last `retain` restorable
releases are kept (default 5). Rolling back re-runs a previous release's images:
no rebuild, no transfer, a couple of seconds.

```sh
shunt rollback                  # previous successful release
shunt rollback 20260726-225110  # a specific one
```

Releases that failed or left the host degraded are skipped when picking
"previous" — rolling onto one would just repeat the problem.

## What retention actually bounds

Failed releases do not consume a retain slot. Counting them would evict the last
good release after a run of failed deploys, which is precisely the situation in
which a rollback is the thing you need.

Images and the per-release secrets expire together on the same keep set, because
either one alone makes a release unrestorable. `shunt status` marks entries it
can no longer restore rather than offering them:

```
history
  * 20260728-093012  active       2026-07-28 09:30
    20260728-091133  superseded   2026-07-28 09:11
    20260727-224510  superseded   2026-07-27 22:45  (beyond retention — images pruned)
```

## Where the restored secrets come from

The ledger stores a salted hash of every secret, never the value — so the release
description a rollback replays cannot start a container on its own. The values
come back from the retained `0600` env-file, or in `mode = "file"` from that
release's secrets directories, which is the only plaintext copy on the host.
Per-service `secrets = [...]` scoping is preserved: each service is restored with
the same narrowed set it was deployed with.

That is the mechanism behind the retention rule above, and it fails by name
rather than by starting something broken:

```
✗ the secrets for release 20260727-224510 are no longer on this host
  (DATABASE_URL, STRIPE_KEY); roll back to a newer release, or redeploy that commit
```

## Recovering from a partial failure

A deploy that fails *after* replacing a container leaves the host running a mix
of releases. `shunt status` reports that as `degraded` and prints the recovery
command.

To have it recovered for you:

```sh
shunt up --rollback-on-failure
```

It restores the release that was serving, inside the same operation and under
the same lock, so nothing can land in between.

{: .warning }
Opt-in on purpose. Automatic rollback is right for a stateless app and wrong for
a release whose stages already migrated a database: the code would go back and
the schema would not.

## What rollback does not do

`shunt rollback` restores containers and images. It does not revert data.

Silently rolling a database back as a side effect of rolling code back is the
kind of helpfulness that loses data. The superseded artifact copy is kept as
`<dest>.prev.<release-id>`, and a failed health check prints the exact `mv` that
puts it back.
