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
- **Optional:** S3-compatible object storage for recordings. Without it, the
  control plane keeps the recordings agents ship in
  `/var/lib/argus-control/recordings` (`recordings_dir` in `control.yaml`), and
  the console replays them from there.

- **A Linux kernel with the CVE-2026-98432 fix** on the control plane host.
  It is a use-after-free in the kernel's IPv4 TCP handshake that anyone who can
  reach `8080/tcp` can trigger, so no setting in Argus fully closes it. Argus
  listens with plain TCP rather than MPTCP, which the only known exploit needs,
  but install your distribution's fixed kernel and reboot. Fixed upstream:
  7.3, 7.2.10, 6.18.56, 6.12.113, 6.6.159, 6.1.190, 5.15.223 and 5.10.272.
  Until then, `net.ipv4.tcp_migrate_req = 0` and `net.mptcp.enabled = 0` in
  `/etc/sysctl.d/` narrow it.

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

The control plane, the agent and `argus-verify` are also released for macOS
and Windows, as `argus_<version>_darwin_<arch>.tar.gz` and
`argus_<version>_windows_<arch>.zip`. These are archives, not packages: nothing
installs a service for you. Run `argus-control` from the unpacked folder and
it finds the console in the `console` folder beside it, serves its setup page,
and writes `control.yaml` in the working directory (or wherever `--config`
points). Use
`shasum -a 256 -c` on macOS, or `Get-FileHash` in PowerShell, in place of
`sha256sum`.

On Windows, a private key file (TLS or CA) must be readable
only by its owner, the account Argus runs as, SYSTEM and Administrators, just
as on Linux it must be mode `600`. A key copied under `C:\ProgramData`
inherits read access for every local user and is refused at startup; the error
gives the `icacls` command that fixes it.

Both archives also carry the agent; see [macOS servers](#macos-servers) and
[Windows servers](#windows-servers).

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
| Recordings | how long servers keep their copy of a stored recording (7 days by default), and the S3-compatible bucket recordings are copied to |
| Alerts | sign-in alerts (who is told when someone signs in), and the email, Telegram and Firebase push channels session alerts go to |
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

### The control server's own helper

The server argus-control runs on is looked after from the console too, with
nothing to install: the argus-control package carries the agent as
`argus-control-host`, and once argus-control is running it enrolls it for its
own hostname. Within a few minutes the server appears under Servers, marked
**argus-control's server**, with the same Updates, Logs, Disks and Terminal
tabs as any other. Choose who may open a terminal there on its Access tab.

- argus-control writes a single-use token to
  `/var/lib/argus-control/host/enroll.json` (readable by the `argus` account
  and root only); `argus-control-host-enroll.path` starts
  `argus-control-host.service`, which exchanges it once and removes nothing
  else. The token, the enrollment and every action afterwards are in the
  audit log, the token and enrollment with `argus-control` as the actor.
- Plain SSH to this server is never hooked, gated or recorded by Argus. That
  is deliberate: it is how you get back in when Argus itself is broken.
  Sessions opened from the console are recorded as on any server.
- The helper updates and restarts with argus-control (below), never from the
  server's Agent tab, and the server cannot be removed from Argus.
- The helper reaches argus-control on this server only. Where argus-control
  serves and pins its own certificate, that is the main listener. Behind a
  proxy or a Cloudflare Tunnel (`behind_tls_proxy`), on plain HTTP, or with
  `tls.client_ca_file`, argus-control opens a loopback-only TLS listener for
  it, `127.0.0.1:8449` (`control_host_listen`), with a key of its own in
  `/var/lib/argus-control/host/tls/` that the helper pins. Nothing off this
  server can reach it.
- If it cannot run, argus-control logs `the console cannot look after this
  server` with the reason at start: `journalctl -u argus-control | grep "look after"`.
- Where argus-agent is enabled on the same server, the agent takes over with
  the same enrollment and the helper stands down. An `argus-agent` package
  that is installed but not enabled does not count.
- To turn it off: `control_host: false` in the configuration, then
  `systemctl disable --now argus-control-host argus-control-host-enroll.path`
  and revoke its agent under Settings.

## 4. Backups

Do this before anyone records anything you would miss.

```sh
sudo systemctl enable --now argus-backup.timer
sudo systemctl start argus-backup.service        # the first one, now
journalctl -u argus-backup -n 30
```

A backup lands in `/var/backups/argus/<timestamp>/`: a `pg_dump` of the
database, a manifest of checksums, and an incremental mirror of the
recordings, from the bucket or, without one, from the control plane's
recordings directory. It first checks that the audit chain is
intact, so a backup never silently preserves a broken chain. The last 14 are
kept; set `ARGUS_BACKUP_KEEP` and `ARGUS_BACKUP_DIR` in `/etc/argus/backup.env`
to change that.

**Settings › Control plane › Backups** in the console shows the same thing
without a shell: how the last backup went (and its error, if it failed),
whether the nightly timer is on, every backup on disk, and which ones the next
run removes. **Back up now** starts `argus-backup.service` through
`argus-backup-now.path`, which the package enables, and **How many to keep**
sets the count from there. A count saved in the console wins over
`ARGUS_BACKUP_KEEP`.

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

**On Alpine, install OpenSSH with the CVE-2026-123158 fix first** (`apk update
&& apk upgrade openssh`, then `rc-service sshd restart`): 10.6_p2-r0 on edge,
10.3_p1-r2 on 3.24, 10.2_p1-r1 on 3.23, 10.0_p1-r11 on 3.22, 9.9_p2-r1 on 3.21.
Before it, Alpine's sshd let any password of 257 bytes or more into an account
whose shadow password is `*`. Argus no longer writes `*` for the accounts it
makes (an upgraded agent rewrites them to `*NP*` on its first pass), but other
accounts on the server may still have it; `awk -F: '$2=="*"{print $1}'
/etc/shadow` lists them. `PasswordAuthentication no` in `sshd_config` closes
it too. Distributions on glibc, and any sshd that uses PAM, were never affected.

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

### Disk use on each server

Every recording is written on the server plainly while the session runs, so
a killed agent still leaves a replayable prefix. When the session ends it is
compressed with zstd (`<id>.cast.zst`, `<id>.argusrdp.zst`): the compressed
copy is read back to the same bytes before the plain one is removed, and the
hash chain covers the recording's lines rather than the file, so it verifies
to the same chain head. It ships compressed and the control plane stores it
compressed; a recording from an older agent that arrives plain is compressed
by the control plane before it is stored.

A desktop recording is also *packed* inside the zstd: the base64 its screen
updates are written in is turned back into the bytes it encodes, which
compress about three times better, and turned back into the same base64 when
it is read. Read recordings with `argus-verify --cat` (it reads `.zst` and
`.gz` files, packed or not, and gives back the exact recording). `zstd -dc`
still gives back a terminal or database recording exactly, but not a packed
desktop one.

Measured: a 1080p desktop session shrinks about 21 times, a terminal session
about 5 times. gzip, which older agents used on their own copies, managed 3
to 4.

Once the control plane holds a verified copy, the server's copy is a second
copy and is deleted after 7 days. Change how long for the whole fleet in the
console, under Settings → Recordings → "Keep on servers for"; servers pick it
up on their next heartbeat. `--keep-shipped` on one daemon's command line
(`--keep-shipped=8760h` for a year, `--keep-shipped=0` for ever) overrides the
console on that server. A recording the control plane does not have -- still
queued, or refused -- is never deleted.

### Recording archive

The bucket (or the control plane's recordings directory) keeps recordings
for ever. Once one is older than the archive age -- 30 days unless changed
under Settings → Recordings → "Archive recordings older than", or
`recordings_archive_after_days` in `control.yaml` (`-1` never archives) --
the control plane recompresses it at zstd's strongest setting with a 128 MB
window, verifies the new copy against the session's chain head, stores it
under `archive/` and only then moves the session to it and deletes the old
object. A desktop recording comes out about 25 times smaller than it was
recorded, and one stored before packing is packed as it is archived. Each move is an audit entry (`recording.archived`); a stored
recording that no longer verifies is never archived and is reported as
`recording.unarchivable` instead.

An archived recording replays from the console like any other, and is
verified on every replay. With a bucket, `recordings_archive_storage_class`
(Settings → "Archive storage class") puts archived objects in a cheaper class.
Only classes that read back at once are accepted -- `STANDARD_IA`,
`ONEZONE_IA`, `INTELLIGENT_TIERING`, `GLACIER_IR` -- because a recording in
Glacier Flexible Retrieval or Deep Archive could not be replayed without a
restore that takes hours.

### Log volume

Both services log to the systemd journal. A line that repeats -- an agent
that cannot reach the control plane, a heartbeat from a host the inventory
does not know -- is written the first time and then held back for ten
minutes; the next one after that carries `repeated=N` for the lines in
between. The control plane's level is under Settings → Sign-in and limits →
Log level. An agent's is `ARGUS_LOG_LEVEL=warn` in `/etc/argus/agent.env`, or
`--log-level` on its command line. The journal itself is capped by journald,
which by default may use up to 10% of the filesystem (at most 4 GB); set
`SystemMaxUse=` in `/etc/systemd/journald.conf` for less.

`sessions.jsonl` starts a new file at 16 MB and keeps the previous one as
`sessions.jsonl.1`. The report spool is capped at 64 MB. On Windows the
service's `agent.log` starts a new file at 10 MB while it runs, keeping
`agent.log.1`.

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

### Windows servers

Each release has an installer for the agent,
`argus-agent_<version>_windows_<arch>.msi` (amd64 and arm64). It puts
`argus-agent.exe` in `C:\Program Files\Argus` and registers the `argus-agent`
service, set up as `service install` sets it up below. Run unattended, it
enrolls the server too, from an elevated PowerShell:

```powershell
msiexec /i argus-agent_<version>_windows_amd64.msi /qn /l*v argus-install.log `
    CONTROL_URL=https://argus.example.com:8080 PIN=sha256/... TOKEN=argus-enrol-...
```

| Property | |
|---|---|
| `CONTROL_URL` | the control plane, as this server reaches it |
| `TOKEN` | the single-use enrollment token from the console; kept out of the log |
| `PIN` | the key pin shown beside the token |
| `CONTROL_CA` | a CA file to trust instead of the pin |
| `FORCE=1` | on removal only: see below |

With `TOKEN`, the server is enrolled before the agent first starts, and a
failed enrollment fails the install (the reason is in the log). Without it, a
new install starts unenrolled until `argus-agent enroll` is run, and an
upgrade keeps the enrollment the server has. Opened with a double click, the
installer asks nothing and installs that way.

- **Upgrading** is running the newer installer the same way; the service,
  enrollment and recordings carry over. An older installer is refused. An
  agent installed by hand is taken over: its service is replaced by the
  installer's.
- **Updates from the console** work on an agent the installer put there, as on
  one installed by hand. Windows' list of apps then still shows the version
  last installed by the installer; `argus-agent version` says what runs.
- **Removing** it, from Settings > Apps or with
  `msiexec /x argus-agent_<version>_windows_amd64.msi /qn`, runs
  `argus-agent uninstall --yes`: it ships what has not reached the control
  plane, puts back the `SoftwareSASGeneration` policy if the agent set it (for
  the console's Ctrl+Alt+Del), revokes the server's credential, and removes the
  service and the program. When recordings would be stranded it refuses and leaves the agent
  running; `FORCE=1` removes it anyway, leaving them on the server. Recordings
  in `C:\ProgramData\Argus` are always kept.

The installer is signed with Sigstore like `checksums.txt`, as
`<name>.msi.sig` and `<name>.msi.pem`:

```sh
cosign verify-blob argus-agent_<version>_windows_amd64.msi \
  --signature argus-agent_<version>_windows_amd64.msi.sig \
  --certificate argus-agent_<version>_windows_amd64.msi.pem \
  --certificate-identity-regexp 'https://github.com/gsoultan/argus/.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

It has no Authenticode signature, so SmartScreen warns when it is opened from
a browser download.

Without the installer, `argus-agent.exe` is also in
`argus_<version>_windows_<arch>.zip`. In an elevated PowerShell:

```powershell
New-Item -ItemType Directory -Force 'C:\Program Files\Argus' | Out-Null
Copy-Item argus-agent.exe 'C:\Program Files\Argus\'
cd 'C:\Program Files\Argus'
.\argus-agent.exe enroll --control-url https://argus.example.com:8080 `
    --pin sha256/... --token argus-enrol-...
.\argus-agent.exe service install
```

`service install` registers the `argus-agent` service to start at boot as
LocalSystem, restarts it if it fails, and starts it. `service stop` returns
once the agent has stopped and sealed its open recordings. `service start`
and `service uninstall` do what they say. To take the agent off
the server altogether, use `.\argus-agent.exe uninstall` (see
[Uninstalling the agent](#uninstalling-the-agent)); `service uninstall` only
removes the service.

It works as on Linux, with these differences:

- State, recordings and the log (`agent.log`) are in `C:\ProgramData\Argus`,
  which the agent restricts to SYSTEM and Administrators before writing
  anything there.
- A session from the console runs as the local account on a Windows
  pseudoconsole, with the account's profile loaded (it is created on the
  account's first session). The shell is OpenSSH's `DefaultShell` if one is
  set, otherwise `cmd.exe`. The agent logs the account on without its
  password (Windows' S4U logon), so the session cannot reach network shares as
  the account, the same limit an OpenSSH key login has.
- A local administrator's session gets the filtered, non-elevated token
  Windows gives every network logon of a local account (remote UAC), so
  commands that need elevation fail in it.
- Only local accounts can be opened for now. A domain account (`CORP\ana`,
  `ana@corp.example`) is refused.
- Direct logins through Windows' OpenSSH server are **not refused yet**: the
  sshd hook is not ported, `argus-agent install` says so, and
  `argus-agent scan` reports the host as open while sshd accepts them. Until
  that lands, stop the `sshd` service.
- There is no kernel command tracing on Windows, so recordings are terminal
  output only, and each session says so in the console.

### Direct logins are refused

Every deployment is console-only, with no setting to change it. The agent
refuses any `ssh`, `scp` or `sftp` login that reached
sshd without going through Argus, before anything runs, tells the person to use
the console, and writes a `session.direct_refused` entry to the audit log. The
agent remembers this in its state directory, so a restart during a
control-plane outage keeps refusing.

**When the agent itself is stopped**, it cannot record or decide anything, so
ordinary users are refused. Root and members of `sudo`, `wheel` or `admin` are
still let in, unrecorded, with a warning on their terminal and an
`argus-agent` line in the auth log, so they can run
`sudo systemctl start argus-agent`. Without this a crashed agent locked every
account out of the server. Pick the groups with
`ForceCommand /usr/bin/argus-agent shim --break-glass=oncall,ops`, or turn it
off with `--break-glass=none` and rely on the machine's own console
(hypervisor or physical) instead.

If a server ever locks everyone out, follow [lockout.md](lockout.md). If the
last administrator loses their password or authenticator, follow
[admin-recovery.md](admin-recovery.md): recovery runs on this host with
`argus-control users reset-mfa|set-password|set-role|sign-out`, never in the
console.

Every login that is admitted -- every session opened from the console -- is
recorded. There is no setting to record fewer.

### Upgrading the agent

**Which version is offered.** Every six hours the control plane checks the
project's GitHub releases for the newest one with agent packages, and offers
agents whichever is newer: that release or its own version. A release with no
agent packages (a build that failed) is skipped. If the check fails, for
example with no internet access, agents are offered the control plane's own
version. To turn the check off, set `agent_release_check: false` in
`control.yaml`.

A new server added with the console's one-line install gets the same version.
If the control plane knows of no release, or that release has no package for
the server, the install script asks GitHub itself for the newest release that
has one, and checks the package against the release's `checksums.txt` before
installing it. Releases are tagged `v<version>`; the release workflow refuses
any other tag and fails if the agent packages were not attached.

**Every server at once:** the **Agents** card at the top of **Servers** lists
each server whose agent is behind the newest version, or can't update itself
and why. **Update N servers** asks every agent that is behind, and **Update**
on a row asks one. An admin can also turn on **Update agents automatically
when a new version is out**. With it on, each agent that is behind is asked on
its next check-in, within 30 seconds, and from then on whenever a new version
comes out. A version that went back on a server is not tried there again
automatically. The row says what happened, and **Try again** retries it.
Turning the setting on or off is audited (`agent.auto_update_on`,
`agent.auto_update_off`).

**One server, from the console** (Linux servers with systemd and the agent
installed from the package, or Windows servers with the agent installed as the
`argus-agent` service): open the server under **Servers**. The **Agent** card shows
the version it runs and offers **Update to** the newest version. Either way,
the agent then:

1. downloads the package for the new version, and the one for the version it
   runs, and checks both against the release's `checksums.txt`;
2. waits until nobody is connected to the server: no SSH login, no console
   terminal, desktop, file transfer or database session. **Update now** skips
   the wait and closes sessions opened from the console; SSH logins stay
   connected and their recording carries on under the new agent;
3. restarts on the new version, in a second or two. SSH logins that arrive
   meanwhile wait for it instead of being refused;
4. checks the new version takes SSH logins and reconnects to Argus within 90
   seconds, and if it does not (or keeps exiting), reinstalls the version that
   was running.

The card shows each step as the agent reports it, and the audit log records
who asked and how it went (`agent.update_requested`, then `agent.updated`,
`agent.update_rolled_back` or `agent.update_failed`). Agents download from
GitHub releases; servers that cannot reach GitHub can use a mirror laid out the
same way (`<url>/v<version>/argus-agent_<version>_linux_<arch>.deb` beside
`checksums.txt`), named in `control.yaml`:

```yaml
agent_releases_url: "https://mirror.internal/argus/releases/download"
# Optional: the mirror's list of releases, in the shape GitHub's releases API
# answers. Without one, a control plane using a mirror offers agents its own
# version only.
agent_releases_api: "https://mirror.internal/argus/releases.json"
```

On Windows there is no package: the agent downloads the release's
`argus_<version>_windows_<arch>.zip`, checks it against `checksums.txt`, and
takes `argus-agent.exe` out of it before anything stops. When the server is
free, a helper puts the new `argus-agent.exe` in place of the running one (the
running file is renamed aside, which Windows allows), then stops and starts the
`argus-agent` service, so the agent is down only for the stop and the start. If
the new version does not take logins and reconnect within 90 seconds, or keeps
exiting, the helper puts the previous `argus-agent.exe` back and restarts the
service on it. What the helper did is in
`C:\ProgramData\Argus\update-helper.log`. A mirror for Windows servers holds
the zip beside `checksums.txt` in the same `<url>/v<version>/` folder.

An agent older than this needs one upgrade by hand; after that it updates from
the console (Windows agents from the release after this one). macOS agents are
upgraded by hand.

**Restarting an agent, from the console** (Linux servers with systemd, or
Windows servers running the `argus-agent` service): the
same card offers **Restart agent**, which restarts it on the version it runs.
It waits until nobody is connected unless you choose **Restart now**, holds
SSH logins that arrive during the restart instead of refusing them, and
reports **Restarted** once the agent takes SSH logins and reconnects to Argus.
An agent that is not back within 90 seconds is reported as failed, with the
command to look at it. An update already restarts the agent, so a restart
cannot be asked while an update is waiting, and an update replaces a restart
that has not begun. The audit log records `agent.restart_requested`, then
`agent.restarted`, `agent.restart_failed` or `agent.restart_cancelled`. An
agent older than this release is restarted by updating it once.

**By hand:** install the new package over the old one. An enrolled agent is
restarted on the new version about 10 seconds after the package finishes, and
enabled if an older package had disabled it. A console session to that server
ends when the agent restarts; reconnect once the server shows connected again.
SSH logins stay connected, and logins that arrive during the restart wait a
few seconds for it rather than being refused.

### Android devices

An Android device is managed by the Argus app on it, not by a package: the
app is the agent. In the console, **Servers > Add server or device**, pick
**Android**, name the device and scan the code it shows from the app. The
app enrols under that name, appears under **Enrolled agents** like a server,
and the next step says how it is controlled: **attended** (whoever holds it
accepts each session and sees a banner while it runs) and whether control
needs an approval first.

The app is handed out by the control plane at
`/api/v1/install/argus-android.apk`, which the add page shows. Without
anything configured, that address sends the phone to the project's release
of the control plane's own version. A deployment that builds the app itself
(the `android/` project in the repository, signed with its own key) names the
file in `control.yaml`:

```yaml
android_apk: "/etc/argus/argus-android.apk"
```

On the device, the app asks for two things once: to run as an accessibility
service (how it touches the screen for you) and to show a notification. Each
session also goes through Android's own screen-capture prompt, which the
holder sees with the person's name on the app's own question first; nothing
is captured before they accept. The session is recorded on the device,
sealed, and shipped like a server's; **Watch live** and the player show it
as a desktop recording whose protocol is `android`.

### Removing an Android device

**From the console:** open the device and choose **Remove** under Settings. On
its next heartbeat the Argus app takes the device off Argus: it sends what is
still queued, revokes its own link and deletes it, turns off unattended control
(the shell helper stopped, wireless debugging switched back off, the pairing
key deleted, so it does not re-arm at boot), switches off its accessibility
service, and asks whoever holds the device to uninstall the app. If the control
plane cannot be told, the link is kept and it tries again later.

**On the device:** open the Argus app and tap **Remove from Argus**. The same
steps run; if recordings or reports have not reached Argus it says so and asks
before going on, since nothing can send them once the link is gone.

Android lets no app uninstall itself, or take back "Install unknown apps" for
itself, so the last step is the holder's: the app offers Android's own
uninstall question. If wireless debugging was used, revoke its authorisations
in Developer options. The unattended helper serves only the Argus app that
started it, by uid, and stops itself within seconds of the app being
uninstalled.

### Uninstalling the agent

**From the console:** open the server under **Servers** and choose **Remove
server**. It leaves Argus at once, and its agent uninstalls itself on its next
heartbeat: it takes itself out of sshd (checked with `sshd -t`, then reloaded),
locks the accounts Argus made on the server, sends what is still queued,
revokes its own credential, deletes it, and switches its service off. Plain SSH
works again. Recordings stay on the server,
and the package stays installed but off; remove it whenever you like. An agent
that is not connected does this when it next connects. To add the server back,
install and enroll the agent again.

**On the server**, as root (an elevated PowerShell on Windows):

```sh
sudo argus-agent uninstall
```

It asks first (pass `--yes` from a script), then, in this order:

1. **Checks someone can still log in over SSH afterwards.** Once the agent is
   gone, plain SSH is the way in. If no account has a key in
   `authorized_keys` and password logins are off, it stops before changing
   anything and says so. An `AuthorizedKeysCommand` or a non-default
   `AuthorizedKeysFile` counts; certificates from Argus's CA do not, because
   nothing signs them once the agent is gone. The result is printed:
   `Plain SSH works for: ana with 2 key(s)`.
2. **Stops the agent**, so it seals every open recording.
3. **Ships what has not reached the control plane yet**: spooled reports and
   queued recordings. If any are left, it starts the agent again and stops,
   because nothing can send them once the credential is revoked.
4. **Takes the agent out of sshd.** Every `ForceCommand` that runs the agent,
   in `sshd_config` or `sshd_config.d/*.conf`, is commented out with
   `# Removed by argus-agent uninstall:`. It then runs `sshd -t` and `sshd -T`.
   If sshd rejects the result, or still runs the agent from a file it did not
   read, the files are put back as they were, the agent is started again, and
   it stops. Then it reloads sshd. A `ForceCommand` left pointing at a removed
   binary would refuse every SSH login.
5. **Locks the accounts Argus made** on the server: personal accounts and the
   login accounts made from the console, known by `argus-managed` in their
   comment field. Each one's `authorized_keys` is emptied, it is locked and
   expired, and whatever still runs as it is stopped; its home is kept.
   `/etc/sudoers.d/argus-personal` is deleted. Left open, they kept keys that
   let people in with nothing recording, and a `NOPASSWD` sudo rule nothing
   would rewrite. Step 1 does not count these accounts as a way in, since they
   are about to be locked. `usermod --unlock --expiredate '' <name>` opens one
   again; `--keep-accounts` leaves them all as they are.
6. **Puts back what the agent changed** on its own: on Windows, the
   `SoftwareSASGeneration` policy, where the agent set it and it still holds
   the agent's value.
7. **Revokes this server's credential** with the control plane. This writes
   `agent.uninstalled` to the audit log, with who ran it as the server reports
   it. If the control plane cannot be reached, it says to revoke the server in
   the console instead.
8. **Deletes the credential**, the CA and grant key it trusted, and its
   working state, then **unregisters the service** (systemd, launchd or the
   Windows service).

Recordings, `sessions.jsonl` and the report spool stay in the state
directory. Delete them yourself once you are sure they are no longer needed.
Last, it prints what is left for you to do, usually removing the package
(`apt purge argus-agent`, `dnf remove argus-agent`, `apk del argus-agent`),
removing **Argus agent** from Apps on a Windows server the installer set up, or
deleting the binary.

**Removing the package runs all of this.** `apt remove`, `dnf remove` and
`apk del` run `argus-agent uninstall --yes` first, as removing the Windows
installer does, so a server is never left with an enrolled agent the console
waits for, or with Argus-made accounts nobody manages. If it refuses (stranded
recordings, or nobody could log in over SSH afterwards) the package stays and
it says why; `sudo ARGUS_FORCE=1 apt remove argus-agent` removes it anyway.

`--force` uninstalls even when reports or recordings have not reached the
control plane (they stay on the server), or when nobody could log in over SSH
afterwards.

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
control plane. There are two ways to lay it out:

- **Console through the tunnel, agents direct** (below): the tunnel gets the
  console, and the fleet keeps reaching `8080` directly over QUIC. The fastest
  for agents, and it needs `8080` reachable from your servers.
- **Everything through the tunnel** ([Agents through the tunnel too](#agents-through-the-tunnel-too)):
  no inbound port at all. Agents connect over a WebSocket, as fast as the
  HTTPS fallback but without QUIC.

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
cannot enrol through the tunnel. Without it the command would fail with curl's
`public key does not match pinned public key`, since curl meets Cloudflare's
certificate rather than the control plane's; "Add a server" checks the address
first and says to set this instead of showing such a command. The control plane's own certificate is
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

### Agents through the tunnel too

When every name has to go through Cloudflare, agents can use the same tunnel.
They try QUIC and the plain HTTPS upgrade first; a Cloudflare Tunnel carries
neither, so they fall back to a WebSocket, which it does carry, and remember
that for the next reconnect. Agents that reach the control plane directly are
unaffected.

Under **Settings**, unlike the layout above:

| Setting | Value |
|---|---|
| Addresses > Listen | `:8080`, unchanged |
| Addresses > Console listen | empty: one listener serves people and agents |
| Addresses > Console address | `https://argus.example.com` |
| Addresses > Address servers use | empty: servers use the console address |

and point cloudflared at that one listener, `service: https://localhost:8080`,
with the same `originRequest` as below. `8080` then needs no inbound rule.

When cloudflared runs on another machine, point it at the control plane's
address on your network instead (`service: https://10.0.0.10:8080`), keep
`originServerName` set to a name on the control plane's certificate, allow
`8080/tcp` from that machine alone, and put its address, not loopback, under
**Trusted proxies**.

**Add a server** sees Cloudflare's certificate at `argus.example.com`, which a
public authority signed, so the command it shows has no pin: curl and the agent
check that certificate the ordinary way, as a browser does. The server's page
says "Connected over WebSocket".

When the control plane reaches `argus.example.com` itself rather than
Cloudflare (a local DNS record, a hosts entry, or a router that resolves the
name to it), it sees its own certificate there and puts its pin in the
command. Servers on that network meet the same certificate; servers anywhere
else meet Cloudflare's. The command and the agent therefore accept either the
pinned key or a publicly trusted certificate for that exact name, so the same
command works from inside the network and from outside it. Before this, a
server outside stopped with curl's `public key does not match pinned public
key`.

What this costs is QUIC. Measured, a WebSocket carries what the HTTPS fallback
does (see [capacity](capacity.md#what-one-tunnel-carries)): typing feels the
same, and one large download is bounded by the round trip to Cloudflare's edge
and back rather than straight to the control plane.

Cloudflare refuses a request body over 100 MB (200 MB on Business plans). A
recording larger than that, which in practice means a long desktop session, is
refused on the way in; the agent keeps it on the host as it does any recording
the control plane will not take, and says so in its log.

Gateways reporting with the shared reporter token still need the client
certificate on their requests, which Cloudflare cannot pass on, so a gateway
still reaches `8080` directly.

### What cannot go through the tunnel

A Cloudflare Tunnel carries HTTP and WebSocket traffic from browsers. Two
things Argus does are not that:

| Traffic | Why it cannot use the tunnel | Instead |
|---|---|---|
| Agents' QUIC | UDP, which a public hostname does not carry. | Agents fall back to a WebSocket through the tunnel ([above](#agents-through-the-tunnel-too)), or keep `8080/tcp` and `8080/udp` reachable from your servers for QUIC: open them only to their addresses, or put them on a private network (WireGuard, Tailscale, a VPC). |
| Gateways' reports | The shared reporter token also needs the reporter's client certificate, and Cloudflare terminates TLS, so the certificate never arrives. Agents' own per-host credentials do not need it. | The direct `listen` address. |

Terminals, desktops and file transfer are browser traffic to the console, so
they go through the tunnel like the rest of it.

## Upgrading

### From the console

**Settings › Control plane › Updates** (owners and admins of the default
organization) shows the version running and the newest release, and updates to
it. Before anything changes it says:

- which open sessions the restart would end. Terminals on agents that hold a
  shell pause for a few seconds and pick up where they were; desktops,
  database sessions and terminals on older agents end. **When nothing would be
  cut off** waits for those, up to two hours; **Now** does not.
- whether going back would be clean. Each release publishes the migrations it
  carries (`argus-control_<version>_migrations.json`); if any of the new ones
  takes something away, the previous version may not start on the migrated
  database, and going back means restoring the backup.

The control plane cannot install a package itself. It writes the request to
`/var/lib/argus-control/update/request.json`, and `argus-control-update.path`
starts `argus-control-update.service` as root, which:

1. downloads the release's package from GitHub, or from the mirror in
   `ARGUS_CONTROL_RELEASES_URL` in `/etc/argus/control-update.env`, and checks
   it against the release's `checksums.txt`;
2. runs `argus-backup.service`, and changes nothing if the backup fails. If
   `pg_dump` is missing it first installs the distribution's PostgreSQL client
   (`postgresql-client`, or `postgresql` on RHEL); if that client is older
   than your database server, the backup fails with a version mismatch and
   you install the matching one, e.g. `postgresql-client-16` from the
   PostgreSQL repository;
3. installs the package and restarts `argus-control`;
4. waits up to 90 seconds for the new version to say it is listening with its
   database, and if it does not, or it exits three times, reinstalls the
   version that was running.

Each step shows on the page and in `journalctl -u argus-control-update`; the
outcome goes in the audit log (`control.updated`, `control.update_rolled_back`,
`control.update_failed`). With more than one replica, update each from its own
address, one at a time. Updating from the console needs Linux with systemd and
the package from this version on; the first upgrade to it is by hand, below.
A mirror of the releases needs the migrations file too, and the control plane
reads it from `agent_releases_url`.

### By hand

Install the new package over the old one, then check the new version is the
one running:

```sh
sudo apt install ./argus-control_<new>_linux_amd64.deb
dpkg -l argus-control | tail -1                      # the package: <new>
curl -sk https://localhost:8080/console-config.js    # the process: "version":"<new>"
```

The package restarts the control plane on the new binary about 10 seconds
after it installs, and leaves it enabled. **About** in the console reports the
running process's version, so it changes once that restart has happened.

If `dpkg -l` still shows the old version, the package did not install: run the
`apt install` again and read its output. A restart cannot help, because it
starts the binary that is installed.

Upgrading *from* 1.2.14 or older, the old package's removal script stops and
disables the service on the way out. The new package starts it again, but if
the console does not come back within a minute, run
`sudo systemctl enable --now argus-control`.

Upgrade the control plane first, then agents. Migrations run when
the control plane starts. The organization registry is added to its own schema,
`argus_platform`, and the install's existing tables become the default
organization in place; nothing else changes until a second organization is
created under **Settings > Organizations**. They mostly add, but 0.5.0 drops the columns behind
two settings it removed (the native-clients switch and the per-server
recording scope), so take a backup first
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

### Coming from argus-gateway

There is no `argus-gateway` any more. Every terminal and desktop opens from the
console through the server's own agent, so a server needs `argus-agent`
enrolled and connected before anyone can open it. Nothing else replaces the
gateway. v0.3.0 was the last release to ship it; retire it before or while you
upgrade to 0.5.0:

1. Enroll `argus-agent` on every server people use (**Servers > Add a server**),
   including Windows servers, and check each shows as connected.
2. Stop and remove the gateway: `sudo systemctl disable --now argus-gateway`,
   then `sudo apt remove argus-gateway` (or `rpm -e`, `apk del`).
3. Its data stays in `/var/lib/argus-gateway`, as removing a package never
   deletes evidence. Recordings there that reached object storage are already
   in the console; check any others with `argus-verify` before you delete the
   folder.
4. Remove `console.gateway_url` from `control.yaml` if you set it. It is ignored
   now.
5. In each server's `sshd_config`, `ForceCommand argus-agent shim` no longer
   needs `--gateway-keys`, and `ExposeAuthInfo` is no longer read. The old
   flags are accepted, so existing lines keep working.

Restart `argus-agent` after upgrading it (`sudo systemctl restart
argus-agent`). Until it restarts, each login waits about two seconds for an
answer the old daemon does not send, then is recorded as before.

A control plane that already has a configuration and accounts never offers the
setup page, so an upgrade changes nothing about how people sign in.

Agents that report with the shared reporter token keep working. Move them to
their own credential by enrolling each one (step 5) and removing
`--control-url` and `--control-token` from any override of the agent's unit.
