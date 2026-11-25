#!/usr/bin/env bash
# Exercises the host-compatibility paths against hosts that actually have the
# problem, rather than reasoning about them.
#
#   test/compat.sh                     the minimal host, below
#   test/compat.sh docker <version>    the whole e2e suite against that Docker
#
# The minimal host is two cases, neither of which a healthy modern host can
# reproduce, exercised together:
#
#   old-rsync   Ubuntu 20.04 ships rsync 3.1.3, which has no --compress-choice
#               at all. Sending it fails the transfer outright, so shunt has to
#               detect and fall back. This is the case that would break a deploy
#               to a large share of real VPS hosts.
#
#   minimal     No curl and no tar. Both used to be required on the host — curl
#               for url health checks, tar for streaming the layout into `docker
#               load` — and the helper now does both itself.
#
# A Docker version is its own host because servers run old ones for years, and
# the daemon a CI runner ships says nothing about them: `docker stop --timeout`
# reached production unnoticed because only Docker 28 accepts it.
#
# Each host is a container running sshd, with its ssh port published. The
# minimal one mounts this machine's docker socket; a versioned one runs its own
# daemon. Deliberately not --network host: that shares a namespace only when
# the daemon is local, and on Docker Desktop the daemon lives in its own VM, so
# the harness would be unrunnable on a developer machine. The minimal fixture
# health-checks via `docker exec` for the same reason — it needs no agreement
# about what 127.0.0.1 means.
set -euo pipefail

SHUNT="${SHUNT:-$(cd "$(dirname "$0")/.." && pwd)/shunt}"
WORK="$(mktemp -d)"
KEY="$WORK/id_ed25519"
SSH_PORT="${COMPAT_SSH_PORT:-22222}"
CONTAINER="shunt-compat-host"

pass=0
fail=0
ok()   { printf '  \033[32m✓\033[0m %s\n' "$1"; pass=$((pass + 1)); }
bad()  { printf '  \033[31m✗\033[0m %s\n' "$1"; fail=$((fail + 1)); }
step() { printf '\n\033[1m%s\033[0m\n' "$1"; }

cleanup() {
  # -v: a versioned host keeps its whole daemon in an anonymous volume.
  docker rm -fv "$CONTAINER" >/dev/null 2>&1 || true
  # Through xargs: more than one id in a single quoted argument removes nothing.
  docker ps -aq --filter label=shunt.project=compat 2>/dev/null | xargs -r docker rm -f >/dev/null 2>&1 || true
  docker images -q --filter 'reference=shunt/compat-*' 2>/dev/null | xargs -r docker rmi -f >/dev/null 2>&1 || true
  docker network rm compat-net >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT

ssh_host() { ssh -i "$KEY" -p "$SSH_PORT" -o BatchMode=yes -o StrictHostKeyChecking=no \
  -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR "root@127.0.0.1" "$@"; }

# start_host <base-image> <extra-packages-command> <command> <docker-run-flags...>
#
# Builds and starts a container that can run sshd and reach a docker daemon.
start_host() {
  local base="$1" setup="$2" cmd="$3"
  shift 3
  docker rm -fv "$CONTAINER" >/dev/null 2>&1 || true

  cat > "$WORK/Dockerfile.host" <<EOF
FROM $base
ENV DEBIAN_FRONTEND=noninteractive
RUN $setup
RUN mkdir -p /var/run/sshd /root/.ssh && \\
    sed -i 's/^#*PermitRootLogin.*/PermitRootLogin prohibit-password/' /etc/ssh/sshd_config && \\
    sed -i 's/^#*Port .*/Port 22/' /etc/ssh/sshd_config
COPY id_ed25519.pub /root/.ssh/authorized_keys
RUN chmod 700 /root/.ssh && chmod 600 /root/.ssh/authorized_keys
CMD $cmd
EOF
  # --load, because this image has to end up in the daemon for `docker run`
  # below to find it. With a docker-container builder selected — which is what
  # shunt tells you to create when the default one cannot export an OCI layout,
  # and therefore what CI and plenty of developer machines have — a build with
  # no output named leaves the result in the build cache and nowhere else. The
  # failure is not a build error: it is this host never accepting ssh, because
  # `docker run` quietly got a stale image or none at all.
  docker build -q --load -f "$WORK/Dockerfile.host" -t shunt-compat-host-img "$WORK" >/dev/null

  docker run -d --name "$CONTAINER" -p "127.0.0.1:$SSH_PORT:22" "$@" shunt-compat-host-img >/dev/null
}

# await <description> <command...>: polls until the command succeeds.
await() {
  local what="$1"; shift
  for _ in $(seq 1 120); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 0.5
  done
  echo "the compat host never $what" >&2
  docker logs "$CONTAINER" >&2 || true
  return 1
}

fixture() { # fixture <dir>
  mkdir -p "$1"
  cat > "$1/Dockerfile" <<'EOF'
FROM alpine:3.20
RUN apk add --no-cache busybox-extras curl
COPY index.html /srv/index.html
EXPOSE 8080
CMD ["httpd", "-f", "-p", "8080", "-h", "/srv"]
EOF
  echo "compat" > "$1/index.html"
  cat > "$1/shunt.toml" <<EOF
project = "compat"
host    = "compat"

[images.app]
context = "."

[services.app]
image   = "app"

[services.app.health]
command = ["cat", "/srv/index.html"]
retries = 20
EOF
}

# The public key lands next to the private one, which is also the docker
# build context, so the Dockerfile can COPY it directly.
ssh-keygen -t ed25519 -N "" -f "$KEY" -q

# An ssh alias keeps the port and key out of the manifest, the way a real user's
# ~/.ssh/config would.
cat > "$WORK/ssh_config" <<EOF
Host compat
  HostName 127.0.0.1
  Port $SSH_PORT
  User root
  IdentityFile $KEY
  StrictHostKeyChecking no
  UserKnownHostsFile /dev/null
  LogLevel ERROR
EOF
chmod 600 "$WORK/ssh_config"

# ssh resolves ~/.ssh/config from getpwuid, not from $HOME, so overriding HOME
# does not redirect it — the alias has to reach ssh another way. shunt invokes
# `ssh` by name and hands rsync the same command, so a shim earlier on PATH
# reaches both without shunt needing to know this test exists.
REAL_SSH="$(command -v ssh)"
mkdir -p "$WORK/bin"
cat > "$WORK/bin/ssh" <<EOF
#!/bin/sh
exec "$REAL_SSH" -F "$WORK/ssh_config" "\$@"
EOF
chmod +x "$WORK/bin/ssh"
export PATH="$WORK/bin:$PATH"

# -------------------------------------------------------- a docker version ----
if [ "${1:-}" = "docker" ]; then
  version="${2:?usage: test/compat.sh docker <version>}"
  step "the e2e suite against Docker $version"
  # --upgrade: the image pins an older openssh client that the server package
  # will not install beside.
  start_host "docker:$version-dind" \
    "apk add --no-cache --upgrade openssh rsync curl >/dev/null && ssh-keygen -A >/dev/null" \
    '[]' --privileged
  # Nothing may run in the container until the daemon is up, sshd included: the
  # image's entrypoint rearranges cgroups first, and any process started beside
  # it in that moment makes it give up. So readiness is read off the log. Not
  # grep -q, which would close the pipe on `docker logs` under pipefail.
  for _ in $(seq 1 120); do
    docker logs "$CONTAINER" 2>&1 | grep "API listen on" >/dev/null && break
    sleep 0.5
  done
  docker exec -d "$CONTAINER" /usr/sbin/sshd -e
  await "accepted an ssh connection" ssh_host docker version
  echo "  host runs Docker $(ssh_host docker version --format '{{.Server.Version}}')"
  SHUNT="$SHUNT" "$(dirname "$0")/e2e.sh" compat
  exit
fi

# ------------------------------------------------------------- old rsync ------
step "a host with rsync 3.1.3, no curl and no tar still deploys"
start_host "ubuntu:20.04" \
  "apt-get update -qq && apt-get install -y -qq openssh-server rsync docker.io >/dev/null && (apt-get remove -y -qq curl >/dev/null 2>&1 || true) && rm -f /bin/tar /usr/bin/tar" \
  '["/usr/sbin/sshd", "-D", "-e"]' \
  -v /var/run/docker.sock:/var/run/docker.sock
await "accepted an ssh connection" ssh_host docker version

fixture "$WORK/app"

ver="$(ssh_host rsync --version | head -1 | awk '{print $3}')"
case "$ver" in
  3.1.*) ok "the host really is on rsync $ver" ;;
  *)     bad "expected rsync 3.1.x on ubuntu:20.04, got $ver" ;;
esac
if ssh_host rsync --version | grep -qi zstd; then
  bad "this rsync unexpectedly has zstd; the fallback would not be exercised"
else
  ok "this rsync has no zstd, so the fallback is what is under test"
fi
if ssh_host "command -v tar || command -v curl" >/dev/null 2>&1; then
  bad "this host still has tar or curl; the minimal-host path would not be exercised"
else
  ok "this host has neither tar nor curl"
fi

cd "$WORK/app"
if "$SHUNT" up -y >"$WORK/old-rsync.log" 2>&1; then
  ok "the deploy succeeded on a host with old rsync, no curl and no tar"
else
  bad "the deploy failed: $(tail -5 "$WORK/old-rsync.log")"
fi

# Read the file out of the running container rather than over the network, so
# the assertion holds wherever the daemon happens to live.
served="$(ssh_host "docker exec compat-app cat /srv/index.html" 2>/dev/null | tr -d '\r\n' || true)"
if [ "$served" = "compat" ]; then
  ok "the app is running and serving the deployed content"
else
  bad "the app is running — got '$served'"
fi

# A second deploy must still dedup: the fallback changes compression, not the
# content-addressed transfer the whole design rests on.
echo "compat-v2" > index.html
if "$SHUNT" up -y >"$WORK/old-rsync-2.log" 2>&1; then
  wire="$(grep -o '[0-9.]* [KMG]*B on the wire' "$WORK/old-rsync-2.log" | head -1)"
  ok "a redeploy still ships only the delta ($wire)"
else
  bad "the redeploy failed: $(tail -5 "$WORK/old-rsync-2.log")"
fi

printf '\n\033[1m%d passed, %d failed\033[0m\n\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
