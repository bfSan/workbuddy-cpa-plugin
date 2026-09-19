#!/usr/bin/env bash
# Build the workbuddy plugin on a remote CPA host and restart the service.
#
# Why build on the target instead of cross-compiling here: the plugin is
# -buildmode=c-shared with CGO enabled, and a macOS host cannot produce a
# linux/amd64 .so without a cross toolchain. Building on the target is one
# less moving part.
#
# Usage: ./scripts/deploy-remote.sh <ssh-host> [plugin-dir] [service]
#   ./scripts/deploy-remote.sh cpa /opt/cpa/plugins cpa
set -euo pipefail

SSH_HOST="${1:?usage: deploy-remote.sh <ssh-host> [plugin-dir] [service]}"
PLUGIN_DIR="${2:-/opt/cpa/plugins}"
SERVICE="${3:-cpa}"
SRC_DIR=/opt/src/workbuddy-cpa-plugin

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ARCHIVE="$(mktemp -t wb-plugin.XXXXXX).tar.gz"
trap 'rm -f "$ARCHIVE"' EXIT

echo "==> packaging HEAD from $REPO_ROOT"
git -C "$REPO_ROOT" archive --format=tar.gz --prefix=workbuddy-cpa-plugin/ -o "$ARCHIVE" HEAD

echo "==> uploading to $SSH_HOST"
scp -q "$ARCHIVE" "$SSH_HOST:/tmp/wb-plugin-src.tar.gz"

echo "==> building on $SSH_HOST"
ssh "$SSH_HOST" "set -euo pipefail
export PATH=/usr/local/go/bin:\$PATH
export HOME=\${HOME:-/root}
command -v go >/dev/null || { echo 'go is not installed at /usr/local/go/bin/go' >&2; exit 1; }
sudo mkdir -p $(dirname $SRC_DIR)
sudo rm -rf $SRC_DIR
sudo tar -xzf /tmp/wb-plugin-src.tar.gz -C $(dirname $SRC_DIR)
sudo env PATH=/usr/local/go/bin:\$PATH HOME=\$HOME go clean -cache
cd $SRC_DIR/workbuddy
sudo env PATH=/usr/local/go/bin:\$PATH HOME=\$HOME go test -count=1 ./...
sudo env PATH=/usr/local/go/bin:\$PATH HOME=\$HOME CGO_ENABLED=1 go build -trimpath -buildmode=c-shared \
  -ldflags '-s -w' -o /tmp/workbuddy.so .
file /tmp/workbuddy.so"

echo "==> installing and restarting $SERVICE"
ssh "$SSH_HOST" "set -euo pipefail
sudo cp $PLUGIN_DIR/workbuddy.so $PLUGIN_DIR/workbuddy.so.bak-\$(date +%Y%m%d-%H%M%S)
sudo install -m 755 /tmp/workbuddy.so $PLUGIN_DIR/workbuddy.so
sudo chown bofeng:bofeng $PLUGIN_DIR/workbuddy.so 2>/dev/null || true
sudo rm -rf $SRC_DIR /tmp/workbuddy.so /tmp/wb-plugin-src.tar.gz
sudo systemctl restart $SERVICE
sleep 3
sudo systemctl is-active $SERVICE
sudo journalctl -u $SERVICE --since '1 min ago' --no-pager | grep -i 'plugin registered plugin_id=workbuddy' || true"

echo "==> done"
