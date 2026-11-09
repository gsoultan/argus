#!/bin/sh
# Runs before argus-agent is removed or upgraded, on deb, rpm and apk alike.
set -e

# Removing, not upgrading: deb passes "remove", rpm passes 0, apk's
# pre-deinstall passes nothing and runs only on removal.
case "${1:-remove}" in
    remove|0) removing=1 ;;
    *) removing=0 ;;
esac

# Removing the package takes the agent off the host properly, as removing the
# Windows installer does: `argus-agent uninstall` ships what has not reached
# the control plane, takes the agent out of sshd (checking sshd accepts the
# result), locks the accounts Argus made and takes back their sudo, revokes
# this host's credential and deletes it. Removing only the files did none of
# that: the console went on listing an enrolled agent that would never call
# again, and agent-made accounts kept their keys and a NOPASSWD sudo rule with
# nothing recording them.
#
# It refuses, and the package stays, when recordings would be stranded or
# nobody could log in over SSH afterwards. ARGUS_FORCE=1 removes it anyway;
# recordings always stay in /var/lib/argus.
if [ "$removing" = 1 ]; then
    hooked=0
    for f in /etc/ssh/sshd_config /etc/ssh/sshd_config.d/*.conf; do
        [ -f "$f" ] || continue
        if grep -Eiq '^[[:space:]]*ForceCommand[[:space:]=]+"?[^[:space:]"]*/argus[^[:space:]"/]*("|[[:space:]]|$)' "$f"; then
            hooked=1
        fi
    done
    # Already uninstalled (`argus-agent uninstall`, perhaps with
    # --keep-accounts) leaves neither, and is not done again.
    if [ -f /var/lib/argus/enrollment.json ] || [ "$hooked" = 1 ]; then
        set -- --yes --from-package
        if [ "${ARGUS_FORCE:-}" = 1 ]; then
            set -- "$@" --force
        fi
        if [ -x /usr/bin/argus-agent ] && /usr/bin/argus-agent uninstall "$@"; then
            :
        else
            echo "argus-agent: the agent could not be taken off this host, so the package" >&2
            echo "was not removed (see above). Fix what it says and remove it again, or:" >&2
            echo "  sudo ARGUS_FORCE=1 <your package manager> remove argus-agent" >&2
            echo "to remove it anyway (recordings stay in /var/lib/argus)." >&2
            exit 1
        fi
    fi
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
