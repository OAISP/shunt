---
title: Design decisions
parent: Internals
nav_order: 1
---

# Design decisions

## The helper is content-addressed

The helper binary is embedded in the CLI and uploaded under a filename derived
from its own sha256. A rebuilt helper always lands at a new path, and an
unchanged one is never re-uploaded, so version skew between the two ends is
impossible by construction. The wire protocol carries a version check as a
second line of defence.

## The store is a mirror, not an archive

rsync runs with `--delete`, so the host's OCI layout matches the latest build
exactly and can never grow without bound. Rollback does not depend on it: it
re-uses the release-tagged images Docker already holds.

That is why the alternative — a shared append-only blob pool with a garbage
collector — was rejected when closing the concurrent-deploy window. Holding the
lock across the transfer closes the same window without giving up the property.

## ssh is the real binary, not a library

That is why your existing ssh config, agent, jump hosts and hardware keys work
with no configuration, and why shunt never has to invent key management. It also
lets rsync ride the very same multiplexed connection.

## Colour lives in exactly one package

`internal/ui` is the only place an escape code appears, so `NO_COLOR`, dumb
terminals and redirected output are handled once rather than per command. Piped
output is plain text everywhere, including the progress stream.

## The plan describes the host, not the ledger

Every container carries a hash of the definition it was started with. A plan
compares the manifest against those labels, not only against what shunt last
recorded, so a container deleted, stopped or replaced by hand is reported as
work rather than read as "unchanged".

## Only the delta is loaded

The daemon accepts an OCI layout whose already-known layers are absent, on both
the containerd snapshotter and the classic overlay2 store. Since verification
already knows exactly which blobs rsync rewrote this transfer — which is
precisely the set the daemon cannot hold — those are the only layer blobs the
load archive carries. Measured at 404 MB of tar becoming 40 KB.

A partial load that does not produce the image falls back to sending the whole
layout, so this is an optimisation and never a requirement.

The archive is built in Go rather than shelled out to `tar`, which is how the
filtering is expressed and why the host does not need `tar` at all.

## mtimes are load-bearing

rsync's quick check compares size *and* modification time, and shunt's own
artifact comparison uses the same pair. Three separate bugs came from mtimes not
being preserved or not being stable:

- A re-exported layout gave every blob a fresh mtime, so rsync considered the
  whole image changed and rewrote all of it. Blob mtimes are normalised at build
  time; a blob's filename already identifies its contents, so its mtime carries
  no information.
- Bundle extraction stamped files with the current time, so every artifact in a
  bundle looked changed on every apply. Modification times are carried through
  the archive.
- The size-and-mtime summary of a directory artifact is computed by one shared
  function, because two implementations differing by so much as a directory
  inode would make every directory artifact differ from itself forever.

## Testing the deploy path

Host-side logic reaches docker through a `runner` seam, so container swaps,
health gating and rollback failures are assertable without a daemon.

Above that, `test/e2e.sh` runs a real deploy over real ssh against a real
daemon, and `test/compat.sh` builds hosts that actually have the compatibility
problems a healthy machine cannot reproduce — rsync 3.1.3 with no
`--compress-choice`, and a host with neither `curl` nor `tar`.
