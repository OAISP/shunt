---
title: Artifacts
parent: Manifest reference
nav_order: 5
---

# Artifacts

An artifact is a large file or directory your app needs and the image does not
carry: a SQLite database, model weights, a prebuilt index. If your state lives
in a database server or object storage, you do not need one.

```toml
[[artifacts]]
name     = "index"
src      = "data/index.db"            # on your machine, relative to shunt.toml
dest     = "/opt/acme/data/index.db"  # on the server
magic    = "SQLite format 3"          # refuse a file that is not one
retain   = 1                          # superseded copies kept for recovery
required = true                       # fail the deploy if src is missing

[services.app]
volumes = ["/opt/acme/data:/data:ro"]
```

| Key | Default | Meaning |
|:--|:--|:--|
| `name` | — | Required, unique. |
| `src` | — | Required. A file or directory, relative to `shunt.toml`. |
| `dest` | — | Required. An absolute path on the server. |
| `magic` | none | A literal prefix the file must start with. Files only. |
| `retain` | `1` | Superseded copies kept beside `dest`. |
| `required` | `false` | Fail the deploy when `src` is missing, instead of skipping it. |

An artifact whose `src` is missing and not `required` is skipped, and the
server keeps the copy it has — the right default for a file an occasional job
regenerates.

A changed artifact is a deploy in its own right: regenerate the file, run
`shunt up`, and only the changed blocks travel. "Changed" means a different
size or modification time, the same test rsync uses.

## Directories

`src` may be a directory. The whole tree is mirrored and swapped in as one
unit, and files inside it that did not change are not sent again.

```toml
[[artifacts]]
name = "weights"
src  = "models/embed"
dest = "/opt/acme/models/embed"
```

## How the swap works

An artifact whose size and modification time already match the server's copy is
neither sent nor swapped: the file in place stays in place, along with the
containers reading it. One that differs replaces every service, since a
container keeps the file or directory it mounted rather than the one swapped
into its place.

The new copy is transferred next to the old one, as `<dest>.new.<release-id>`.
After the stages have run, every artifact in the release is checked — exact
size, non-empty, and `magic` if set — and only when all of them pass are they
swapped in. If one swap fails, the ones already done are put back.

- A **file** is swapped by renaming the new copy over the old one, after
  linking the old one aside, so `dest` always holds either the old contents or
  the new — never nothing.
- A **directory** is swapped with `RENAME_EXCHANGE`, which exchanges the two
  trees in one atomic step. On kernels older than 3.15, or filesystems without
  it, shunt moves the old tree aside first and says the swap is not atomic.

The superseded copy stays as `<dest>.prev.<release-id>`, and if the release then
fails its health check, the error includes the `mv` that puts it back.

{: .warning }
`shunt rollback` restores containers, not data. Rolling a database back as a
side effect of rolling code back is the kind of help that loses data, so the
`.prev` copy and the command to restore it are what you get instead.

## Checks on arrival

A file shorter than the release recorded is an interrupted transfer. It is kept,
so the next deploy resumes it rather than starting again. A file that fails its
`magic` check is the wrong file, not a partial one, so it is deleted.

`magic` is worth setting for any format that has one. Every SQLite database
starts with `SQLite format 3`; a truncated or mistaken upload almost never does,
and the check turns it into a failed deploy instead of an outage.

## Transfers are incremental

Measured on a 5.5 MB SQLite file with a small change:

| | On the wire |
|:--|--:|
| Whole file | 1.5 MB |
| rsync delta | **31 KB** |

A file uses rsync's `--fuzzy`, which finds the current copy next to the staged
name and diffs against it. A directory uses `--link-dest` against the live tree,
so unchanged files become hard links instead of transfers; on a 195 KB tree with
one small change, 249 bytes moved.

## Getting data back

The same transport works in the other direction:

```sh
shunt fetch                        # list artifacts and captures on the server
shunt fetch index                  # pull an artifact back over its src
shunt fetch index -o /tmp/prod.db  # or somewhere else
```

Refreshing a stale local copy of a large database moves only the changed blocks.
