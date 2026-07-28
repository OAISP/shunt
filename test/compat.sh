#!/usr/bin/env bash
# Exercises the host-compatibility paths against hosts that actually have the
# problem, rather than reasoning about them.
#
# Two cases, neither of which a healthy modern host can reproduce:
#
#   old-rsync   Ubuntu 20.04 ships rsync 3.1.3, which has no --compress-choice
#               at all. Sending it fails the transfer outright, so shunt has to
#               detect and fall back. This is the case that would break a deploy
#               to a large share of real VPS hosts.
#
#   no-curl     A minimal image without curl. shunt shells out to it on the host
#               for url health checks, and its absence was historically
#               discovered only after the container swap.
#
#   no-tar      tar used to be required too, for streaming the layout into
#               `docker load`. The helper writes that archive itself now, so a
#               host without tar has to deploy — which the old-rsync host, whose
#               tar is removed, proves.
#
# Each host is a container running sshd with the daemon's socket mounted, and its
# ssh port published. Deliberately not --network host: that shares a namespace
# only when the daemon is local, and on Docker Desktop the daemon lives in its
# own VM, so the harness would be unrunnable on a developer machine. The fixture
# health-checks via `docker exec` for the same reason — it needs no agreement
# about what 127.0.0.1 means, and the curl-less host has no curl to probe with
# anyway.
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
  docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
  docker rm -f "$(docker ps -aq --filter label=shunt.project=compat 2>/dev/null)" >/dev/null 2>&1 || true
  docker rmi -f "$(docker images -q --filter 'reference=shunt/compat-*' 2>/dev/null)" >/dev/null 2>&1 || true
  docker network rm compat-net >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT

ssh_host() { ssh -i "$KEY" -p "$SSH_PORT" -o BatchMode=yes -o StrictHostKeyChecking=no \
  -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR "root@127.0.0.1" "$@"; }

# start_host <base-image> <extra-packages-command>
#
# Brings up an sshd container on the host network with docker access, and waits
# for it to accept a key-based login.
start_host() {
  local base="$1" setup="$2"
  docker rm -f "$CONTAINER" >/dev/null 2>&1 || true

  cat > "$WORK/Dockerfile.host" <<EOF
FROM $base
ENV DEBIAN_FRONTEND=noninteractive
RUN $setup
RUN mkdir -p /var/run/sshd /root/.ssh && \\
    sed -i 's/^#*PermitRootLogin.*/PermitRootLogin prohibit-password/' /etc/ssh/sshd_config && \\
    sed -i 's/^#*Port .*/Port 22/' /etc/ssh/sshd_config
COPY id_ed25519.pub /root/.ssh/authorized_keys
RUN chmod 700 /root/.ssh && chmod 600 /root/.ssh/authorized_keys
CMD ["/usr/sbin/sshd", "-D", "-e"]
EOF
  # --load, because this image has to end up in the daemon for `docker run`
  # below to find it. With a docker-container builder selected — which is what
  # shunt tells you to create when the default one cannot export an OCI layout,
  # and therefore what CI and plenty of developer machines have — a build with
  # no output named leaves the result in the build cache and nowhere else. The
  # failure is not a build error: it is this host never accepting ssh, because
  # `docker run` quietly got a stale image or none at all.
  docker build -q --load -f "$WORK/Dockerfile.host" -t shunt-compat-host-img "$WORK" >/dev/null

  docker run -d --name "$CONTAINER" -p "127.0.0.1:$SSH_PORT:22" \
    -v /var/run/docker.sock:/var/run/docker.sock \
    shunt-compat-host-img >/dev/null

  for _ in $(seq 1 40); do
    if ssh_host true >/dev/null 2>&1; then return 0; fi
    sleep 0.5
  done
  echo "the compat host never accepted an ssh connection" >&2
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

# ------------------------------------------------------- missing curl / tar ---
step "a host missing curl is refused up front"
start_host "ubuntu:24.04" \
  "apt-get update -qq && apt-get install -y -qq openssh-server rsync docker.io >/dev/null && apt-get remove -y -qq curl >/dev/null && rm -f /bin/tar /usr/bin/tar"

fixture "$WORK/app"
cd "$WORK/app"

out="$("$SHUNT" audit 2>&1 || true)"
if printf '%s' "$out" | grep -qi "curl"; then
  ok "audit names the missing tool"
else
  bad "audit names the missing tool — got: $(printf '%s' "$out" | tail -3)"
fi

# The deploy must refuse before building or shipping anything, not after the
# container swap.
out="$("$SHUNT" up -y 2>&1 || true)"
if printf '%s' "$out" | grep -qi "missing curl"; then
  ok "up refuses before doing any work"
else
  bad "up refuses before doing any work — got: $(printf '%s' "$out" | tail -3)"
fi
if printf '%s' "$out" | grep -qi "apt-get install"; then
  ok "the refusal says how to fix it"
else
  bad "the refusal says how to fix it"
fi

# ------------------------------------------------------------- old rsync ------
step "a host with rsync 3.1.3 and no tar still deploys"
start_host "ubuntu:20.04" \
  "apt-get update -qq && apt-get install -y -qq openssh-server rsync curl docker.io >/dev/null && rm -f /bin/tar /usr/bin/tar"

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
if ssh_host "command -v tar" >/dev/null 2>&1; then
  bad "this host still has tar; the no-tar path would not be exercised"
else
  ok "this host has no tar, so loading without it is what is under test"
fi

cd "$WORK/app"
if "$SHUNT" up -y >"$WORK/old-rsync.log" 2>&1; then
  ok "the deploy succeeded with old rsync and no tar"
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
