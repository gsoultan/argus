#!/bin/sh
# Runs after argus-secret is installed or upgraded, on deb, rpm and apk alike.
set -e

# Its own account, not argus: the control plane must never be able to read
# the root key, or the split between rules and values is gone.
if ! getent group argus-secret >/dev/null 2>&1; then
    groupadd --system argus-secret
fi
if ! getent passwd argus-secret >/dev/null 2>&1; then
    useradd --system --gid argus-secret --home-dir /var/lib/argus-secret \
            --shell /usr/sbin/nologin \
            --comment "Argus secrets server" argus-secret
fi

install -d -o root -g argus-secret -m 0750 /etc/argus-secret
install -d -o argus-secret -g argus-secret -m 0700 /var/lib/argus-secret
[ -f /etc/argus-secret/secret.yaml ] && chmod 0640 /etc/argus-secret/secret.yaml && chown root:argus-secret /etc/argus-secret/secret.yaml

if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload >/dev/null 2>&1 || true
fi
# Not started: it needs argus-control's key in /etc/argus-secret/secret.yaml first.
exit 0
