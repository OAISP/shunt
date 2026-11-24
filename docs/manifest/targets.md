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

They do still share everything a release names on the server itself, because
what a release contains is the same for every target:

- a `publish` port or a `proxy` host, which two targets would both claim;
- an artifact `dest`, a stage's `capture` path, a bind mount or a named volume,
  which two targets would both write. Deploying staging would replace
  production's data.

Sharing a server suits targets that use none of these — stateless workers, say.
Otherwise give each target its own server.
