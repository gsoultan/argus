# Capacity, load testing and the restore drill

## What one gateway holds

Measured 2026-09-07 on one machine: one gateway, one target, one host.
Published in `README.md` under "Measured capacity".

| concurrent | succeeded | connect+auth p95 | session+PTY p95 | gateway RSS |
| ---------- | --------- | ---------------- | --------------- | ----------- |
| 50         | 100%      | 19 ms            | 195 ms          | 30 MB       |
| 100        | 100%      | 20 ms            | 371 ms          | 40 MB       |
| 200        | 100%      | 36 ms            | 703 ms          | 50 MB       |
| 400        | 52%       | 31 ms            | 1274 ms         | 65 MB       |

**200 concurrent is the number to plan against.** Idle steady state is ~24 MB.
Across 1232 sessions: 1232 recordings sealed and uploaded, zero panics, no
upward memory trend across repeated 200-session runs.

The 400-session failures were **not** the gateway. The target refused TCP
(`dial tcp …: connect: operation timed out`) and the gateway reported each
refusal with its reason instead of hanging.

## Two things will masquerade as a gateway limit

Both must be raised before a run means anything, and restored afterwards.

1. `dev/argus.yaml` sets `rate_limit.connections_per_minute: 6`. That is a
   defence against password spraying, not a capacity ceiling.
2. The target's sshd defaults to `MaxStartups 10:30:100` and starts refusing at
   ten unauthenticated connections. Left alone it caps success near 76% and
   looks exactly like a gateway fault. `MaxSessions 10` matters too.

`scripts/tools/loadtest` documents both in its header. It does a full session
each time -- TCP, handshake, auth, PTY, a command whose output is checked, a
hold, a clean close. A test that stops after the handshake measures the
listener, not the product.

**A load harness that races manufactures its own findings.** The first version
pointed `Stdout` and `Stderr` at one `bytes.Buffer`; x/crypto/ssh fills those
from two goroutines, output vanished at a rate that rose with load, and healthy
sessions were scored as failures. Guard the writer.

## The restore drill lied for its whole life

`scripts/drill.sh` step 6 iterated with `while read` fed from stdin and called
`container exec -i` inside the loop. `exec -i` consumed the remaining
filenames on the first iteration. The drill checked **one** recording out of
23, printed "1 recording(s) verified", and passed.

Fixed by reading on fd 3, covering `.argusrdp` as well as `.cast`, and refusing
to report at all unless `checked + failed + unknown == n`. That last guard is
the important one: it is the only way this check can lie while looking green.

Run against a real backup the fix turned "1 verified, PASSED" into 24 verified
and 2 that do not match their sealed chain heads.

## The drill verified nothing for a long time, and said so

Fixed 2026-09-11. Two faults, the second hidden by the first.

The chain-head lookup ran one `container exec` per recording: 54 ms a round
trip, eleven minutes on a 12,271-file backup, and it never finished. One query
joined against the file list now.

With it finishing, the real fault appeared. The same session id is written two
ways — `newSessionID()` is `hex.EncodeToString(16 bytes)`, 32 characters and no
dashes, and the object key uses it verbatim, while `sessions.id` is a `uuid`
column that reads back dashed. Compared as strings they never match, so the
drill found a stored chain head for **none** of the 12,268 real recordings and
counted every one "unverifiable". Both sides are normalised now.

  before   did not complete
  after    12,263 verified, 6 with no stored head, 41 seconds

That was the first run to reach the pass path. Note the reporting order: the
`failed > 0` check fires before the `checked == 0` check, so "2 corrupt" was
printed while 12,269 files were going unchecked. The louder failure hid the
bigger one.

The product's read path never depended on this. The console fetches a recording
by the `recording_key` stored on the session row, never by deriving it from the
id. Only the drill maps the other way.

**Still open:** session ids are stored as `uuid` but generated as bare hex, so
the object key and the database id are different strings for the same session.
Making them agree is a data migration — every object already in every bucket
carries the undashed name — so the drill normalises instead.

## The reporting order hid the real fault

Fixed 2026-09-11. The corruption check ran before the coverage check and
exited, so a run that found two bad files said "2 of 12271 recording(s) are
corrupt" and stopped -- while 12,269 had gone unverified in the same pass.

That is how the id-format mismatch above stayed hidden for as long as it did.
A small true statement standing in front of a large one. Coverage is reported
first now, and a backup where more than half the recordings have no stored
chain head fails on that by name, whether or not anything else is wrong.

If a check can only report one thing, make sure it is the one that says how
much of the job it actually did.

## Known-bad artefacts in the dev object store

Session `3ae54ffe-7cd2-faf2-c920-559e98b9f713` exists in the MinIO bucket under
**two** filenames (dashed and undashed UUID), both 453 bytes, and neither
verifies against the chain head the gateway sealed. The contents contain
`echo INNOCENT-COMMAND`, so they are almost certainly a hand-planted tamper
demo from earlier work -- but nothing records that, so they have been left in
place as evidence rather than deleted.

**Consequence: `scripts/drill.sh` fails on a full backup until these are
resolved.** That is the control working correctly. The other 24 recordings
verify. Confirm both directions when changing the drill.

## Nil host key store wedges rather than panics

`hostkey.Store.Lookup` now returns `(Pin{}, false)` on a nil receiver. On
darwin/arm64 `RLock` on a nil `*Store` does not panic -- it spins with no
recoverable stack, so a gateway assembled without a store presents as a hung
session. `New()` already refuses one; this covers hand-built stores in tests.
