# Installing Argus in production

This is the whole path from an empty server to a recorded SSH session, in the
order it has to happen. Each step ends with a check you can run; do not move on
until it passes.

## What runs where

| Component | Package | Runs on | Listens on |
|---|---|---|---|
| Control plane and console | `argus-control` | one server (two behind a load balancer if you need it) | `8080/tcp` HTTPS: the console, the API, and what gateways and agents report to |
| Gateway | `argus-gateway` | one or more bastion servers | `8081/tcp` HTTPS + WebSocket: the browser terminal. `2222/tcp` SSH, loopback until you open it |
| Agent | `argus-agent` | every server people log in to | a local Unix socket only |
| Verifier | `argus-verify` | gateways, the control plane, and any machine an auditor checks recordings on | nothing; it is a command |

Browsers need to reach `8080` and `8081`. Gateways and agents need to reach
`8080/tcp`; agents also use `8080/udp` when it is open (see below). Gateways
need to reach port 22 on the servers they front. Nothing needs to reach an
agent.

You also need:

- **PostgreSQL 13 or newer**, reachable from the control plane. Install the
  PostgreSQL client tools of the **same major version or newer** on the control
  plane host, because backups use `pg_dump` and it refuses an older client.
- **TLS certificates** for the control plane and the gateway. Either from your
  own CA or a public one; both work (see step 2).
- **Optional:** S3-compatible object storage for recordings. Without it,
  recordings stay on the gateways' and agents' disks.

## 1. Download and verify

From the release page, download the packages for your architecture plus
`checksums.txt`, `checksums.txt.sig` and `checksums.txt.pem`, then:

```sh
cosign verify-blob checksums.txt \
  --signature checksums.txt.sig --certificate checksums.txt.pem \
  --certificate-identity-regexp 'https://github.com/gsoultan/argus/.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
sha256sum -c checksums.txt --ignore-missing
```

Both commands must succeed. A package that fails either is not ours.

## 2. Certificates

Every listener is TLS. Put the files in `/etc/argus/tls/` on each server:

| File | What it is |
|---|---|
| `server.crt`, `server.key` | this server's certificate. Its names must include the hostname browsers, gateways and agents use to reach it |
| `ca.crt` | the CA that issued the gateway's client certificate |
| `client.crt`, `client.key` | gateway only: the certificate it presents to the control plane |

If you have no internal CA, a small one with `openssl` is enough:

```sh
# Once, somewhere safe. Keep ca.key offline.
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
  -keyout ca.key -out ca.crt -days 3650 -subj "/CN=Argus internal CA"

# For each server (repeat with -subj "/CN=argus-gateway-client" for client.crt).
openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
  -keyout server.key -out server.csr -subj "/CN=argus.example.com"
openssl x509 -req -in server.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
  -days 397 -out server.crt \
  -extfile <(printf "subjectAltName=DNS:argus.example.com\nextendedKeyUsage=serverAuth,clientAuth")
```

Install them so the service account can read the private keys and nobody else
can. The control plane and gateway refuse to start with a key that group or
others can read:

```sh
sudo install -o root  -g argus -m 0640 server.crt ca.crt /etc/argus/tls/
sudo install -o argus -g argus -m 0600 server.key /etc/argus/tls/
# gateway only:
sudo install -o root  -g argus -m 0640 client.crt /etc/argus/tls/
sudo install -o argus -g argus -m 0600 client.key /etc/argus/tls/
```

Agents do not need a certificate. They prove who they are with a per-server
credential (step 6), and they learn to trust an internal CA during enrollment.

## 3. Control plane

```sh
sudo apt install ./argus-control_<version>_linux_amd64.deb ./argus-verify_<version>_linux_amd64.deb   # or rpm -i / apk add
```

Create the database and a role that owns it:

```sql
CREATE ROLE argus LOGIN PASSWORD '...';
CREATE DATABASE argus OWNER argus;
```

Edit `/etc/argus/control.env`. Every `CHANGE-ME` must go:

```sh
ARGUS_DATABASE_URL=postgres://argus:...@db.internal:5432/argus?sslmode=require
ARGUS_SIGNING_SECRET=$(openssl rand -base64 48)   # paste the value, not the command
ARGUS_REPORTER_TOKEN=$(openssl rand -base64 32)   # same; gateways use it
```

Then review `/etc/argus/control.yaml`. The parts that matter on day one:

- `tls:` points at the files from step 2. `client_ca_file` is what makes a
  gateway's client certificate required.
- `console.gateway_url` is the address browsers use to reach the gateway, for
  example `https://bastion.example.com:8081`. Until it is set, people can sign in
  but the terminal cannot open.
- `console_url` is the address people use to reach the console. Alerts link to it.

Start it and check it:

```sh
sudo systemctl enable --now argus-control
journalctl -u argus-control -n 20        # "database ready, migrations applied"
curl -fsS --cacert /etc/argus/tls/ca.crt https://argus.example.com:8080/healthz
```

Migrations run on start. There is no separate migrate step.

### The first administrator

Nothing is seeded. Create the first account on the control plane host:

```sh
sudo sh -c 'set -a; . /etc/argus/control.env; set +a
  argus-control -config /etc/argus/control.yaml users add you@example.com --role admin'
```

The `set -a` line loads the secrets the config refers to, the same way systemd
does for the service. The password is read without echo. Open `https://argus.example.com:8080`, sign
in, and enrol a second factor when the console asks.

## 4. Backups

Do this before anyone records anything you would miss.

```sh
sudo systemctl enable --now argus-backup.timer
sudo systemctl start argus-backup.service        # the first one, now
journalctl -u argus-backup -n 30
```

A backup lands in `/var/backups/argus/<timestamp>/`: a `pg_dump` of the
database, a manifest of checksums, and, when object storage is configured, an
incremental mirror of the recordings. It first checks that the audit chain is
intact, so a backup never silently preserves a broken chain. The last 14 are
kept; set `ARGUS_BACKUP_KEEP` and `ARGUS_BACKUP_DIR` in `/etc/argus/backup.env`
to change that.

**A backup you have not restored is a hope.** Prove it against a scratch
database (never the live one, which it refuses):

```sh
createdb argus_restore_check
sudo sh -c 'set -a; . /etc/argus/control.env; set +a
  argus-control -config /etc/argus/control.yaml backup verify \
    -scratch-db postgres://argus:...@db.internal:5432/argus_restore_check \
    /var/backups/argus/<timestamp>'
```

It restores the dump, checks the audit chain end to end, checks the recorded
chain head is present, and checks every recording is present and verifies.
Copy `/var/backups/argus` off the host: a backup on the server it protects
covers mistakes, not the loss of the server.

## 5. Gateway

```sh
sudo apt install ./argus-gateway_<version>_linux_amd64.deb ./argus-verify_<version>_linux_amd64.deb
sudo ssh-keygen -t ed25519 -N '' -f /etc/argus/ssh/gateway_host_key
sudo chown argus:argus /etc/argus/ssh/gateway_host_key
```

In `/etc/argus/gateway.env`, set the same `ARGUS_SIGNING_SECRET` and
`ARGUS_REPORTER_TOKEN` as the control plane. In `/etc/argus/gateway.yaml`:

- `control.url` is the control plane's address.
- `web.allowed_origins` must include the console's origin, for example
  `https://argus.example.com:8080`. A browser terminal from any other origin is
  refused.
- `listen` stays on `127.0.0.1:2222` unless you want native `ssh` clients. The
  browser terminal does not need it.

```sh
sudo systemctl enable --now argus-gateway
journalctl -u argus-gateway -n 20
```

The gateway appears in the console under Status once it reports.

Leave the `rdp:` block out. The native RDP listener is a preview in this
release, and it authenticates nobody; the gateway warns at start-up if it is
configured.

## 6. Agents

In the console, go to **Servers > Add a server**. Add the server, then make an
enrollment token. On the server:

```sh
sudo apt install ./argus-agent_<version>_linux_amd64.deb
sudo argus-agent enroll --control-url https://argus.example.com:8080 \
    --pin sha256/... --token argus-enrol-...      # copy this line from the console
sudo systemctl enable --now argus-agent
sudo argus-agent install    # prints the sshd_config lines; add them, then reload sshd
```

The token works once, expires within the hour, and can be bound to one
hostname. The pin is the control plane's key, so the very first connection
cannot be intercepted; if the control plane's certificate is from an internal
CA, the agent keeps that CA and survives a certificate renewal.

The server appears under **Enrolled agents** on the same page, with its
tunnel: the one outbound connection the agent holds to the control plane. It
says **Connected** with the measured round trip when the agent is on QUIC, and
**Connected over HTTPS** when UDP to the control plane is blocked. Both work;
QUIC is smoother on a lossy network, so open `8080/udp` on the control plane if
you can. Nothing needs to reach the server for this. To take a
server's access away, revoke it there. The control plane stops accepting its reports at
once (within 30 seconds on any other control plane replica).

Start the agent before adding the `sshd_config` lines. Once they are in, every
login goes through the agent, and a login is refused while the agent is not
running.

### Which logins are recorded

Each server's page in the console has **Which logins are recorded**. Logins
through Argus are always recorded; this decides what happens when someone
logs in to the server directly, skipping Argus:

- **Record logins through Argus and direct logins** (the default): the direct
  login is recorded too, and flagged as a bypass.
- **Record logins through Argus only**: the direct login is let
  through unrecorded. It is still written to the audit log as a critical
  `session.direct_unrecorded` entry with the account, source address and time,
  and the user is told on login that it is not recorded.

The agent picks the setting up at its next heartbeat (every 30 seconds) and
the page says once it has. Until it has heard from the control plane, an agent
records every login. Changing the setting is in the audit log under the
administrator who made it.

## 7. Check it end to end

1. Assign yourself to the new server under **Servers**.
2. Open a terminal to it from **Connect**.
3. Run a command, close the session, and open it under **Sessions**. The
   replay should show what you typed.
4. Under **Audit**, the session, the enrollment and your sign-in are listed.

## Upgrading

Install the new packages over the old ones and restart:

```sh
sudo apt install ./argus-control_<new>_linux_amd64.deb
sudo systemctl restart argus-control
```

Upgrade the control plane first, then gateways, then agents. Since this
release `argus-verify` is its own package; on a host upgraded from 0.1.x,
install it in the same command as the server package, for example
`sudo apt install ./argus-gateway_<new>_linux_amd64.deb ./argus-verify_<new>_linux_amd64.deb`
(or one `rpm -U` with both files). Migrations run when
the control plane starts and only ever add; take a backup first anyway
(`systemctl start argus-backup.service`).

Your edited files in `/etc/argus` are kept. The package manager puts the new
defaults beside them (`.dpkg-dist` or `.rpmnew`), so compare when you upgrade.
In particular, a control plane installed before the console shipped has no
`console:` block; add this to `control.yaml` to serve it:

```yaml
console:
  dir: "/usr/share/argus/console"
  gateway_url: "https://bastion.example.com:8081"
```

A control plane upgraded from an earlier release has no `tunnel:` block, so
agents connect over HTTPS only. Add this to `control.yaml` for QUIC, and open
the port for UDP:

```yaml
tunnel:
  quic_listen: "0.0.0.0:8080"
```

Restart `argus-agent` after upgrading it (`sudo systemctl restart
argus-agent`). Until it restarts, each login waits about two seconds for an
answer the old daemon does not send, then is recorded as before.

Agents that report with the shared reporter token keep working. Move them to
their own credential by enrolling each one (step 6) and removing
`--control-url` and `--control-token` from any override of the agent's unit.
