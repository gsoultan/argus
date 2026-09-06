/// <reference lib="webworker" />
/**
 * asciicast decoding and playback service.
 *
 * The decoded recording stays *here*. It used to be posted to the main thread —
 * every frame as its own `{t, kind, data}` object, plus a keyframe holding an
 * accumulated copy of all output every five seconds — and then held in React
 * state. A recording emits roughly one frame per keystroke, so an hour-long
 * session put hundreds of thousands of objects and a chain of ever-larger
 * strings on the main thread's heap, where they stayed for as long as the page
 * was open.
 *
 * Now the main thread receives metadata and asks for text: `screen` for a seek,
 * `advance` for ordinary playback. Both answers are bounded — one by the
 * keyframe interval, the other by how far the cursor moved — so the cost of
 * drawing a frame no longer grows with how much of the recording is behind it.
 */

import {
  decodeCast,
  deltaBetween,
  screenAt,
  type CastHeader,
  type Decoded,
  type KernelExec,
} from '~/lib/castDecode'
import { extractCommands, type TimedCommand } from '~/lib/ansi'

export type { CastHeader, KernelExec } from '~/lib/castDecode'

export type ReplayRequest =
  | { type: 'decode'; source: string; keyframeIntervalMs?: number }
  /** Full screen contents at `t` — for seeking, and for the initial paint. */
  | { type: 'screen'; id: number; t: number }
  /** Output emitted in `(from, to]` — for ordinary forward playback. */
  | { type: 'advance'; id: number; from: number; to: number }

export type ReplayResponse =
  | { type: 'progress'; done: number; total: number }
  | {
      type: 'decoded'
      header: CastHeader
      /** Kernel-observed executions; empty for a PTY-only recording. */
      execs: KernelExec[]
      /**
       * Commands inferred from the PTY stream.
       *
       * Computed here because it needs every frame, and the frames no longer
       * leave this worker. Only populated when there is no kernel evidence —
       * a guess presented next to real evidence invites reading them as equals.
       */
      heuristicCommands: TimedCommand[]
      frameCount: number
      duration: number
      /** Byte total of stdout, for the "recording size" readout. */
      outputBytes: number
      ms: number
    }
  | { type: 'screen'; id: number; text: string; t: number }
  | { type: 'advance'; id: number; text: string; to: number }
  | { type: 'error'; message: string }

const post = (m: ReplayResponse) => (self as unknown as Worker).postMessage(m)

let decoded: Decoded | null = null

self.onmessage = (ev: MessageEvent<ReplayRequest>) => {
  const msg = ev.data

  try {
    if (msg.type === 'decode') {
      const started = performance.now()
      decoded = decodeCast(msg.source, msg.keyframeIntervalMs, (done, total) =>
        post({ type: 'progress', done, total }),
      )
      post({
        type: 'decoded',
        header: decoded.header,
        execs: decoded.execs,
        heuristicCommands:
          decoded.execs.length > 0 ? [] : extractCommands(decoded.frames),
        frameCount: decoded.frames.length,
        duration: decoded.duration,
        outputBytes: decoded.outputBytes,
        ms: Math.round(performance.now() - started),
      })
      return
    }

    if (!decoded) {
      post({ type: 'error', message: 'no recording decoded' })
      return
    }

    if (msg.type === 'screen') {
      post({
        type: 'screen',
        id: msg.id,
        text: screenAt(decoded.frames, decoded.keyframes, msg.t),
        t: msg.t,
      })
      return
    }

    if (msg.type === 'advance') {
      post({
        type: 'advance',
        id: msg.id,
        text: deltaBetween(decoded.frames, msg.from, msg.to),
        to: msg.to,
      })
    }
  } catch (err) {
    post({ type: 'error', message: err instanceof Error ? err.message : String(err) })
  }
}
