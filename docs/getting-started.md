---
title: Getting started
nav_order: 2
---

# Getting started

## Install

The installer detects your platform, verifies the archive against the release's
`checksums.txt`, and needs no toolchain:

```sh
curl -sSL https://raw.githubusercontent.com/OAISP/shunt/main/install.sh | sh
```

It never runs sudo. It installs to `/usr/local/bin` when that is writable and to
`~/.local/bin` otherwise. `SHUNT_VERSION=v0.3.0` pins a release and
`PREFIX=<dir>` chooses the directory. Read the script first if you would rather
not pipe one into a shell — a reasonable instinct for a tool that will hold ssh
access to your servers.

Release archives exist for `linux_amd64`, `linux_arm64`, `darwin_amd64` and
`darwin_arm64`:

```sh
curl -sSL https://github.com/OAISP/shunt/releases/latest/download/shunt_linux_amd64.tar.gz | tar xz
sudo install -m755 shunt /usr/local/bin/shunt
```

With Go — note the `/cmd/shunt` suffix:

```sh
go install github.com/OAISP/shunt/cmd/shunt@latest
```

A `go install` build carries no prebuilt host helper, so the first time it
connects to a host it compiles one with your Go toolchain. That takes a second,
and the host keeps the result.

From source:

```sh
git clone https://github.com/OAISP/shunt && cd shunt
make install    # builds ./shunt with the helpers embedded, installs to ~/.local/bin
```

## Requirements

**Your machine:** docker with a buildx builder that can export an OCI layout,
rsync and ssh. Docker is only needed for images shunt builds; a
[prebuilt image]({% link manifest/images.md %}#prebuilt-images) needs none.

**The server:** docker, rsync, and an ssh user that can reach the docker socket.
Nothing else — shunt uploads its own helper, which does the health probes and
builds the load archive itself.

### A builder that can export an OCI layout

The OCI layout is how shunt ships an image, so this is not optional. Docker's
default `docker` builder can only export one when the daemon uses the
containerd image store, which is the default on new Docker 29 installs but not
on older or upgraded ones. If yours cannot, create a builder that can:

```sh
docker buildx create --use --name shunt --driver docker-container
```

`shunt audit` reports which case you are in and prints that command when it is
needed.

### Either image store on the server

The server needs nothing of the kind. shunt writes a layout that both the
containerd image store and the classic overlay2 store can load, so a laptop on
one can deploy to a server on the other.

### rsync versions

Any rsync works. Image layers travel as they are, since they are already
compressed. Artifacts are compressed in transit: with zstd when both ends run
rsync 3.2 or newer, and with plain `-z` otherwise — Ubuntu 20.04 ships 3.1.3 and
macOS ships 2.6.9.

## A first deploy

```sh
cd my-project
shunt init --host deploy@vps.example.com
shunt audit
shunt plan
shunt up
```

**`shunt init`** writes `shunt.toml`, taking the port from the first `EXPOSE` in
your Dockerfile:

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

That is a complete manifest. It guesses rather than asks: a wrong guess in a
file you are about to read is cheaper than a wizard. `host` is resolved through
your own `~/.ssh/config`, so an alias works.

**`shunt audit`** connects and checks everything a deploy depends on — docker,
buildx and rsync here; ssh, docker, rsync and free disk on the server; artifact
destinations; and whether each image's platform matches the server's
architecture. It changes nothing.

**`shunt plan`** builds, then shows what a deploy would change. It changes
nothing on the server either.

**`shunt up`** builds, ships, swaps and health-checks. It shows the plan and asks
before applying when run in a terminal; in CI, where there is no terminal, it
proceeds.

## When the first deploy fails

**The image exits immediately.** An arm64 Mac builds arm64 images, which load
on an amd64 server and then fail to run. Set `platform = "linux/amd64"` under
`[images.app]`. `shunt audit` warns about the mismatch.

**An artifact destination is not writable.** Paths under `/opt` belong to root.
shunt checks every destination before it builds anything and prints the
`mkdir` and `chown` that fix it.

**The health check passes on a broken app.** A redirect counts as healthy by
default, which proves only that something is listening. See
[health checks]({% link manifest/services.md %}#health-checks).

**`ssh` works by hand but shunt cannot connect.** shunt runs ssh with
`BatchMode=yes`, so a key that needs a passphrase must already be in your agent,
and the host key must already be in `known_hosts`.
