---
title: Design decisions
parent: Internals
nav_order: 1
---

# Design decisions

## The helper is content-addressed

The helper is embedded in the CLI and uploaded under a filename derived from its
own sha256. A rebuilt helper always lands at a new path and an unchanged one is
never uploaded again, so the two ends cannot drift apart: whatever a CLI talks
to is exactly the helper it was built with. The protocol number in every spec
is a second line of defence.

## ssh is the real binary, not a library

Your ssh config, agent, jump hosts and hardware keys work with no configuration,
and shunt never has to invent key management. rsync rides the same multiplexed
connection.

## The store is a mirror, not an archive

rsync runs with `--delete`, so each image's layout on the server matches the
latest build exactly and never grows without bound. Rollback does not depend on
it: it reuses the release-tagged images Docker already holds.

A shared append-only blob pool with a garbage collector would let concurrent
deploys write without stepping on each other, but costs that bound. Holding the
deploy lock across the transfer gives the same safety and keeps it.

## The lock belongs to the helper

The deploy lock is a `flock` taken by the helper, in the same ssh session that
later receives and applies the release, and opened before the transfer.

Two properties follow, and both matter most in CI, where jobs get cancelled.
A session that ends before the spec arrives — a cancelled build, a failed
transfer — leaves the helper reading end-of-input, so it releases the lock
having changed nothing. A session that ends after has handed over the whole
release, so the helper finishes it under the lock, ignoring the broken pipe its
status output now writes to. Had the lock belonged to the CLI instead, the
second case would either kill the deploy halfway — the default reaction to a
broken pipe — or let the next queued deploy rsync into the store while this one
still loads from it.

Because the lock is announced together with the release then serving, the
check that a plan still holds costs no round trip of its own.

## The plan describes the server, not the history

Every container carries a hash of the definition it was created from. A plan
compares the manifest against those labels, not only against what shunt last
recorded, so a container deleted, stopped or replaced by hand is reported as
work rather than read as unchanged.

## A container is replaced only when it would come out different

A second label carries a keyed digest of everything else a container is created
from: what its image runs, the deploy network, how secrets are delivered and
the values of the secrets it receives. A deploy leaves alone any running
container whose two labels match what it would create, which is also how a
rollback knows what a failed deploy never got to. The decision is read off the
container rather than the release history, so it holds however many releases
the container has outlived.

The image half of that digest is a fingerprint of the layers and runtime
settings, not the manifest digest: CI stamps the commit and build time into
every image, and a digest that changes on every commit would replace every
container on every deploy.

## Only the delta is loaded

The helper rehashes exactly the blobs rsync rewrote, and those are precisely the
layers Docker cannot already hold — so they are the only layers the load
archive carries. Measured: 404 MB of archive becoming 40 KB.

Neither image store needs a layer it already has. The classic overlay2 store
does need the image config, which it reads from the archive rather than from
its own store, so config and manifest blobs always travel. They are a few KB,
and without them a deploy whose image had not changed — only env or secrets —
would have nothing in the archive and fall back to loading the whole image.
Measured on Docker 24, 27 and 29.

The partial load stays an attempt rather than a step. A daemon that lacks a
layer — after a `docker system prune`, say — declines it either with an error or
with a load that produces no image, and both fall back to sending the whole
layout. The archive is written by the helper rather than by `tar`, which is how
the filtering is expressed and why the server needs no `tar`.

## Images travel uncompressed

Layer blobs are already gzip or zstd compressed, and their filenames are bare
digests, so rsync's skip-compress list cannot recognise them. Compressing them
again costs CPU on both ends — badly with the zlib fallback older rsyncs get —
to save almost nothing. Images therefore go uncompressed, at the price of a few
KB of uncompressed JSON per deploy. Artifacts, which are often compressible
databases, are still compressed.

## A replayed release is not the spec the history holds

Rollback re-applies a stored release, and two things in what is stored are true
only of the deploy that produced it.

Its images may be marked as pulled, because that is how they arrived; on a
replay they are already on the server, so the flag is cleared. And its secret
values are salted hashes, because the history is a file on the server and must
never hold plaintext.

Every path that creates a container writes secrets from the spec it is given,
so a replay handed the stored spec would start containers holding hashes where
passwords belong — and in file mode overwrite the retained plaintext with them.
A replay therefore rebuilds the spec: values are read back from the release's
env-file, or from the union of its secrets directories, since a manifest whose
services all narrow their secrets has no unnarrowed directory at all. A release
missing any value is refused by name. The rebuilt spec is a copy; the history
keeps what was deployed.

## Modification times are load-bearing

rsync's quick check compares size *and* modification time, and shunt's artifact
comparison uses the same pair, so both have to be stable:

- Blob mtimes are fixed at export. A blob's name already identifies its
  contents, and a fresh mtime on every export would make rsync treat the whole
  image as changed.
- A bundle carries modification times through the archive, or every artifact
  in it would look changed on every apply.
- A directory artifact's size-and-mtime summary is computed by one function on
  both machines. Two implementations that disagreed on anything — a directory
  entry counted, a symlink — would make that artifact differ from itself on
  every deploy.

## Colour lives in one package

`internal/ui` is the only place an escape code appears, so `NO_COLOR`, dumb
terminals and redirected output are handled once. Piped output is plain text
everywhere, progress included.

## Testing the deploy path

Host-side logic reaches Docker through a `runner` interface, so container swaps,
health gating and rollback failures are testable without a daemon. Above that,
`test/e2e.sh` deploys for real over ssh — including a deploy cancelled midway
and one queued behind another — and `test/compat.sh` builds a server with the
problems a healthy one cannot reproduce: rsync 3.1.3 with no zstd, and neither
`curl` nor `tar`.
