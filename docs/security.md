---
title: Security
nav_order: 6
---

# Security

## The trust boundary

**ssh access to a host in the `docker` group is root on that host.** That is
inherent to deploying containers this way, not something shunt adds. Prefer
rootless Docker or a dedicated deploy user.

Anyone who can reach the Docker API on that host can exec into a container, read
a `0600` file directly, or read a process's memory. Treat "can talk to the
Docker socket" as "can read every secret in this project", and scope your deploy
user accordingly. No secret delivery mechanism changes that.

What delivery *does* change is the passive copy — see
[secrets]({% link manifest/secrets.md %}#delivery-mode). By default the values
appear in `docker inspect` and therefore in anything that captures it;
`mode = "file"` removes that.

## What shunt does not do

- **No credential storage.** shunt shells out to the system `ssh`, so your
  `~/.ssh/config`, agent, jump hosts, hardware keys and `known_hosts` all apply
  unchanged. It stores nothing.
- **No way to skip host key checking.** There is deliberately no flag for it.
- **No `DOCKER_HOST=tcp://` mode.** That is an unauthenticated root socket.
- **No secrets in argv.** Values are streamed to the host inside the release
  spec over the ssh channel, never passed as arguments, so they cannot appear in
  `ps` on either machine.
- **No secrets in images.** Nothing is baked into a layer.
- **No secrets in the ledger.** The on-host release history stores a salted hash
  of each value, which is enough for `shunt plan` to report which secrets
  changed without plaintext crossing back.

## Integrity

Every blob is rehashed on the host and checked against its own filename before
loading, so a corrupted transfer fails loudly rather than deploying silently.
Blob filenames are content hashes, which is what makes that check total.

A blob that has already been verified is not rehashed while its size and mtime
are unchanged. The trade is stated plainly: a blob rewritten with the same size
*and* mtime would be trusted. Nothing rsync does produces that, so what this
gives up is detection of silent on-disk corruption between deploys.
`SHUNT_NO_VERIFY_CACHE=1` on the host restores the full rehash, and
`SHUNT_NO_VERIFY=1` skips verification altogether.

## Concurrency

Deploys are serialised per project. The lock is held for the whole deploy —
including the transfer, which is the part that would otherwise let two deploys
write into the same store — and released by the kernel when the ssh session
ends, however it ends.

A plan whose premise has expired is refused rather than applied, so a deploy
that waited on the lock cannot silently revert the one it waited for.

## Reporting a vulnerability

Open an issue at [github.com/OAISP/shunt](https://github.com/OAISP/shunt/issues)
if it is not sensitive, or contact the maintainer directly if it is.
