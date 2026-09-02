/// <reference lib="webworker" />
/**
 * asciicast v2 decoding + indexing worker.
 *
 * asciicast v2 is newline-delimited JSON: a header object, then one array per
 * event — `[time, type, data]` where type is "o" (stdout) or "i" (stdin).
 * See https://docs.asciinema.org/manual/asciicast/v2/
 *
 * Scrubbing a terminal recording is not like scrubbing video: to know what the
 * screen looks like at t=90s you must have applied every byte before it. So we
 * decode once here and build periodic *keyframes* — snapshots of accumulated
 * output — letting the player seek to the nearest keyframe and replay only the
 * remainder. Doing this on the main thread stalls the UI on long sessions.
 */

export interface CastHeader {
  version: number
  width: number
  height: number
  timestamp?: number
  title?: string
  env?: Record<string, string>
}

/**
 * One execution the kernel observed, carried in the recording's `x` stream.
 *
 * Inside the hash chain like every other frame, so the command list is exactly
 * as tamper-evident as the terminal output beside it.
 */
export interface KernelExec {
  t: number
  pid: number
  ppid: number
  uid: number
  comm: string
  filename: string
  args: string[]
  truncated?: boolean
}

export interface CastFrame {
  t: number
  kind: 'o' | 'i'
  data: string
}

/** Accumulated output at a checkpoint, so seeking doesn't replay from zero. */
export interface Keyframe {
  t: number
  frameIndex: number
  text: string
}

export type ReplayRequest = { type: 'decode'; source: string; keyframeIntervalMs?: number }

export type ReplayResponse =
  | { type: 'progress'; done: number; total: number }
  | {
      type: 'decoded'
      execs: KernelExec[]
      header: CastHeader
      frames: CastFrame[]
      keyframes: Keyframe[]
      duration: number
      /** Byte total of stdout, for the "recording size" readout. */
      outputBytes: number
      ms: number
    }
  | { type: 'error'; message: string }

const post = (m: ReplayResponse) => (self as unknown as Worker).postMessage(m)

self.onmessage = (ev: MessageEvent<ReplayRequest>) => {
  const started = performance.now()
  const { source, keyframeIntervalMs = 5_000 } = ev.data

  try {
    const lines = source.split('\n').filter((l) => l.trim().length > 0)
    if (lines.length === 0) throw new Error('empty recording')

    const header = JSON.parse(lines[0]!) as CastHeader
    if (header.version !== 2) {
      throw new Error(`unsupported asciicast version ${header.version}, expected 2`)
    }

    const frames: CastFrame[] = []
    const execs: KernelExec[] = []
    const keyframes: Keyframe[] = []
    let acc = ''
    let outputBytes = 0
    let nextKeyframeAt = 0

    for (let i = 1; i < lines.length; i++) {
      const raw = lines[i]!
      let parsed: [number, string, string]
      try {
        parsed = JSON.parse(raw) as [number, string, string]
      } catch {
        continue // tolerate a truncated tail — recordings can be cut mid-write
      }
      const [t, kind, data] = parsed

      if (kind === 'x') {
        // Kernel evidence: what actually ran, as opposed to what the terminal
        // displayed. Rendered as its own lane rather than mixed into the
        // output, because the two are different kinds of claim.
        try {
          const e = JSON.parse(data) as Omit<KernelExec, 't'>
          execs.push({ ...e, t })
        } catch {
          // A frame this build cannot read is skipped rather than failing the
          // whole replay: an unreadable command must not cost the recording.
        }
        continue
      }
      if (kind !== 'o' && kind !== 'i') continue

      frames.push({ t, kind, data })
      if (kind === 'o') {
        acc += data
        outputBytes += data.length
      }

      if (t * 1000 >= nextKeyframeAt) {
        keyframes.push({ t, frameIndex: frames.length - 1, text: acc })
        nextKeyframeAt = t * 1000 + keyframeIntervalMs
      }

      if (i % 500 === 0) post({ type: 'progress', done: i, total: lines.length })
    }

    const last = frames.at(-1)
    post({
      type: 'decoded',
      execs,
      header,
      frames,
      keyframes,
      duration: last ? last.t : 0,
      outputBytes,
      ms: Math.round(performance.now() - started),
    })
  } catch (err) {
    post({ type: 'error', message: err instanceof Error ? err.message : String(err) })
  }
}
