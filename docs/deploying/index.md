---
title: Deploying
nav_order: 4
has_children: true
---

# Deploying

`shunt up` runs one pipeline:

```
build ─→ ship ─→ accessories ─→ stages ─→ artifacts ─→ services ─→ health
                 ╰──── only if declared ─────────╯
```

A project that declares no accessories, stages or artifacts goes straight from
ship to services. Where they are declared, the order is the safety property:

1. **Build** happens on your machine; **ship** sends the server only the image
   files it lacks, plus any artifacts, to staging paths beside their
   destinations.
2. **Accessories** are created if missing and health-checked, so everything
   after them has its database.
3. **Stages** must all succeed before anything running is touched. If one
   fails, production is exactly as it was.
4. **Artifacts** are checked and swapped in only after the stages pass, because
   that swap is the point of no return for data. If the release then fails
   before any service is replaced, they are put back.
5. **Services** that the release changes are replaced in dependency order; the
   rest [keep their containers]({% link manifest/services.md %}#how-a-service-is-replaced).
6. **Health** gates the release: a replaced service that never becomes healthy,
   or cannot stay up, fails it.

## Change detection

`shunt up` does nothing when the server already matches the manifest — including
with `--json` and `--no-plan`, which is how CI runs it. A deploy happens when
the plan finds any of these:

- an image that would run differently — new layers or runtime settings; a
  rebuild that only changes labels or timestamps is not a change, so a commit
  that touches nothing in the image deploys nothing;
- a service, stage or release-wide setting (network, secret delivery mode)
  whose definition changed;
- an artifact whose size or modification time differs from the server's copy;
- a secret that was added, removed or given a new value;
- an accessory that does not exist, or exists but is not running;
- a container that differs from what the server recorded — deleted, stopped or
  replaced by hand. Every container carries a hash of the definition it was
  created from, so the plan describes the server rather than the last deploy;
- a serving release that is not `active` — one left `degraded`, say.

## One deploy at a time

A deploy holds a per-project lock on the server from before the transfer until
the release is applied, so two deploys cannot write into the same image store,
and a rollback, `boot` or `down` waits for a deploy in progress.

The plan is computed before the lock is taken, because a build can take
minutes. When the lock is granted, the server reports which release it is
serving, and a deploy whose plan assumed a different one is refused before
anything is shipped:

```
error: this plan was built when 20260728-014515-1c9e2a was serving, but
20260728-015902-7f03bd is serving now — another deploy or rollback landed
while this one waited for the lock
  rerun it to plan against the current state
```

That is what stops a deploy that queued behind another from silently undoing it.
In CI, let the pipeline queue deploys too; see
[Deploying from CI]({% link deploying/ci.md %}#run-one-deploy-per-target-at-a-time).

### If the connection drops

The lock belongs to the helper process on the server, not to your terminal or CI
job, so what an interruption does depends on when it happens:

- **Before the release has been handed over** — during the build or the
  transfer — the helper sees the session end, releases the lock, and nothing on
  the server has changed.
- **After** — the helper already has the whole release, so it finishes the
  deploy, records the outcome and only then releases the lock. A cancelled job
  cannot leave a container stopped with its replacement never started.
  `shunt status` shows how it ended.

A machine that vanishes without closing the connection — a laptop lid, a dead
NAT mapping — sends no such signal. So the CLI sends a heartbeat while it
transfers, and a helper that hears nothing for five minutes releases the lock,
having changed nothing.

## Outcomes

Every attempt is recorded with one of these:

| Status | Meaning |
|:--|:--|
| `active` | Serving. |
| `superseded` | Replaced by a later release. |
| `failed` | Failed before any running container was replaced. Production is untouched. |
| `degraded` | Failed after replacing at least one container. The server is running a mix of releases. |

A failed attempt does not change which release `shunt status` calls serving, so
status never contradicts the error you just saw:

```
release  20260728-012635-4be1d0  ● active  2026-07-28 01:26:48

last attempt 20260728-012823-9a71c2 failed — the release above is still serving
stage "doomed" failed: exit status 1
```

A `degraded` release is recovered with `shunt rollback`, or automatically with
`shunt up --rollback-on-failure`; see
[Rollback]({% link deploying/rollback.md %}#recovering-from-a-partial-failure).
