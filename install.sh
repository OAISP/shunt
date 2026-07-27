#!/bin/sh
# Install shunt — registry-free Docker deploys over ssh.
#
#   curl -sSL https://raw.githubusercontent.com/OAISP/shunt/main/install.sh | sh
#
# Environment:
#   SHUNT_VERSION   pin a release, e.g. v0.1.0 (default: latest)
#   PREFIX          install directory (default: /usr/local/bin, else ~/.local/bin)
#
# This script never invokes sudo. If it cannot write to /usr/local/bin it
# installs to ~/.local/bin instead and says so — a tool that ends up with root
# on your servers should not be teaching you to pipe privilege escalation into
# a shell.
set -eu

REPO="OAISP/shunt"
BIN="shunt"

say()  { printf '%s\n' "$*"; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

# ---- platform ---------------------------------------------------------------

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
  linux|darwin) ;;
  *) die "unsupported OS: $os (shunt drives the system ssh and rsync; Windows is not supported)" ;;
esac

arch=$(uname -m)
case "$arch" in
  x86_64|amd64)  arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) die "unsupported architecture: $arch" ;;
esac

asset="${BIN}_${os}_${arch}.tar.gz"

# ---- source -----------------------------------------------------------------

version="${SHUNT_VERSION:-latest}"
if [ "$version" = "latest" ]; then
  base="https://github.com/${REPO}/releases/latest/download"
else
  base="https://github.com/${REPO}/releases/download/${version}"
fi

if command -v curl >/dev/null 2>&1; then
  fetch() { curl -sSfL "$1" -o "$2"; }
elif command -v wget >/dev/null 2>&1; then
  fetch() { wget -qO "$2" "$1"; }
else
  die "need curl or wget"
fi

tmp=$(mktemp -d)
# shellcheck disable=SC2064
trap "rm -rf '$tmp'" EXIT INT TERM

say "downloading ${BIN} ${version} for ${os}/${arch}"
fetch "${base}/${asset}" "${tmp}/${asset}" || die "download failed: ${base}/${asset}"

# ---- verify -----------------------------------------------------------------
#
# The release publishes checksums.txt; refusing to install an unverified binary
# is cheap insurance for something that will hold ssh access to your servers.

if fetch "${base}/checksums.txt" "${tmp}/checksums.txt" 2>/dev/null; then
  expected=$(grep " ${asset}\$" "${tmp}/checksums.txt" | awk '{print $1}')
  [ -n "$expected" ] || die "checksums.txt has no entry for ${asset}"

  if command -v sha256sum >/dev/null 2>&1; then
    actual=$(sha256sum "${tmp}/${asset}" | awk '{print $1}')
  elif command -v shasum >/dev/null 2>&1; then
    actual=$(shasum -a 256 "${tmp}/${asset}" | awk '{print $1}')
  else
    actual=""
    say "warning: no sha256 tool found, skipping checksum verification"
  fi

  if [ -n "$actual" ]; then
    [ "$actual" = "$expected" ] || die "checksum mismatch for ${asset}
  expected $expected
  got      $actual"
    say "checksum verified"
  fi
else
  say "warning: checksums.txt unavailable, skipping verification"
fi

# ---- install ----------------------------------------------------------------

tar -xzf "${tmp}/${asset}" -C "$tmp" || die "could not extract ${asset}"
[ -f "${tmp}/${BIN}" ] || die "archive did not contain ${BIN}"
chmod +x "${tmp}/${BIN}"

if [ -n "${PREFIX:-}" ]; then
  dest="$PREFIX"
  mkdir -p "$dest" 2>/dev/null || die "cannot create $dest"
  [ -w "$dest" ] || die "$dest is not writable"
elif [ -w /usr/local/bin ]; then
  dest=/usr/local/bin
else
  dest="$HOME/.local/bin"
  mkdir -p "$dest"
  say "note: /usr/local/bin is not writable, installing to $dest"
fi

mv "${tmp}/${BIN}" "${dest}/${BIN}"
say "installed ${dest}/${BIN}"

# ---- confirm ----------------------------------------------------------------

case ":${PATH}:" in
  *":${dest}:"*) ;;
  *) say ""
     say "  ${dest} is not on your PATH. Add it:"
     say "    export PATH=\"${dest}:\$PATH\"" ;;
esac

say ""
"${dest}/${BIN}" version
say ""
say "next: cd into a project with a Dockerfile and run \`${BIN} init\`"
