#!/bin/sh
set -e
if command -v systemctl >/dev/null 2>&1; then
    for svc in argus-agent argus-gateway argus-control; do
        systemctl stop "$svc" >/dev/null 2>&1 || true
        systemctl disable "$svc" >/dev/null 2>&1 || true
    done
fi
exit 0
