# Argus

Privileged access management that knows who reached which server, as whom, and
what they did there, and can prove it later. Every session opens from the Argus
console and runs through an agent on the server, which records it there and
streams the record back as it happens. The audit trail is a hash chain, so
tampering shows.

```
browser ──HTTPS / WSS──▶ argus-control ──agent tunnel──▶ argus-agent ──▶ local login (shell, files)
          sign-in, MFA,     checks access, signs           checks the grant, opens the
          the console       a one-minute grant, relays     session, records it on the server
```

You install two things:

| Component | Package | Runs on |
|---|---|---|
| Control plane and console | `argus-control` | one server (two behind a load balancer if you need it) |
| Agent | `argus-agent` | every server people log in to |
| Verifier (optional) | `argus-verify` | anywhere recordings are checked, including an auditor's laptop |

The agent holds one outbound connection to the control plane (QUIC, or HTTPS
when UDP is blocked). Nothing needs to reach the servers, and nothing on them
listens beyond a local Unix socket.

**[docs/install.md](docs/install.md) is the production install, step by step.**
This page is the overview and the reference behind it.

## Contents

- [How people get in](#how-people-get-in)
- [Installing](#installing)
- [Reaching the console through a Cloudflare Tunnel](#reaching-the-console-through-a-cloudflare-tunnel)
- [Servers, assignments and roles](#servers-assignments-and-roles)
- [Verifying a recording: `argus-verify`](#verifying-a-recording-argus-verify)
- [Security posture](#security-posture)
- [Measured capacity](#measured-capacity)
- [Not done yet](#not-done-yet)
- [Running locally](#running-locally)

## How people get in

**Every connection goes through the console.** Sign in and open **Servers**.
Each server you may use is a card with a **Connect** button and the accounts you
can log in as there, plus any temporary access you have been granted. Pick an
account and choose what to open:

- **Open terminal**: a shell on the server, started by its agent as that
  account (a normal login, in its own systemd unit with a PAM session) and
  recorded on the server before you see any of it.
- **Transfer files**: the console's scp. Browse folders, upload and download.
  The agent reads and writes as the account you picked, so it can touch exactly
  what that account can, and every transfer goes in the audit log with the
  person, server, account, path, size and SHA-256.
- **Open desktop**: Remote Desktop to a Windows server, in the browser. The
  agent runs on the server as a service and connects to its own Remote Desktop;
  you type the account's Windows password when the desktop opens, it is used
  once in memory to sign in, and the session is recorded on the server.

The same checks run whichever you open. You must be assigned the server and the
account; an elevated account (`root`, `Administrator`) also needs an approved
access request; and the grant that opens the session is single-use, signed for
that person, server and account, and expires in about a minute. The control
plane checks it before relaying, and the agent checks the signature again
before opening anything, so a compromised relay cannot open sessions by itself.

### Direct SSH logins

Sessions are meant to open from the console. What happens when someone runs
`ssh`, `scp` or `sftp` straight to a server is a setting, **Settings → How
people connect → Direct SSH logins**, and each server can override it on its
own page:

| Setting | What happens |
|---|---|
| **Refuse** (the default) | The agent refuses the login before anything runs, tells the person to use the console, and writes `session.direct_refused` to the audit log with the account, source address and command. |
| **Ask for approval** | The person's terminal says the login is waiting. It shows on **Access requests** with the server, account, source address and command, for an approver, admin or owner to approve or deny. Nobody answering in two minutes, a denial, or the control plane being unreachable all refuse it. Each step is in the audit log (`session.direct_requested`, `_approved`, `_denied`, `_expired`). |
| **Allow and record** | The login opens with nobody approving it and is reported as `session.direct_allowed`. |

Whatever the setting, a login that is let in is recorded like any other
session. A direct login only tells Argus the server account and the address it
came from, not which person it was, so its recording is attributed to
`account@server`; an approval records who let it in.

| Path | What happens |
|---|---|
| A server whose agent is not connected | Nothing opens from the console. The Servers page says the agent is offline and when it last checked in. |
| An agent restarted while the control plane is down | It keeps applying the last setting it heard; it remembers it in its state directory. Under **Ask for approval** that means refusing, since there is nobody to ask. |
| An agent older than the setting | It refuses direct logins under **Refuse** and **Ask for approval**, and records them under **Allow and record**. |

The machine's own console (hypervisor or physical) is not affected, and is the
break-glass path when the control plane is down.

## Installing

Releases publish `.deb`, `.rpm` and `.apk` for amd64 and arm64, built by
GoReleaser in CI and signed with Sigstore.

```sh
VERSION=0.5.0            # latest release: https://github.com/gsoultan/argus/releases/latest
ARCH=amd64               # or arm64
PKG=argus-control        # or argus-agent, argus-verify
FMT=deb                  # deb (Debian/Ubuntu), rpm (RHEL/Rocky/Alma), apk (Alpine)
BASE=https://github.com/gsoultan/argus/releases/download/v$VERSION

curl -fsSLO "$BASE/${PKG}_${VERSION}_linux_${ARCH}.${FMT}"
curl -fsSLO "$BASE/checksums.txt"
curl -fsSLO "$BASE/checksums.txt.sig"
curl -fsSLO "$BASE/checksums.txt.pem"
```

Check the download ([Verifying a download](#verifying-a-download)), then:

```sh
sudo apt install "./${PKG}_${VERSION}_linux_${ARCH}.deb"      # Debian / Ubuntu
sudo dnf install "./${PKG}_${VERSION}_linux_${ARCH}.rpm"      # RHEL / Rocky / Alma
sudo apk add --allow-untrusted "./${PKG}_${VERSION}_linux_${ARCH}.apk"   # Alpine
```

`apt install ./file.deb` and `dnf install ./file.rpm` resolve dependencies,
where `dpkg -i` and `rpm -i` do not. Install the control plane together with
`argus-verify` in one command, as [docs/install.md](docs/install.md) does, so
both land at the same version.

macOS and Windows get archives rather than packages:
`argus_<version>_darwin_<arch>.tar.gz` (control plane, verifier and the macOS
agent with its launchd job) and `argus_<version>_windows_<arch>.zip` (control
plane, verifier and the Windows agent, which installs itself with
`argus-agent service install`). Nothing else installs a service from them. To check recordings
from a workstation, `argus-verify` from the archive is all you need.

### First run: `/setup`

**Nothing starts on install.** You need a PostgreSQL 13+ database and a role
that owns it; then:

```sh
sudo systemctl enable --now argus-control
sudo cat /var/lib/argus-control/setup-token
```

Open `https://<that host>:8080`. With no configuration, the control plane makes
its own CA and certificate, serves only its setup page, and sends every address
to `/setup` (the API answers 503 until setup is done). The page asks for:

1. **The setup token** from the host. It proves you have the host, and works once.
2. **The database**, with **Test connection**. A database that already holds an
   Argus deployment keeps its accounts.
3. **Addresses**: where people open the console, where servers reach the
   control plane if that differs (behind a Cloudflare Tunnel, for one), the
   port, and whether agents may use QUIC.
4. **Certificate**: the control plane's own, or yours.
5. **The first administrator**.

**Finish setup** writes the configuration, its secrets and certificates, runs
migrations and starts properly in the same process. `control.installed` and
`user.created` are the first entries in the audit log. There is no YAML or env
file to edit, and no default account: nothing is seeded, because a product that
ships a default administrator has shipped a backdoor.

### Settings

After setup, everything is on the console's **Settings** page, for owners and
administrators. Pick a section on the left; each fits on one screen:

| Section | Holds |
|---|---|
| SSH channels, Recording, Hosts and trust | the session policy |
| Addresses | console and fleet addresses, listeners, QUIC, CORS origins |
| Sign-in and limits | session and ticket lifetimes, agent silence, rate limits, trusted proxies, log level |
| Database | where PostgreSQL is, with a connection test |
| Certificate | what is served, upload a replacement, download the CA |
| Object storage | the S3-compatible bucket recordings are copied to |
| Session alerts | email, Telegram and Firebase push |
| People | add, disable and enable accounts |

Edits in several sections wait for one **Save and apply**. Secrets are
write-only. The save is checked with the same checks start-up runs, then the
control plane reloads in place within a few seconds; if it cannot start on the
new settings, it puts the previous ones back and says why. Every save is a
`settings.changed` entry in the audit log.

Where things live, for backups and the curious:

| Path | Holds |
|---|---|
| `/var/lib/argus-control/control.yaml` | the configuration, written by setup and Settings |
| `/var/lib/argus-control/secrets/` | database URL, signing secret, reporter token, alert credentials; one owner-only file each |
| `/var/lib/argus-control/tls/` | the control plane's own CA and server certificate, renewed automatically |
| `/var/lib/argus/` | on each server: the agent's state and recordings, root's only |

A control plane installed before setup moved into the console keeps reading
`/etc/argus/control.yaml` until the first save from Settings moves it across.
The commands on the host (`users list|add|disable`, `audit verify`,
`backup run|verify`) find the configuration themselves.

### Enrolling servers

Admins add servers from **Servers > Manage > Add a server**, which hands you
the one command to run on each one:

```sh
sudo apt install ./argus-agent_<version>_linux_amd64.deb
sudo argus-agent enroll --control-url https://argus.example.com:8080 \
    --pin sha256/... --token argus-enrol-...      # copied from the console
sudo systemctl enable --now argus-agent
sudo argus-agent install    # prints the sshd_config lines; add them, then reload sshd
```

The token works once and expires within the hour; the pin means the first
connection cannot be intercepted. The server then shows its tunnel as
**Connected** (QUIC, with the round trip) or **Connected over HTTPS**. Open
`8080/udp` on the control plane if you can; QUIC is smoother on a lossy
network. macOS servers work the same way with the agent from the darwin archive;
see [docs/install.md](docs/install.md#macos-servers).

To take the agent off a server, run `sudo argus-agent uninstall` there before
removing the package; see [docs/install.md](docs/install.md#uninstalling-the-agent).
Uninstalling leaves `/var/lib/argus` and `/var/lib/argus-control` in place.
Removing the software must never destroy the evidence it produced.

### What the packages set up

A system account `argus` with `nologin`, used by the control plane. The **agent
runs as root**, deliberately: sessions run as whoever connected, so if the
daemon were not root the recorded user could edit the record of their own
session. Recordings are `0700 root:root` for that reason.

The systemd units are hardened, not merely present: `ProtectSystem=strict`, an
empty `CapabilityBoundingSet` where none is needed, and a syscall filter. The
agent keeps three capabilities: `CAP_DAC_READ_SEARCH`, because its posture scan
reads other users' `authorized_keys`, and `CAP_BPF` with `CAP_PERFMON`, which
load the probe that records commands as the kernel saw them. Its filter returns
`EPERM` rather than killing the agent, because a dead agent makes sshd refuse
every login on the host.

### Backups

`argus-backup.timer` takes a `pg_dump`, a checksum manifest and, with object
storage, an incremental mirror of recordings, after checking the audit chain is
intact. `sudo argus-control backup verify -scratch-db <url> <dir>` proves a
backup by restoring it into a scratch database. See
[docs/install.md](docs/install.md#4-backups).

### Verifying a download

Every release signs `checksums.txt` with Sigstore keyless signing from the
release workflow, so a package can be traced to this repository's CI rather
than to whoever handed it over. With [cosign](https://docs.sigstore.dev/cosign/system_config/installation/),
in the directory holding the downloads:

```sh
cosign verify-blob checksums.txt \
  --signature checksums.txt.sig --certificate checksums.txt.pem \
  --certificate-identity-regexp 'https://github.com/gsoultan/argus/.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
sha256sum -c checksums.txt --ignore-missing
```

Both must succeed. A package that fails either is not ours. On macOS use
`shasum -a 256 -c checksums.txt --ignore-missing`; in PowerShell compare
`Get-FileHash <file>` against its line in `checksums.txt`.

## Reaching the console through a Cloudflare Tunnel

The console can be published through a
[Cloudflare Tunnel](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/),
so people reach `https://argus.example.com` with no inbound port open on the
control plane. Terminals, file transfers and desktops are WebSockets to the
control plane, so they go through the tunnel too and need nothing else
published.

Agents cannot use the tunnel: their connection is QUIC (UDP) or a protocol
upgrade Cloudflare does not forward, and their reports need a client
certificate that Cloudflare's TLS termination would strip. So the control plane
gives the two audiences separate listeners:

| Setting | Value |
|---|---|
| Addresses > Listen | `:8080`, for agents (TLS and QUIC); keep it reachable from your servers only |
| Addresses > Console listen | `127.0.0.1:8090`, for cloudflared |
| Addresses > Console address | `https://argus.example.com` |
| Addresses > Address servers use | `https://argus-internal.example.com:8080` |
| Sign-in and limits > Trusted proxies | `127.0.0.1/32` and `::1/128`, so the real address comes from CF-Connecting-IP |

A fresh install's `/setup` page works through the tunnel and still needs the
token from the host. The full cloudflared config, the checks and Cloudflare
Access notes are in
[docs/install.md](docs/install.md#through-a-cloudflare-tunnel).

## Servers, assignments and roles

The sidebar has **Overview, Servers, Sessions, Access requests**, and a folded
**Admin** group (Coverage, Audit log, Users & roles, Settings). **Servers** has
two views:

- **Connect**, the default for everyone: a card per server you may use.
- **Manage**, for admins, owners and auditors: the inventory table, Coverage,
  **Add a server** (with a count waiting for approval) and **Add by hand**. It is
  in the URL as `?view=manage`, and any inventory filter opens it. Operators
  only ever get Connect.

| | Servers page | Add, edit, retire | Assign | Connect | Sessions & recordings |
| :--- | :--- | :--- | :--- | :--- | :--- |
| owner, admin | Connect and Manage, the fleet | yes | yes | any server | all |
| auditor | Manage, the fleet | no | no | no sessions at all | all |
| operator | Connect, assigned servers only | no | no | assigned servers | their own |

**Nobody can reach a server until someone is assigned it**, not even the person
who added it. Open **Who can reach** on the server, pick an account and the
principals they may log in as. A server's principal list is the authority, not
its `/etc/passwd`, and an assignment can never exceed it. Assignment is
permission to ask, not to have: an elevated principal still needs an approved
access request on the day, and an approved request is also how someone reaches
a server they were never assigned, narrower than an assignment and expiring.

Admins and owners are exempt from assignment, for the same reason they are
exempt from the approval chain: someone must be able to act when the chain is
broken. Their sessions are recorded and flagged like everyone else's. Someone
else's session answers 404 rather than 403, because a 403 confirms the id is a
real session.

Argus will not guess initial assignments from who used what before; that would
be the product deciding who is authorised from past behaviour, which is what it
exists to stop.

### Signing in

Argus holds its own accounts; there is no identity provider to stand up. The
first administrator comes from `/setup`; add more under **Settings > People**, or
on the host with `sudo argus-control users add you@example.com --role admin`
(the password is read without echo). Sign in, then enrol a second factor: any
TOTP authenticator works, and enrolment issues ten single-use recovery codes,
shown once and stored only as digests.

`user_tokens:` is a bootstrap hatch that skips password, second factor and role,
so the control plane refuses to start when it is set alongside any local
account.

## Verifying a recording: `argus-verify`

`argus-verify` checks that a session recording has not been altered since it was
sealed. It needs no configuration, no network and no running Argus: it reads the
`.cast` file, recomputes its hash chain from the bytes on disk and compares the
result with the chain head published when the session ended. An auditor can
verify a recording without trusting Argus, its storage, or whoever handed the
file over.

Install the `argus-verify` package on Linux, or take the binary from the macOS
or Windows archive. From the session's page in the console, take the recording
(**Export .cast**, the bytes exactly as stored) and the **Chain head** under
*Recording integrity*, then:

```sh
argus-verify session.cast <chain-head>
```

```
INTACT   session.cast
  lines  1842
  head   9f2c…
```

Any byte changed, removed or appended and the heads no longer match:

```
TAMPERED session.cast
  lines     1842
  expected  9f2c…
  computed  41ab…
```

| Flag | Does |
|---|---|
| `--head` | print the computed chain head and exit |
| `--quiet` | print nothing; rely on the exit status |
| `--version` | print the version and exit |

Exit status is `0` intact, `1` tampered, `2` usage error or unreadable file, so
it drops into a compliance pipeline:

```sh
argus-verify --quiet session.cast "$HEAD" || alert "recording $ID failed verification"
```

Two related checks need the database and so live in `argus-control`:
`sudo argus-control audit verify` recomputes the audit log's chain and reports
where it breaks, and `backup verify` checks a backup end to end.

## Security posture

- **The record leaves the server as it happens.** The agent streams recording
  chunks and their chain heads to the control plane during the session.
  Anything already sent cannot be rewritten; root on a server can at most stop
  future recording, and a stream that stops mid-session shows in the console as
  a gap, never as a clean session.
- **No standing credential on the server.** For SSH the agent is root and starts
  the account's shell directly, so there is no key to place, rotate or lose.
- **Grants are checked twice**, by the control plane and by the agent, and the
  agent pins the grant key it first receives.
- **Private keys must be owner-only** (mode `0600`, or the Windows ACL
  equivalent); a readable one is refused at start-up with the fix in the message.
- **Failed logins are budgeted separately** from logins, and a 429 says nothing
  about whether anything supplied was valid.
- **Command detection from terminal output is an audit aid, not a boundary.**
  The eBPF probe gives kernel-observed `execve` evidence, the console labels
  which one produced a timeline, and a session claiming eBPF whose recording
  has no kernel events is flagged. macOS has no kernel tracing, and says so per
  session.
- **No object storage** is warned about at start-up: without it the audit chain
  and recordings exist in one database on one host.
- **`/stats`** (goroutines, heap, session count, pool size) answers loopback
  callers only, and a request carrying proxy forwarding headers is not treated
  as loopback. It is deliberately not `pprof`: a heap dump of a process relaying
  sessions is a transcript of everyone's privileged work.

## Measured capacity

One control plane relayed 1000 browser terminals at once to 50 agents, and all
1000 opened, ran and sealed their recordings. It cost about 110 KB of heap and
five goroutines per open terminal, and a keystroke's answer came back in 1–2 ms
at the median. One agent takes up to 64 sessions and file transfers at once.
On the HTTPS fallback one stream carries about 6 MB/s at an 80 ms round trip;
QUIC carries more. The method, the full tables and what was not measured are in
[docs/capacity.md](docs/capacity.md).

## Not done yet

What the design in front of this page does not cover yet, stated rather than
implied:

- **Direct logins on Windows.** The Windows agent opens and records sessions
  started in the console, but does not hook OpenSSH for Windows yet: a direct
  login to its sshd is neither refused nor recorded. Stop that service if you
  do not need it.
- **"Which logins are recorded"** is still on each server's page, but every
  direct login is now refused before it applies, so it has no effect.
- **Faster transports.** WebRTC and direct browser-to-agent paths for the
  terminal and desktop are designed but not built; today the control plane
  relays every session. The [measurements](docs/capacity.md) do not justify
  them yet: the relay adds about a millisecond to a keystroke and little CPU,
  so a direct path would only save distance, which wants a measurement from a
  deployment where the control plane is far from its users.

## Running locally

```bash
./scripts/setup.sh    # once
./scripts/dev.sh      # every day
```

Everything is a plain shell script under `scripts/`. `dev.sh` checks tool
versions, resolves stale port holders, starts Postgres and MinIO, and streams
every service into one prefixed log; Ctrl-C stops everything. The `Makefile` is
a thin wrapper (`make dev`, `make check`, …) over the scripts.

| | |
|---|---|
| Web UI | http://localhost:5290 |
| Control plane API | http://localhost:8480 |

Without a control plane configured, the web UI runs against an in-memory mock
API (`web/src/lib/api.ts`), so it is usable on its own.

To sign in against the dev control plane, make an account (nothing is seeded):

```bash
set -a; . dev/secrets.env; set +a
go run ./cmd/argus-control -config dev/control.yaml \
  users add you@example.com --role admin
```

`-config` goes **before** the subcommand (after it you get
`unknown flag "-config"`), and `dev/secrets.env` must be loaded first or the
config cannot resolve its `${VAR}` references. If the console says the control
plane could not be reached, it is down or the console points at the wrong
address; `dev.sh` names the cause on start-up.

| Script | Does |
|---|---|
| `scripts/setup.sh` | First-time setup: verify toolchain, install dependencies |
| `scripts/dev.sh` | Run every service present locally (`web`, `control`, …; `--force`, `--clean`) |
| `scripts/deps.sh` | Postgres and MinIO: `up`, `down`, `reset`, `status`, `logs` |
| `scripts/check.sh` | Typecheck, vet, test: what CI runs |
| `scripts/build.sh` | Production build |

Each takes `--help`. Requirements: **Bun** ≥ 1.4 (Node is not needed), **Go** ≥
1.25, and **Apple `container`** or Docker for Postgres and MinIO
(`brew install --cask container`, then `container system start`).
`deps.sh` handles two Apple-container quirks: `PGDATA` must be a subdirectory of
the volume mount, and containers cannot resolve each other by name.

To add a service to `dev.sh`, add a line to its `SERVICES` registry
(`name | port | colour | working dir | detect path | command`); it starts only
when its detect path exists.

### Tests

```bash
./scripts/deps.sh up
ARGUS_TEST_DATABASE_URL="postgres://argus:argus@localhost:5433/argus?sslmode=disable" go test ./...
cd web && bun run test && bun run e2e     # unit, then a real browser
```

The browser suite builds the console, serves it as deployed, and checks every
route with zero console errors, the audit chain verifying in WASM, and that
replaying a large terminal or desktop recording leaves the main-thread heap
flat.

### Layout

```
cmd/argus-control     control plane and console server
cmd/argus-agent       the agent on each server
cmd/argus-verify      standalone recording verifier
internal/             tunnel, agent, control, recorder, hashchain, …
web/                  React 19 · Mantine 9 · TanStack Router/Query/Form · Vite 8
docs/install.md       the production install
packaging/            systemd units, launchd job, packaged config
scripts/              setup · dev · deps · check · build
```
