# Replay player design

The constraint: a recording can be hours long, and the console must scrub it
without holding it on the main thread.

## What was wrong (fixed 2026-09-04)

The player rebuilt the **entire accumulated scrollback** on every animation
frame — re-ran two regexes over all output so far, then reconciled one DOM
`<span>` per style run. Work proportional to the whole recording, 60 times a
second. It also held every frame as a `{t, kind, data}` object in React state,
and asciicast emits roughly one frame per keystroke.

## How it works now

`Replay.tsx` renders through **xterm.js** — the same emulator that draws a live
session, so a recording and the session it recorded cannot disagree. Full-screen
programs (vim, htop, less) replay correctly instead of showing raw redraw
escapes.

The worker owns the frames. The main thread holds metadata and asks for text:

- `advance(from, to)` → output emitted in `(from, to]`. Ordinary playback.
- `screen(t)` → full contents at `t`, from the nearest keyframe. Seeking only.

Both are bounded — one by how far the cursor moved, the other by the keyframe
interval. Requests are id-correlated and **one in flight at a time**: if the
worker is slower than the frame rate the player asks for a bigger slice next
time rather than queueing 60 requests a second.

Backward seek clears and replays from a keyframe. A terminal cannot un-apply
bytes it has already been given.

`onTimeChange` is throttled to whole seconds. Forwarding every frame re-rendered
the whole session page — command timeline included — to move a highlight that
only ever moves between commands.

## The invariant worth protecting

Successive `deltaBetween` slices must compose to exactly what one `screenAt`
produces. If they diverge, a scrubbed recording shows something the session
never displayed. `lib/__tests__/castDecode.test.ts` asserts it directly.

## RDP replay

Same shape, different pixels: `rdpreplay.worker.ts` holds the protocol stream
and turns only the requested window into ImageBitmaps, each `close()`d the
moment it is drawn. Its frame index is **binary-searched** — it used to be a
full linear scan on every request, contradicting the comment above it.

Both players share `PlayerControls.tsx`. They had diverged: terminal replay had
five speeds, skips and shortcuts; desktop replay had none and ran at 1x only.


## Measured

A 35 MB asciicast — 213,000 frames, 75 minutes — decoded and played in the real
browser: the main-thread JS heap held flat at ~104 MB across decode, playback at
8x, and seven shift-seeks. It does not grow with the recording, because the
frames stay in the worker and the terminal's own buffer is capped
(`scrollback: 5_000`, ~34 visible rows). This is the property the rewrite
exists to guarantee; measure it again with `public/big.cast` if the worker
boundary is ever touched.

## The player must never substitute the demo recording for a real one

`buildCast` writes a scripted session -- `systemctl status payments-worker`,
invented output, invented colours. It is the right thing to show in the demo
build and a fabrication anywhere else.

`live.recording()` used to answer `null` for every failure, so the page could
not tell **"there is no control plane to ask"** from **"one answered and said
the artefact is not there"**, and fell back to the fiction in both cases. In the
dev control plane 18 sessions are sealed, carry real recorded bytes, and have an
artefact still sitting on the gateway that produced them -- every one of them
replayed as a story, on a page badged as live data.

It now returns `RecordingResult`: `null` means unconfigured and nothing else,
`{ kind: 'unavailable', reason }` carries the control plane's own wording. The
reason matters and is not flattened into a generic failure -- an artefact still
on the gateway is waiting on a retry, a storage outage is waiting on somebody.

**The RDP path never had this bug**, and comparing the two is how it was found:
`setRdpError('This recording is not available for replay.')` refuses to
fabricate. When the two replay paths on one page disagree about something like
this, the terminal one is the one to check.

This is the page-level instance of the rule in `core.md`: the console must never
present fixture data as real.

## Sessions whose artefact never reached object storage

`chain_head` set and `recording_key` null. The gateway is well-behaved here --
`uploadRecording` queues a failed upload in `pending_uploads.go` and reports
without a `recordingPath`, and the control plane answers the recording endpoint
with a precise 404. The defect was only ever in the console.

Two groups in dev, 44 rows: 26 terminated with zero bytes (nothing was ever
recorded), and 18 closed with real bytes (the artefact exists, on a gateway).
`recordingKey` is now on the console's `Session` type -- the control plane had
always sent it and there was simply no field to put it in. `ArtefactPending` in
`primitives.tsx` marks a finished session whose artefact has not landed, on the
sessions list, the asset detail table and the session detail's fidelity field.

It says nothing about an **active** session: that recording is still being
written and has no business being in storage yet, so keying off `recordingKey`
alone would put the marker on every live row.

The fixture gives most sessions a key and two of them none, the same way it
carries two silent sessions -- a demo fleet where every recording were
unretrievable would say nothing useful.

## Recordings at rest are zstd (2026-11)

Prompted by 8 GB of recordings in a week. Desktop recordings are the bulk: a
synthetic 5-minute 1080p session through the real screen encoder is 587 MB
plain, because every changed 64x64 tile is RLE at 24 bpp, base64'd inside
JSON, twenty times a second. The bucket stored every recording exactly as
shipped, uncompressed; only the host's own copy was gzipped, after an hour.

Now (`internal/recorder/compress.go`):

- Written plain while live (a killed agent still leaves a replayable
  prefix), compressed at seal by `Uploads.Pass` (zstd "better", 32 MB
  window), read back to the same sha256 before the plain file goes.
- Shipped compressed only to a control plane whose heartbeat reply carries
  `recording_encodings: ["zstd"]`; to an older one the agent streams it
  decompressed. An old control plane would verify zstd bytes as lines, fail,
  and refuse the evidence permanently -- that is why it is negotiated.
- The control plane compresses anything that arrives plain before storing.
  Keys are `<date>/<id>.cast.zst` / `.argusrdp.zst`.
- **The chain covers lines, not file bytes.** `recorder.Verify` reads through
  zstd and gzip itself, so every verifier (upload, replay, backup drill,
  argus-verify) works unchanged. Readers call `recorder.Open`.
- Archive (`internal/control/recording_archive.go`): past the archive age
  (default 30 days), recompress at zstd best / 128 MB window, verify *the new
  copy*, store under `archive/`, conditional UPDATE of the key (two replicas
  cannot both act), then delete the old object and audit
  `recording.archived`. A stored copy that fails verification is never
  archived; it is audited once as `recording.unarchivable`.
- Only instant-retrieval storage classes are allowed for the archive.

Measured: desktop 18.6x hot, 28.9x archived (gzip was 4.1x); terminal ~5x.
The base64 framing still costs about 2x after compression (the same desktop
as length-prefixed binary compresses to 9.6 MB); hex does not help. Removing
it means a binary recording format, which touches the line-based chain, the
live tap and replay -- left for its own change.

Replay still buffers the decompressed recording up to 256 MB in memory, so a
desktop recording longer than a few minutes is cut off and then reported as
failing verification. Compression does not change that bound.
