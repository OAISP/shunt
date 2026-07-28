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

check() { # check <description> <condition-command...>
  local desc="$1"; shift
  if "$@" >/dev/null 2>&1; then ok "$desc"; else bad "$desc"; fi
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
check "validate accepts a good manifest" "$SHUNT" validate
check "audit reports the host is deployable" "$SHUNT" audit

# ------------------------------------------------------------- first deploy --
step "first deploy"
"$SHUNT" up -y </dev/null
[ "$(served)" = "v1" ] && ok "the app serves v1" || bad "the app serves v1"
R1="$(current)"
[ -n "$R1" ] && ok "a release was recorded" || bad "a release was recorded"

# ------------------------------------------------------------------- no-op ---
step "no-op"
# plan exits 2 when there are changes, 0 when there are none.
set +e
"$SHUNT" plan </dev/null >/dev/null 2>&1; code=$?
set -e
[ "$code" -eq 0 ] && ok "plan exits 0 when the host already matches" \
                  || bad "plan exits 0 when the host already matches (got $code)"

# --json must not redeploy when nothing changed.
"$SHUNT" up -y --json </dev/null >/dev/null 2>&1
[ "$(current)" = "$R1" ] && ok "up --json does not redeploy a no-op" \
                         || bad "up --json does not redeploy a no-op"

# ------------------------------------------------------------ code change ----
step "code change"
echo "v2" > index.html
"$SHUNT" up -y </dev/null >/dev/null
[ "$(served)" = "v2" ] && ok "the app serves v2" || bad "the app serves v2"
R2="$(current)"
[ "$R2" != "$R1" ] && ok "a new release was recorded" || bad "a new release was recorded"

set +e
"$SHUNT" plan </dev/null >/dev/null 2>&1; code=$?
set -e
[ "$code" -eq 0 ] && ok "plan is clean again after deploying" \
                  || bad "plan is clean again after deploying (got $code)"

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

set +e
"$SHUNT" up -y </dev/null >/dev/null 2>&1; code=$?
set -e
[ "$code" -ne 0 ] && ok "the deploy failed" || bad "the deploy failed"
[ "$(served)" = "v2" ] && ok "the old release is still serving" || bad "the old release is still serving"
[ "$(current)" = "$R2" ] && ok "the serving release did not move" || bad "the serving release did not move"

"$SHUNT" status </dev/null 2>&1 | grep -q "last attempt" \
  && ok "status reports the failed attempt separately" \
  || bad "status reports the failed attempt separately"

mv shunt.toml.bak shunt.toml
echo "v3" > index.html
"$SHUNT" up -y </dev/null >/dev/null
R3="$(current)"

# --------------------------------------------------------------- rollback ----
step "rollback"
"$SHUNT" rollback "$R2" -y </dev/null >/dev/null
[ "$(served)" = "v2" ] && ok "rollback restored v2" || bad "rollback restored v2"
[ "$(current)" = "$R2" ] && ok "the ledger points at the restored release" \
                         || bad "the ledger points at the restored release"

# ------------------------------------------------------------------- drift ---
# A plan built from the ledger alone would call this "unchanged".
step "drift detection"
ssh -o BatchMode=yes "$HOST" "docker rm -f $PROJECT-worker" >/dev/null 2>&1
set +e
"$SHUNT" plan </dev/null >/dev/null 2>&1; code=$?
set -e
[ "$code" -eq 2 ] && ok "a hand-deleted container is reported as work" \
                  || bad "a hand-deleted container is reported as work (got $code)"
"$SHUNT" up -y </dev/null >/dev/null
ssh -o BatchMode=yes "$HOST" "docker ps --filter name=$PROJECT-worker --format '{{.Names}}'" | grep -q worker \
  && ok "the missing container was restored" || bad "the missing container was restored"

# ------------------------------------------------------------------ exec -----
step "exec and logs"
"$SHUNT" exec app -- cat /srv/index.html </dev/null 2>/dev/null | grep -q . \
  && ok "exec runs in the serving container" || bad "exec runs in the serving container"
"$SHUNT" logs --tail 5 </dev/null 2>/dev/null | grep -q "worker" \
  && ok "logs cover every service, prefixed" || bad "logs cover every service, prefixed"

# ----------------------------------------------------------------- retire ----
step "retire"
python3 - <<'PY'
import pathlib
p = pathlib.Path("shunt.toml")
s = p.read_text()
s = s.split("[services.worker]")[0]
p.write_text(s)
PY
set +e
"$SHUNT" plan </dev/null 2>&1 | grep -q "orphaned"; orph=$?
set -e
[ "$orph" -eq 0 ] && ok "a dropped service is reported as orphaned" \
                  || bad "a dropped service is reported as orphaned"
"$SHUNT" retire worker -y </dev/null >/dev/null
ssh -o BatchMode=yes "$HOST" "docker ps -aq --filter name=$PROJECT-worker" | grep -q . \
  && bad "retire removed the orphan" || ok "retire removed the orphan"

# ----------------------------------------------------------------- summary ---
printf '\n\033[1m%d passed, %d failed\033[0m\n\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
