#!/usr/bin/env bash
# End-to-end test against a real host over real ssh with a real Docker daemon.
#
# Everything here is a path unit tests cannot reach: containers actually swap,
# health checks actually probe, a failed stage actually has to leave production
# alone. These are the failures that cost someone their site, and until now the
# only thing standing behind them was the honour system in CONTRIBUTING.
#
# Usage:  test/e2e.sh <ssh-host>     (defaults to localhost)
#
# The host needs docker, rsync, tar and curl, and an ssh account that can reach
# the docker socket. CI points it at the runner itself.
set -euo pipefail

HOST="${1:-localhost}"
SHUNT="${SHUNT:-$(cd "$(dirname "$0")/.." && pwd)/shunt}"
PROJECT="shunte2e$$"
PORT="${E2E_PORT:-19090}"
WORK="$(mktemp -d)"

pass=0
fail=0

cleanup() {
  ssh -o BatchMode=yes "$HOST" "
    docker rm -f \$(docker ps -aq --filter label=shunt.project=$PROJECT) 2>/dev/null
    docker rmi -f \$(docker images -q --filter reference='shunt/$PROJECT-*') 2>/dev/null
    docker network rm $PROJECT-net 2>/dev/null
    rm -rf ~/.shunt/$PROJECT
  " >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT

ok()   { printf '  \033[32m✓\033[0m %s\n' "$1"; pass=$((pass + 1)); }
bad()  { printf '  \033[31m✗\033[0m %s\n' "$1"; fail=$((fail + 1)); }
step() { printf '\n\033[1m%s\033[0m\n' "$1"; }

# Assertions are written as if/else rather than `A && B || C`: that idiom runs
# C when B fails, not only when A does, which in a test harness means a silent
# double-report rather than the result you asked for.
succeeds() { # succeeds <description> <command...>
  local desc="$1"; shift
  if "$@" >/dev/null 2>&1; then ok "$desc"; else bad "$desc"; fi
}

fails() { # fails <description> <command...>
  local desc="$1"; shift
  if "$@" >/dev/null 2>&1; then bad "$desc"; else ok "$desc"; fi
}

eq() { # eq <description> <got> <want>
  if [ "$2" = "$3" ]; then ok "$1"; else bad "$1 — got '$2', want '$3'"; fi
}

neq() { # neq <description> <got> <unwanted>
  if [ "$2" != "$3" ]; then ok "$1"; else bad "$1 — got '$2', wanted anything else"; fi
}

# says <description> <pattern> <command...>
#
# Asserts on a command's stdout, and on nothing else. Deliberately not
# `cmd | grep -q`: this script runs under `set -o pipefail`, which gives the
# pipeline the command's exit status, and `shunt plan` exits 2 by design when it
# finds changes. Piping therefore turned "the output says this" into "...and the
# host had nothing else to apply" — a second, unrelated claim that silently
# breaks as soon as an earlier test leaves the host and the manifest legitimately
# out of step.
says() {
  local desc="$1" pattern="$2"; shift 2
  local out
  out="$("$@" </dev/null 2>/dev/null || true)"
  case "$out" in
    *"$pattern"*) ok "$desc" ;;
    *) bad "$desc — nothing in the output mentioned '$pattern'" ;;
  esac
}

# exits <description> <expected-code> <command...>
exits() {
  local desc="$1" want="$2"; shift 2
  local got=0
  "$@" >/dev/null 2>&1 || got=$?
  eq "$desc" "$got" "$want"
}

# ---------------------------------------------------------------- fixture ----
cd "$WORK"
cat > Dockerfile <<'EOF'
FROM alpine:3.20
RUN apk add --no-cache busybox-extras curl
COPY index.html /srv/index.html
EXPOSE 8080
CMD ["httpd", "-f", "-p", "8080", "-h", "/srv"]
EOF
echo "v1" > index.html

cat > shunt.toml <<EOF
project = "$PROJECT"
host    = "$HOST"

[images.app]
context = "."

[services.app]
image   = "app"
publish = ["127.0.0.1:$PORT:8080"]

[services.app.health]
url     = "/index.html"
retries = 20

[services.worker]
image   = "app"
command = ["sh", "-c", "while true; do echo tick; sleep 5; done"]
EOF

served() { ssh -o BatchMode=yes "$HOST" "curl -sS --max-time 5 http://127.0.0.1:$PORT/index.html"; }
current() { "$SHUNT" status --json </dev/null 2>/dev/null | grep -o '"current":"[^"]*"' | head -1 | cut -d'"' -f4; }

# ------------------------------------------------------------- offline ----
step "offline checks"
succeeds "validate accepts a good manifest" "$SHUNT" validate
succeeds "audit reports the host is deployable" "$SHUNT" audit

# ------------------------------------------------------------- first deploy --
step "first deploy"
"$SHUNT" up -y </dev/null
eq "the app serves v1" "$(served)" "v1"
R1="$(current)"
neq "a release was recorded" "$R1" ""

# ------------------------------------------------------------------- no-op ---
step "no-op"
# plan exits 2 when there are changes, 0 when there are none.
exits "plan exits 0 when the host already matches" 0 "$SHUNT" plan

# --json must not redeploy when nothing changed.
"$SHUNT" up -y --json </dev/null >/dev/null 2>&1 || true
eq "up --json does not redeploy a no-op" "$(current)" "$R1"

# ------------------------------------------------------------ code change ----
step "code change"
echo "v2" > index.html
"$SHUNT" up -y </dev/null >/dev/null
eq "the app serves v2" "$(served)" "v2"
R2="$(current)"
neq "a new release was recorded" "$R2" "$R1"

exits "plan is clean again after deploying" 0 "$SHUNT" plan

# ------------------------------------------------------------ failed stage ---
# The safety invariant of the whole tool: a stage that fails must leave the
# running release exactly as it was.
step "a failing stage leaves production untouched"
cp shunt.toml shunt.toml.bak
cat >> shunt.toml <<'EOF'

[[stages]]
name    = "doomed"
image   = "app"
command = ["sh", "-c", "exit 1"]
EOF
echo "v3-must-not-be-served" > index.html

fails "the deploy failed" "$SHUNT" up -y
eq "the old release is still serving" "$(served)" "v2"
eq "the serving release did not move" "$(current)" "$R2"

if "$SHUNT" status </dev/null 2>&1 | grep -q "last attempt"; then
  ok "status reports the failed attempt separately"
else
  bad "status reports the failed attempt separately"
fi

mv shunt.toml.bak shunt.toml
echo "v3" > index.html
"$SHUNT" up -y </dev/null >/dev/null
R3="$(current)"
neq "the host recovers once the stage is fixed" "$R3" "$R2"
eq "the recovered release serves v3" "$(served)" "v3"

# --------------------------------------------------------------- rollback ----
step "rollback"
"$SHUNT" rollback "$R2" -y </dev/null >/dev/null
eq "rollback restored v2" "$(served)" "v2"
eq "the ledger points at the restored release" "$(current)" "$R2"

# ------------------------------------------------------------------- drift ---
# A plan built from the ledger alone would call this "unchanged".
step "drift detection"
ssh -o BatchMode=yes "$HOST" "docker rm -f $PROJECT-worker" >/dev/null 2>&1 || true
exits "a hand-deleted container is reported as work" 2 "$SHUNT" plan
"$SHUNT" up -y </dev/null >/dev/null
if ssh -o BatchMode=yes "$HOST" "docker ps --filter name=$PROJECT-worker --format '{{.Names}}'" | grep -q worker; then
  ok "the missing container was restored"
else
  bad "the missing container was restored"
fi

# ------------------------------------------------------------------ exec -----
step "exec and logs"
if "$SHUNT" exec app -- cat /srv/index.html </dev/null 2>/dev/null | grep -q .; then
  ok "exec runs in the serving container"
else
  bad "exec runs in the serving container"
fi
if "$SHUNT" logs --tail 5 </dev/null 2>/dev/null | grep -q "worker"; then
  ok "logs cover every service, prefixed"
else
  bad "logs cover every service, prefixed"
fi

# ------------------------------------------------------- rollback on failure --
# A deploy that fails *after* replacing a container leaves the host running a
# mix. Opt-in recovery has to actually put the previous release back.
step "up --rollback-on-failure restores the previous release"
BEFORE="$(current)"
cp shunt.toml shunt.toml.bak
# Deployable but unhealthy: it stays up, so this is not an "exited during
# startup" shortcut, and nothing listens on the exposed port, so the health
# probe genuinely fails — after `app` has already been swapped, which is what
# makes the host degraded and gives the rollback something to undo.
cat >> shunt.toml <<'TOML'

[services.broken]
image   = "app"
command = ["sh", "-c", "while true; do sleep 5; done"]
expose  = 9999

[services.broken.health]
url      = "/"
retries  = 2
interval = "200ms"
TOML
echo "v4-should-be-rolled-back" > index.html

fails "the deploy failed" "$SHUNT" up -y --rollback-on-failure
eq "the previous release is serving again" "$(current)" "$BEFORE"
eq "it serves the previous content" "$(served)" "v3"

mv shunt.toml.bak shunt.toml
echo "v3" > index.html
"$SHUNT" up -y </dev/null >/dev/null 2>&1 || true

# ----------------------------------------------------------------- bundle ----
# A bundle has to apply from a directory with no manifest, no Dockerfile and no
# source — that is the entire point of it.
step "bundle and apply"
BUNDLE_DIR="$WORK/elsewhere"
mkdir -p "$BUNDLE_DIR"
echo "v5" > index.html
succeeds "bundle builds a release into a file" "$SHUNT" bundle -o "$BUNDLE_DIR/rel.shuntpkg"

succeeds "inspect reads a bundle without applying it" \
  "$SHUNT" bundle inspect "$BUNDLE_DIR/rel.shuntpkg"
succeeds "verify rehashes its blobs" \
  "$SHUNT" bundle verify "$BUNDLE_DIR/rel.shuntpkg"

# --plan must answer without touching anything.
BEFORE_PLAN="$(current)"
exits "apply --plan reports changes with exit 2" 2 \
  "$SHUNT" apply --plan "$BUNDLE_DIR/rel.shuntpkg"
eq "apply --plan changed nothing" "$(current)" "$BEFORE_PLAN"

( cd "$BUNDLE_DIR" && "$SHUNT" apply rel.shuntpkg -y ) >/dev/null 2>&1 || true
eq "the bundle deployed" "$(served)" "v5"

# Re-planning an applied bundle must be a clean no-op, which only holds if
# mtimes survived the archive.
exits "apply --plan is clean once applied" 0 "$SHUNT" apply --plan "$BUNDLE_DIR/rel.shuntpkg"

# Immutable release ids mean the host has already recorded this one.
if ( cd "$BUNDLE_DIR" && "$SHUNT" apply rel.shuntpkg -y ) >/dev/null 2>&1; then
  bad "applying the same bundle twice is refused"
else
  ok "applying the same bundle twice is refused"
fi

# ---------------------------------------------------------------- secrets ----
# Rollback replays the ledger's stored spec, whose secret values are hashes —
# the ledger never holds plaintext. Every path that recreates a container
# rewrites the secrets from the spec it is handed, so unless the real values are
# read back off the host first, a rollback starts containers holding "h:3f2a…"
# where the password should be, and in file mode overwrites the one good copy
# with them on the way past. Nothing but a real deploy can catch that.
step "secrets survive a rollback"
TOKEN='s3cr3t=with=equals'
printf 'APP_TOKEN=%s\n' "$TOKEN" > secrets.env
chmod 600 secrets.env
cp shunt.toml shunt.toml.bak
cat >> shunt.toml <<'TOML'

[secrets]
provider = "file"
path     = "secrets.env"
mode     = "file"
TOML
# Both services narrow their scope, so the host writes only scoped directories
# and no unscoped one — the shape a rollback used to report as pruned.
python3 - <<'PY'
import pathlib
p = pathlib.Path("shunt.toml")
s = p.read_text()
s = s.replace("[services.app]\n", '[services.app]\nsecrets = ["APP_TOKEN"]\n')
s = s.replace("[services.worker]\n", '[services.worker]\nsecrets = ["APP_TOKEN"]\n')
p.write_text(s)
PY

secret_seen() { "$SHUNT" exec app -- cat /run/secrets/APP_TOKEN </dev/null 2>/dev/null; }

echo "v6" > index.html
"$SHUNT" up -y </dev/null >/dev/null
R_SEC="$(current)"
eq "the container reads its secret as a file" "$(secret_seen)" "$TOKEN"

echo "v7" > index.html
"$SHUNT" up -y </dev/null >/dev/null

succeeds "rollback accepts a release whose services all scoped their secrets" \
  "$SHUNT" rollback "$R_SEC" -y
eq "the restored release serves its own content" "$(served)" "v6"
eq "the restored container still holds the real secret" "$(secret_seen)" "$TOKEN"

mv shunt.toml.bak shunt.toml
rm -f secrets.env

# ----------------------------------------------------------------- retire ----
step "retire"
python3 - <<'PY'
import pathlib
p = pathlib.Path("shunt.toml")
s = p.read_text()
s = s.split("[services.worker]")[0]
p.write_text(s)
PY
says "a dropped service is reported as orphaned" "orphaned" "$SHUNT" plan
"$SHUNT" retire worker -y </dev/null >/dev/null
if ssh -o BatchMode=yes "$HOST" "docker ps -aq --filter name=$PROJECT-worker" | grep -q .; then
  bad "retire removed the orphan"
else
  ok "retire removed the orphan"
fi

# ------------------------------------------------------------------- down ----
# Last, because it takes the project away. The trap-based cleanup still runs
# afterwards on purpose: this section asserts that `down` works, and the trap
# guarantees the host is clean even when it does not.
step "down"
running() { ssh -o BatchMode=yes "$HOST" "docker ps -aq --filter label=shunt.project=$PROJECT" | tr -d '[:space:]'; }
onhost()  { ssh -o BatchMode=yes "$HOST" "$@" >/dev/null 2>&1; }

"$SHUNT" down -y </dev/null >/dev/null
eq "down removed every container" "$(running)" ""

# Reversible: the release history and the secrets are what make that true, so
# they have to have survived.
succeeds "down kept the release history" onhost "test -f ~/.shunt/$PROJECT/releases.json"
succeeds "down kept the deploy network" onhost "docker network inspect $PROJECT-net"

"$SHUNT" up -y </dev/null >/dev/null
# Against the working tree rather than a literal: earlier sections move the
# content on, and the claim being made is "up rebuilds what is here now".
eq "up brings the project back after down" "$(served)" "$(cat index.html)"

# --purge is the decommission button: nothing shunt put on this host survives it.
"$SHUNT" down --purge -y </dev/null >/dev/null
eq "purge removed every container" "$(running)" ""
fails "purge removed the deploy network" onhost "docker network inspect $PROJECT-net"
fails "purge removed the release history and secrets" onhost "test -d ~/.shunt/$PROJECT"
fails "purge removed the release-tagged images" \
  ssh -o BatchMode=yes "$HOST" "docker images -q --filter reference='shunt/$PROJECT-*' | grep -q ."

# ----------------------------------------------------------------- summary ---
printf '\n\033[1m%d passed, %d failed\033[0m\n\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
