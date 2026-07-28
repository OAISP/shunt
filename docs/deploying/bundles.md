---
title: Bundles
parent: Deploying
nav_order: 3
---

# Bundles

Sometimes the machine that can build a release cannot reach the machine that
should run it: an air-gapped network, a change-approval queue, a laptop with the
source but no VPN.

```sh
shunt bundle                          # writes <project>-<release>.shuntpkg
scp acme-....shuntpkg ops:            # or a USB stick, or an approval queue

shunt bundle inspect acme-....shuntpkg
shunt bundle verify  acme-....shuntpkg
shunt apply --plan   acme-....shuntpkg
shunt apply          acme-....shuntpkg
```

`shunt bundle` does everything `shunt up` does up to the transfer and writes the
result to a file instead of a host. It never connects, so it works with the
target unreachable.

`shunt apply` needs **no Dockerfile, no `shunt.toml` and no source** — and no
Docker at all on the machine that runs it. It deploys through exactly the same
path as `shunt up`: the same lock held across the transfer, the same check that
the host has not moved on since the plan, the same helper. Layer deduplication
still applies, so re-applying a bundle to a host that already has most of the
image ships only the difference.

## Inspecting before you trust

`shunt bundle inspect` is the command for someone handed a bundle to approve:
which release, from which commit and whose build, for which host, running what,
and which secrets it will want.

```
acme-20260728-093012.shuntpkg ·  84.2 MB

  release  20260728-093012-a1b2c3
  built    2026-07-28T09:30:14Z
  for      deploy@prod.example.com
  from     main@d4d427195445 by ci@runner

  images
    app            a4c78dc30c9f

  services
    app            image app · publish 127.0.0.1:9090:3000 · health /health

  secrets
    1 required, resolved where this is applied:
      DATABASE_URL
```

It reads only the description, which is the first entry in the archive, so it
answers immediately on a bundle of any size. It also says so loudly when the
build came from a modified working tree, since that is precisely when the commit
id describes nothing.

`shunt bundle verify` rehashes every blob against its own filename and checks
that the release's images and artifacts are all present. That is the same check
the host performs on load, run without a host, so a bundle can be proven intact
before being carried somewhere retrying is expensive.

{: .note }
Verification proves the bytes are intact. It does not prove the release works.

`shunt apply --plan` answers "what will this do to my host" before it does it,
with the same output and exit codes as `shunt plan`. It works even where the
secrets are not reachable, saying the secret diff was skipped rather than
reporting every key as removed.

## What a bundle carries

The release spec, the OCI layouts, and any artifacts. Deliberately not:

- **Secret values.** A file that sits in a queue or on a stick is the last place
  production credentials belong, and encrypting them into it only moves the key
  problem. The provider block and the key *names* travel instead, and
  `shunt apply` resolves the values locally — so whoever applies a bundle needs
  access to the secrets, which is the correct requirement. Applying refuses if
  the local provider does not supply a key the release needs.
- **The host helper.** The CLI applying the bundle already embeds one.
- **A checksum of its own.** Every blob inside is content-addressed and rehashed
  on load, so a damaged bundle fails on the way in.

Bundles are always complete rather than a delta against a particular host. A
delta is only valid against the state it was computed from, and that is not a
promise a file sitting in a queue can keep.

Applying the same bundle twice is refused: release ids are immutable and the
host has already recorded that one. Deploy it elsewhere with `--host`.
