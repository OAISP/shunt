---
title: Deploying
nav_order: 4
has_children: true
---

# Deploying

```
build ─→ ship ─→ accessories ─→ stages ─→ artifacts ─→ services ─→ health
                 ╰──────── declare only if needed ────╯   ╰─── always ───╯
```

Most projects use the two ends and nothing in the middle: build an image, ship
it, run it, check it. The middle three steps do not exist unless the manifest
declares them, and a deploy that declares none skips straight from ship to
services.

Where they are used, the ordering *is* the safety property.

1. **Accessories** come up first, so anything after them has its dependencies
   available.
2. **Stages** are one-shot containers that must all succeed before any running
   service is touched, whatever they happen to do. If one fails, production is
   exactly as it was.
3. **Artifacts** are swapped in as late as possible. The swap is the point of no
   return for data, so it happens after everything that could still fail.
4. **Services** are replaced only once everything before them has passed.
5. **Health** gates the release. Containers that never come up healthy mean a
   failed release, not a silent one.

## What a deploy holds

A deploy takes a project lock on the host for its whole duration, including the
transfer. Two deploys of the same project therefore cannot interleave their
rsyncs into the same store.

The lock is an ssh session holding `flock` on an open file descriptor, so the
kernel releases it when the session ends for any reason — including the network
dropping or the CLI being killed. There is no lease to expire and no stale
lockfile to clean up.

The plan is computed before the lock is taken, because a build can take minutes.
The spec therefore carries the release the host was serving at plan time, and a
plan whose premise has expired is refused rather than applied:

```
✗ this plan was built when 20260728-014515 was serving, but 20260728-015902 is
  serving now — another deploy or rollback landed while this one waited for the
  lock
  rerun `shunt up` to plan against the current state
```

That is what stops a deploy that waited on the lock from silently reverting the
one it waited for.

It is checked twice, in two different places, for two different reasons. The
helper checks it under its own lock and is the authority — that is the check
that makes the guarantee. The CLI checks it as soon as it takes the lock and
before it ships anything, because the helper only sees the spec *after* the
transfer, and spending minutes uploading an image to be told the plan expired is
a bad way to find out.

## Outcomes

| Status | Meaning |
|:--|:--|
| `active` | Currently serving. |
| `superseded` | Replaced by a later release. |
| `failed` | Failed **before** any running container was replaced. Production is untouched. |
| `degraded` | Failed partway. The host is running a mix of releases. |

The distinction between `failed` and `degraded` is the reason a status is
recorded at all. A deploy that fails before touching anything does not move what
`shunt status` calls the serving release, so the report never contradicts the
error you were just shown:

```
release  20260728-012635  ● active  2026-07-27 21:26:48

last attempt 20260728-012823 failed — the release above is still serving
stage "doomed" failed: exit status 1
```

## Change detection

`shunt up` does nothing when the host already matches the manifest. That
includes `--json` and `--no-plan`, which is the CI path where a redundant deploy
costs the most.

What counts as a change: a rebuilt image, a changed service definition, a
changed or added stage, an artifact whose size or mtime differs, an accessory
that does not exist yet, a release-wide setting such as the secret delivery mode
or the network name, and drift between the manifest and the containers actually
running.

That last one matters: a plan built from the ledger alone describes what shunt
last did, not what the host is doing. A container deleted, stopped or replaced
by hand is reported as work to be done.
