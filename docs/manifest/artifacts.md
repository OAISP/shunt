---
title: Artifacts
parent: Manifest reference
nav_order: 5
---

# Artifacts

An artifact is a large file, or a directory, that your app needs and the image
does not carry: a SQLite database, model weights, a prebuilt index, a dataset.
Declare one only if you need it — if your state lives in a database server or
object storage, you do not.

```toml
[[artifacts]]
name     = "index"
src      = "data/index.db"            # local, relative to shunt.toml
dest     = "/opt/acme/data/index.db"  # on the host
magic    = "SQLite format 3"          # refuse a file that is not one
retain   = 1                          # generations kept for recovery
required = true                       # absent locally fails the deploy

[services.app]
volumes = ["/opt/acme/data:/data:ro"]
```

| Key | Default | Meaning |
|:--|:--|:--|
| `name` | — | Required, unique. |
| `src` | — | Local file or directory, relative to `shunt.toml`. |
| `dest` | — | Absolute path on the host. |
| `magic` | none | Literal prefix the file must start with. |
| `retain` | 1 | Superseded generations kept for recovery. |
| `required` | false | Fail the deploy when the local copy is missing. |

An artifact that is absent locally and not `required` is skipped, and the host
keeps whatever it already has. That is the right default for a file produced by
an occasional ETL run.

## Directories

`src` may be a directory — model weights, a prebuilt index, an asset tree. The
whole tree is mirrored and swapped in as a unit, and unchanged files inside it
are not re-sent.

```toml
[[artifacts]]
name = "weights"
src  = "models/embed"
dest = "/opt/acme/models/embed"
```

## How the swap works

The new copy lands beside the old under a `.new.<release-id>` suffix. Before
anything is promoted, *every* artifact in the release is validated: exact size
against what the release recorded, `magic` if set, and non-empty. Only then are
they promoted, and if a later promotion fails the earlier ones are put back.

A file is promoted by hard-linking the old copy aside and renaming the new one
over the top, so the destination only ever holds the old contents or the new
ones — never nothing.

A directory is swapped with `RENAME_EXCHANGE`, which exchanges the two trees in
a single atomic step. Verified by polling the destination continuously across a
live swap: 12,104 checks, zero misses. On a kernel older than 3.15, or a
filesystem without `renameat2`, shunt falls back to moving the old tree aside
and says that the guarantee is weaker there.

The superseded copy is kept as `<dest>.prev.<release-id>`, and a failed health
check prints the exact `mv` that puts it back.

{: .warning }
`shunt rollback` does not revert data. It restores containers and images.
Silently rolling a database back as a side effect of rolling code back is the
kind of helpfulness that loses data — the `.prev` copy and the exact command are
printed instead.

## `magic`

`magic` is a literal prefix the file must begin with. Every SQLite database
starts with `SQLite format 3`; a truncated or half-written upload almost never
does. It is worth setting for any format that has one, because it turns a
truncated upload into a failed deploy instead of an outage.

A file that fails its magic check is deleted rather than kept — it is the wrong
file, not a partial one, and leaving it would give the next transfer a bogus
delta basis. A file that is merely *short* is kept, so `--partial` can resume.

## Transfers are incremental

Measured on a 5.5 MB file with a small change:

| | On the wire |
|:--|--:|
| Whole file | 1.5 MB |
| rsync delta | **31 KB** |

That delta works because shunt passes `--fuzzy`: the file is staged under a name
that does not exist yet, so rsync has nothing to diff against unless told to
find the current copy next door.

A directory uses `--link-dest` against the live tree instead, because `--fuzzy`
looks for a basis inside the destination directory and a release-scoped staging
directory is empty. Measured on a 195 KB tree with one small file changed:
195 KB re-sent becomes 249 B.

A data-only change is a deploy in its own right. Regenerate the file, run
`shunt up`, and only the changed blocks move — no image is rebuilt.

## Getting data back

rsync does not care which end is the source, so the transport works in both
directions:

```sh
shunt fetch                        # list what can be fetched
shunt fetch index                  # pull it back over its local source
shunt fetch index -o /tmp/prod.db  # ...or somewhere else
```

Incrementally, like everything else: refreshing a stale local copy of a 500 MB
database moves only the blocks that changed. Useful for working against real
data, and for retrieving a capture a stage produced.
