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
verifies against the chain head the gateway sealed.

**Resolved 2026-09-11 in `9ff98f98`, and confirmed 2026-09-20.** It is a
deliberate tamper demonstration: session `f37acc68`'s recording with its header
rewritten by hand to claim `3ae54ffe`. Header and trailer name different
sessions, which no gateway produces, so it verifies against neither -- exactly
the swap the hash chain exists to catch. It now lives in the tree as
`internal/recorder/testdata/relabelled-session.cast` with three tests in
`internal/recorder/verify_test.go`, under a README that explains it.

The two bucket copies were byte-identical to the committed fixture -- all three
sha256 to `ffa934e9…` -- and were uploaded 20 seconds apart on 2026-09-01, which
is what planting the same file under two spellings by hand looks like. **Removed
from the dev bucket on 2026-09-20**, on the strength of that: the bytes live in
git, so restoring them is `git show` and `mc cp`.

**The drill passes end to end now.** 4,220 events restored with a matching head,
12,263 of 12,269 recordings verified against their stored chain heads.

That was the control working correctly, and the drill passes now they are gone.
Confirm both directions when changing it.

**Six recordings have no stored chain head, and the drill says so rather than
counting them.** 12,263 verified + 6 unverifiable = the 12,269 in the bucket, so
nothing is quietly skipped. Traced 2026-09-20; five are a real defect and one is
litter.

- `0001/01/01/sess-9.cast`, 14 bytes, no session row. A hand-made artefact, the
  zero-value date prefix being what an unset `StartedAt` writes.
- Five belong to sessions stuck in `active` since 2026-09-07, with
  `ended_at IS NULL` and no head. They are part of a larger set.

## A terminal state with no end time

`ended_at IS NULL` and an empty `chain_head` correlate **exactly** -- every
session with an end time has a head, every session without one has neither:

| state | ended_at null | rows |
| :--- | :--- | ---: |
| closed | no | 12,276 |
| terminated | no | 30 |
| **terminated** | **yes** | **26** |
| **active** | **yes** | **15** |

Twenty-six sessions say they were terminated and also say they never ended. The
fifteen `active` ones have been live for 13-19 days.

**Cause, fixed 2026-09-20.** `Session.report` wrote `endedAt` *inside*
`if chainHead != ""`, and `Session.Close` returns `("", nil)` when
`s.rec == nil` -- no recorder, no error, no head. So a session that ended
without a recorder was reported with a terminal state and no end time, and the
control plane's `COALESCE(EXCLUDED.ended_at, sessions.ended_at)` kept the NULL.

An end time is now written whenever the state is terminal, in all three report
paths -- SSH, native RDP and browser RDP, which all had the same shape. The seal
keeps its own branch: an unsealed recording still carries no `chainHead` and no
byte count, because neither is true of it. What changed is that the session no
longer claims to be running.

`UpsertSession` also fills a missing end time for any terminal state, because
this side owns the invariant and an older gateway reporting into a newer control
plane is exactly when it would be broken. It is applied in Go rather than in the
`ON CONFLICT` clause so it covers the first report too, which is an INSERT.

**The 41 existing bad rows are left alone.** We do not know when those sessions
ended, and stamping them `now()` would invent a time rather than record one.

**Closed 2026-09-20.** The 15 stuck in `active` were the second half of this,
and nothing can reap them -- see the session-liveness section in
`shutdown-and-evidence.md` for why, and for what was built instead. The console
no longer counts them as live.

**The rows are still there, and the console now renders them honestly.** A
session with no end time can be any of three things and only one of them is
"running": `SessionDuration` in `web/src/components/primitives.tsx` is the single
place that decides. A terminal session with no end time gets no figure at all --
"—" with the reason in a tooltip -- because we know it stopped and not when.
Before that, `duration(startedAt, null)` counted to now at every call site, so
the 26 terminated rows rendered as up to 310 hours of uptime on the Overview, the
sessions list and the asset detail.

The Overview's "Live sessions" card had a second, independent version of the
same overstatement: it fetches `state = 'active'` and badged the whole count, so
it said 17 while the Stat tile two rows above said 2. It now filters on
`state === 'active' && !silent` explicitly rather than trusting the query string
to have narrowed it -- splitting a state filter across server and client is what
produced the disagreement.

**Why it matters beyond tidiness.** `fidelityUnsupported` in
`internal/control/api.go` returns false when `EndedAt == nil`, on the reasonable
ground that a session in flight has legitimately observed nothing yet -- so
these sessions are exempt from the evidence check by accident. The console
computes elapsed from `now()` against a null `endedAt`, which is why the
Overview shows sessions running for 287 hours. And the split is worst where it
matters most: every one of 12,276 `closed` sessions is sealed, against 30 of 56
`terminated` ones. An administrator kills a session because something is wrong,
and that is the session whose recording may never become evidence.

## Nil host key store wedges rather than panics

`hostkey.Store.Lookup` now returns `(Pin{}, false)` on a nil receiver. On
darwin/arm64 `RLock` on a nil `*Store` does not panic -- it spins with no
recoverable stack, so a gateway assembled without a store presents as a hung
session. `New()` already refuses one; this covers hand-built stores in tests.
