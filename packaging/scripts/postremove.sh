#!/bin/sh
set -e
if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload >/dev/null 2>&1 || true
fi

# Recordings, pins and the report spool are deliberately left in place.
# Uninstalling the software must never destroy the evidence it produced —
# that is exactly what someone covering their tracks would reach for.
if [ -d /var/lib/argus ]; then
    echo "Argus data kept at /var/lib/argus (recordings, host key pins, spooled reports)."
    echo "Remove it deliberately if you are sure: rm -rf /var/lib/argus"
fi
exit 0
