#!/bin/sh
set -e
if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload >/dev/null 2>&1 || true
fi

# Recordings and the report spool are deliberately left in place.
# Uninstalling the software must never destroy the evidence it produced —
# that is exactly what someone covering their tracks would reach for.
for d in /var/lib/argus; do
    if [ -d "$d" ]; then
        echo "Argus data kept at $d (recordings and spooled reports)."
        echo "Remove it deliberately if you are sure: rm -rf $d"
    fi
done
exit 0
