# Installing Argus in production

This is the whole path from an empty server to a recorded SSH session, in the
order it has to happen. Each step ends with a check you can run; do not move on
until it passes.

## What runs where

| Component | Package | Runs on | Listens on |
|---|---|---|---|
| Control plane and console | `argus-control` | one server (two behind a load balancer if you need it) | `8080/tcp` HTTPS: the console, the API, and what agents report to |
| Agent | `argus-agent` | every server people log in to | a local Unix socket only |
| Verifier | `argus-verify` | the control plane, servers, and any machine an auditor checks recordings on, including macOS and Windows | nothing; it is a command |

Every server people log in to runs the agent: it is how sessions are opened,
recorded and refused. Browsers and agents need to reach `8080/tcp`; agents also
use `8080/udp` when it is open (see below). Nothing needs to reach an agent.

You also need:

- **PostgreSQL 13 or newer**, reachable from the control plane. Install the
  PostgreSQL client tools of the **same major version or newer** on the control
  plane host, because backups use `pg_dump` and it refuses an older client.
- **Optional: TLS certificates.** The control plane makes its own CA and
  issues itself a certificate from it, so none are needed.
  Bring your own from an internal or public CA if browsers should trust it
  without installing that CA (step 2).
- **Optional:** S3-compatible object storage for recordings. Without it,
  recordings stay on the servers' and the control plane's disks.

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

### macOS and Windows

The control plane and `argus-verify` are also released for macOS
and Windows, as `argus_<version>_darwin_<arch>.tar.gz` and
`argus_<version>_windows_<arch>.zip`. These are archives, not packages: nothing
installs a service for you. They also carry `argus-gateway`, which only browser
Remote Desktop uses until the Windows agent ships. Run `argus-control` from the unpacked folder and
it finds the console in the `console` folder beside it, serves its setup page,
and writes `control.yaml` in the working directory (or wherever `--config`
points). Use
`shasum -a 256 -c` on macOS, or `Get-FileHash` in PowerShell, in place of
`sha256sum`.

On Windows, a private key file (TLS, CA) must be readable
only by its owner, the account Argus runs as, SYSTEM and Administrators, just
as on Linux it must be mode `600`. A key copied under `C:\ProgramData`
inherits read access for every local user and is refused at startup; the error
gives the `icacls` command that fixes it.

The macOS archive also carries the agent; see [macOS servers](#macos-servers).
The Windows agent is not out yet, so Windows hosts cannot be recorded yet.

## 2. Certificates

Nothing to do unless you want your own. On first start the control plane makes
a small certificate authority in `/var/lib/argus-control/tls/`, issues its own server
certificate from it, and renews that certificate before it expires. Agents
learn to trust the CA when they enroll.

Browsers warn about that CA until you trust it. Download it from **Settings >
Certificate > Download the CA** and add it to your operating system's or
browser's trust store, or upload a certificate from your own CA or a public one
under **Settings > Certificate**. Its names must include every address people
and agents use to reach the control plane.

## 3. Control plane

```sh
sudo apt install ./argus-control_<version>_linux_amd64.deb ./argus-verify_<version>_linux_amd64.deb   # or dnf / apk
```

Create the database and a role that owns it:

```sql
CREATE ROLE argus LOGIN PASSWORD '...';
CREATE DATABASE argus OWNER argus;
```

Start the control plane. With no configuration it serves only its setup page:

```sh
sudo systemctl enable --now argus-control
journalctl -u argus-control -n 5          # "argus-control is not set up yet: finish setup in the browser"
sudo cat /var/lib/argus-control/setup-token
```

Open `https://argus.example.com:8080`. Every address goes to `/setup`, which
asks for, in order:

1. **The setup token** printed above. It proves you have the host, so nobody
   who merely finds the address can set Argus up. It works once.
2. **The database**: server, port, name, user, password and whether to use
   TLS. **Test connection** must succeed before you can go on. A database that
   already holds an Argus deployment keeps its accounts.
3. **Addresses**: where people open the console, where servers reach the
   control plane if that differs (behind a Cloudflare Tunnel, for one), the port, and whether
   agents may use QUIC on it.
4. **Certificate**: the control plane's own, or yours.
5. **The first administrator**: email, name and password.

**Finish setup** writes `/var/lib/argus-control/control.yaml`, the secrets beside it in
`/var/lib/argus-control/secrets/` (the database URL, a fresh signing secret and
reporter token, each readable by the `argus` account alone), and the
certificates in `/var/lib/argus-control/tls/`. The control plane then restarts on
them in the same process, and `control.installed` and `user.created` are the
first entries in the audit log, with the address setup came from.

Sign in and enrol a second factor when the console asks. Check it from the
host:

```sh
curl -fsS --cacert /var/lib/argus-control/tls/ca.crt https://argus.example.com:8080/healthz   # {"status":"ok"}
```

Migrations run on start. There is no separate migrate step.

### Settings

Everything the control plane is configured with is under **Settings** in the
console, for owners and administrators. Pick a section from the list on the
left; each fits on one screen:

| Section | Holds |
|---|---|
| Addresses | console and fleet addresses; the listen, console-only and QUIC addresses; CORS origins |
| Sign-in and limits | sign-in and ticket lifetimes, how long an agent may be silent, rate limits, trusted proxies, log level |
| Database | where PostgreSQL is, with a connection test |
| Certificate | what is served, upload a replacement, download the CA |
| Object storage | the S3-compatible bucket recordings are copied to |
| Session alerts | email, Telegram and Firebase push |
| People | add accounts, disable and enable them |
Secrets are write-only: the page says whether one is set, never what it is.
Changes in several sections are kept until you save, and **Save and apply** writes the file and restarts the control plane on it within
a few seconds: people stay signed in, and agents reconnect. If it cannot start
on the new settings, an address already in use for example, it puts the
previous ones back and the page says why. Every save is a `settings.changed`
entry in the audit log naming what changed.

The commands on the host still work and need no environment to load:
`sudo argus-control users list`, `users add`, `users disable`, `audit verify`
and `backup run|verify` all read `/var/lib/argus-control/control.yaml`.

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
sudo argus-control backup verify \
    -scratch-db postgres://argus:...@db.internal:5432/argus_restore_check \
    /var/backups/argus/<timestamp>
```

It restores the dump, checks the audit chain end to end, checks the recorded
chain head is present, and checks every recording is present and verifies.
Copy `/var/backups/argus` off the host: a backup on the server it protects
covers mistakes, not the loss of the server.

## 5. Agents

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

Once the tunnel is connected, **Connect** opens SSH sessions to that server
through its agent. The control plane checks you may
open the session and signs a one-minute grant for that server, that account
and you; the agent checks the signature itself, starts the account's login
shell as a normal login (in its own systemd unit, with a PAM session), and
records it on the server before anything reaches your browser. The recording
and the audit log name you, not only the account. A server whose agent is not connected cannot be
opened from **Connect** until it reconnects.

The agent trusts the first grant key the control plane sends and keeps it in
`/var/lib/argus/grant-key.pub`. The key is derived from `signing_secret`, so
if you change that secret on purpose, delete the file on each agent and
restart it; until then those agents refuse sessions and say why in their log.

Start the agent before adding the `sshd_config` lines. Once they are in, every
login goes through the agent, and a login is refused while the agent is not
running.

### macOS servers

The agent for macOS is in `argus_<version>_darwin_<arch>.tar.gz`, with its
launchd job in `launchd/`. As root:

```sh
install -m 0755 argus-agent /usr/local/bin/argus-agent
install -m 0644 launchd/com.argus.agent.plist /Library/LaunchDaemons/
argus-agent enroll --control-url https://argus.example.com:8080 \
    --pin sha256/... --token argus-enrol-...
launchctl bootstrap system /Library/LaunchDaemons/com.argus.agent.plist
argus-agent install    # sshd_config lines; Remote Login must be on for direct logins
```

It works as on Linux, with these differences:

- State and recordings are in `/var/db/argus`, the socket in
  `/var/run/argus`, and the log in `/var/log/argus-agent.log`.
- Accounts come from Directory Services (`dscl`), not `/etc/passwd`.
- Sessions through the console start the account's login shell directly, as
  root's child, without a PAM session.
- There is no kernel command tracing on macOS, so recordings are terminal
  output only, and each session says so in the console.

### Direct logins are refused

Every deployment is console-only, with no setting to change it. The agent
refuses any `ssh`, `scp` or `sftp` login that reached
sshd without going through Argus, before anything runs, tells the person to use
the console, and writes a `session.direct_refused` entry to the audit log. The
agent remembers this in its state directory, so a restart during a
control-plane outage keeps refusing. Use the machine's own console (hypervisor
or physical) as the break-glass path.

Every login that is admitted -- every session opened from the console -- is
recorded. There is no setting to record fewer.

## 6. Check it end to end

1. Assign yourself to the new server under **Servers**.
2. Open a terminal to it from **Connect**.
3. Run a command, close the session, and open it under **Sessions**. The
   replay should show what you typed.
4. Under **Audit**, the session, the enrollment and your sign-in are listed.

## Through a Cloudflare Tunnel

The console and its API can be published through a
[Cloudflare Tunnel](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/),
so people reach `https://argus.example.com` with no inbound port open on the
control plane. Agents cannot use the same tunnel (see
[What cannot go through the tunnel](#what-cannot-go-through-the-tunnel)), so
the control plane gives the two audiences separate listeners: the tunnel gets
the console, and the fleet keeps reaching `8080` directly.

### Control plane

Under **Settings**:

| Setting | Value |
|---|---|
| Addresses > Listen | `:8080`, unchanged: agents, TLS and QUIC |
| Addresses > Console listen | `127.0.0.1:8090`: the console, for cloudflared only |
| Addresses > Console address | `https://argus.example.com` |
| Addresses > Address servers use | `https://argus-internal.example.com:8080`: what servers enrol against |
| Sign-in and limits > Trusted proxies | `127.0.0.1/32` and `::1/128` |

Trusting loopback makes the control plane read each person's address from
CF-Connecting-IP, which Cloudflare sets and a client cannot. Without it every
sign-in, session and audit entry says 127.0.0.1, and one person's mistyped
passwords lock everyone out of the console for an hour.

Leave "Something in front terminates TLS" off. The console listener still
serves TLS, so cookies are already `Secure` and HSTS is already sent, and
turning it on would stop enrollment pinning the control plane's certificate.

"Address servers use" matters because "Add a server" otherwise writes the
address the console was opened at into the enrollment command, and agents
cannot enrol through the tunnel. The control plane's own certificate is
reissued to cover the new names when you save.

**Save and apply**, then check the new listener answers locally:

```sh
journalctl -u argus-control -n 20     # "argus-control listening (TLS)" surface=console addr=127.0.0.1:8090
curl -fsS --cacert /var/lib/argus-control/tls/ca.crt --resolve argus-internal.example.com:8090:127.0.0.1 \
  https://argus-internal.example.com:8090/healthz
```

A fresh install can be set up through the tunnel too: point cloudflared at
`https://localhost:8080` with `originServerName` set to this host's name
until setup is done, then at the console listener.

### cloudflared

Create the tunnel and route the hostnames to it as Cloudflare describes
(`cloudflared tunnel create argus`, then `cloudflared tunnel route dns argus
argus.example.com`). Then
`/etc/cloudflared/config.yml` on the control plane host:

```yaml
tunnel: <tunnel id>
credentials-file: /etc/cloudflared/<tunnel id>.json

ingress:
  # The console and its API, including the WebSockets the terminal uses.
  - hostname: argus.example.com
    service: https://localhost:8090
    originRequest:
      # A name on server.crt, and the CA that signed it. Without these,
      # cloudflared refuses the internal certificate; noTLSVerify would work
      # too, and would let anything on this host answer for Argus.
      originServerName: argus-internal.example.com
      caPool: /var/lib/argus-control/tls/ca.crt

  - service: http_status:404
```

`caPool` must be readable by the user cloudflared runs as; copy the CA (from
**Settings > Certificate > Download the CA**) next to its config if that is not
root. If the certificate is from a public CA, drop both `originRequest`
settings.

```sh
sudo cloudflared service install
sudo systemctl restart cloudflared
curl -fsS https://argus.example.com/healthz          # {"status":"ok"}
curl -s -o /dev/null -w '%{http_code}\n' https://argus.example.com/stats   # 404
```

`/stats` answers loopback callers only, and a request that arrives with
Cloudflare's or any proxy's forwarding headers is not treated as one, so the
second command must say `404`. A fresh install's `/setup` page works through
the tunnel and still needs the token from the host.

WebSockets need nothing extra. Cloudflare closes a WebSocket that carries
nothing for about 100 seconds, so the control plane pings every
open terminal and live view every 30 seconds to keep an idle shell open.

### The browser terminal

**Connect** reaches a server through its agent's tunnel, so the terminal is a
WebSocket to the control plane itself and already goes through
`argus.example.com`. Nothing else needs publishing, and sessions record the
person's real address.

### Cloudflare Access

An Access application in front of `argus.example.com` adds Cloudflare's sign-in
ahead of Argus's own. It does not replace it: Argus still asks for its password
and second factor and attributes everything to its own account. The console and
its API are one origin, so one Access application on that hostname covers both.

### What cannot go through the tunnel

A Cloudflare Tunnel carries HTTP and WebSocket traffic from browsers. Two
things Argus does are not that:

| Traffic | Why it cannot use the tunnel | Instead |
|---|---|---|
| Agents' connection to the control plane | On QUIC it is UDP, which a public hostname does not carry. Its HTTPS fallback upgrades the connection to a protocol of its own, and Cloudflare forwards only WebSocket upgrades. | Keep `8080/tcp` and `8080/udp` reachable from your servers: open them only to their addresses, or put them on a private network (WireGuard, Tailscale, a VPC). Cloudflare's private networking (WARP Connector on the servers) can carry both, but is not something we have tested. |
| Agents' reports | The control plane requires the reporter's client certificate on these routes, and Cloudflare terminates TLS, so the certificate never arrives. | The same direct `listen` address as above. This is why the console and the fleet have separate listeners. |

## Upgrading

Install the new packages over the old ones and restart:

```sh
sudo apt install ./argus-control_<new>_linux_amd64.deb
sudo systemctl restart argus-control
```

Upgrade the control plane first, then agents. `argus-verify` is its own
package; on a host upgraded from 0.1.x, install it in the same command as the server package, for example
`sudo apt install ./argus-control_<new>_linux_amd64.deb ./argus-verify_<new>_linux_amd64.deb`
(or one `rpm -U` with both files). Migrations run when
the control plane starts and only ever add; take a backup first anyway
(`systemctl start argus-backup.service`).

Your edited files in `/etc/argus` are kept. A control plane installed before
setup moved into the console goes on reading `/etc/argus/control.yaml` and the
variables in `/etc/argus/control.env`, and every setting appears under
**Settings**. The first save there writes `/var/lib/argus-control/control.yaml`, which
is read from then on. Secrets already referenced from `control.env` stay
there; a secret you change from Settings moves to `/var/lib/argus-control/secrets/`.
A control plane from before the console shipped, or before agents' tunnels,
gets both by saving Settings once: the console is found where the package
installs it, and **Addresses > QUIC** turns the tunnel's UDP listener on.

Sessions no longer go through `argus-gateway`: its SSH listener, its browser
SSH terminal and its native RDP listener all refuse, and it will not start
without `control.url`. It is still packaged only because browser Remote
Desktop to Windows goes through it until the Windows agent ships. If you do not
use that, stop and disable it (`sudo systemctl disable --now argus-gateway`)
after the agents on your Linux and macOS servers show **Connected**. If you
keep it, upgrade it after the control plane; its state (recordings, host key
pins, vaulted credentials) stays in `/var/lib/argus-gateway`, and an upgraded
one keeps the paths in its existing `gateway.yaml`.

Restart `argus-agent` after upgrading it (`sudo systemctl restart
argus-agent`). Until it restarts, each login waits about two seconds for an
answer the old daemon does not send, then is recorded as before.

A control plane that already has a configuration and accounts never offers the
setup page, so an upgrade changes nothing about how people sign in.

Agents that report with the shared reporter token keep working. Move them to
their own credential by enrolling each one (step 5) and removing
`--control-url` and `--control-token` from any override of the agent's unit.
