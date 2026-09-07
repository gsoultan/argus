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
