#!/usr/bin/env bash
# Curl-bashable bootstrap for swarm-auto-drain.
#
#   curl -sSL https://github.com/matchory/swarm-auto-drain/releases/download/v0.2.0/install.sh \
#     | sudo VERSION=v0.2.0 UNIX_SOCKET_GID=9999 bash
#
# Re-runnable. Verifies the binary's SHA-256 against the matching .sha256
# file published with the release before placing it under /usr/local/sbin.

set -euo pipefail

VERSION="${VERSION:-latest}"
GID="${UNIX_SOCKET_GID:?UNIX_SOCKET_GID is required}"

ARCH="$(dpkg --print-architecture 2>/dev/null || uname -m)"
case "$ARCH" in
  amd64|x86_64)  ARCH=amd64 ;;
  arm64|aarch64) ARCH=arm64 ;;
  *) echo "unsupported arch: $ARCH" >&2; exit 1 ;;
esac

URL="https://github.com/matchory/swarm-auto-drain/releases/download/${VERSION}/swarm-auto-drain-linux-${ARCH}"
SHA="https://github.com/matchory/swarm-auto-drain/releases/download/${VERSION}/swarm-auto-drain-linux-${ARCH}.sha256"

tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT

echo "fetching ${URL}"
curl -fsSL -o "$tmp" "$URL"

expected="$(curl -fsSL "$SHA" | awk '{print $1}')"
actual="$(sha256sum "$tmp" | awk '{print $1}')"
if [ "$expected" != "$actual" ]; then
  echo "checksum mismatch: expected ${expected}, got ${actual}" >&2
  exit 1
fi

install -m 0755 -o root -g root "$tmp" /usr/local/sbin/swarm-auto-drain
/usr/local/sbin/swarm-auto-drain install --gid "$GID"

cat <<'NEXT'

next: deploy the cluster stack from any manager —

  swarm-auto-drain stack | docker stack deploy -c - swarm-auto-drain
NEXT
