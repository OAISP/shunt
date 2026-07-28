---
title: Stages
parent: Manifest reference
nav_order: 4
---

# Stages

A stage is a one-shot container that must run to completion *before* any running
service is replaced. What it does is up to you. Database migrations and
pre-deploy backups are the common cases, but a stage is just "run this, and stop
the deploy if it fails".

```toml
[[stages]]
name    = "migrate"
image   = "app"                  # any image in this manifest, or a pullable ref
command = ["npm", "run", "migrate"]
```

| Key | Default | Meaning |
|:--|:--|:--|
| `name` | — | Required, unique. |
| `image` | — | Required. |
| `command` | — | Required. |
| `env` | none | Extra environment for this stage. |
| `capture` | none | Redirect stdout to a file on the host. |
| `require_nonempty` | false | Fail the deploy if the capture is empty. |
| `retain` | 10 | Capture generations to keep. |

Stages run after accessories and before anything is swapped. If one fails,
production is exactly as it was — that ordering is the safety property the whole
pipeline is built around.

## Captures

`capture` redirects a stage's stdout to a file on the host, and
`require_nonempty` fails the deploy if that file comes out empty. Together they
are what makes a pre-migration dump trustworthy rather than decorative:

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

{% raw %}`{{.Release}}`{% endraw %} expands to the release id and
{% raw %}`{{.Timestamp}}`{% endraw %} to a UTC timestamp. Captures are written `0600`: a `pg_dump` is every row of production,
and the host has other people's containers on it.

Retention keeps the newest `retain` generations of each stage's capture, matched
on the template's literal prefix and suffix rather than on the rendered filename.

Pull a capture back down with `shunt fetch`:

```sh
shunt fetch                                              # list what is available
shunt fetch /opt/acme/backups/pre-migrate-20260728.sql
```
