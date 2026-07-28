---
title: Manifest reference
nav_order: 3
has_children: true
---

# Manifest reference

`shunt.toml` is the committed description of production. Everything the host
runs derives from this file plus resolved secrets. There are no deploy-time
flags that change *what* gets deployed — only which host it goes to.

A complete, working manifest:

```toml
project = "acme"
host    = "deploy@vps.example.com"

[images.app]
context = "."

[services.app]
image   = "app"
publish = ["127.0.0.1:9090:3000"]

[services.app.health]
url = "/health"
```

Everything in the pages below is optional. Each block does nothing at all unless
you declare it, and plenty of projects never need any of them.

## Top-level keys

| Key | Type | Meaning |
|:--|:--|:--|
| `project` | string | Names containers, the network and the on-host ledger. Lowercase alphanumeric with `-` or `_`. Required. |
| `host` | string | `user@host`, resolved through your own `~/.ssh/config`. Required. |
| `network` | string | Docker network for this project. Defaults to `<project>-net`. |
| `retain` | int | Restorable releases kept on the host. Defaults to 5. |

## Validation

Unknown keys are a hard error. A typo in a field that silently does nothing is
worse than a failed load.

Validation reports every problem at once rather than the first one, so a
misconfigured manifest takes one round of fixes rather than five:

```
error: invalid shunt.toml:
  - service "app": proxy needs a host
  - service "app": cannot combine `publish` with `proxy`
  - stage "backup": require_nonempty has no meaning without capture
```

`shunt validate` runs exactly this check with no ssh and no build, which makes
it suitable for an editor hook or a pull-request check.
