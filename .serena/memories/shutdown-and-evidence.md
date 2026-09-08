# Shutting down without losing the evidence

`systemctl restart` is the most common operation anyone performs on this
product, and it was destroying recordings in all three services. Fixed
2026-09-07. The bug family below recurred five times; check for it in any new
long-running loop.

## The shape

Four faults that chain together. Finding one hides the next.

1. **An unbounded drain.** `Close()` did `wg.Wait()` with no deadline. A
   session that does not end on its own -- an idle shell, an attached shim, an
   open desktop -- holds the shutdown until systemd sends SIGKILL.
2. **SIGKILL means nothing is sealed.** `Session.Close` computes the chain head
   and uploads. Killed, the recording sits on disk with no head, never
   uploaded, its row `active` forever. Present but unverifiable, which for
   evidence is the same as absent.
3. **main returned through its own drain.** Closing the listener is the *first*
   thing a shutdown does, so `Listen`/`ListenAndServe` returns immediately. If
   main returns on that, the process exits while the drain is still running.
   Fix: run the listener on its own goroutine and `select` on it against the
   signal, so a bind failure and a shutdown stay distinguishable.
4. **Terminate closed one end of two.** The connection to the *target* and the
   *operator's own* are different transports. Closing only the target leaves
   the operator attached to a dead shell and leaves the handler blocked on a
   socket nobody will close. Both must go. This applied to SSH `Terminate`,
   RDP `disconnect`, and the agent's shim connections.

Plus: **detached report goroutines**. Every `Reporter.Session`/`Reporter.Audit`
call was `go func(){...}()` with nothing tracking it, so the last thing a
session said about itself was lost at exit. They are on `Server.reports` now and
shutdown waits for them, bounded.

## The constants

`DefaultDrain` 30s, `sealGrace` 10s, `reportGrace` 10s. Both listeners drain
**concurrently** -- sequentially they add to 80s, past `TimeoutStopSec`, which
reintroduces the SIGKILL. All three units now state `TimeoutStopSec` explicitly
(75s gateway, 75s agent, 45s control) so a distro default cannot undo this.

## The control plane's milder version

`http.Server.Shutdown` drains, but nothing waited for `Shutdown` itself. A
gateway mid-report had its connection cut. Spooled and retried, so nothing was
lost for good -- but a "graceful" shutdown was dropping work it had promised.

## What made all of this reachable in the record

`control.Session` had no `terminatedBy`/`terminationReason`, `decode()` rejects
unknown fields by design, and the gateway has always sent both -- so **every
terminated session's report came back 400**. Not the fields, the whole report.
Migration 009 adds them, COALESCEd on upsert so a later report cannot erase why
a session ended. Both SSH and RDP now report state `terminated` rather than
`closed`; reporting both the same made an administrative terminate
indistinguishable from a logout.

## Tests that pass against the bug

Two traps hit while writing these:

- **`net.Pipe` blocks a write until the far end reads**, so a deadline error is
  indistinguishable from a closed connection. Use real loopback sockets, and
  assert on a *read* from the far end.
- **A shutdown that returns is not a shutdown that worked.** If the drain gives
  up and the grace expires, `Close` still returns. Bound how long it may take,
  or the test passes on a gateway that sealed nothing.

Also: a unix socket path is capped near 104 bytes and `t.TempDir()` on macOS
is long enough to break it on its own. Symptom is a bare "invalid argument" on
dial. Use `os.MkdirTemp("/tmp", ...)`.

## Measured

Soak: 3650 sessions through one continuously running gateway, sampled at idle
between waves. Drift +2.3 goroutines, +0.39 MB heap -- noise. See
[[capacity-and-drills]] and [[upgrades-and-bootstrap-auth]].

`GET /stats` on both servers (loopback only) is what makes this measurable:
goroutines, live heap, heap objects, session count, DB pool. Deliberately not
`net/http/pprof` -- the gateway holds session plaintext, so a heap dump of it is
a transcript of everyone's privileged work including typed passwords. A test
asserts every field stays a number so it cannot grow one that carries content.


## Controls that were claimed but not enforced

Two found 2026-09-07 by testing the claim rather than reading the code. Both had
the same shape: **the control existed on one path and not on the one customers
use.**

**Elevated principals needed no approval over SSH.** `handleTicket` enforced it
for browser terminals; the SSH gateway checked authorized_keys and the asset's
principal list, then dialled the target as root. It looked like a refusal on the
dev fleet only because pay-01 has no root key installed -- Argus never declined.
The gateway now asks the control plane (`GET /api/v1/report/authorize`, reporter
mTLS) and fails closed. The elevated list ships with the policy sync so the two
services cannot disagree; an absent list falls back to the default, never to
empty.

**Installing the agent broke SFTP on the host.** sshd applies ForceCommand to
subsystem requests, handing the shim `SSH_ORIGINAL_COMMAND=internal-sftp`, which
is compiled into sshd and is not a program. Every SFTP and SCP transfer to a
managed host failed silently. The file-transfer auditing had only ever been
demonstrated on ca-01, which has no agent -- the one configuration that could
still perform a transfer.

**The lesson worth keeping:** when a feature is demonstrated, check which host
configuration it was demonstrated on. Both bugs survived because the demo used
the path that still worked.


## The strict decoder killed three features

`decode()` used `DisallowUnknownFields`, and reporters are deployed separately
from the control plane. A newer sender lost **the whole message**, not the
field. Invisible from both ends: the sender spools and retries forever, the
receiver logs one bad request among many.

- `terminatedBy` on session reports → every administrative terminate unrecorded
- `exec_tracing` on heartbeats → every agent stale for seven days, taking
  session counts, posture and drift with it
- and so the probe state `RequireEbpfForRoot` needs could never arrive, which is
  why that switch was unenforceable

`decodeReport` (internal/control/decode_report.go) keeps what it understands and
names what it does not, once per endpoint+field-set. **The console keeps the
strict decoder** — it ships with this build, so an unknown field there is a typo,
not skew.

**If you add a field to any reporter payload, add it to the receiver too.** The
warning now tells you, but only if someone reads the log.

## Auditing the Settings page against the code

Six of eight switches were enforced. The two that were not:

- `EncryptRecordingsSeparateKey` — **nothing encrypted anything.** Recordings
  went to object storage as plaintext with no `ServerSideEncryption` option. The
  switch defaulted on and promised the opposite. Removed in migration 010;
  implementing it needs an answer to key custody and key loss (lose it and every
  recording, including backups, is gone).
- `RequireEbpfForRoot` — now enforced via the authorize endpoint, refusing an
  elevated session unless the host's agent reports a loaded probe, and
  surfacing the agent's own reason.

**Check any new policy toggle has a reader before it has a switch.**

## Environment hazard on this machine

`panmail-dev-postgres` also publishes host port **5433**, colliding with
`argus-postgres`. `localhost:5433` may authenticate against another project's
database. Use the container IP (`192.168.65.8:5432`) for anything that must be
certain. Apple `container` networking also stalls intermittently, which produced
`dial tcp ...: i/o timeout` refusals during the soak and cost two wrong
diagnoses.
