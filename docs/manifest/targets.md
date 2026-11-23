---
title: Deploy targets
parent: Manifest reference
nav_order: 7
---

# Deploy targets

Declare targets if you deploy the same project to more than one server —
staging and production, typically.

```toml
project = "acme"
host    = "deploy@prod.example.com"

[targets.staging]
host = "deploy@staging.example.com"
# project = "acme-stg"        # default: "acme-staging"

[targets.staging.secrets]     # staging should not hold production credentials
provider = "file"
path     = "secrets/staging.env"
```

```sh
shunt up               # the host at the top of the file
shunt up -t staging    # the staging target
```

`-t` / `--target` works on every command that talks to a server, including
`shunt apply`, which is how one [bundle]({% link deploying/bundles.md %}#promoting-across-targets)
goes to staging and then to production.

| Key | Default | Meaning |
|:--|:--|:--|
| `host` | — | Required. |
| `project` | `<project>-<target>` | The project name on that server. |
| `secrets` | the manifest's own | A different `[secrets]` block for this target. |

## What a target may change

A target changes **where** a release goes — the server, the project name it
runs under there, and the secrets it gets — and never **what** it contains.
Images, services, stages and artifacts are the ones declared in the file,
whichever target you pick.

The project name gets the target as a suffix by default, and the network
follows it unless you set `network` yourself. Two targets can therefore share
one server without sharing containers, a network or a release history.

They do still share the server's ports and proxy. A `publish` mapping or a
`proxy` host is part of what a release contains, so it is the same for every
target, and two targets on one server would claim the same port or hostname.
Sharing a server suits targets whose services do neither — workers, say.
Otherwise give each target its own server.
