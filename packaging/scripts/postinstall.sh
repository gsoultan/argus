#!/bin/sh
# Runs after install or upgrade, on deb, rpm and apk alike.
set -e

# A dedicated system account. The control plane runs as this; the
# agent runs as root and does not use it.
if ! getent group argus >/dev/null 2>&1; then
    groupadd --system argus
fi
if ! getent passwd argus >/dev/null 2>&1; then
    useradd --system --gid argus --home-dir /var/lib/argus \
            --shell /usr/sbin/nologin \
            --comment "Argus privileged access" argus
fi

install -d -o root -g argus -m 0750 /etc/argus
install -d -o root -g argus -m 0750 /etc/argus/tls
install -d -o root -g argus -m 0750 /etc/argus/ssh
install -d -o argus -g argus -m 0700 /var/lib/argus
install -d -o root -g root  -m 0700 /var/lib/argus/recordings
# Backups hold the audit chain and every recording. Only the control plane's
# account writes them; nobody else reads them.
if [ -x /usr/bin/argus-control ]; then
    install -d -o argus -g argus -m 0700 /var/backups/argus
    # The control plane's configuration, secrets and certificates, apart from
    # the agent's /var/lib/argus, which the agent's unit makes root's.
    install -d -o argus -g argus -m 0700 /var/lib/argus-control
fi

# Config may name a database, an issuer and internal hostnames. The env files
# hold credentials outright.
for f in /etc/argus/control.yaml; do
    [ -f "$f" ] && chmod 0640 "$f" && chown root:argus "$f"
done
for f in /etc/argus/control.env /etc/argus/agent.env; do
    [ -f "$f" ] && chmod 0600 "$f" && chown root:argus "$f"
done
# Anyone who can read a private key owns everything that trusts it.
for d in /etc/argus/tls /etc/argus/ssh; do
    [ -d "$d" ] && find "$d" -type f -name '*.key' -exec chmod 0600 {} \; 2>/dev/null || true
    [ -d "$d" ] && find "$d" -type f ! -name '*.pub' ! -name '*.crt' -exec chmod 0600 {} \; 2>/dev/null || true
done

if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload >/dev/null 2>&1 || true
fi

# The watcher for updates asked from the console. It only starts the root
# helper when the control plane writes a request, so it is on from install;
# the control plane itself is still not started until it has been set up.
if [ -x /usr/bin/argus-control ] && command -v systemctl >/dev/null 2>&1 &&
   systemctl cat argus-control-update.path >/dev/null 2>&1; then
    systemctl enable --now argus-control-update.path >/dev/null 2>&1 || true
fi
# The same for "Back up now" in the console: it only starts the backup unit
# when the control plane writes a request.
if [ -x /usr/bin/argus-control ] && command -v systemctl >/dev/null 2>&1 &&
   systemctl cat argus-backup-now.path >/dev/null 2>&1; then
    install -d -o argus -g argus -m 0700 /var/lib/argus-control/backup
    systemctl enable --now argus-backup-now.path >/dev/null 2>&1 || true
fi

# An enrolled agent is one somebody set up, so after an upgrade it runs again,
# on the new binary. Packages before this stopped and disabled it on every
# upgrade, which left every SSH login refused while the sshd hook was in
# place, so it is enabled here too rather than only restarted.
#
# A few seconds later, not now: an upgrade run from a console session goes
# through this agent, and restarting it ends that session -- and the package
# manager running in it -- before it has finished.
if [ -x /usr/bin/argus-agent ] && [ -f /var/lib/argus/enrollment.json ] &&
   command -v systemctl >/dev/null 2>&1 && systemctl cat argus-agent >/dev/null 2>&1; then
    systemctl enable argus-agent >/dev/null 2>&1 || true
fi
# An update the console asked for restarts the agent itself, once nothing is
# open on the server, and checks the new version came up; a second restart
# from here would only get in its way.
if [ -n "${ARGUS_AGENT_UPDATING:-}" ]; then
    :
elif [ -x /usr/bin/argus-agent ] && [ -f /var/lib/argus/enrollment.json ] &&
   command -v systemctl >/dev/null 2>&1 && systemctl cat argus-agent >/dev/null 2>&1; then
    if ! systemd-run --quiet --on-active=10s --unit="argus-agent-upgrade-$$" \
            systemctl restart argus-agent >/dev/null 2>&1; then
        systemctl restart --no-block argus-agent >/dev/null 2>&1 || true
    fi
    echo "argus-agent restarts on the new version in about 10 seconds."
fi

# A configured control plane runs again after an upgrade, on the new binary.
# Before this, the package's preremove stopped and disabled it on every
# upgrade and nothing started it again; where someone brought the old process
# back, the files on disk were new but the process was not, and the console's
# About page (which asks the process) kept showing the old version.
#
# "Configured" is setup having written its configuration: a fresh install has
# none and stays unstarted (below). Enabled as well as restarted, because the
# package being replaced may carry the old preremove, which disables it -- and
# on rpm that runs after this script, which is the other reason this waits.
#
# A few seconds later, not now: an upgrade run from a console terminal goes
# through this control plane, and restarting it ends that session -- and the
# package manager running in it -- before it has finished.
# An update the console asked for restarts the control plane itself and checks
# the new version came up; a second restart from here would only get in its way.
if [ -n "${ARGUS_CONTROL_UPDATING:-}" ]; then
    :
elif [ -x /usr/bin/argus-control ] && command -v systemctl >/dev/null 2>&1 &&
   systemctl cat argus-control >/dev/null 2>&1 &&
   { systemctl is-active --quiet argus-control 2>/dev/null ||
     [ -f /var/lib/argus-control/control.yaml ]; }; then
    if ! systemd-run --quiet --on-active=10s --unit="argus-control-upgrade-$$" \
            /bin/sh -c 'systemctl enable argus-control; systemctl restart argus-control' >/dev/null 2>&1; then
        systemctl enable argus-control >/dev/null 2>&1 || true
        systemctl restart --no-block argus-control >/dev/null 2>&1 || true
    fi
    echo "argus-control restarts on the new version in about 10 seconds."
fi

# A fresh install is deliberately not enabled or started. These services need
# certificates, secrets and an inventory before they can do anything, and a package that
# starts a half-configured privileged-access service on install is a package
# that fails loudly in the worst possible place.
cat <<'MSG'

Argus installed. Nothing has been started — these services need configuration
first.

On the control plane, start it and finish setup in the browser:

  systemctl enable --now argus-control
  open https://<this host>:8080/setup      # asks for: sudo cat /var/lib/argus-control/setup-token
  systemctl enable --now argus-backup.timer

Everything else is under Settings in the console. Every server people log
in to needs argus-agent: enroll it from Servers > Add a server.

The agent additionally needs sshd configured to record sessions:

  argus-agent install          # prints the sshd_config to add
  argus-agent scan             # reports how reachable this host is without Argus

MSG
exit 0
