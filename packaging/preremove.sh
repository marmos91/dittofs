#!/bin/sh
set -e
if ! command -v systemctl >/dev/null 2>&1 || [ ! -d /run/systemd/system ]; then
  exit 0
fi

# Skip on upgrade (deb passes "upgrade", rpm passes $1=1)
case "${1:-}" in
  upgrade|1) exit 0 ;;
esac

if systemctl is-active --quiet dfs 2>/dev/null; then
  systemctl stop dfs || true
fi
if systemctl is-enabled --quiet dfs 2>/dev/null; then
  systemctl disable dfs || true
fi
