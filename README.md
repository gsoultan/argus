# Argus

Privileged access management for Linux infrastructure. Brokered SSH with session
recording, credential injection, just-in-time approvals, and a tamper-evident
audit trail.

## Running locally

```bash
./scripts/setup.sh    # once
./scripts/dev.sh      # every day
```

Everything is a plain shell script under `scripts/` — no build tool, no task
runner, nothing to install. `dev.sh` checks tool versions, resolves stale port
holders, and streams every service into one prefixed log. Ctrl-C stops
everything, including grandchildren.

A `Makefile` exists as a thin wrapper (`make dev`, `make check`, …) if you
prefer it, but every target is a one-line delegation to the script — the
scripts are the interface.

The web UI runs against an in-memory mock API, so it is fully usable before any
backend exists — every function in `web/src/lib/api.ts` maps 1:1 to an endpoint
the Go control plane will serve.

| | |
|---|---|
| Web UI | http://localhost:5290 |
| Control plane API | http://localhost:8480 *(once built)* |
| SSH gateway | `localhost:2222` *(once built)* |

### The scripts

| Script | Does |
|---|---|
| `scripts/setup.sh` | First-time setup — verify toolchain, install dependencies |
| `scripts/dev.sh` | Run every service present locally |
| `scripts/deps.sh` | Postgres + MinIO lifecycle |
| `scripts/check.sh` | Typecheck, vet, test — what CI runs |
| `scripts/build.sh` | Production build |
| `scripts/lib.sh` | Shared helpers, sourced by the rest |

Each takes `--help`. Common flags:

```bash
./scripts/dev.sh web           # only named services
./scripts/dev.sh --force       # kill whatever holds a needed port
./scripts/dev.sh --clean       # reinstall web deps first
./scripts/check.sh web         # or: go
./scripts/build.sh web         # or: go
./scripts/deps.sh up|down|reset|status|logs
```

### Requirements

- **Bun** ≥ 1.4 — the only hard requirement for the web app
- **Node** — *not needed.* Bun runs Vite, `tsc` and the dev server; the whole
  toolchain is verified to work with no node on PATH. If you happen to have one
  installed, the scripts note its version but never depend on it.
- **Go** ≥ 1.25 — only once `cmd/` exists
- **Apple `container`** (or Docker) — only once the control plane exists, for
  Postgres and MinIO. `make deps-up` starts them.
  Install with `brew install --cask container`; start with `container system start`.

### Local dependencies

`scripts/deps.sh` owns Postgres and MinIO.

```bash
./scripts/deps.sh up       # create, start, wait until ready, ensure buckets
./scripts/deps.sh down     # stop, keep data
./scripts/deps.sh reset    # stop and delete all data
./scripts/deps.sh status
./scripts/deps.sh logs postgres
```

Two Apple-container specifics the script handles, both of which fail confusingly
if you hit them by hand:

- **`PGDATA` must be a subdirectory of the volume mount.** Apple container
  volumes are formatted filesystems containing `lost+found`, and `initdb`
  refuses to initialise into a non-empty directory.
- **There is no DNS for user-defined networks.** Containers cannot resolve each
  other by name, so the script addresses MinIO by IP. (`container system dns
  create` would fix it, but requires sudo.)

### Adding a service

`scripts/dev.sh` detects services rather than assuming them. Add one line to the
`SERVICES` registry:

```
name | port | colour | working dir | detect path | command
```

The `detect path` is what must exist for the service to be considered present,
so a half-built backend never breaks `make dev` for someone working on the UI.

## Installing

Linux only — the gateway and control plane are servers, and the agent has to
read another user's `authorized_keys` and own recordings the recorded user
cannot touch. None of that means anything elsewhere.

Releases publish `.deb`, `.rpm` and `.apk` for amd64 and arm64, built by
GoReleaser in CI. Three packages, because the agent goes on every managed host
while the gateway and control plane go on a handful of servers:

| Package | Installed on |
|---|---|
| `argus-agent` | every managed host |
| `argus-gateway` | the bastion servers |
| `argus-control` | the control plane servers |

```sh
sudo dpkg -i argus-agent_<version>_linux_amd64.deb    # Debian / Ubuntu
sudo rpm -i  argus-agent-<version>.linux-amd64.rpm    # RHEL / Rocky / Alma
sudo apk add --allow-untrusted argus-agent_<version>_linux_amd64.apk
```

**Nothing starts on install.** These services need certificates, secrets and an
inventory first, and a package that starts a half-configured privileged-access
gateway fails in the worst possible place. Configure, then:

```sh
sudo systemctl enable --now argus-agent
```

Uninstalling leaves `/var/lib/argus` in place. Removing the software must never
destroy the evidence it produced — that is precisely what someone covering
their tracks would reach for.

### What the packages set up

A system account `argus` with `nologin`, used by the gateway and control plane.
The **agent runs as root**, deliberately: the shim runs as whoever connected, so
if the daemon were not root the recorded user could edit the record of their own
session. Recordings are `0700 root:root` for that reason.

The systemd units are hardened rather than merely present — `ProtectSystem=strict`,
an empty `CapabilityBoundingSet` for the two that need none, and a syscall
filter. The agent keeps exactly one capability, `CAP_DAC_READ_SEARCH`, because
its posture scan reads other users' `authorized_keys`; it uses
`ProtectHome=read-only` rather than `yes` for the same reason.

### Verifying a download

```sh
cosign verify-blob checksums.txt \
  --signature checksums.txt.sig --certificate checksums.txt.pem \
  --certificate-identity-regexp 'https://github.com/gsoultan/argus/.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
sha256sum -c checksums.txt --ignore-missing
```

## Connecting

The target is encoded in the SSH username, so ordinary tooling works unchanged:

```sh
ssh  ops:pay-01@argus.example.com
sftp ops+pay-01@argus.example.com          # note the +
scp  report.csv ops+pay-01@argus.example.com:/tmp/
```

`sftp` and `scp` need `+` rather than `:`, because their own syntax is
`host:path` — they would read `ops:pay-01@gateway` as a path on a host called
`ops`. `/` works too.

SFTP sessions are decoded rather than relayed as opaque bytes, so a transfer
becomes an audit event naming the file and the volume moved:

```
notice   file.download   download /var/lib/payments/dump.sql (195.3 KB)
warning  file.delete     delete /tmp/uploaded-back.dat
```

## Layout

```
web/                  React 19 · Mantine 9 · TanStack Router/Query/Form · Vite 8
  src/types/domain.ts The wire contract the Go control plane implements
  src/lib/api.ts      Mock API — swap for fetch against argus-control
  src/workers/        Audit hash-chain verification, asciicast decoding
scripts/              setup · dev · deps · check · build (plain shell, no task runner)
Makefile              Optional thin wrapper over scripts/
```

## Measured capacity

One gateway, one target, one host. Every session in these runs did the whole
thing: TCP, handshake, publickey auth, PTY request, a command whose output was
checked, then a hold and a clean close. Reproduce with
`go run ./scripts/tools/loadtest` — read its header first, because the target's
own sshd will otherwise be the thing you measure.

| concurrent | succeeded | connect+auth p95 | session+PTY p95 | gateway RSS peak |
| ---------- | --------- | ---------------- | --------------- | ---------------- |
| 50         | 100%      | 19 ms            | 195 ms          | 30 MB            |
| 100        | 100%      | 20 ms            | 371 ms          | 40 MB            |
| 200        | 100%      | 36 ms            | 703 ms          | 50 MB            |
| 400        | 52%       | 31 ms            | 1274 ms         | 65 MB            |

200 concurrent sessions is the number to plan against. The 400-session row is
included because it is where the wheels come off, and because the wheels are
not the gateway's: the failures were the target refusing TCP connections, and
the gateway reported each one with the reason rather than hanging.

Across 1232 sessions the gateway sealed and uploaded 1232 recordings, panicked
zero times. Steady state is ~24 MB idle.

### Soak

5300 sessions in waves through one gateway over 90 minutes, with the numbers
read from `/stats` at idle between waves — after every session has torn down,
which is the only moment a leak is visible.

| | first half | second half |
| --- | --- | --- |
| goroutines at idle | 38.1 | 34.3 |
| heap in use at idle | 7.31 MB | 6.91 MB |

Both went **down**. A goroutine leaked per session would have shown +5300.

13 of 212 waves failed, every one because the gateway could not reach the
target over the development container network (`dial tcp: i/o timeout`). It
refused each session with that reason rather than hanging, which is the
behaviour worth having. Run this on a laptop and expect the same: the limit
you hit first is the environment.

### Two gateways at once

Two instances sharing one control plane, 100 concurrent sessions driven through
each at the same time: 200 of 200 succeeded, 200 session rows landed, every id
distinct, and the audit chain verified intact afterwards across both writers.

The state that has to be shared is shared. The second gateway starts with an
empty local pin file and gets its host-key pins from the control plane, so
trust-on-first-use cannot let it accept a host the first would refuse. A
terminal ticket redeemed on one gateway is refused by the other with 401.

Reproduce with `dev/argus2.yaml.example`.

These figures are from a development environment on one machine. They are a
floor to plan against, not a datasheet, and nobody has yet measured a fleet of
targets or a gateway under sustained multi-hour load.

### Checking a running process

Both servers report on themselves at `GET /stats`, over loopback only:

```sh
curl -sk https://127.0.0.1:8081/stats   # gateway
curl -sk https://127.0.0.1:8480/stats   # control plane
```

Goroutines, live heap, heap objects, session count, uptime — and for the control
plane, database pool size. A goroutine count that climbs while the session count
does not is a leak, stated directly; it is the failure that looks fine for a
week and then does not.

Counts only, and deliberately not `net/http/pprof`. The gateway holds session
plaintext — every keystroke and every byte of output for every live session — so
a heap dump of it is a transcript of everyone's privileged work, including
anything typed that was a password. That is not a debugging endpoint. Loopback
is enforced in the handler rather than by the listen address, and a caller from
anywhere else gets 404 rather than 403, so the endpoint is not advertised to
someone who may not use it.

## Security posture

Argus terminates SSH, which means the gateway holds session plaintext in memory.
That is the deliberate cost of working against hosts with no agent installed.
Treat gateway nodes as the highest-value asset in the fleet: no shared tenancy,
hardware-backed keys for the vault, and the audit log replicated somewhere the
gateway cannot write.

PTY-derived command detection is an audit aid, not a security boundary — a user
can obscure intent with base64 or a script whose body never reaches the screen.
The eBPF agent tier provides kernel-observed `execve` evidence. The UI labels
which one produced any given timeline and never presents them as equivalent.

## Signing in

Argus holds its own accounts. There is no identity provider to stand up and
nothing external to configure -- create the first one on the host and sign in:

```bash
argus-control users add you@example.com --role admin
```

The password is read from the terminal without echo, so it does not reach shell
history or a process list. Nothing is seeded: a product that ships a default
administrator with a known password has shipped a backdoor.

Then open the console, sign in, and enrol a second factor. Any authenticator app
works -- the codes are ordinary TOTP. Enrolment issues ten single-use recovery
codes, shown once and stored only as digests, for the phone that ends up in a
river.

SSO remains available for deployments that want it. Set `oidc:` in the control
plane's config and the sign-in screen offers both; leave it out and it offers
the password form alone. Neither requires the other.

`user_tokens:` is a bootstrap hatch for a deployment that has no other way in
yet, and it is the only thing here that skips the password, the second factor
and the role. The control plane therefore **refuses to start** when it is set
alongside an identity provider or any local account. Where it does apply, the
role comes from the named account rather than from holding the string.

## Controls an operator should know exist

The console explains each of these where it appears; this is the list.

- **Gateway policy** is held by the control plane and enforced by the gateway.
  Every SSH forwarding channel is closed by default and every recording
  guarantee is on. Only an owner or admin can change it, every change is
  audited with what moved and why it matters, and a session keeps the policy it
  started under — tightening policy does not stop a session in progress;
  terminating it does. A gateway that cannot reach the control plane keeps the
  policy it has, in either direction.
- **Port, agent and X11 forwarding** are implemented, not merely permitted.
  Local forwards dial from the *target*, so `-L` reaches what the target can
  reach and nothing more. A tunnel's contents are not recorded; its endpoints,
  duration and bytes each way go to the audit chain. Both are bounded per
  session (32 local, 8 remote).
- **Private keys must be mode 0600.** The gateway host key, the CA key, injected
  credentials and every TLS key are refused at load if group or others can
  read them. The process will not start; the message says `chmod 600`.
- **Failed logins are budgeted separately from logins.** `failures_per_hour`
  is charged only by actual failures and refused before any work is done. A
  429 says nothing about whether anything supplied was valid.
- **Browser terminal tickets** are single-use, bound to one target and
  principal, and expire in about a minute — which is what makes a credential
  in a WebSocket query string acceptable.
- **Recording fidelity is checked against the artefact.** A session reported
  as eBPF whose recording carries no kernel events is flagged in the console;
  the badge alone reads the claim, not the bytes.
- **No object storage** is warned about loudly at startup: without it the
  audit chain and every recording exist only in one database on one host.

### Verifying it yourself

```bash
./scripts/deps.sh up
ARGUS_TEST_DATABASE_URL="postgres://argus:argus@localhost:5433/argus?sslmode=disable" go test ./...
cd web && bun run test && bun run e2e     # unit, then a real browser
```

The browser suite builds the console, serves it as deployed, and asserts on
every route with zero console errors, the fonts actually loading, the audit
chain verifying in WASM, and — the reason it exists — that replaying a large
terminal or desktop recording leaves the main-thread heap flat.
