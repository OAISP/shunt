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

Verification already knows exactly which blobs rsync rewrote this transfer,
which is precisely the set the daemon cannot already hold — so those are the
only layer blobs the load archive carries. Measured at 404 MB of tar becoming
40 KB.

Whether a daemon will take that depends on its image store, and shunt does not
try to know in advance. The containerd snapshotter resolves the absent layers
out of its own content store. The classic overlay2 importer resolves every layer
path out of the archive and fails on the first one missing, which is most
servers — so on those the attempt is refused and the whole layout follows.

That is why the partial load is an attempt rather than a step. A daemon declines
it in two shapes — an error, or a success that produces no image — and both mean
the same thing, so both fall through to sending everything. Treating only the
second as recoverable is a bug that hides until you meet the store that produces
the first, at which point nothing deploys at all.

The archive is built in Go rather than shelled out to `tar`, which is how the
filtering is expressed and why the host does not need `tar` at all.

## A replayed release is not the spec the ledger holds

Rollback re-applies a previous release from its stored description, and two
things in that description are true only of the deploy that produced it.

Its images are marked external because that is how they were obtained
originally; on a replay they are already on this host, so a replay clears the
flag and goes straight to the swap. And its secret values are salted hashes,
because the ledger is a file on the host and must never hold plaintext.

The second one is the trap. Every path that creates a container writes the
secrets out from the spec it is handed — an env-file, or a directory of files
under `/run/secrets`. Handed the stored spec verbatim, those paths do exactly
what they are told: containers come up holding `h:3f2a…` where the password
should be, and file mode clears and rewrites the retained plaintext with the
hashes on the way past, losing it. Only one shape escaped it — `mode = "env"`
with no per-service scoping, where the retained env-file is passed through
untouched, which is the default and is why it went unseen.

So a replay reconstructs the spec rather than reusing it: values are read back
from the env-file or from the union of that release's secrets directories, and
a release missing any of them is refused by name. The union is not incidental —
a manifest whose services all narrow their secrets has no unscoped directory at
all, so looking for one reported such a release as pruned when nothing had been.

The correction is made on a copy. The ledger entry is a pointer into the record
about to be saved, and neither adjustment belongs in the permanent history.

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
