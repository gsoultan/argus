# Agent tunnel

Step 2 of the no-gateway plan (`/mnt/project-files/plans/no-gateway-architecture.md`
in the project). Each enrolled agent holds one outbound connection to
argus-control; the control plane opens streams on it and names a *service*,
never an address. Code: `internal/tunnel` (wire format, both transports),
`internal/agent/tunnel.go` (reconnect loop, service table),
`internal/control/tunnel.go` (hub, admission, ping).

## Transports

- **QUIC** on `tunnel.quic_listen` (UDP, ALPN `argus-tunnel/1`). The agent's
  first stream is a `Hello` with its credential.
- **HTTPS fallback**: `GET /api/v1/agent/tunnel` with `Upgrade: argus-tunnel/1`,
  hijacked into yamux. Always on, on `listen`. The 101 advertises the QUIC port;
  the agent then tries QUIC in the background and moves when it works.
  QUIC failing parks the agent on HTTPS for 10 minutes before trying again.
  yamux's stream window is 1 MB (`yamuxWindow`), not the 256 KB default: a
  stream carries one window per round trip. Capacity: `docs/capacity.md`.

## Rules

- Admitted only on a live **per-host** credential, checked against the database
  (not the 30s report cache). The shared reporter token never opens a tunnel.
  Hostname claimed must match the credential's.
- One tunnel per credential; the newest replaces the oldest.
- Every 30s the hub pings each tunnel and, in the same round trip to Postgres,
  re-checks the credential (`tunnelSeen`). A revocation on another replica ends
  a tunnel here within one ping. A DB error keeps the tunnel (don't take the
  fleet down because Postgres blinked).
- The agent refuses unknown service names and bounds concurrent streams (64).
  It offers `ping` and `terminal`.
- State lives on `agent_credentials.tunnel_*` (migration 018). A row whose
  `tunnel_seen_at` is older than three pings is listed as down: the replica
  holding it may be gone. Freshness is judged with the database's clock.

## Sessions (`terminal` service, step 3)

`GET /api/v1/terminal?target&principal&ticket` (WebSocket, `control/terminal.go`)
redeems the console's ordinary terminal ticket, then signs a **grant**
(`tunnel/grant.go`): Ed25519, key derived by HKDF from `signing_secret` (no new
config, same key on every replica), for one host, one account, one email, 60s,
single use. The agent verifies it itself -- the HTTPS fallback may cross a
TLS-terminating proxy, and a bug in the relay must not be enough to open a
shell. Agent pins the first key it sees in `/var/lib/argus/grant-key.pub`
(`agent/grant_pin.go`); a different key later refuses sessions, it does not
re-pin.

The agent then (`agent/terminal.go`, `terminal_spawn.go`):
- allocates a pty, starts a **held** launcher (`read < fifo; exec shell -l`) so
  the pid exists for kernel tracing but nothing has run;
- opens the recording via `Collector.StartRecording` (same file, chain,
  kernel tracing and report as an sshd login; `SessionStart.Actor` is the
  grant's email and becomes the reported `userEmail`); refuses if that fails;
- releases the launcher, answers with the session id, relays binary frames
  (`tunnel/term.go`: `i` input, `r` resize, `o` output, `x` exit+chain head).
  Output is recorded before it is sent.

**Why systemd-run.** The agent's unit is sandboxed (read-only /home and /usr,
private /tmp, NoNewPrivileges, three capabilities). A shell started as its child
inherits all of it -- root could not install a package, sudo fails. So on a
systemd host (`SpawnAuto` as root) the shell runs in a transient
`argus-session-*.service`: `--uid`, `PAMName=login`, `TTYPath=` our pty slave,
`--wait` for the exit status. Elsewhere (and in tests) a direct child.
The CI drill proves it by writing to the account's home.

**rabbitmqctl runs as the rabbitmq account (2026-11).** The packaged
`/usr/sbin/rabbitmqctl` is a wrapper that, started by root, does `su rabbitmq`.
su needs CAP_SETGID/CAP_SETUID, which the unit does not keep, so every RabbitMQ
session and vhost listing failed with "su: cannot set groups: Operation not
permitted". `agent/rabbitmqctl_unix.go` starts it as the broker's account
instead: `systemd-run --pipe --wait --uid=rabbitmq` as root on systemd, a direct
child with credentials as root elsewhere, unchanged when there is no rabbitmq
account or the agent is not root. Do not fix this by giving the unit
SETUID/SETGID: that hands every agent child the means to become anyone.

Browser protocol is the gateway's (JSON control messages) except output is
**binary** WebSocket messages; `LiveTerminal` handles both. The console picks
the agent path when `asset.tunnelConnected` (EXISTS on a fresh, unrevoked
credential by hostname, 90s = `tunnelFreshFor`, asserted in a test).

Not yet: the recording still reaches the control plane only at the end (live
chunk streaming is next), and a tunnel held by another replica is "not
connected" from this one.

## Held terminals (2026-11)

A terminal outlives the stream between control and agent. The control plane
opens `terminal` with `Header.Hold{ForS, RunningS}` (reattach window 5m,
keep-running 4h; config `terminal_reattach_window`, `terminal_keep_running`);
an agent built with `TerminalConfig.Held` answers `Holds: true`. Then:

- A stream that drops without `FrameHangup` ('h') keeps the shell, still
  recorded; the agent keeps the last 128 KB of output and a byte count.
- `terminal.resume` (its own service name, so an old agent refuses it as
  unknown instead of opening a fresh shell) on a fresh terminal grant for the
  **same email and account** picks it up from `Resume.From` bytes; the answer
  says where it starts and whether output in between is gone (`Gap`). A
  replaced stream gets `FrameNotice{kind:"moved"}` first so the replica holding
  it does not take it straight back.
- Control reconnects by itself when the stream drops while the browser stays
  (`reconnectAgent`, notices `agent-reconnecting`/`agent-back` to the browser,
  typing dropped meanwhile); a browser coming back to a control plane that does
  not have the session (restart, other replica) goes through `resumeFromAgent`;
  `retry` (non-final) while the agent's tunnel is not back yet.
- When control's own window runs out it sends `FrameRelease` "1" and keeps the
  stream (so watch and terminate still work); the agent hangs up at once if the
  shell is at its prompt, otherwise when its foreground job ends
  (`foregroundBusy`: TIOCGPGRP vs TIOCGSID on the master, Linux; pid on other
  Unix; never on Windows) or RunningS runs out. `sudo -i` looks busy for its
  whole life, which errs toward keeping.
- The first `terminal` handler blocks for the shell's life, so the update gate
  (`openStreams`) counts held shells and waits for them; `terminal.resume` is
  neither gated nor counted.
- Agent drain (`Collector.OnDrain`) sends `FrameNotice{kind:"restarting",
  ends_in_s}`; control forwards notices as `{"type":"notice"}`. Control's
  shutdown tells browsers `control-restarting`.
- Console (`LiveTerminal`): session id in sessionStorage per account@host, so
  a reload or another page and back resumes; unmount no longer hangs up, only
  Disconnect (with a confirm) does; `beforeunload` asks while live.

Not yet: an admin cannot end a held session from the console when no replica
is relaying it (terminate goes through the relaying replica); desktops and
database sessions still end with their browser connection.

## macOS (step 6)

Same code, three seams (`internal/agent/paths_*.go`, `accounts*.go`,
`host_*.go`): paths under `/var/db/argus` and `/var/run/argus` (SIP makes
`/usr/libexec` read-only, so the shim is `/usr/local/bin/argus-agent`);
accounts from `dscl` turned into passwd lines by `dsclToPasswd`, so every
reader (posture, discovery, shim shell, tunnel lookup) goes through
`openPasswd` and they cannot disagree; machine id and OS from `ioreg` and
`sw_vers`. No systemd, so tunnel sessions are direct children of the root
launchd daemon (no sandbox to escape there, no PAM session). No kernel
tracing: execlog's non-Linux probe reports why. launchd job in
`packaging/launchd/`. The "Agent on macOS" CI job is the install drill.

## Files (`files` service)

`GET /api/v1/files?target&principal&ticket` (WebSocket, `control/files.go`)
redeems the same terminal ticket and signs a grant with `svc: "files"`.
`Grant.ForService` keeps the two apart: a grant with no `svc` is a terminal
(all an old control plane could sign), so a files grant never opens a shell
and a terminal grant never opens files.

The agent (`agent/files.go`) runs `argus-agent file-op list|download|upload`
per request **as the account** -- `systemd-run --pipe --uid` on a systemd host
for the same sandbox reason as the shell, a direct child with credentials
elsewhere. The agent never reads or writes a user's file as root. Uploads go
through a temp file and `link` (no clobber) or `rename` (overwrite).

Frames are the terminal's (`tunnel/files.go`): `q` request, `a` answer, `d`
data, `e` end of upload. One operation at a time; an error ends the operation,
not the stream. The control plane hashes the bytes itself and writes
`file.upload` / `file.download` with size and SHA-256, warning when the agent's
count disagrees. The console's browser is `components/FileBrowser.tsx`, offered
on Connect only when the agent's tunnel is up.

## Windows (step 7)

The terminal is split at `openShell` (`terminal_unix.go` / `terminal_windows.go`)
behind `terminalIO` (read/write/close/resize) and `shellProc`. Windows: a
ConPTY; the shell is created with `CreateProcessAsUser` on a token from an
**S4U logon** (`logon_windows.go`: `LsaRegisterLogonProcess` +
`LsaLogonUser`, MSV1_0, network logon type) -- needs SeTcb, so the agent must
run as LocalSystem. `lpDesktop = ""` so the account gets its own window
station. Created `CREATE_SUSPENDED` (held), resumed by `Release` once the
recording is open; every process in a kill-on-close job object. Profile via
`LoadUserProfileW`. Local accounts only: `splitAccount` refuses domain names
because nothing tests the Kerberos path. Accounts from `NetUserEnum`, as
passwd lines whose home has a drive colon -- hence `passwdFields`, which reads
home as everything up to the last colon. State dir protected by
`ProtectStateDir` (protected DACL, SYSTEM + Administrators), once per dir per
process because setting it walks the tree. The sshd shim is NOT ported:
`RunShim` refuses, `install` says so, posture's empty `ShimPath` no longer
matches every ForceCommand. Service via `x/sys/windows/svc`
(`platform_windows.go`), `argus-agent service install`, log at
`C:\ProgramData\Argus\agent.log`. CI: "Agent on Windows".

## Desktop, live view and recordings (step 7, gateway retired)

- **Remote Desktop**: service `"desktop"` (`internal/agent/desktop.go`), Windows
  only. Control (`internal/control/desktop.go`, `GET /api/v1/desktop`) reads a
  first `{"type":"logon",password,width,height}` message and passes it in the
  stream header (`tunnel.DesktopOpen`); the agent dials `127.0.0.1:3389` with
  CredSSP/NTLM (falls back to TLS-only unless the error is an auth failure),
  clears the password and records `<id>.argusrdp`. Flags:
  `no-credential-injection`, `legacy-credssp-binding`, `no-network-level-auth`.
- **Live view / terminate**: `internal/control/live_sessions.go` keeps a
  registry of sessions this replica is relaying; `/api/v1/sessions/{id}/watch`
  (shadow ticket) and `/terminate` (terminate ticket). Terminate goes to the
  agent as `FrameTerminate`. Only the replica relaying a session can serve it.
- **Recordings** reach storage via `PUT /api/v1/report/recording/{id}` from the
  agent's upload queue (`internal/agent/upload.go`), verified before storing.
- **sshd shim**: every login on the collector socket is forced to `Direct`;
  `--gateway-keys`, `--gateways`, `--skip-brokered` are accepted and ignored.
- Tickets are burned in `redeemed_tickets` (`Store.Redeem`), shared across
  control plane replicas.

## Screen sharing (Windows, no RDP)

Per-server `assets.desktop_mode` (migration 042): `rdp` (default) or
`screen`. The control plane, not the browser, picks the tunnel service from
it (`AssetDesktopMode`): `"desktop"` dials the host's RDP listener, `"screen"`
(`internal/agent/screen*.go`) shares the physical console. Separate service
names so a grant for one cannot open the other, and an old agent answers
"unknown service", which the console turns into "update the agent". No
password crosses in screen mode; it acts as whoever is signed in at the
console (the owner's choice, 2026-11-03), so the agent records the account as
a `console-account` meta event at open and on every change, flags the session
`console-takeover`, and the audit says so.

Mechanics: the service (session 0) starts `argus-agent screen-helper` in the
console session with its own SYSTEM token moved there (`TokenSessionId`), only
two pipes inherited (`PROC_THREAD_ATTRIBUTE_HANDLE_LIST`), kill-on-close job.
The helper locks one OS thread, follows `OpenInputDesktop` (sign-in and UAC
desktops too), captures with GDI BitBlt + cursor, diffs 64px tiles and encodes
them with `rdp.CompressRect` (24-bit interleaved RLE). The agent re-packs them
as Fast-Path bitmap PDUs (`rdp.FastPathBitmaps`) for the recording, so replay,
live view and the browser decoder are unchanged. Ctrl+Alt+Del becomes
`SendSAS`; the agent sets `SoftwareSASGeneration=1` only if the policy is
unset. A console session change (sign-out) gets a new helper. Session and
account are checked on a schedule independent of NextBitmaps calls: a
per-call timer never fired on a busy screen.
