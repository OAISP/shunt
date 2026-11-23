---
title: Bundles
parent: Deploying
nav_order: 3
---

# Bundles

A bundle is a release in a file. It covers two cases:

- **The builder cannot reach the server** — an air-gapped network, a
  change-approval queue, a laptop with the source but no VPN.
- **You want to build once and deploy the same bytes everywhere** — to staging,
  and after an approval to production.

```sh
shunt bundle -o release.shuntpkg         # build; never connects to a server

shunt bundle inspect release.shuntpkg    # what is in it
shunt bundle verify  release.shuntpkg    # are the bytes intact
shunt apply --plan   release.shuntpkg    # what would it change
shunt apply          release.shuntpkg    # deploy it
```

`shunt bundle` does everything `shunt up` does up to the transfer, then writes
the result to a file. Without `-o` it is named `<project>-<release>.shuntpkg`.

`shunt apply` needs no `shunt.toml`, no source and no Docker on the machine
that runs it — only ssh and rsync. It deploys through the same path as
`shunt up`: the same lock, the same check that the server has not moved on, the
same helper. Deduplication still applies, so applying a bundle to a server that
already has most of the image sends only the difference.

## What a bundle carries

The release description, the image layouts, any artifacts, and the destination
of every target in the manifest. Deliberately not:

- **Secret values.** A file that waits in a queue or travels on a USB stick is
  the last place for credentials. The bundle records the provider block and the
  key names; `shunt apply` resolves the values on the machine applying it, and
  refuses if a key the release needs is missing. Building a bundle does not
  need the values either: if they are not reachable, the bundle records the key
  names `shunt.toml` declares.
- **The server helper.** The `shunt` that applies the bundle carries its own.
- **A checksum of its own.** Every blob is named by its hash and rehashed on the
  server, so a damaged bundle fails on the way in.

A bundle is always complete, never a delta against one server's state: a file
sitting in a queue cannot promise that state still holds.

## Promoting across targets

A target changes where a release goes and never what it contains, so one bundle
can be applied to every target in turn:

```sh
shunt bundle -o release.shuntpkg
shunt apply -t staging release.shuntpkg   # a target from [targets.*]
shunt apply release.shuntpkg              # the manifest's own host
```

Applying with `-t` uses that target's host, project name and secrets provider.
Staging and production then run identical images, rather than two builds that
are probably the same. [Deploying from CI]({% link deploying/ci.md %}#build-once-promote)
has a pipeline with an approval step in between.

Build the bundle without `-t`. A bundle built with `-t staging` treats staging
as its own destination, and carries no entry for the manifest's own host.

## Inspecting before you trust

`shunt bundle inspect` is for whoever is asked to approve a bundle: which
release, from which commit and whose build, for which server, running what, and
which secrets it will need.

```
release.shuntpkg ·  84.2 MB

  release  20260728-093012-a1b2c3
  built    2026-07-28T09:30:14Z
  for      deploy@prod.example.com
  from     main@d4d427195445 by octocat@github-actions

  images
    app            a4c78dc30c9f

  services
    app            image app · publish 127.0.0.1:9090:3000 · health /health

  secrets
    1 required, resolved where this is applied:
      DATABASE_URL
```

It reads only the description at the start of the file, so it answers at once
whatever the bundle's size, and it says so plainly when the build came from a
modified working tree, since then the commit describes nothing.

`shunt bundle verify` rehashes every blob and checks that the release's images
and artifacts are all there — the check the server makes on load, done before
the file travels somewhere a retry is expensive. It proves the bytes are
intact, not that the release works.

`shunt apply --plan` shows what applying would change, like `shunt plan`. It
works where the secrets are not reachable, and says it skipped the secret diff
rather than reporting every key as removed.

Each release id is applied to a server once; applying the same bundle again is
refused. `--host` sends it to a different server instead.
