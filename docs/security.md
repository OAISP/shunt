---
title: Security
nav_order: 6
---

# Security

## The trust boundary

**ssh access to a user in the `docker` group is root on that server.** That is
true of any tool that deploys containers this way; shunt does not add to it or
take it away. Prefer a dedicated deploy user, or rootless Docker.

Anyone who can use the server's Docker socket can exec into a container, read a
`0600` file, or read a process's memory. Treat "can talk to the Docker socket"
as "can read every secret of every project on that server", and give the
deploy user — and any CI key for it — accordingly little else.

What secret delivery *does* change is the passive copy. By default the values
appear in `docker inspect`, and so in anything that captures it;
[`mode = "file"`]({% link manifest/secrets.md %}#delivery-mode) removes that.

## What shunt does not do

- **Store credentials.** shunt runs the system `ssh`, so your `~/.ssh/config`,
  agent, jump hosts, hardware keys and `known_hosts` apply as they are. It keeps
  no keys of its own.
- **Skip host key checking.** There is no option for it.
- **Talk to Docker over TCP.** There is no `DOCKER_HOST=tcp://` mode; that is an
  unauthenticated root socket.
- **Put secrets in argv.** Values travel inside the release description on the
  ssh session's stdin, so they never appear in `ps` on either machine. (Values
  in a service's `env` are not secrets and do; see
  [Secrets]({% link manifest/secrets.md %}#secrets-and-env).)
- **Put secrets in images.** Nothing is baked into a layer.
- **Put secrets in the release history.** The history stores a salted hash of
  each value — enough for `shunt plan` to say which ones changed.

## Secrets stay on the server

Each retained release keeps its resolved values on the server in `0600` files
under `~/.shunt/<project>/`, because a rollback has to restart that release with
the environment it was deployed with, and the history holds only hashes. They
expire with the release when it falls out of `retain`, but the serving
release's never do — so a project you stop deploying keeps live credentials on
that server until you remove them:

```sh
shunt down --purge     # containers, network, images, history and secrets; never volumes
```

Worth running when you decommission a server or move a project.

## Integrity

Every blob that arrives on the server is rehashed and checked against its own
filename before Docker loads it, so a corrupted transfer fails the deploy
instead of deploying silently. Because blob filenames are content hashes, that
check is complete.

A blob already checked is not rehashed again while its size and modification
time are unchanged. The trade is stated plainly: a blob rewritten with the same
size *and* mtime would be trusted. rsync never produces that, so what it gives
up is detecting silent on-disk corruption between deploys.
`SHUNT_NO_VERIFY_CACHE=1` on the server restores the full rehash;
`SHUNT_NO_VERIFY=1` turns verification off.

## Concurrency

Deploys of a project are serialised. The lock is held by the helper process on
the server for the whole deploy — transfer included — and released by the
kernel when that process exits, however it exits. A deploy whose session drops
after the release reached the server is finished there rather than abandoned
halfway, and one whose plan went stale while it waited is refused rather than
allowed to undo another. See
[One deploy at a time]({% link deploying/index.md %}#one-deploy-at-a-time).

## Reporting a vulnerability

Open an issue at [github.com/OAISP/shunt](https://github.com/OAISP/shunt/issues)
if it is not sensitive, or contact the maintainer directly if it is.
