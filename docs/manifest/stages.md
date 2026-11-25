---
title: Stages
parent: Manifest reference
nav_order: 4
---

# Stages

A stage is a one-shot container that must exit successfully before any running
service is replaced. Migrations and pre-deploy backups are the usual ones, but a
stage is simply "run this, and stop the deploy if it fails".

```toml
[[stages]]
name    = "migrate"
image   = "app"                  # an image from this manifest, or a pullable one
command = ["npm", "run", "migrate"]
```

| Key | Default | Meaning |
|:--|:--|:--|
| `name` | — | Required, unique. |
| `image` | — | Required. |
| `command` | — | Required. |
| `env` | none | Extra environment. Supports [`${env:NAME}`]({% link manifest/index.md %}#environment-interpolation). |
| `capture` | none | Write the stage's stdout to this file on the server. |
| `require_nonempty` | `false` | Fail the deploy if the capture comes out empty. |
| `retain` | `10` | Capture files to keep for this stage. |

Stages run in order, after accessories are up and before anything is replaced,
on the deploy network and with every secret. If one fails, production is
exactly as it was.

A stage that changes data in a way the previous release cannot run against can
say so, by writing `irreversible=true` to the file named in
`SHUNT_STAGE_OUTPUT`. That stops `shunt up --rollback-on-failure` from putting
the old code back on top of it; see
[stages that cannot be undone]({% link deploying/rollback.md %}#stages-that-cannot-be-undone).

Stages run on every deploy, not only when they change — a migration that has
nothing to do should exit 0. Adding or changing a stage counts as a change, so
a manifest whose only edit is a new migration still deploys.

## Captures

`capture` sends a stage's stdout to a file on the server, and
`require_nonempty` fails the deploy if that file is empty. Together they turn a
pre-migration dump from decorative into something you can rely on:

{% raw %}
```toml
[[stages]]
name             = "backup"
image            = "postgres:18-alpine"
command          = ["sh", "-c", "exec pg_dump --no-owner \"$DATABASE_URL\""]
capture          = "/opt/acme/backups/pre-migrate-{{.Release}}.sql"
require_nonempty = true
retain           = 10
```
{% endraw %}

In the path, {% raw %}`{{.Release}}`{% endraw %} becomes the release id and
{% raw %}`{{.Timestamp}}`{% endraw %} a UTC timestamp. Captures are written
`0600`: a database dump is every row of production.

`retain` keeps the newest captures of that stage. Without a placeholder in the
path, every deploy overwrites the same file — but only with a capture that
succeeded: a stage that fails, or comes out empty under `require_nonempty`,
leaves the previous one in place.

Fetch one with `shunt fetch`, which lists the captures on the server when run
with no arguments:

```sh
shunt fetch
shunt fetch /opt/acme/backups/pre-migrate-20260728-093012-a1b2c3.sql
```
