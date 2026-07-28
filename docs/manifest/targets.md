---
title: Deploy targets
parent: Manifest reference
nav_order: 7
---

# Deploy targets

Declare targets only if you deploy the same project to more than one server —
staging and production, typically.

```toml
project = "acme"
host    = "deploy@prod.example.com"

[targets.staging]
host = "deploy@staging.example.com"

# project = "acme-stg"        # defaults to "acme-staging"
#
# [targets.staging.secrets]   # staging should not hold production credentials
# provider = "file"
# path     = "secrets/staging.env"
```

```sh
shunt up               # the host at the top of the file
shunt up -t staging    # the staging target
```

`-t` / `--target` works on every command that talks to a host.

| Key | Default | Meaning |
|:--|:--|:--|
| `host` | — | Required. |
| `project` | `<project>-<target>` | Namespaces containers, network and ledger. |
| `secrets` | the manifest's own | A different provider block for this target. |

## What a target may change

A target changes **where** a release goes, never **what** it contains. The
images, services, stages and artifacts are the ones declared at the top of the
file, whichever target you select. That preserves the rule that no deploy-time
flag alters what gets deployed.

The project name is namespaced per target by default, so `acme` becomes
`acme-staging`. Two targets can therefore share one machine without colliding on
container names, the network, or the ledger — which is the case that otherwise
corrupts state silently.
