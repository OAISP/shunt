# `shunt bundle` — a design note

**Status: not built.** This records a design so that nothing in the current
codebase forecloses it, and so the idea does not have to be re-derived later.
Nothing below is implemented.

## The idea

Package a release — spec, OCI metadata, the blobs a target does not have, and
artifacts — into one file that can be carried to a host by some means other than
a live ssh connection, and applied there later.

```sh
shunt bundle -o acme-20260728.shuntpkg     # on a machine with the source
scp acme-20260728.shuntpkg air-gapped:     # or a USB stick, or an approval queue
shunt apply acme-20260728.shuntpkg         # on, or from, the target
```

## Why it fits

The registry-free story already implies most of it. shunt builds a
self-contained OCI layout, resolves a complete `release.Spec` that the helper
applies without ever reading the manifest, and ships a content-addressed helper
binary. A bundle is those three things in a container format rather than on a
socket.

It is the one direction that is genuinely unavailable to registry-based tools:
they need a registry reachable from the target, and the whole point of an
air-gapped or change-controlled environment is that there isn't one.

## Why it is deferred

It is a new subsystem, not a feature. It needs its own format, its own
versioning, its own integrity story, and its own failure modes — and every one
of those is a thing that can be wrong in a way the current code cannot be.
A correctness release is the wrong place for it.

## What a bundle would contain

```
acme-20260728.shuntpkg          (uncompressed tar; the blobs inside are already compressed)
  manifest.json                 bundle format version, project, release id, created-at
  spec.json                     the release.Spec, secrets omitted (see below)
  provenance.json               commit, branch, dirty, deployer, cli version
  images/<name>/                OCI layout, blobs pruned to what the target lacks
  artifacts/<name>              file or tree
  helper/shunt-helper-<arch>    content-addressed, as uploaded today
```

### Blob selection

A bundle is either *full* or *differential*.

- **Full** carries every blob. Always applicable; the size of the image.
- **Differential** carries only the blobs a named target lacks, which requires
  having asked that target what it holds — `shunt bundle --for prod` would do
  the `ls` the plan's estimate already does. Applying it to a host that turns
  out to lack a blob must fail loudly and name the missing digest, never
  half-load an image.

The `--for` form is the interesting one and the one that needs the most care:
a differential bundle is only valid against the state it was computed from.
Recording the set of digests it *assumes* present, and checking that set before
loading anything, is what makes that safe. This is the same shape as the
`expected_current` check the deploy path already uses.

### Secrets

Secrets must not be in the bundle. A file that sits in an approval queue or on a
USB stick is exactly the artifact you do not want carrying production
credentials, and encrypting them into it just moves the key problem.

`spec.json` therefore records secret *keys* and their hashes but no values, and
`shunt apply` resolves them at apply time from the applying machine's own
provider — meaning the operator applying a bundle needs access to the secrets,
which is the correct requirement.

### Integrity

Every blob is already content-addressed and re-verified on load, so the bundle
inherits that check for free. The bundle itself needs a detached digest so the
carrying medium can be checked before anything is unpacked; a `.sha256` beside
the file, matching what `install.sh` and the release archives already do, is
enough and needs no new tooling.

## What would have to change

Almost nothing structural, which is the point of writing this down now:

- `release.Spec` is already fully self-contained and serialisable. The only
  field that would need care is `StorePath`, which is a host path baked at plan
  time — a bundle would have to resolve it at apply time instead.
- `engine.Build` already produces exactly the layout a bundle would carry.
- The helper already accepts a spec on stdin and needs no other input, so
  `shunt apply` is largely `bundle → stdin → existing helper`.
- The verification sidecar is host-side state and must be excluded from a
  bundle, the same way it is excluded from the mirror and the load tar.

The one genuinely new piece is the format itself and its version negotiation.

## Open questions

- Does `shunt apply` run from the bundle on the operator's machine (ssh to the
  target as usual) or from the target itself (no ssh at all)? The second is more
  useful for air-gapped environments and needs the helper to grow a mode that
  reads a bundle rather than a spec.
- Should a bundle be applicable more than once? Idempotence is nearly free given
  release ids are immutable, but "apply this twice" and "roll back to a bundle"
  are different questions with different answers.
- How does a bundle interact with `expected_current`? An approval queue means
  arbitrary delay between building and applying, so the check that makes deploys
  safe is precisely the one most likely to reject a bundle. It may need to be
  opt-out for this path, with the consequences stated plainly.
