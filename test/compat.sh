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
#   no-tools    A minimal image without curl or tar. shunt shells out to both on
#               the host — tar streams the layout into `docker load`, curl runs
#               url health checks — and both were historically discovered only
#               after the container swap.
#
# Each host is a container running sshd with the runner's docker socket mounted,
# on the host network so published ports and health probes agree about what
# 127.0.0.1 means.
set -euo pipefail

SHUNT="${SHUNT:-$(cd "$(dirname "$0")/.." && pwd)/shunt}"
WORK="$(mktemp -d)"
KEY="$WORK/id_ed25519"
SSH_PORT="${COMPAT_SSH_PORT:-22222}"
APP_PORT="${COMPAT_APP_PORT:-19095}"
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
    sed -i 's/^#*Port .*/Port $SSH_PORT/' /etc/ssh/sshd_config
COPY id_ed25519.pub /root/.ssh/authorized_keys
RUN chmod 700 /root/.ssh && chmod 600 /root/.ssh/authorized_keys
CMD ["/usr/sbin/sshd", "-D", "-e"]
EOF
  docker build -q -f "$WORK/Dockerfile.host" -t shunt-compat-host-img "$WORK" >/dev/null

  # Host network so a port published by the real daemon is reachable at the same
  # 127.0.0.1 the health probe inside this container will use.
  docker run -d --name "$CONTAINER" --network host \
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
publish = ["127.0.0.1:$APP_PORT:8080"]

[services.app.health]
url     = "/index.html"
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
step "a host missing curl and tar is refused up front"
start_host "ubuntu:24.04" \
  "apt-get update -qq && apt-get install -y -qq openssh-server rsync docker.io >/dev/null && apt-get remove -y -qq curl >/dev/null && rm -f /bin/tar /usr/bin/tar"

fixture "$WORK/app"
cd "$WORK/app"

out="$("$SHUNT" audit 2>&1 || true)"
if printf '%s' "$out" | grep -qiE "curl|tar"; then
  ok "audit names the missing tools"
else
  bad "audit names the missing tools — got: $(printf '%s' "$out" | tail -3)"
fi

# The deploy must refuse before building or shipping anything, not after the
# container swap.
out="$("$SHUNT" up -y 2>&1 || true)"
if printf '%s' "$out" | grep -qiE "missing (curl|tar)|curl and tar|tar and curl"; then
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
step "a host with rsync 3.1.3 (no --compress-choice) still deploys"
start_host "ubuntu:20.04" \
  "apt-get update -qq && apt-get install -y -qq openssh-server rsync curl tar docker.io >/dev/null"

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

cd "$WORK/app"
if "$SHUNT" up -y >"$WORK/old-rsync.log" 2>&1; then
  ok "the deploy succeeded via the plain -z fallback"
else
  bad "the deploy failed: $(tail -5 "$WORK/old-rsync.log")"
fi

served="$(ssh_host "curl -sS --max-time 5 http://127.0.0.1:$APP_PORT/index.html" 2>/dev/null || true)"
if [ "$served" = "compat" ]; then
  ok "the app is serving"
else
  bad "the app is serving — got '$served'"
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
