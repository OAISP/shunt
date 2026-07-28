---
title: Getting started
nav_order: 2
---

# Getting started

## Install

The installer detects your platform, verifies the checksum, and needs no
toolchain:

```sh
curl -sSL https://raw.githubusercontent.com/OAISP/shunt/main/install.sh | sh
```

It never invokes sudo: it installs to `/usr/local/bin` when that is writable and
`~/.local/bin` otherwise. Pin a release with `SHUNT_VERSION=v0.1.0`, choose a
directory with `PREFIX=...`, and read the script first if you would rather not
pipe one into a shell. That is a reasonable instinct for a tool that ends up
holding ssh access to your servers.

Archives are published for `linux_amd64`, `linux_arm64`, `darwin_amd64` and
`darwin_arm64`, with `checksums.txt` alongside:

```sh
curl -sSL https://github.com/OAISP/shunt/releases/latest/download/shunt_linux_amd64.tar.gz | tar xz
sudo install -m755 shunt /usr/local/bin/shunt
```

With Go:

```sh
go install github.com/OAISP/shunt/cmd/shunt@latest
```

Note the `/cmd/shunt` suffix — the module root is a library, not a command. A
`go install` build carries no prebuilt host helper, so shunt compiles one from
its own source the first time it connects to a host. That takes a second and
uses the Go toolchain you already have; the helper is cached on the host
afterwards.

From source:

```sh
git clone https://github.com/OAISP/shunt && cd shunt
make install    # builds ./shunt with helpers embedded, installs to ~/.local/bin
```

## Requirements

**Your machine:** docker with a buildx builder that can export an OCI layout,
rsync, ssh.

**The server:** docker, rsync, curl, and an ssh account that can reach the
docker socket. Nothing else — shunt uploads its own helper.

That OCI layout is how shunt ships an image, so exporting one is not optional.
Docker's default `docker` builder can only do it when the daemon runs the
containerd image store; without that it refuses outright. Either fix works:

```sh
docker buildx create --use --name shunt --driver docker-container
```

`shunt audit` reports which of the two you have and names the fix if you have
neither — worth running once before your first deploy, since otherwise this
surfaces as a build error on the very first `shunt up`.

`curl` runs url health checks. It is present on essentially every distro image
but not on some minimal ones, so `shunt audit` checks for it, and so does every
command that connects.

rsync 3.2 or newer on *both* ends gets zstd transfer compression. Older versions
still work: Ubuntu 20.04 ships 3.1.3 and macOS ships 2.6.9 as `/usr/bin/rsync`,
and shunt detects both and falls back rather than failing.

Either image store works **on the server**. shunt writes a layout that both the
containerd snapshotter and the classic overlay2 store can load, so a laptop can
deploy to a server running either one with no daemon configuration there. The
builder side is the asymmetric one, for the reason above: loading a layout works
everywhere, producing one does not.

## A first deploy

```sh
cd my-project
shunt init --host deploy@vps.example.com   # writes shunt.toml, guesses the port
shunt audit                                # check the host can accept a deploy
shunt plan                                 # see exactly what would change
shunt up
```

`shunt init` scaffolds a manifest, taking the port from the first `EXPOSE` in
your Dockerfile. It guesses rather than interrogates: a wrong guess in a file you
are about to edit is cheaper than a wizard.

`shunt audit` connects and checks everything a deploy depends on — docker and
buildx locally, ssh, docker, rsync and curl on the host, free disk, artifact
paths, and whether the image platform matches the host architecture. It changes
nothing. Running it before the first deploy turns most first-deploy failures
into a list you can read.

`shunt plan` builds and diffs but changes nothing on the host. Read it before
your first `up`.

## The manifest it writes

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

That is a complete, working manifest. Everything else in the
[manifest reference]({% link manifest/index.md %}) is optional, and each block
does nothing at all unless you declare it.

{: .note }
Unknown keys are a hard error. A typo in a field that silently does nothing is
worse than a failed load.

## Common first-deploy problems

**Architecture mismatch.** Building on an arm64 Mac for an amd64 server produces
an image that loads fine and then exits immediately. Set
`platform = "linux/amd64"` under `[images.app]`. `shunt audit` warns about this
before you hit it.

**`/opt` is root-owned.** Artifact destinations under `/opt` are not writable by
a normal deploy user. shunt checks every destination before building anything
and prints the `mkdir`/`chown` to run.

**Health check passes on a broken app.** By default a 3xx counts as healthy,
which proves only that the server is listening. See
[health checks]({% link manifest/services.md %}#health-checks).
