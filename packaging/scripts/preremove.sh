#!/bin/sh
# Runs before argus-control is removed or upgraded, on deb, rpm and apk alike.
set -e

# Removing, not upgrading: deb passes "remove", rpm passes 0, apk's
# pre-deinstall passes nothing and runs only on removal.
case "${1:-remove}" in
    remove|0) removing=1 ;;
    *) removing=0 ;;
esac

# Only on removal. An upgrade used to stop and disable the control plane here,
# and nothing started it again on the new binary: the console either went dark
# or, where someone started the old process back up, kept reporting the old
# version on its About page. The new package's postinstall restarts it instead,
# or, for an update asked for from the console, the update helper, which checks
# the new version came up and goes back if not.
if [ "$removing" = 1 ] && command -v systemctl >/dev/null 2>&1; then
    for svc in argus-agent argus-control argus-control-update.path argus-backup-now.path \
               argus-control-host-enroll.path argus-control-host; do
        systemctl stop "$svc" >/dev/null 2>&1 || true
        systemctl disable "$svc" >/dev/null 2>&1 || true
    done
fi
exit 0
