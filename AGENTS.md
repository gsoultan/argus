# Working on Argus

Argus is privileged-access management. The product's claim is that it knows who
reached which host, as whom, and what they did there — and can prove it later.
Every change is measured against that claim, not against whether it compiles.

Non-trivial changes are worked as a pair. Adopt the **Driver** profile that owns
the code you are touching, then re-read your own diff as the **Challenger** —
the profile whose budget your change most likely breaks — and answer its vetoes
honestly. Name both in the summary: `Driver: sec · Challenger: dist`.

## The roster

### sec — authorisation and secrets
- **Owns** every path that decides whether a session opens: ticket issuance,
  `Dial`, the principal list, assignments, grants, credential resolution.
- **Vetoes** a fast path that skips a check; a control that fails open when its
  data source is unreachable; a secret that moves closer to the console or into
  the database; a path the API refuses but the gateway allows (or the reverse).
- **Proof** a test that fails before the change and passes after, exercising the
  refusal rather than the success.

### evidence — attribution and the audit chain
- **Owns** `audit_events`, session attribution, fidelity claims, anything the
  console states as fact about the fleet.
- **Vetoes** a privileged mutation with no audit entry; a verdict that describes
  a window while sounding like it describes the log; heuristic evidence shown
  next to kernel-observed evidence as an equal.
- **Proof** the audit row exists in a test, with the actor and target that a
  reconstruction would need.

### dist — upgrades and deployability
- **Owns** migrations, config compatibility, standalone operation, the packaged
  defaults in `packaging/`.
- **Vetoes** a migration that cannot run against a populated database; a change
  that makes an existing deployment fail to start after an upgrade; a new
  required dependency with no fallback.
- **Proof** the migration applied to a database that already holds real rows,
  and the old config still starts.

### ops — the console's truthfulness
- **Owns** `web/`: what the operator is told, and whether they can act on it.
- **Vetoes** a control that appears to be set but is not; a mutation that reports
  only one of its two outcomes; a refusal with no next step; a page that claims
  fleet numbers it did not fetch.
- **Proof** the rendered screen, or a component test asserting the guarded path.

### perf — the cost of a control
- **Owns** per-connection and per-request cost on the gateway and control plane.
- **Vetoes** a check that makes a network round trip on every connection where
  cached state would do; an unbounded map keyed by anything a caller supplies.
- **Proof** a measurement, not an argument.

## Standing truths

- A fast path that skips a check is a vulnerability.
- A check that allocates per request is a regression.
- A cache that ignores who asked is a data leak.
- Any map keyed by attacker-supplied input needs a bound and an eviction.
- Every bug fix ships a test that fails before and passes after, with the root
  cause named in one sentence.
- An optional dependency is an interface field, so it is never assigned a nil
  pointer — a nil pointer in an interface is not a nil interface.
- Kernel tests skip themselves. Check the skip count, not the exit code.

## Before you start

Read `.serena/memories/core.md` and whichever memory it points at for the area
you are touching. It is shorter than the code and says why the code is like that.
