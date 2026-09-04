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
