#!/bin/sh
# Runs before argus-agent is removed or upgraded, on deb, rpm and apk alike.
set -e

# Removing, not upgrading: deb passes "remove", rpm passes 0, apk's
# pre-deinstall passes nothing and runs only on removal.
case "${1:-remove}" in
    remove|0) removing=1 ;;
    *) removing=0 ;;
esac

# A ForceCommand still pointing at the agent refuses every SSH login once the
# binary is gone, and the machine's own console becomes the only way back in.
# `argus-agent uninstall` takes it out, checks sshd accepts the result, and
# revokes this host's credential; removing the package does none of that.
if [ "$removing" = 1 ]; then
    for f in /etc/ssh/sshd_config /etc/ssh/sshd_config.d/*.conf; do
        [ -f "$f" ] || continue
        if grep -Eiq '^[[:space:]]*ForceCommand[[:space:]=]+"?[^[:space:]"]*/argus[^[:space:]"/]*("|[[:space:]]|$)' "$f"; then
            echo "argus-agent: $f still sends every SSH login to the agent." >&2
            echo "Removing the package now would refuse every login to this host." >&2
            echo "Run this first, then remove the package:" >&2
            echo "  sudo argus-agent uninstall" >&2
            exit 1
        fi
    done
fi

# Only on removal. An upgrade used to stop and disable the agent here, and
# nothing started it again: every session through it dropped, and with the
# sshd hook in place every SSH login was refused until someone reached the
# machine's own console. The new package's postinstall restarts it instead.
if [ "$removing" = 1 ] && command -v systemctl >/dev/null 2>&1; then
    systemctl stop argus-agent >/dev/null 2>&1 || true
    systemctl disable argus-agent >/dev/null 2>&1 || true
fi
exit 0
