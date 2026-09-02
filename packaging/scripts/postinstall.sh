#!/bin/sh
# Runs after install or upgrade, on deb, rpm and apk alike.
set -e

# A dedicated system account. The gateway and control plane run as this; the
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

# Config may name a database, an issuer and internal hostnames. The env files
# hold credentials outright.
for f in /etc/argus/control.yaml /etc/argus/gateway.yaml; do
    [ -f "$f" ] && chmod 0640 "$f" && chown root:argus "$f"
done
for f in /etc/argus/control.env /etc/argus/gateway.env /etc/argus/agent.env; do
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

# Deliberately not enabled or started. These services need certificates,
# secrets and an inventory before they can do anything, and a package that
# starts a half-configured privileged-access gateway on install is a package
# that fails loudly in the worst possible place.
cat <<'MSG'

Argus installed. Nothing has been started — these services need configuration
first.

  1. Put certificates in /etc/argus/tls/
  2. Fill in the CHANGE-ME values in /etc/argus/*.env
  3. Review /etc/argus/*.yaml
  4. systemctl enable --now argus-<component>

The agent additionally needs sshd configured to record sessions:

  argus-agent install          # prints the sshd_config to add
  argus-agent scan             # reports how reachable this host is without Argus

MSG
exit 0
